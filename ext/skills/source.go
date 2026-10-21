package skills

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

type Frontmatter struct {
	Name           string            `json:"name"`
	Description    string            `json:"description"`
	License        string            `json:"license,omitempty"`
	Compatibility  string            `json:"compatibility,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	AllowedTools   []string          `json:"allowed_tools,omitempty"` // experimental in the spec; parsed, not enforced
	UserInvocable  bool              `json:"user_invocable"`          // vendor field user-invocable, default true
	ModelInvocable bool              `json:"model_invocable"`         // negation of disable-model-invocation, default true
	Extra          map[string]any    `json:"extra,omitempty"`         // other vendor fields kept verbatim (when_to_use, arguments ...)
	Location       string            `json:"location"`
}

type Skill struct {
	Frontmatter
	Body      string   // SKILL.md without frontmatter
	Resources []string // relative paths, listed but never read eagerly
}

type Source interface {
	List(ctx context.Context) ([]Frontmatter, error)
	Load(ctx context.Context, name string) (*Skill, error)
	Open(ctx context.Context, name, path string) (io.ReadCloser, error)
}

var (
	ErrNotFound    = errors.New("skills: not found")
	ErrInvalidPath = errors.New("skills: invalid resource path")
)

// Scan limits, so a mistaken root such as a home directory costs a bounded walk.
const (
	maxDepth = 4
	maxDirs  = 2000
)

var skillName = regexp.MustCompile(`^[a-z0-9](?:-?[a-z0-9])*$`)

type FSOption func(*fsConfig)

type fsConfig struct {
	parse  func(raw []byte) (Frontmatter, error)
	logger *slog.Logger
}

// WithFrontmatterParser plugs a full YAML parser; the default handles the Agent Skills subset only.
func WithFrontmatterParser(fn func(raw []byte) (Frontmatter, error)) FSOption {
	return func(c *fsConfig) { c.parse = fn }
}

// WithLogger receives the warnings about skills loaded leniently or skipped; they are discarded by default.
func WithLogger(l *slog.Logger) FSOption { return func(c *fsConfig) { c.logger = l } }

type fsSource struct {
	fsys fs.FS
	cfg  fsConfig
	mu   sync.Mutex
	dirs map[string]string // skill name -> its directory, from the last scan
}

// FS discovers <name>/SKILL.md directories inside fsys; embed.FS and os.DirFS both work.
func FS(fsys fs.FS, opts ...FSOption) Source {
	cfg := fsConfig{parse: parseFrontmatter, logger: slog.New(slog.DiscardHandler)}
	for _, o := range opts {
		o(&cfg)
	}
	return &fsSource{fsys: fsys, cfg: cfg}
}

func skipDir(name string) bool { return name == ".git" || name == "node_modules" }

// List scans afresh, so a source over a live directory reflects it whenever the extension lists again.
func (f *fsSource) List(ctx context.Context) ([]Frontmatter, error) {
	var out []Frontmatter
	dirs := map[string]string{}
	seen := 0
	err := fs.WalkDir(f.fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == "." {
				return err
			}
			return nil // an unreadable corner does not hide the rest
		}
		if !d.IsDir() {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if p != "." && (skipDir(d.Name()) || strings.Count(p, "/")+1 > maxDepth) {
			return fs.SkipDir
		}
		if seen++; seen > maxDirs {
			f.cfg.logger.Warn("skills: directory limit reached, the rest is not scanned", "limit", maxDirs)
			return fs.SkipAll
		}
		doc, err := fs.ReadFile(f.fsys, path.Join(p, "SKILL.md"))
		if err != nil {
			return nil
		}
		fm, ok := f.parse(p, doc)
		if !ok {
			return fs.SkipDir
		}
		if prev, dup := dirs[fm.Name]; dup {
			f.cfg.logger.Warn("skills: duplicate name, the first one wins", "name", fm.Name, "kept", prev, "skipped", p)
			return fs.SkipDir
		}
		dirs[fm.Name] = p
		out = append(out, fm)
		return fs.SkipDir // a skill's own subdirectories are its resources
	})
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.dirs = dirs
	f.mu.Unlock()
	slices.SortFunc(out, func(a, b Frontmatter) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// parse reads one SKILL.md leniently: only a name that breaks the Agent Skills rules, a missing description or frontmatter that never parses skips it.
func (f *fsSource) parse(dir string, doc []byte) (Frontmatter, bool) {
	loc := path.Join(dir, "SKILL.md")
	raw, _, err := split(doc)
	if err != nil {
		f.cfg.logger.Warn("skills: skipped", "location", loc, "error", err)
		return Frontmatter{}, false
	}
	fm, err := f.cfg.parse(raw)
	if err != nil {
		if fm, err = f.cfg.parse(quoteColons(raw)); err != nil {
			f.cfg.logger.Warn("skills: skipped, frontmatter does not parse", "location", loc, "error", err)
			return Frontmatter{}, false
		}
	}
	base := path.Base(dir)
	if dir == "." {
		base = ""
	}
	switch {
	case fm.Name == "" && base == "":
		f.cfg.logger.Warn("skills: skipped, no name", "location", loc)
		return Frontmatter{}, false
	case fm.Name == "":
		fm.Name = base
		f.cfg.logger.Warn("skills: no name, using the directory's", "location", loc)
	case fm.Name != base:
		f.cfg.logger.Warn("skills: name differs from its directory", "name", fm.Name, "location", loc)
	}
	if !skillName.MatchString(fm.Name) || len(fm.Name) > 64 {
		f.cfg.logger.Warn("skills: skipped, name does not follow the Agent Skills rules", "name", fm.Name, "location", loc)
		return Frontmatter{}, false // listed, it would show a skill whose /name command cannot register
	}
	if strings.TrimSpace(fm.Description) == "" {
		f.cfg.logger.Warn("skills: skipped, no description", "name", fm.Name, "location", loc)
		return Frontmatter{}, false
	}
	fm.Location = loc
	return fm, true
}

// quoteColons double-quotes plain top-level values that contain ": ", which a strict YAML parser reads as a nested map.
func quoteColons(raw []byte) []byte {
	lines := strings.Split(string(raw), "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, " ") || strings.HasPrefix(l, "#") {
			continue
		}
		key, v, ok := strings.Cut(l, ": ")
		v = strings.TrimSpace(v)
		if !ok || v == "" || strings.ContainsAny(v[:1], "\"'|>[{") || !strings.Contains(v, ": ") {
			continue
		}
		lines[i] = key + ": " + strconv.Quote(v)
	}
	return []byte(strings.Join(lines, "\n"))
}

func (f *fsSource) dir(ctx context.Context, name string) (string, error) {
	f.mu.Lock()
	d, ok := f.dirs[name]
	scanned := f.dirs != nil
	f.mu.Unlock()
	if ok {
		return d, nil
	}
	if !scanned {
		if _, err := f.List(ctx); err != nil {
			return "", err
		}
		return f.dir(ctx, name)
	}
	return "", ErrNotFound
}

func (f *fsSource) Load(ctx context.Context, name string) (*Skill, error) {
	dir, err := f.dir(ctx, name)
	if err != nil {
		return nil, err
	}
	doc, err := fs.ReadFile(f.fsys, path.Join(dir, "SKILL.md"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	fm, ok := f.parse(dir, doc)
	if !ok {
		return nil, ErrNotFound
	}
	_, body, _ := split(doc)
	sk := &Skill{Frontmatter: fm, Body: body}
	seen := 0
	_ = fs.WalkDir(f.fsys, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || ctx.Err() != nil {
			return nil
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, dir), "/")
		if dir == "." {
			rel = p
		}
		if d.IsDir() {
			if p != dir && (skipDir(d.Name()) || strings.Count(rel, "/")+1 > maxDepth) {
				return fs.SkipDir
			}
			return nil
		}
		if seen++; seen > maxDirs {
			return fs.SkipAll
		}
		if rel != "SKILL.md" && d.Type().IsRegular() {
			sk.Resources = append(sk.Resources, rel)
		}
		return nil
	})
	slices.Sort(sk.Resources)
	return sk, nil
}

// Open refuses any path that could leave the skill's directory: .., absolute or encoded paths, backslashes and symbolic links.
func (f *fsSource) Open(ctx context.Context, name, rel string) (io.ReadCloser, error) {
	if !validResource(rel) {
		return nil, ErrInvalidPath
	}
	dir, err := f.dir(ctx, name)
	if err != nil {
		return nil, err
	}
	full := path.Join(dir, rel)
	if lf, ok := f.fsys.(fs.ReadLinkFS); ok {
		for p := full; p != dir && p != "."; p = path.Dir(p) {
			info, err := lf.Lstat(p)
			if errors.Is(err, fs.ErrNotExist) {
				return nil, ErrNotFound
			}
			if err != nil {
				return nil, err
			}
			if info.Mode()&fs.ModeSymlink != 0 {
				return nil, ErrInvalidPath
			}
		}
	}
	file, err := f.fsys.Open(full)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, ErrInvalidPath
	}
	return file, nil
}

func validResource(p string) bool {
	return p != "" && p != "." && utf8.ValidString(p) && fs.ValidPath(p) &&
		!strings.ContainsAny(p, `\%:`) && !strings.HasPrefix(p, "/")
}

// ---- Merge ----

type merged struct{ sources []Source }

// Merge lets earlier sources win on name collisions; the extension logs the shadowing among its own sources.
func Merge(sources ...Source) Source { return &merged{sources: slices.Clone(sources)} }

func (m *merged) List(ctx context.Context) ([]Frontmatter, error) {
	list, _, err := listAll(ctx, m.sources)
	return list, err
}

func (m *merged) owner(ctx context.Context, name string) (Source, error) {
	_, owners, err := listAll(ctx, m.sources)
	if err != nil {
		return nil, err
	}
	if src, ok := owners[name]; ok {
		return src, nil
	}
	return nil, ErrNotFound
}

func (m *merged) Load(ctx context.Context, name string) (*Skill, error) {
	src, err := m.owner(ctx, name)
	if err != nil {
		return nil, err
	}
	return src.Load(ctx, name)
}

func (m *merged) Open(ctx context.Context, name, p string) (io.ReadCloser, error) {
	src, err := m.owner(ctx, name)
	if err != nil {
		return nil, err
	}
	return src.Open(ctx, name, p)
}

type shadow struct {
	name   string
	winner string // location of the skill that stays
	loser  string
}

// listAll merges sources in order, the first of a name winning, sorted by name.
func listAll(ctx context.Context, sources []Source) ([]Frontmatter, map[string]Source, error) {
	list, owners, _, err := mergeLists(ctx, sources)
	return list, owners, err
}

func mergeLists(ctx context.Context, sources []Source) ([]Frontmatter, map[string]Source, []shadow, error) {
	var out []Frontmatter
	owners := map[string]Source{}
	at := map[string]string{}
	var shadows []shadow
	for _, src := range sources {
		list, err := src.List(ctx)
		if err != nil {
			return nil, nil, nil, err
		}
		for _, fm := range list {
			if _, dup := owners[fm.Name]; dup {
				shadows = append(shadows, shadow{name: fm.Name, winner: at[fm.Name], loser: fm.Location})
				continue
			}
			owners[fm.Name], at[fm.Name] = src, fm.Location
			out = append(out, fm)
		}
	}
	slices.SortFunc(out, func(a, b Frontmatter) int { return strings.Compare(a.Name, b.Name) })
	return out, owners, shadows, nil
}
