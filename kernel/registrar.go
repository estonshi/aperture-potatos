package kernel

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/estonshi/aperture-potatos/message"
)

// Extension is the shared shape of built-in and caller capabilities; std.Defaults installs nothing but Extensions.
type Extension interface {
	Name() string              // unique, e.g. "tasks"; std.Without filters on it; "app" is reserved
	Install(r Registrar) error // called once per scope
}

type extensionFunc struct {
	name    string
	install func(Registrar) error
}

func (e extensionFunc) Name() string              { return e.name }
func (e extensionFunc) Install(r Registrar) error { return e.install(r) }

// ExtensionFunc makes an Extension from a name and an install function, for extensions that keep no state of their own.
func ExtensionFunc(name string, install func(Registrar) error) Extension {
	return extensionFunc{name: name, install: install}
}

// Scope says whether a registration serves every session of an agent or one session only.
type Scope int

const (
	ScopeAgent   Scope = iota + 1 // the agent's registrar, used by kernel.New options and Use
	ScopeSession                  // a session's registrar, which can shadow agent registrations for that session alone
)

// String returns "agent" or "session", as registry.changed reports the scope.
func (s Scope) String() string {
	if s == ScopeSession {
		return "session"
	}
	return "agent"
}

// Registrar is the single entry for registration points; methods stay typed per point.
type Registrar interface {
	Scope() Scope
	Agent() *Agent
	Session() *Session // nil at ScopeAgent

	Tool(t Tool) (*Registration, error)
	HostedTool(t message.HostedTool) (*Registration, error) // a tool the provider runs; never gated, since the call never reaches the kernel
	Command(c Command) (*Registration, error)
	PromptSection(ps PromptSection) (*Registration, error)
	ContextProvider(p ContextProvider) (*Registration, error)
	Compactor(c Compactor) (*Registration, error)       // one per scope; a session's shadows the agent's
	TaskKind(k TaskKind) (*Registration, error)         // a session's shadows the agent's kind of the same name
	Hide(kind Kind, name string) (*Registration, error) // shadows an inherited registration in this scope

	registry() *registry // unexported: only the kernel implements Registrar
}

// Kind names a kind of registration as registry.changed reports it; Hide takes every kind but KindCompactor and KindTaskKind, which a session shadows by registering its own.
type Kind string

// The kinds of registration, one per typed Registrar method.
const (
	KindTool            Kind = "tool"
	KindCommand         Kind = "command"
	KindPromptSection   Kind = "section"
	KindContextProvider Kind = "context_provider"
	KindCompactor       Kind = "compactor"
	KindTaskKind        Kind = "task_kind"
	KindHostedTool      Kind = "hosted_tool"
)

// Registration is the uniform undo handle; Remove is idempotent and emits registry.changed.
type Registration struct {
	once   sync.Once
	remove func()
}

// NewRegistration wraps an extension's own undo, so what it registers by itself is removed like any kernel registration.
func NewRegistration(remove func()) *Registration { return &Registration{remove: remove} }

// ToolChanged tells the sessions in r's scope that the named tool's definition, such as an enum its Spec computes, differs from the next freeze point on.
func ToolChanged(r Registrar, name string) { r.registry().changed(KindTool, name, "update") }

// Remove undoes the registration; it is safe on nil and when called again.
func (r *Registration) Remove() {
	if r != nil {
		r.once.Do(r.remove)
	}
}

// appName is the extension that every option-form registration of one kernel.New call joins.
const appName = "app"

var (
	toolName    = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
	commandName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
)

type toolReg struct {
	id   uint64
	name string
	tool Tool
}

type cmdReg struct {
	id  uint64
	cmd Command
}

type sectionReg struct {
	id uint64
	ps PromptSection
}

type providerReg struct {
	id uint64
	p  ContextProvider
}

type hideKey struct {
	kind Kind
	name string
}

type regState struct {
	tools     map[string]*toolReg
	commands  map[string]*cmdReg
	aliases   map[string]string // alias → canonical name
	hooks     map[uint64][]*hookEntry
	sections  map[string]*sectionReg
	providers map[string]*providerReg
	hidden    map[hideKey]uint64
	compactor *compactorReg
	taskKinds map[string]*taskKindReg
	hosted    map[string]*hostedReg
}

