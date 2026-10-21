package compare

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Case is one case found under a cases root: its id relative to that root, the case's path, and its output directory.
type Case struct {
	ID   string
	Path string
	Out  string
}

// Cases finds the cases under root, a conformance directory, a subtree of it or one case, and pairs each with its directory under out; a case only out has, one not yet given expected files, is included too.
func Cases(root, out string) ([]Case, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return []Case{{ID: ".", Path: abs, Out: out}}, nil
	}
	found := map[string]string{}
	err = filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case !d.IsDir():
			if strings.HasSuffix(p, ".json") && filepath.Base(filepath.Dir(filepath.Dir(p))) == "errors" {
				found[strings.TrimSuffix(rel(abs, p), ".json")] = p
			}
		case strings.HasPrefix(d.Name(), "expected."):
			return filepath.SkipDir
		case holds(p, "expected."):
			found[rel(abs, p)] = p
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if outAbs, err := filepath.Abs(out); err == nil {
		_ = filepath.WalkDir(outAbs, func(p string, d fs.DirEntry, err error) error {
			switch {
			case err != nil || !d.IsDir():
				return err
			case strings.HasPrefix(d.Name(), "actual."):
				return filepath.SkipDir
			case holds(p, "actual."):
				id := rel(outAbs, p)
				if _, ok := found[id]; !ok {
					path := filepath.Join(abs, filepath.FromSlash(id))
					if _, err := os.Stat(path + ".json"); err == nil {
						path += ".json"
					}
					found[id] = path
				}
			}
			return nil
		})
	}
	ids := make([]string, 0, len(found))
	for id := range found {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	cases := make([]Case, len(ids))
	for i, id := range ids {
		cases[i] = Case{ID: id, Path: found[id], Out: filepath.Join(out, filepath.FromSlash(id))}
	}
	return cases, nil
}

func rel(root, p string) string {
	r, _ := filepath.Rel(root, p)
	return filepath.ToSlash(r)
}

// holds reports whether dir has an entry whose name starts with prefix.
func holds(dir, prefix string) bool {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			return true
		}
	}
	return false
}
