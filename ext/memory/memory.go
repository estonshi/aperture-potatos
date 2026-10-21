// Package memory writes instruction files and the long-term memory of a user and project into the head of every new epoch, lets the model change that memory, and consolidates it when a session ends or compacts.
package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/spec"
)

// Name is the extension's name; it is also the name of its hooks and the bucket of its model calls.
const Name = "memory"

const (
	instructionsCap   = 25 << 10
	defaultShared     = 10000
	defaultUser       = 10000
	consolidateWait   = 60 * time.Second
	consolidateOutput = 2048
)

// Scope is the session's user and project, the tags its memories are matched on, and where a change came from.
type Scope struct {
	User    string `json:"user,omitempty"`
	Project string `json:"project,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Session string `json:"session,omitempty"`
}

type InstructionDoc struct {
	Source  string `json:"source"` // e.g. AGENTS.md
	Content string `json:"content"`
}

// InstructionSource is loaded in order at the start of every epoch.
type InstructionSource interface {
	Load(ctx context.Context, scope Scope) ([]InstructionDoc, error)
}

// Item is one memory; an empty tag matches every user or every project.
type Item struct {
	ID        string            `json:"id"`
	Content   string            `json:"content"`
	User      string            `json:"user"`
	Project   string            `json:"project"`
	Author    string            `json:"author,omitempty"`
	Timestamp time.Time         `json:"timestamp"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// Change is one edit; in an update a nil field keeps its value, and an add gets its new id back from Apply.
type Change struct {
	Op      string  `json:"op"` // add | update | delete
	ID      string  `json:"id,omitempty"`
	Content *string `json:"content,omitempty"`
	User    *string `json:"user,omitempty"`
	Project *string `json:"project,omitempty"`
}

type Store interface {
	// List returns every memory whose user and project tags are each empty or the scope's, oldest first.
	List(ctx context.Context, scope Scope) ([]Item, error)
	// Apply makes all the changes or none, and returns them with the ids of the added memories.
	Apply(ctx context.Context, scope Scope, changes []Change) ([]Change, error)
}

// Ingester is implemented by a store that extracts memories from a conversation itself; the extension then hands it the conversation instead of consolidating.
type Ingester interface {
	Ingest(ctx context.Context, scope Scope, lines []message.Message) ([]Change, error)
}

var (
	ErrNotFound     = errors.New("memory: not found")
	ErrNotInstalled = errors.New("memory: the extension with a store is not installed for this session")
)

type Options struct {
	Instructions  InstructionSource
	Store         Store
	ScopeFunc     func(s *kernel.Session) Scope // default: {User: Labels["user_id"], Project: Labels["project_id"], Agent, Session}
	ConsolidateOn []string                      // default {"pre_compact"}; add "session_end" with a CloseTimeout that fits a model call, or call Consolidate before Close
	UserTokens    int                           // default 10000: the memories with the session's user tag
	SharedTokens  int                           // default 10000: the memories without a user tag
	NoTools       bool
	SharedWrites  bool // let the model add, change and delete memories every user shares; off, its writes stay under the session's user tag
}

// Loaded reports the memories written into an epoch head.
type Loaded struct {
	IDs []string `json:"ids"`
}

func (*Loaded) EventType() event.Type { return "memory.loaded" }

// Saved reports the changes a tool call or a consolidation made.
type Saved struct {
	Changes []Change `json:"changes"`
	Trigger string   `json:"trigger"` // tool, session_end, pre_compact or host
}

func (*Saved) EventType() event.Type { return "memory.saved" }

// Payloads teaches an event.Decoder this extension's events.
func Payloads() []event.Constructor {
	return []event.Constructor{
		{Type: "memory.loaded", New: func() event.Payload { return &Loaded{} }},
		{Type: "memory.saved", New: func() event.Payload { return &Saved{} }},
	}
}

// instKey holds the extension on the agent or session that installed it, where Consolidate finds it.
var instKey = kernel.NewKey[*mem]("memory.ext")

type mem struct {
	o    Options
	mu   sync.Mutex
	open map[*kernel.Session]Scope // started and not ended, so a change reaches the others that load it
}

func New(o Options) kernel.Extension {
	if o.ScopeFunc == nil {
		o.ScopeFunc = defaultScope
	}
	if o.ConsolidateOn == nil {
		o.ConsolidateOn = []string{"pre_compact"} // a close waits at most CloseTimeout, 10s by default, too short for most models
	}
	if o.UserTokens <= 0 {
		o.UserTokens = defaultUser
	}
	if o.SharedTokens <= 0 {
		o.SharedTokens = defaultShared
	}
	return kernel.ExtensionFunc(Name, func(r kernel.Registrar) error {
		m := &mem{o: o, open: map[*kernel.Session]Scope{}}
		if o.Instructions == nil && o.Store == nil {
			return nil
		}
		if _, err := kernel.On(r, kernel.SessionStart, Name, m.start); err != nil {
			return err
		}
		if o.Store == nil {
			return nil
		}
		if s := r.Session(); s != nil {
			kernel.Set(s, instKey, m)
		} else {
			kernel.Set(r.Agent(), instKey, m)
		}
		if _, err := kernel.On(r, kernel.SessionEnd, Name+"_closed", m.end); err != nil {
			return err
		}
		if !o.NoTools {
			if err := m.tools(r); err != nil {
				return err
			}
		}
		for _, on := range o.ConsolidateOn {
			var err error
			switch on {
			case "session_end":
				_, err = kernel.OnAsync(r, kernel.SessionEnd, Name, m.consolidateOnEnd, kernel.Timeout(consolidateWait))
			case "pre_compact": // sync, so the model call sees the lines the compaction is about to take out
				_, err = kernel.On(r, kernel.PreCompact, Name, m.consolidateBeforeCompaction, kernel.Timeout(consolidateWait))
			default:
				err = fmt.Errorf("memory: unknown consolidation point %q", on)
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func defaultScope(s *kernel.Session) Scope {
	info := s.Info()
	return Scope{User: info.Labels["user_id"], Project: info.Labels["project_id"], Agent: info.Agent, Session: info.SessionID}
}

// ---- texts ----

var text = kernel.SpecTexts("ext/memory/texts.md")

func list(items []Item) string {
	if len(items) == 0 {
		return text("empty")
	}
	lines := make([]string, len(items))
	for i, it := range items {
		lines[i] = text("memory", "id", it.ID, "user", it.User, "project", it.Project, "content", strings.Join(strings.Fields(it.Content), " "))
	}
	return strings.Join(lines, "\n")
}

// estimate is the kernel's text heuristic: ASCII at four bytes a token, any other code point one.
func estimate(s string) int {
	ascii, other := 0, 0
	for _, r := range s {
		if r < utf8.RuneSelf {
			ascii++
		} else {
			other++
		}
	}
	return (ascii+3)/4 + other
}

// usage sums the two budgets: memories with the session's user tag, and the rest the session sees.
func usage(items []Item) (user, shared int) {
	for _, it := range items {
		if it.User != "" {
			user += estimate(it.Content)
		} else {
			shared += estimate(it.Content)
		}
	}
	return user, shared
}

// ---- loading ----

// start writes the instructions and the memory into each new epoch's head; a resume keeps the head it has, and a fork or rewind copies one.
func (m *mem) start(ctx context.Context, in kernel.SessionStartInput) (kernel.SessionStartOutput, error) {
	scope := m.o.ScopeFunc(in.Session)
	if m.o.Store != nil {
		m.mu.Lock()
		m.open[in.Session] = scope
		m.mu.Unlock()
	}
	switch in.Source {
	case "new", "clear", "compact":
	default:
		return kernel.SessionStartOutput{}, nil
	}
	var parts []string
	if m.o.Instructions != nil {
		docs, err := m.o.Instructions.Load(ctx, scope)
		if err != nil {
			return kernel.SessionStartOutput{}, err
		}
		for _, d := range docs {
			parts = append(parts, text("instruction_doc", "source", d.Source, "content", d.Content))
		}
	}
	if m.o.Store != nil {
		items, err := m.o.Store.List(ctx, scope)
		if err != nil {
			return kernel.SessionStartOutput{}, err
		}
		parts = append(parts, text("loaded", "user", scope.User, "project", scope.Project, "memories", list(items))) // even empty, so the model knows the tags it may save with
		ids := make([]string, len(items))
		for i, it := range items {
			ids[i] = it.ID
		}
		in.Session.Emit(&Loaded{IDs: ids})
	}
	var out kernel.SessionStartOutput
	out.AdditionalContext = strings.Join(parts, "\n\n")
	return out, nil
}

func (m *mem) end(_ context.Context, in kernel.SessionEndInput) (kernel.SessionEndOutput, error) {
	m.mu.Lock()
	delete(m.open, in.Session)
	m.mu.Unlock()
	return kernel.SessionEndOutput{}, nil
}

// announce tells the other open sessions that load a changed memory what their memory now reads, since their epoch heads still hold the old one.
func (m *mem) announce(ctx context.Context, from *kernel.Session, changed []Item) {
	m.mu.Lock()
	others := map[*kernel.Session]Scope{}
	for s, sc := range m.open {
		if s != from {
			others[s] = sc
		}
	}
	m.mu.Unlock()
	for s, sc := range others {
		if !slices.ContainsFunc(changed, func(it Item) bool { return matches(it, sc) }) {
			continue
		}
		items, err := m.o.Store.List(ctx, sc)
		if err != nil {
			continue
		}
		s.Announce(Name, text("changed", "memories", list(items)), nil)
	}
}

func matches(it Item, sc Scope) bool {
	return (it.User == "" || it.User == sc.User) && (it.Project == "" || it.Project == sc.Project)
}

// ---- changes ----

type changeError struct{ text string }

func (e *changeError) Error() string { return e.text }

// check validates changes against the memories the session sees and both budgets, and returns the memories they touch as they will read.
func (m *mem) check(scope Scope, items []Item, changes []Change) ([]Item, error) {
	next := slices.Clone(items)
	var touched []Item
	tag := func(name string, v *string, want string) error {
		if v != nil && *v != "" && *v != want {
			return &changeError{text("bad_tag", "tag", name, "value", want)}
		}
		return nil
	}
	for _, c := range changes {
		if err := tag("user", c.User, scope.User); err != nil {
			return nil, err
		}
		if err := tag("project", c.Project, scope.Project); err != nil {
			return nil, err
		}
		i := slices.IndexFunc(next, func(it Item) bool { return it.ID == c.ID })
		switch c.Op {
		case "add":
			if c.Content == nil || strings.TrimSpace(*c.Content) == "" {
				return nil, &changeError{text("need_content")}
			}
			it := Item{Content: strings.TrimSpace(*c.Content), User: deref(c.User), Project: deref(c.Project)}
			next = append(next, it)
			touched = append(touched, it)
		case "update":
			if i < 0 {
				return nil, &changeError{text("no_memory", "id", c.ID)}
			}
			it := next[i]
			if c.Content != nil {
				if strings.TrimSpace(*c.Content) == "" {
					return nil, &changeError{text("need_content")}
				}
				it.Content = strings.TrimSpace(*c.Content)
			}
			if c.User != nil {
				it.User = *c.User
			}
			if c.Project != nil {
				it.Project = *c.Project
			}
			touched = append(touched, next[i], it)
			next[i] = it
		case "delete":
			if i < 0 {
				return nil, &changeError{text("no_memory", "id", c.ID)}
			}
			touched = append(touched, next[i])
			next = slices.Delete(next, i, i+1)
		default:
			return nil, fmt.Errorf("memory: unknown op %q", c.Op)
		}
	}
	user, shared := usage(next)
	if user > m.o.UserTokens {
		return nil, &changeError{text("over_limit", "which", "a user tag", "used", strconv.Itoa(user), "limit", strconv.Itoa(m.o.UserTokens))}
	}
	if shared > m.o.SharedTokens {
		return nil, &changeError{text("over_limit", "which", "no user tag", "used", strconv.Itoa(shared), "limit", strconv.Itoa(m.o.SharedTokens))}
	}
	return touched, nil
}

// confine keeps the model's changes under the session's user tag unless SharedWrites, since a shared memory steers every user's sessions; strict refuses a change to a shared memory instead of dropping it.
func (m *mem) confine(scope Scope, items []Item, changes []Change, strict bool) ([]Change, error) {
	if m.o.SharedWrites || scope.User == "" {
		return changes, nil // without a user tag there is no one else's memory to protect
	}
	own := func(p *string) *string {
		if p != nil && *p == "" {
			return &scope.User
		}
		return p
	}
	var out []Change
	for _, c := range changes {
		switch c.Op {
		case "add":
			if c.User == nil {
				c.User = &scope.User
			}
			c.User = own(c.User)
		case "update", "delete":
			if i := slices.IndexFunc(items, func(it Item) bool { return it.ID == c.ID }); i >= 0 && items[i].User != scope.User {
				if strict {
					return nil, &changeError{text("shared_read_only", "id", c.ID)}
				}
				continue
			}
			c.User = own(c.User)
		}
		out = append(out, c)
	}
	return out, nil
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// apply checks, applies and reports one batch, then tells the other sessions.
func (m *mem) apply(ctx context.Context, s *kernel.Session, changes []Change, emit func(event.Payload), trigger string) ([]Change, error) {
	scope := m.o.ScopeFunc(s)
	items, err := m.o.Store.List(ctx, scope)
	if err != nil {
		return nil, err
	}
	if changes, err = m.confine(scope, items, changes, trigger == "tool"); err != nil {
		return nil, err
	}
	touched, err := m.check(scope, items, changes)
	if err != nil {
		return nil, err
	}
	if len(changes) == 0 {
		return nil, nil
	}
	done, err := m.o.Store.Apply(ctx, scope, changes)
	if err != nil {
		return nil, err
	}
	emit(&Saved{Changes: done, Trigger: trigger})
	m.announce(ctx, s, touched)
	return done, nil
}

// ---- tools ----

var toolDefs = func() map[string]kernel.ToolSpec {
	var defs []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
	}
	if err := json.Unmarshal([]byte(spec.MustText("ext/memory/tools.json")), &defs); err != nil {
		panic("ext/memory: tools.json: " + err.Error())
	}
	out := map[string]kernel.ToolSpec{}
	for _, d := range defs {
		out[d.Name] = kernel.ToolSpec{Name: d.Name, Description: d.Description, InputSchema: d.InputSchema, Concurrency: kernel.Serial}
	}
	return out
}()

type saveInput struct {
	Content  string `json:"content"`
	User     string `json:"user"`
	Project  string `json:"project"`
	Replaces string `json:"replaces,omitempty"`
}

type forgetInput struct {
	ID string `json:"id"`
}

func (m *mem) tools(r kernel.Registrar) error {
	if _, err := r.Tool(kernel.NewTool(toolDefs["memory_save"], m.save)); err != nil {
		return err
	}
	_, err := r.Tool(kernel.NewTool(toolDefs["memory_forget"], m.forget))
	return err
}

func (m *mem) save(ctx context.Context, tc *kernel.ToolContext, in saveInput) (kernel.ToolResult, error) {
	c := Change{Op: "add", Content: &in.Content, User: &in.User, Project: &in.Project}
	if in.Replaces != "" {
		c.Op, c.ID = "update", in.Replaces
	}
	done, err := m.apply(ctx, tc.Session(), []Change{c}, tc.Emit, "tool")
	if ce := (*changeError)(nil); errors.As(err, &ce) {
		return kernel.ErrorResult("%s", ce.text), nil
	}
	if err != nil {
		return kernel.ToolResult{}, err
	}
	id := in.Replaces
	if len(done) > 0 && done[0].ID != "" {
		id = done[0].ID
	}
	return kernel.TextResult(text("saved", "id", id)), nil
}

func (m *mem) forget(ctx context.Context, tc *kernel.ToolContext, in forgetInput) (kernel.ToolResult, error) {
	_, err := m.apply(ctx, tc.Session(), []Change{{Op: "delete", ID: in.ID}}, tc.Emit, "tool")
	if ce := (*changeError)(nil); errors.As(err, &ce) {
		return kernel.ErrorResult("%s", ce.text), nil
	}
	if err != nil {
		return kernel.ToolResult{}, err
	}
	return kernel.TextResult(text("forgotten", "id", in.ID)), nil
}

// ---- consolidation ----

// root reports whether the session is its own conversation: a subagent's child session holds delegated work and a side chat talks about its main session, so neither is consolidated.
func root(env kernel.HookEnv) bool { return len(env.Path) == 0 && env.Session.Parent() == nil }

// conversation is the current context without its system line and epoch head, and whether a person said anything in it.
func conversation(lines []message.Message) ([]message.Message, bool) {
	var out []message.Message
	human := false
	for _, l := range lines {
		if l.Role == message.RoleSystem || l.Origin.Kind == message.OriginEpochHead {
			continue
		}
		out = append(out, l)
		human = human || l.Role == message.RoleUser && l.Origin.Kind == message.OriginHuman
	}
	return out, human
}

// consolidate asks the model, over the conversation as it stands, for the changes to long-term memory, and applies them all or none.
func (m *mem) consolidate(ctx context.Context, s *kernel.Session, trigger string) error {
	lines, human := conversation(s.Messages())
	if !human {
		return nil
	}
	scope := m.o.ScopeFunc(s)
	if ing, ok := m.o.Store.(Ingester); ok {
		before, err := m.o.Store.List(ctx, scope) // so an update or delete is announced to the sessions that loaded the old memory
		if err != nil {
			return err
		}
		done, err := ing.Ingest(ctx, scope, lines)
		if err == nil && len(done) > 0 {
			s.Emit(&Saved{Changes: done, Trigger: trigger})
			m.announce(ctx, s, touchedBy(before, done))
		}
		return err
	}
	items, err := m.o.Store.List(ctx, scope)
	if err != nil {
		return err
	}
	user, shared := usage(items)
	prompt := text("consolidate", "user", scope.User, "project", scope.Project, "memories", list(items),
		"user_used", strconv.Itoa(user), "user_limit", strconv.Itoa(m.o.UserTokens), "user_words", strconv.Itoa(m.o.UserTokens*3/4),
		"shared_used", strconv.Itoa(shared), "shared_limit", strconv.Itoa(m.o.SharedTokens), "shared_words", strconv.Itoa(m.o.SharedTokens*3/4))
	reply, _, err := s.SideCall(ctx, kernel.SideRequest{IncludeHistory: true, NoTools: true, Bucket: Name, MaxOutputTokens: consolidateOutput,
		Messages: []message.Message{{Role: message.RoleUser, Origin: message.Origin{Kind: message.OriginReminder, Source: Name}, Content: message.Content{&message.TextBlock{Text: prompt}}}}})
	if err != nil {
		return err
	}
	changes, err := parseChanges(reply.Text())
	if err != nil {
		return err
	}
	_, err = m.apply(ctx, s, changes, s.Emit, trigger)
	return err
}

// touchedBy is the memories applied changes touched, before and after, read against the memories before them.
func touchedBy(before []Item, done []Change) []Item {
	var out []Item
	for _, c := range done {
		old := Item{User: deref(c.User), Project: deref(c.Project)}
		if i := slices.IndexFunc(before, func(it Item) bool { return it.ID == c.ID }); i >= 0 {
			old = before[i]
		}
		out = append(out, old)
		if c.Op == "update" {
			it := old
			if c.User != nil {
				it.User = *c.User
			}
			if c.Project != nil {
				it.Project = *c.Project
			}
			out = append(out, it)
		}
	}
	return out
}

// parseChanges reads the reply's JSON object, tolerating a code fence around it.
func parseChanges(reply string) ([]Change, error) {
	start, end := strings.Index(reply, "{"), strings.LastIndex(reply, "}")
	if start < 0 || end < start {
		return nil, fmt.Errorf("memory: consolidation reply has no JSON object: %q", reply)
	}
	var out struct {
		Changes []Change `json:"changes"`
	}
	if err := json.Unmarshal([]byte(reply[start:end+1]), &out); err != nil {
		return nil, fmt.Errorf("memory: consolidation reply: %w", err)
	}
	return out.Changes, nil
}

// Consolidate runs the consolidation now, while the session is open, so a host can do it when the session goes idle and close it without waiting for a model call.
func Consolidate(ctx context.Context, s *kernel.Session) error {
	m, ok := kernel.Get(s, instKey)
	if !ok {
		if m, ok = kernel.Get(s.Registrar().Agent(), instKey); !ok {
			return ErrNotInstalled
		}
	}
	return m.consolidate(ctx, s, "host")
}

func (m *mem) consolidateOnEnd(ctx context.Context, in kernel.SessionEndInput) (kernel.Injection, error) {
	if !root(in.HookEnv) {
		return kernel.Injection{}, nil
	}
	return kernel.Injection{}, m.consolidate(ctx, in.Session, "session_end")
}

func (m *mem) consolidateBeforeCompaction(ctx context.Context, in kernel.PreCompactInput) (kernel.PreCompactOutput, error) {
	if !root(in.HookEnv) {
		return kernel.PreCompactOutput{}, nil
	}
	return kernel.PreCompactOutput{}, m.consolidate(ctx, in.Session, "pre_compact")
}

// ---- instruction files ----

type files struct {
	fsys  fs.FS
	names []string
}

// Files loads the named files from fsys in order and skips missing ones; total size is capped at 25KB.
func Files(fsys fs.FS, names ...string) InstructionSource { return files{fsys: fsys, names: names} }

func (f files) Load(context.Context, Scope) ([]InstructionDoc, error) {
	var out []InstructionDoc
	total := 0
	for _, n := range f.names {
		b, err := fs.ReadFile(f.fsys, n)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		content := strings.TrimSpace(string(b))
		if content == "" {
			continue
		}
		if total+len(content) > instructionsCap {
			keep := content[:instructionsCap-total]
			for !utf8.ValidString(keep) {
				keep = keep[:len(keep)-1]
			}
			out = append(out, InstructionDoc{Source: n, Content: keep + "\n" + text("instructions_truncated", "limit", strconv.Itoa(instructionsCap))})
			break
		}
		total += len(content)
		out = append(out, InstructionDoc{Source: n, Content: content})
	}
	return out, nil
}