type taskKindReg struct {
	id   uint64
	kind TaskKind
}

type compactorReg struct {
	id uint64
	c  Compactor
}

// hides reports whether this scope shadows an inherited registration.
func (st *regState) hides(kind Kind, name string) bool {
	_, ok := st.hidden[hideKey{kind, name}]
	return ok
}

// regChange is one registry change as the owner announces it; section is set for prompt sections, which may be announced to the model.
type regChange struct {
	kind    Kind
	name    string
	op      string
	section *PromptSection
}

// registry is one scope's registrations: writers copy the state and swap it, readers load it without locking.
type registry struct {
	scope  Scope
	mu     sync.Mutex
	next   uint64
	cur    atomic.Pointer[regState]
	notify atomic.Pointer[func(regChange)] // set once the owner is live, so later changes emit registry.changed
}

func newRegistry(scope Scope) *registry {
	r := &registry{scope: scope}
	r.cur.Store(&regState{tools: map[string]*toolReg{}, commands: map[string]*cmdReg{}, aliases: map[string]string{},
		hooks: map[uint64][]*hookEntry{}, sections: map[string]*sectionReg{}, providers: map[string]*providerReg{}, hidden: map[hideKey]uint64{}, taskKinds: map[string]*taskKindReg{}, hosted: map[string]*hostedReg{}})
	return r
}

func (r *registry) state() *regState { return r.cur.Load() }

func (r *registry) changed(kind Kind, name, op string) {
	r.announce(regChange{kind: kind, name: name, op: op})
}

func (r *registry) announce(c regChange) {
	if fn := r.notify.Load(); fn != nil {
		(*fn)(c)
	}
}

func (r *registry) update(fn func(st *regState) error) (uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	old := r.cur.Load()
	st := &regState{tools: maps.Clone(old.tools), commands: maps.Clone(old.commands), aliases: maps.Clone(old.aliases),
		hooks: maps.Clone(old.hooks), sections: maps.Clone(old.sections), providers: maps.Clone(old.providers), hidden: maps.Clone(old.hidden), compactor: old.compactor, taskKinds: maps.Clone(old.taskKinds), hosted: maps.Clone(old.hosted)}
	if err := fn(st); err != nil {
		return 0, err
	}
	r.next++
	r.cur.Store(st)
	return r.next, nil
}

func (r *registry) addTool(t Tool) (*Registration, error) {
	spec := t.Spec()
	name := spec.Name
	if !toolName.MatchString(name) {
		return nil, fmt.Errorf("%w: tool %q", ErrInvalidName, name)
	}
	if spec.Background {
		if _, err := withBackground(spec.InputSchema, spec.Strict); err != nil {
			return nil, fmt.Errorf("tool %q: %v", name, err)
		}
	}
	id, err := r.update(func(st *regState) error {
		if _, ok := st.tools[name]; ok {
			return fmt.Errorf("%w: tool %q", ErrDuplicate, name)
		}
		if _, ok := st.hosted[name]; ok {
			return fmt.Errorf("%w: tool %q is a hosted tool's name", ErrDuplicate, name)
		}
		st.tools[name] = &toolReg{id: r.next + 1, name: name, tool: t}
		return nil
	})
	if err != nil {
		return nil, err
	}
	r.changed(KindTool, name, "add")
	return &Registration{remove: func() {
		if _, err := r.update(func(st *regState) error {
			if cur, ok := st.tools[name]; !ok || cur.id != id {
				return errGone
			}
			delete(st.tools, name)
			return nil
		}); err == nil {
			r.changed(KindTool, name, "remove")
		}
	}}, nil
}

var errGone = fmt.Errorf("registration already removed")

func (r *registry) addCommand(c Command) (*Registration, error) {
	for _, n := range append([]string{c.Name}, c.Aliases...) {
		if !commandName.MatchString(n) {
			return nil, fmt.Errorf("%w: command %q", ErrInvalidName, n)
		}
	}
	if c.Run == nil {
		return nil, fmt.Errorf("command %q has no Run", c.Name)
	}
	switch c.Busy {
	case "", RunNow, WaitIdle, InterruptFirst, RejectBusy:
	default:
		return nil, fmt.Errorf("command %q: unknown busy policy %q", c.Name, c.Busy)
	}
	id, err := r.update(func(st *regState) error {
		for _, n := range append([]string{c.Name}, c.Aliases...) {
			if _, ok := st.commands[n]; ok {
				return fmt.Errorf("%w: command %q", ErrDuplicate, n)
			}
			if _, ok := st.aliases[n]; ok {
				return fmt.Errorf("%w: command %q", ErrDuplicate, n)
			}
		}
		st.commands[c.Name] = &cmdReg{id: r.next + 1, cmd: c}
		for _, a := range c.Aliases {
			st.aliases[a] = c.Name
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	r.changed(KindCommand, c.Name, "add")
	return &Registration{remove: func() {
		if _, err := r.update(func(st *regState) error {
			if cur, ok := st.commands[c.Name]; !ok || cur.id != id {
				return errGone
			}
			delete(st.commands, c.Name)
			for _, a := range c.Aliases {
				delete(st.aliases, a)
			}
			return nil
		}); err == nil {
			r.changed(KindCommand, c.Name, "remove")
		}
	}}, nil
}

func (st *regState) command(name string) (Command, bool) {
	if canon, ok := st.aliases[name]; ok {
		name = canon
	}
	c, ok := st.commands[name]
	if !ok {
		return Command{}, false
	}
	return c.cmd, true
}

type registrar struct {
	scope   Scope
	agent   *Agent
	session *Session
	reg     *registry
}

func (r *registrar) Scope() Scope                       { return r.scope }
func (r *registrar) Agent() *Agent                      { return r.agent }
func (r *registrar) Session() *Session                  { return r.session }
func (r *registrar) Tool(t Tool) (*Registration, error) { return r.reg.addTool(t) }
func (r *registrar) HostedTool(t message.HostedTool) (*Registration, error) {
	return r.reg.addHosted(t)
}
func (r *registrar) Command(c Command) (*Registration, error) { return r.reg.addCommand(c) }
func (r *registrar) PromptSection(ps PromptSection) (*Registration, error) {
	return r.reg.addSection(ps)
}
func (r *registrar) Compactor(c Compactor) (*Registration, error) { return r.reg.addCompactor(c) }
func (r *registrar) TaskKind(k TaskKind) (*Registration, error)   { return r.reg.addTaskKind(k) }

func (r *registrar) ContextProvider(p ContextProvider) (*Registration, error) {
	return r.reg.addProvider(p)
}
func (r *registrar) Hide(kind Kind, name string) (*Registration, error) {
	return r.reg.hide(kind, name)
}
func (r *registrar) registry() *registry { return r.reg }

func (r *registry) addHook(e *hookEntry) (*Registration, error) {
	id, err := r.update(func(st *regState) error {
		for _, h := range st.hooks[e.point.id] {
			if h.name == e.name {
				return fmt.Errorf("%w: hook %q at %s", ErrDuplicate, e.name, e.point.name)
			}
		}
		e.id = r.next + 1
		st.hooks[e.point.id] = append(slices.Clone(st.hooks[e.point.id]), e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &Registration{remove: func() {
		_, _ = r.update(func(st *regState) error {
			st.hooks[e.point.id] = slices.DeleteFunc(slices.Clone(st.hooks[e.point.id]), func(h *hookEntry) bool { return h.id == id })
			return nil
		})
	}}, nil
}

var sectionName = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)

func (r *registry) addSection(ps PromptSection) (*Registration, error) {
	switch {
	case !sectionName.MatchString(ps.Name):
		return nil, fmt.Errorf("%w: section %q", ErrInvalidName, ps.Name)
	case ps.Slot != SlotCapabilities && ps.Slot != SlotCatalog && ps.Slot != SlotEnvironment:
		return nil, fmt.Errorf("section %q: unknown slot %d", ps.Name, ps.Slot)
	case ps.Render == nil:
		return nil, fmt.Errorf("section %q has no Render", ps.Name)
	}
	id, err := r.update(func(st *regState) error {
		if _, ok := st.sections[ps.Name]; ok {
			return fmt.Errorf("%w: section %q", ErrDuplicate, ps.Name)
		}
		st.sections[ps.Name] = &sectionReg{id: r.next + 1, ps: ps}
		return nil
	})
	if err != nil {
		return nil, err
	}
	r.announce(regChange{kind: KindPromptSection, name: ps.Name, op: "add", section: &ps})
	return &Registration{remove: func() {
		if _, err := r.update(func(st *regState) error {
			if cur, ok := st.sections[ps.Name]; !ok || cur.id != id {
				return errGone
			}
			delete(st.sections, ps.Name)
			return nil
		}); err == nil {
			r.announce(regChange{kind: KindPromptSection, name: ps.Name, op: "remove", section: &ps})
		}
	}}, nil
}

func (r *registry) addProvider(p ContextProvider) (*Registration, error) {
	switch {
	case !sectionName.MatchString(p.Name):
		return nil, fmt.Errorf("%w: context provider %q", ErrInvalidName, p.Name)
	case p.When != AtTurnStart && p.When != AtEveryStep:
		return nil, fmt.Errorf("context provider %q: unknown When %d", p.Name, p.When)
	case p.Provide == nil:
		return nil, fmt.Errorf("context provider %q has no Provide", p.Name)
	}
	id, err := r.update(func(st *regState) error {
		if _, ok := st.providers[p.Name]; ok {
			return fmt.Errorf("%w: context provider %q", ErrDuplicate, p.Name)
		}
		st.providers[p.Name] = &providerReg{id: r.next + 1, p: p}
		return nil
	})
	if err != nil {
		return nil, err
	}
	r.changed(KindContextProvider, p.Name, "add")
	return &Registration{remove: func() {
		if _, err := r.update(func(st *regState) error {
			if cur, ok := st.providers[p.Name]; !ok || cur.id != id {
				return errGone
			}
			delete(st.providers, p.Name)
			return nil
		}); err == nil {
			r.changed(KindContextProvider, p.Name, "remove")
		}
	}}, nil
}

func (r *registry) addCompactor(c Compactor) (*Registration, error) {
	if c == nil {
		return nil, fmt.Errorf("nil compactor")
	}
	id, err := r.update(func(st *regState) error {
		if st.compactor != nil {
			return fmt.Errorf("%w: compactor", ErrDuplicate)
		}
		st.compactor = &compactorReg{id: r.next + 1, c: c}
		return nil
	})
	if err != nil {
		return nil, err
	}
	r.changed(KindCompactor, "compactor", "add")
	return &Registration{remove: func() {
		if _, err := r.update(func(st *regState) error {
			if st.compactor == nil || st.compactor.id != id {
				return errGone
			}
			st.compactor = nil
			return nil
		}); err == nil {
			r.changed(KindCompactor, "compactor", "remove")
		}
	}}, nil
}

func (r *registry) addTaskKind(k TaskKind) (*Registration, error) {
	switch {
	case !toolName.MatchString(k.Name):
		return nil, fmt.Errorf("%w: task kind %q", ErrInvalidName, k.Name)
	case k.Name == ToolExecKind:
		return nil, fmt.Errorf("%w: task kind %q is the kernel's", ErrDuplicate, k.Name)
	case k.New == nil:
		return nil, fmt.Errorf("task kind %q has no New", k.Name)
	}
	id, err := r.update(func(st *regState) error {
		if _, ok := st.taskKinds[k.Name]; ok {
			return fmt.Errorf("%w: task kind %q", ErrDuplicate, k.Name)
		}
		st.taskKinds[k.Name] = &taskKindReg{id: r.next + 1, kind: k}
		return nil
	})
	if err != nil {
		return nil, err
	}
	r.changed(KindTaskKind, k.Name, "add")
	return &Registration{remove: func() {
		if _, err := r.update(func(st *regState) error {
			if cur, ok := st.taskKinds[k.Name]; !ok || cur.id != id {
				return errGone
			}
			delete(st.taskKinds, k.Name)
			return nil
		}); err == nil {
			r.changed(KindTaskKind, k.Name, "remove")
		}
	}}, nil
}

// hide shadows the named registration inherited from an outer scope; the scope's own registration of that name stays visible.
func (r *registry) hide(kind Kind, name string) (*Registration, error) {
	switch kind {
	case KindTool, KindCommand, KindPromptSection, KindContextProvider, KindHostedTool:
	default:
		return nil, fmt.Errorf("cannot hide kind %q", kind)
	}
	k := hideKey{kind, name}
	id, err := r.update(func(st *regState) error {
		if _, ok := st.hidden[k]; ok {
			return fmt.Errorf("%w: hide %s %q", ErrDuplicate, kind, name)
		}
		st.hidden[k] = r.next + 1
		return nil
	})
	if err != nil {
		return nil, err
	}
	r.changed(kind, name, "remove")
	return &Registration{remove: func() {
		if _, err := r.update(func(st *regState) error {
			if cur, ok := st.hidden[k]; !ok || cur != id {
				return errGone
			}
			delete(st.hidden, k)
			return nil
		}); err == nil {
			r.changed(kind, name, "add")
		}
	}}, nil
}

// install runs each extension once in order; names are unique per scope and "app" belongs to the option-form registrations.
func install(r Registrar, exts []Extension, app Extension) ([]string, error) {
	var names []string
	for _, e := range exts {
		n := e.Name()
		if n == appName {
			return nil, fmt.Errorf("%w: extension name %q is reserved", ErrInvalidName, n)
		}
		if n == "" || slices.Contains(names, n) {
			return nil, fmt.Errorf("%w: extension %q", ErrDuplicate, n)
		}
		if err := e.Install(r); err != nil {
			return nil, fmt.Errorf("extension %s: %w", n, err)
		}
		names = append(names, n)
	}
	if app != nil {
		if err := app.Install(r); err != nil {
			return nil, fmt.Errorf("options: %w", err)
		}
		names = append(names, appName)
	}
	return names, nil
}

// Use installs extensions at agent scope.
func Use(exts ...Extension) Option {
	return func(c *config) { c.exts = append(c.exts, exts...) }
}

// SessionUse installs extensions at session scope.
func SessionUse(exts ...Extension) SessionOption {
	return func(c *sessionConfig) { c.exts = append(c.exts, exts...) }
}

// Key is a typed state slot for extensions; it lives in memory only, so a resumed session starts without it.
type Key[T any] struct{ id *keyID }

type keyID struct{ name string }

// NewKey makes a slot; two keys never share a slot, whatever their names.
func NewKey[T any](name string) Key[T] { return Key[T]{id: &keyID{name: name}} }

// Name is the name NewKey was given, for logs only.
func (k Key[T]) Name() string { return k.id.name }

// StateHolder is implemented by *Agent and *Session only.
type StateHolder interface{ stateStore() *stateStore }

type stateStore struct {
	mu sync.Mutex
	m  map[*keyID]any
}

// Get reads k's slot on h; ok is false when nothing was set or the stored value is not a T.
func Get[T any](h StateHolder, k Key[T]) (T, bool) {
	st := h.stateStore()
	st.mu.Lock()
	defer st.mu.Unlock()
	v, ok := st.m[k.id].(T)
	return v, ok
}

// Set replaces k's slot on h; use Update when the new value depends on the old one.
func Set[T any](h StateHolder, k Key[T], v T) {
	st := h.stateStore()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.m == nil {
		st.m = map[*keyID]any{}
	}
	st.m[k.id] = v
}

// Update is an atomic read-modify-write; fn runs under the holder's lock, so it must not touch other keys.
func Update[T any](h StateHolder, k Key[T], fn func(old T, ok bool) T) T {
	st := h.stateStore()
	st.mu.Lock()
	defer st.mu.Unlock()
	old, ok := st.m[k.id].(T)
	v := fn(old, ok)
	if st.m == nil {
		st.m = map[*keyID]any{}
	}
	st.m[k.id] = v
	return v
}
