package kernel

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/internal/hookrun"
	"github.com/estonshi/aperture-potatos/message"
)

// FailPolicy decides what a hook's error, timeout or panic means.
type FailPolicy int

const (
	FailDefault FailPolicy = iota // the point's own policy
	FailOpen                      // the failure counts as no opinion
	FailClosed                    // the failure yields the point's Closed output, usually a block or a deny
)

// HookPointDef describes one hook point; kernel points and extension points share the shape.
type HookPointDef[In, Out any] struct {
	Name     string
	Key      func(In) string                // what matchers see; nil makes registering with a matcher fail
	Chain    func(in In, out Out) In        // hands one hook's rewrite to the next; nil means no rewriting
	Merge    func(acc, out Out) (Out, bool) // folds one output into the result; true skips the remaining sync hooks
	Timeout  time.Duration                  // 0 waits without a deadline, which only approval does because a person decides
	Fail     FailPolicy
	Closed   func(err error) Out // err is a *HookError
	SyncOnly bool                // OnAsync is refused, because the caller must have the answer before it moves on
}

// HookPoint is a typed place where hooks run; extensions can declare their own with NewHookPoint and fire them like the kernel's.
type HookPoint[In, Out any] struct {
	def  HookPointDef[In, Out]
	meta *pointMeta
}

// pointMeta is what registration needs to know about a point without its type parameters.
type pointMeta struct {
	id         uint64
	name       string
	keyed      bool
	syncOnly   bool
	closedOnly bool // a failing hook never counts as no opinion, as for approval
}

var pointIDs atomic.Uint64

// NewHookPoint gives each point its own identity, so two points with the same name never share hooks.
func NewHookPoint[In, Out any](def HookPointDef[In, Out]) *HookPoint[In, Out] {
	return &HookPoint[In, Out]{def: def, meta: &pointMeta{id: pointIDs.Add(1), name: def.Name, keyed: def.Key != nil, syncOnly: def.SyncOnly}}
}

// Name is the point's name as events, scripts and the hook schema spell it.
func (p *HookPoint[In, Out]) Name() string { return p.def.Name }

func (p *HookPoint[In, Out]) point() *pointMeta { return p.meta }

// AnyHookPoint lets OnJSON and the conformance runner address a point without its type parameters.
type AnyHookPoint interface {
	Name() string
	point() *pointMeta
}

// HookFunc is a sync hook; its output is merged with the other hooks' and its rewrites are passed to the next one.
type HookFunc[In, Out any] func(ctx context.Context, in In) (Out, error)

// AsyncHookFunc can only return an Injection, so an async hook cannot block, rewrite or decide by construction.
type AsyncHookFunc[In any] func(ctx context.Context, in In) (Injection, error)

// JSONHookFunc speaks the JSON of spec/schema/hook.schema.json, for out-of-process hooks and declarative scripts.
type JSONHookFunc func(ctx context.Context, in json.RawMessage) (json.RawMessage, error)

// JSONAsyncHookFunc is the async form of JSONHookFunc.
type JSONAsyncHookFunc func(ctx context.Context, in json.RawMessage) (Injection, error)

// Injection is the only output of an async hook; it enters the input queue as a reminder with source "hook:<name>".
type Injection struct {
	AdditionalContext string `json:"additional_context,omitempty"` // a reminder at the next safe point or with the next turn
	SystemMessage     string `json:"system_message,omitempty"`     // shown to people only, as a notice event
	Wake              bool   `json:"wake,omitempty"`               // may start a turn on an idle session, within WakeLimit and MaxConsecutiveAutoTurns
}

// HookEnv is embedded in every kernel hook input.
type HookEnv struct {
	Session   *Session    `json:"-"` // only enqueue-style methods are safe from a hook
	SessionID string      `json:"session_id"`
	Agent     string      `json:"agent"`
	TurnID    string      `json:"turn_id,omitempty"`
	Step      int         `json:"step,omitempty"`
	Path      []event.Hop `json:"path,omitempty"` // hops from the root session; empty at the root
}

func (e *HookEnv) hookEnv() *HookEnv { return e }

// Common is embedded in every kernel hook output except approval's.
type Common struct {
	AdditionalContext string `json:"additional_context,omitempty"` // becomes a reminder with source "hook:<name>"
	SystemMessage     string `json:"system_message,omitempty"`     // shown to people only
	Halt              bool   `json:"halt,omitempty"`               // end the turn after this point; I1 still holds
	HaltReason        string `json:"halt_reason,omitempty"`
}

func (c *Common) hookCommon() *Common { return c }

// Decision is a pre_tool_use hook's verdict on a call; the strictest decision among the hooks wins.
type Decision string

const (
	NoDecision Decision = ""      // leave the call to the approval rules
	Allow      Decision = "allow" // run the call, unless a rule asks or denies
	Ask        Decision = "ask"   // send the call to the approval reviewers
	Deny       Decision = "deny"  // refuse the call with Reason, and skip the remaining hooks
)

func (d Decision) rank() int {
	switch d {
	case Deny:
		return 3
	case Ask:
		return 2
	case Allow:
		return 1
	}
	return 0
}

// HookError is what a point's Closed receives; its Kind, not its message, is what the model may see.
type HookError struct {
	Kind string // timeout, error, panic, or canceled and recursion when a fail-closed hook could not run
	Err  error
}

// Error includes the kind and the underlying error, for logs only.
func (e *HookError) Error() string { return "hook " + e.Kind + ": " + e.Err.Error() }

// Unwrap returns the hook's own error.
func (e *HookError) Unwrap() error { return e.Err }

// HookOption configures one hook registration.
type HookOption func(*hookConfig)

type hookConfig struct {
	priority   int
	timeout    time.Duration
	timeoutSet bool
	fail       FailPolicy
	matcher    Matcher
	wakeN      int
	wakePer    time.Duration
}

// Priority orders hooks of one point: lower runs first, default 0.
func Priority(n int) HookOption { return func(c *hookConfig) { c.priority = n } }

// Timeout replaces the point's timeout for this hook; 0 waits without a deadline.
func Timeout(d time.Duration) HookOption {
	return func(c *hookConfig) { c.timeout, c.timeoutSet = d, true }
}

// Fail overrides the point's failure policy for this hook, except where a point only fails closed.
func Fail(p FailPolicy) HookOption { return func(c *hookConfig) { c.fail = p } }

// Match runs the hook only for keys m matches, such as tool names on pre_tool_use, and fails on a point without a key.
func Match(m Matcher) HookOption { return func(c *hookConfig) { c.matcher = m } }

// WakeLimit bounds how often an async hook may start a turn; beyond it the injection waits for the next turn.
func WakeLimit(n int, per time.Duration) HookOption {
	return func(c *hookConfig) { c.wakeN, c.wakePer = n, per }
}

// hookEntry is one registered hook; fn holds a HookFunc or an AsyncHookFunc of the point's types.
type hookEntry struct {
	id        uint64
	scope     Scope
	point     *pointMeta
	name      string
	cfg       hookConfig
	async     bool
	fn        any
	json      JSONHookFunc
	jsonAsync JSONAsyncHookFunc
	wake      *wakeLimiter
}

func (h *hookEntry) matches(key string) bool { return h.cfg.matcher == nil || h.cfg.matcher.Match(key) }

func (h *hookEntry) timeoutOr(def time.Duration) time.Duration {
	if h.cfg.timeoutSet {
		return h.cfg.timeout
	}
	return def
}

var hookName = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)

// On registers a sync hook; it runs on the goroutine that fires the point, in Priority, scope and registration order.
func On[In, Out any](r Registrar, p *HookPoint[In, Out], name string, fn HookFunc[In, Out], opts ...HookOption) (*Registration, error) {
	if fn == nil {
		return nil, fmt.Errorf("hook %s has no function", name)
	}
	return addHook(r, p.meta, &hookEntry{name: name, fn: fn}, opts)
}

// OnAsync registers a hook that runs beside the point without being waited for.
func OnAsync[In, Out any](r Registrar, p *HookPoint[In, Out], name string, fn AsyncHookFunc[In], opts ...HookOption) (*Registration, error) {
	if fn == nil {
		return nil, fmt.Errorf("hook %s has no function", name)
	}
	return addHook(r, p.meta, &hookEntry{name: name, fn: fn, async: true}, opts)
}

// OnJSON registers a sync hook that reads and writes the point's JSON form.
func OnJSON(r Registrar, p AnyHookPoint, name string, fn JSONHookFunc, opts ...HookOption) (*Registration, error) {
	if fn == nil {
		return nil, fmt.Errorf("hook %s has no function", name)
	}
	return addHook(r, p.point(), &hookEntry{name: name, json: fn}, opts)
}

// OnJSONAsync registers an async hook that reads the point's JSON form.
func OnJSONAsync(r Registrar, p AnyHookPoint, name string, fn JSONAsyncHookFunc, opts ...HookOption) (*Registration, error) {
	if fn == nil {
		return nil, fmt.Errorf("hook %s has no function", name)
	}
	return addHook(r, p.point(), &hookEntry{name: name, jsonAsync: fn, async: true}, opts)
}

func addHook(r Registrar, pm *pointMeta, e *hookEntry, opts []HookOption) (*Registration, error) {
	for _, o := range opts {
		o(&e.cfg)
	}
	switch {
	case !hookName.MatchString(e.name):
		return nil, fmt.Errorf("%w: hook %q", ErrInvalidName, e.name)
	case e.async && pm.syncOnly:
		return nil, fmt.Errorf("%w: %s", ErrAsyncNotAllowed, pm.name)
	case e.cfg.matcher != nil && !pm.keyed:
		return nil, fmt.Errorf("%w: %s", ErrNoMatcher, pm.name)
	case pm.closedOnly && e.cfg.fail == FailOpen:
		return nil, fmt.Errorf("hook point %s fails closed only", pm.name)
	}
	if bad, ok := e.cfg.matcher.(badMatcher); ok {
		return nil, bad.err
	}
	if e.async {
		d := defaults().Hooks
		n, per := e.cfg.wakeN, e.cfg.wakePer
		if n == 0 {
			n = d.WakeLimit
		}
		if per == 0 {
			per = ms(d.WakeWindow)
		}
		e.wake = &wakeLimiter{n: n, per: per}
	}
	e.point, e.scope = pm, r.Scope()
	return r.registry().addHook(e)
}

// WithHook is On at agent scope, as part of the "app" extension.
func WithHook[In, Out any](p *HookPoint[In, Out], name string, fn HookFunc[In, Out], opts ...HookOption) Option {
	return func(c *config) {
		c.app = append(c.app, func(r Registrar) error { _, err := On(r, p, name, fn, opts...); return err })
	}
}

// WithAsyncHook is OnAsync at agent scope, as part of the "app" extension.
func WithAsyncHook[In, Out any](p *HookPoint[In, Out], name string, fn AsyncHookFunc[In], opts ...HookOption) Option {
	return func(c *config) {
		c.app = append(c.app, func(r Registrar) error { _, err := OnAsync(r, p, name, fn, opts...); return err })
	}
}

// wakeLimiter counts recent wakes in real time, so a hook that keeps waking the session is slowed without touching the event clock.
type wakeLimiter struct {
	mu    sync.Mutex
	n     int
	per   time.Duration
	times []time.Time
}

func (w *wakeLimiter) allow() bool {
	if w.n < 0 {
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	w.times = slices.DeleteFunc(w.times, func(t time.Time) bool { return now.Sub(t) >= w.per })
	if len(w.times) >= w.n {
		return false
	}
	w.times = append(w.times, now)
	return true
}

// hookEntries lists a point's hooks of both scopes in run order: Priority, then agent scope before session scope, then registration order.
func (s *Session) hookEntries(pm *pointMeta) []*hookEntry {
	out := slices.Concat(s.outer().hooks[pm.id], s.reg.state().hooks[pm.id])
	sortHooks(out)
	return out
}

func sortHooks(hs []*hookEntry) {
	slices.SortStableFunc(hs, func(a, b *hookEntry) int {
		return cmp.Or(cmp.Compare(a.cfg.priority, b.cfg.priority), cmp.Compare(a.scope, b.scope), cmp.Compare(a.id, b.id))
	})
}

// hookScope says where a point fires: the turn, step and call its events belong to, and where its additional context goes.
type hookScope struct {
	turnID  string
	step    int
	attempt int
	callID  string
	sink    *hookSink
	path    []event.Hop // set inside a task, whose hooks publish under its hop
}

type hookScopeKey struct{}

type hookDepthKey struct{}

func withHookScope(ctx context.Context, sc *hookScope) context.Context {
	return context.WithValue(ctx, hookScopeKey{}, sc)
}

func scopeOf(ctx context.Context) *hookScope {
	sc, _ := ctx.Value(hookScopeKey{}).(*hookScope)
	return sc
}

// hookSink collects what hooks inside a turn hand back: reminders for the next delivery point and whether one asked to halt.
type hookSink struct {
	mu      sync.Mutex
	notes   []*message.ReminderBlock
	halt    bool
	discard bool // additional context is dropped, as after the session ends
}

func (k *hookSink) take() []*message.ReminderBlock {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := k.notes
	k.notes = nil
	return out
}

// absorb moves another sink's notes and halt into this one.
func (k *hookSink) absorb(from *hookSink) {
	if from == nil {
		return
	}
	from.mu.Lock()
	notes, halt := from.notes, from.halt
	from.notes = nil
	from.mu.Unlock()
	k.mu.Lock()
	k.notes = append(k.notes, notes...)
	k.halt = k.halt || halt
	k.mu.Unlock()
}

func (k *hookSink) halted() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.halt
}

func (s *Session) publishScoped(sc *hookScope, p event.Payload) {
	switch {
	case sc == nil:
		s.publish(p)
	case sc.path != nil:
		s.publishUnder(p, sc.path)
	default:
		s.publishAt(p, sc.turnID, sc.step, sc.attempt)
	}
}

// namedOut is the output of one sync hook, for points that report per hook, such as stop reasons.
type namedOut[Out any] struct {
	name string
	out  Out
}

type fired[In, Out any] struct {
	out  Out
	in   In
	outs []namedOut[Out]
}

// Fire runs a point: async hooks start with a copy of the input, then the sync chain runs on the calling goroutine; it returns the merged output and the rewritten input.
func Fire[In, Out any](ctx context.Context, s *Session, p *HookPoint[In, Out], in In) (Out, In) {
	f := fire(ctx, s, p, in, nil)
	return f.out, f.in
}

// fire is Fire with a way to name each hook's decision, which hook.completed carries.
func fire[In, Out any](ctx context.Context, s *Session, p *HookPoint[In, Out], in In, decide func(Out) string) fired[In, Out] {
	f := fired[In, Out]{in: in}
	sc := scopeOf(ctx)
	entries := s.hookEntries(p.meta)
	key := ""
	if p.def.Key != nil && len(entries) > 0 {
		key = p.def.Key(in)
	}
	// unrun is the result when sync hooks that should run cannot: a fail-closed point still refuses, so a canceled or runaway chain never reads as consent.
	unrun := func(rest []*hookEntry, kind string, err error) fired[In, Out] {
		for _, h := range rest {
			if !h.async && h.matches(key) && p.def.Closed != nil && h.policy(p) == FailClosed {
				f.out = p.def.Closed(&HookError{Kind: kind, Err: err})
				break
			}
		}
		return f
	}
	depth, _ := ctx.Value(hookDepthKey{}).(int)
	if depth >= defaults().Hooks.MaxDepth {
		s.publishScoped(sc, &event.Error{Code: "hook_recursion", Message: "hook point " + p.def.Name + " fired from inside its own hooks too deeply"})
		return unrun(entries, "recursion", errors.New("hook recursion limit"))
	}
	if ctx.Err() != nil {
		return unrun(entries, string(hookrun.KindCanceled), context.Cause(ctx))
	}
	if len(entries) == 0 {
		return f
	}
	hctx := context.WithValue(ctx, hookDepthKey{}, depth+1)
	for _, h := range entries {
		if h.async && h.matches(key) {
			launchAsync(hctx, s, p, h, in)
		}
	}
	for i, h := range entries {
		if h.async || !h.matches(key) {
			continue
		}
		if ctx.Err() != nil {
			return unrun(entries[i:], string(hookrun.KindCanceled), context.Cause(ctx))
		}
		out, ok := callSync(hctx, s, p, h, f.in, decide)
		if !ok {
			if ctx.Err() != nil {
				return unrun(entries[i:], string(hookrun.KindCanceled), context.Cause(ctx))
			}
			continue
		}
		f.outs = append(f.outs, namedOut[Out]{name: h.name, out: out})
		s.takeCommon(sc, h.name, &out)
		stop := false
		if p.def.Merge != nil {
			f.out, stop = p.def.Merge(f.out, out)
		} else {
			f.out = out
		}
		if p.def.Chain != nil {
			f.in = p.def.Chain(f.in, out)
		}
		if stop {
			break
		}
	}
	return f
}

// callSync runs one sync hook and applies its fail policy; false means it has no opinion.
func callSync[In, Out any](ctx context.Context, s *Session, p *HookPoint[In, Out], h *hookEntry, in In, decide func(Out) string) (Out, bool) {
	out, ok, _ := callSyncFail(ctx, s, p, h, in, decide)
	return out, ok
}

// callSyncFail is callSync that also reports the failure, which review needs to tell a timeout from a reviewer's error.
func callSyncFail[In, Out any](ctx context.Context, s *Session, p *HookPoint[In, Out], h *hookEntry, in In, decide ...func(Out) string) (Out, bool, *hookrun.Failure) {
	var zero Out
	sc := scopeOf(ctx)
	id := s.agent.cfg.ids.NewID("hook")
	callID := ""
	if sc != nil {
		callID = sc.callID
	}
	s.publishScoped(sc, &event.HookStarted{HookID: id, Point: p.def.Name, Name: h.name, CallID: callID})
	started := time.Now()
	mctx := withMarker(ctx, marker{chain: s.chain, kind: "hook"})
	out, fail := hookrun.Call(mctx, s.wk, "hook "+p.def.Name+" "+h.name, h.timeoutOr(p.def.Timeout), s.limits.HookGrace, func(hctx context.Context) (Out, error) {
		return invokeSync[In, Out](hctx, h, in)
	})
	took := time.Since(started).Milliseconds()
	// A registration the hook made is announced through the actor; waiting for the queue to drain keeps the hook's
	// own announcements ahead of hook.completed, an order a conformance case compares.
	_ = s.exec(func(*state) {})
	if fail == nil {
		done := &event.HookCompleted{HookID: id, Point: p.def.Name, Name: h.name, CallID: callID, DurationMs: took}
		if c := commonOf(&out); c != nil {
			done.SystemMessage = c.SystemMessage
			if c.Halt {
				done.Decision = "halt"
			}
		}
		if d, ok := any(out).(interface{ hookDecision() string }); ok && d.hookDecision() != "" {
			done.Decision = d.hookDecision()
		}
		if len(decide) > 0 && decide[0] != nil {
			done.Decision = decide[0](out)
		}
		s.publishScoped(sc, done)
		return out, true, nil
	}
	closed := fail.Kind != hookrun.KindCanceled && h.policy(p) == FailClosed && p.def.Closed != nil
	applied := "open"
	if closed {
		applied = "closed"
	}
	s.publishScoped(sc, &event.HookFailed{HookID: id, Point: p.def.Name, Name: h.name, CallID: callID, Error: fail.Err.Error(),
		Timeout: fail.Kind == hookrun.KindTimeout, Canceled: fail.Kind == hookrun.KindCanceled, PolicyApplied: applied})
	if !closed {
		return zero, false, fail
	}
	return p.def.Closed(&HookError{Kind: string(fail.Kind), Err: fail.Err}), true, fail
}

// policy is the hook's own fail policy, or the point's when it has none or the point allows no other.
func (h *hookEntry) policy(p interface{ fail() (FailPolicy, bool) }) FailPolicy {
	def, closedOnly := p.fail()
	if h.cfg.fail == FailDefault || closedOnly {
		return def
	}
	return h.cfg.fail
}

func (p *HookPoint[In, Out]) fail() (FailPolicy, bool) { return p.def.Fail, p.meta.closedOnly }

func invokeSync[In, Out any](ctx context.Context, h *hookEntry, in In) (Out, error) {
	var out Out
	if h.json == nil {
		return h.fn.(HookFunc[In, Out])(ctx, in) // guaranteed by On's type parameters
	}
	b, err := json.Marshal(in)
	if err != nil {
		return out, err
	}
	raw, err := h.json(ctx, b)
	if err != nil {
		return out, err
	}
	if t := bytes.TrimSpace(raw); len(t) > 0 && string(t) != "null" {
		if err := json.Unmarshal(t, &out); err != nil {
			return out, fmt.Errorf("invalid output: %w", err)
		}
	}
	return out, nil
}

func commonOf(out any) *Common {
	if c, ok := out.(interface{ hookCommon() *Common }); ok {
		return c.hookCommon()
	}
	return nil
}

// takeCommon hands one hook's Common to where it belongs: system_message as a notice, additional_context and halt to the sink, or to the queue outside a turn.
func (s *Session) takeCommon(sc *hookScope, name string, out any) {
	c := commonOf(out)
	if c == nil {
		return
	}
	if c.SystemMessage != "" {
		s.publishScoped(sc, &event.Notice{Source: "hook:" + name, Text: c.SystemMessage})
	}
	var sink *hookSink
	if sc != nil {
		sink = sc.sink
	}
	if sink != nil {
		sink.mu.Lock()
		sink.halt = sink.halt || c.Halt
		if c.AdditionalContext != "" && !sink.discard {
			sink.notes = append(sink.notes, &message.ReminderBlock{Source: "hook:" + name, Text: c.AdditionalContext})
		}
		sink.mu.Unlock()
		return
	}
	if c.AdditionalContext != "" {
		s.post(func(*state) { s.enqueue(s.reminderItem("hook:"+name, c.AdditionalContext, false)) })
	}
}

// reminderItem is a queue item the runtime writes itself: a reminder in next_loop mode.
func (s *Session) reminderItem(source, text string, wake bool) *queueItem {
	id := s.agent.cfg.ids.NewID("in")
	origin := message.Origin{Kind: message.OriginReminder, Source: source}
	return &queueItem{id: id, origin: origin, mode: ModeNextLoop, wake: wake, parts: []*itemPart{{id: id, origin: origin, text: text, receipt: s.newReceipt(id)}}}
}

// launchAsync starts one async hook on a copy of the input, in the session's async pool; the pool being full drops the call.
func launchAsync[In, Out any](ctx context.Context, s *Session, p *HookPoint[In, Out], h *hookEntry, in In) {
	sc := scopeOf(ctx)
	if !s.asyncPool.TryAcquire() {
		s.publishScoped(sc, &event.Error{Code: "async_hook_dropped", Message: "async hook " + h.name + " at " + p.def.Name + " dropped: the async pool is full"})
		return
	}
	payload, perr := json.Marshal(in)
	id := s.agent.cfg.ids.NewID("hook")
	callID := ""
	if sc != nil {
		callID = sc.callID
	}
	s.publishScoped(sc, &event.HookStarted{HookID: id, Point: p.def.Name, Name: h.name, Async: true, CallID: callID})
	gen := s.clearGen.Load()
	depth, _ := ctx.Value(hookDepthKey{}).(int)
	actx := context.WithValue(withMarker(s.ctx, marker{chain: s.chain, kind: "hook"}), hookDepthKey{}, depth)
	actx, cancel := bounded(actx, h.timeoutOr(ms(defaults().Hooks.AsyncTimeout)))
	s.wk.Go("hook async "+p.def.Name+" "+h.name, func() {
		defer s.asyncPool.Release()
		defer cancel()
		started := time.Now()
		inj, err := func() (inj Injection, err error) {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("panic: %v", r)
				}
			}()
			if h.jsonAsync != nil {
				if perr != nil {
					return Injection{}, perr
				}
				return h.jsonAsync(actx, payload)
			}
			cp := in
			if perr == nil && json.Unmarshal(payload, &cp) == nil {
				restoreEnv(&cp, &in) // the copy is deep, so a hook that lingers never sees later rewrites
			}
			return h.fn.(AsyncHookFunc[In])(actx, cp) // guaranteed by OnAsync's type parameters
		}()
		if err == nil && actx.Err() != nil {
			err = actx.Err()
		}
		if err != nil {
			s.publish(&event.HookFailed{HookID: id, Point: p.def.Name, Name: h.name, CallID: callID, Error: err.Error(),
				Timeout: errors.Is(actx.Err(), context.DeadlineExceeded), Canceled: errors.Is(actx.Err(), context.Canceled), PolicyApplied: "open"})
			return
		}
		s.publish(&event.HookCompleted{HookID: id, Point: p.def.Name, Name: h.name, CallID: callID, DurationMs: time.Since(started).Milliseconds(), SystemMessage: inj.SystemMessage})
		if inj.SystemMessage != "" {
			s.publish(&event.Notice{Source: "hook:" + h.name, Text: inj.SystemMessage})
		}
		if inj.AdditionalContext == "" {
			return
		}
		s.post(func(*state) {
			if s.clearGen.Load() != gen {
				return // a /clear since the hook started: the context it answered no longer exists
			}
			s.enqueue(s.reminderItem("hook:"+h.name, inj.AdditionalContext, inj.Wake && h.wake.allow()))
		})
	})
}

// restoreEnv puts back the fields JSON leaves out of a hook input, such as the session handle.
func restoreEnv(cp, orig any) {
	c, ok1 := cp.(interface{ hookEnv() *HookEnv })
	o, ok2 := orig.(interface{ hookEnv() *HookEnv })
	if ok1 && ok2 {
		c.hookEnv().Session = o.hookEnv().Session
	}
}

// env is the HookEnv of a point fired by this session.
func (s *Session) env(turnID string, step int) HookEnv {
	return HookEnv{Session: s, SessionID: s.id, Agent: s.nick(), TurnID: turnID, Step: step, Path: s.rootPath()}
}

// ---- matchers ----

// Matcher selects the keys of a hook point a hook runs for.
type Matcher interface{ Match(key string) bool }

type namesMatcher []string

func (m namesMatcher) Match(key string) bool { return slices.Contains(m, key) }

type regexMatcher struct{ re *regexp.Regexp }

func (m regexMatcher) Match(key string) bool { return m.re.MatchString(key) }

type funcMatcher func(string) bool

func (m funcMatcher) Match(key string) bool { return m(key) }

// badMatcher carries a compile error to registration, where On reports it.
type badMatcher struct{ err error }

func (badMatcher) Match(string) bool { return false }

// MatchNames matches exact names, case-sensitively.
func MatchNames(names ...string) Matcher { return namesMatcher(slices.Clone(names)) }

// MatchRegexp matches the whole key with an expression of the portable subset of spec/runtime.md; a bad expression makes registration fail.
func MatchRegexp(expr string) Matcher {
	re, err := compilePortable(expr)
	if err != nil {
		return badMatcher{err: err}
	}
	return regexMatcher{re: re}
}

// MatchFunc is Go only, so a hook that uses it cannot be described in a portable script.
func MatchFunc(fn func(key string) bool) Matcher { return funcMatcher(fn) }

var namesForm = regexp.MustCompile(`^[A-Za-z0-9_.:|-]+$`)

// ParseMatcher reads the declarative form: "" or "*" matches everything, names joined by | match exactly, and "re:" starts an anchored expression.
func ParseMatcher(s string) (Matcher, error) {
	switch {
	case s == "" || s == "*":
		return nil, nil
	case strings.HasPrefix(s, "re:"):
		re, err := compilePortable(strings.TrimPrefix(s, "re:"))
		if err != nil {
			return nil, err
		}
		return regexMatcher{re: re}, nil
	case namesForm.MatchString(s):
		return namesMatcher(strings.Split(s, "|")), nil
	}
	return nil, fmt.Errorf("matcher %q is neither names nor re:", s)
}

// compilePortable accepts only the regular-expression subset every binding runs alike, and anchors it to the whole string.
func compilePortable(expr string) (*regexp.Regexp, error) {
	if err := checkPortable(expr); err != nil {
		return nil, fmt.Errorf("regex %q: %w", expr, err)
	}
	return regexp.Compile(`^(?:` + expr + `)$`)
}

// checkPortable walks expr by the grammar of spec/runtime.md §10.4 and rejects anything outside it.
func checkPortable(expr string) error {
	if !utf8.ValidString(expr) {
		return errors.New("invalid UTF-8")
	}
	p := &reParser{src: []rune(expr)}
	if err := p.alt(0); err != nil {
		return err
	}
	if p.i < len(p.src) {
		return fmt.Errorf("unbalanced %q at %d", p.src[p.i], p.i)
	}
	return nil
}

type reParser struct {
	src []rune
	i   int
}

func (p *reParser) peek() (rune, bool) {
	if p.i < len(p.src) {
		return p.src[p.i], true
	}
	return 0, false
}

func (p *reParser) alt(depth int) error {
	for {
		if err := p.concat(depth); err != nil {
			return err
		}
		if r, ok := p.peek(); ok && r == '|' {
			p.i++
			continue
		}
		return nil
	}
}

func (p *reParser) concat(depth int) error {
	for {
		r, ok := p.peek()
		if !ok || r == '|' || r == ')' && depth > 0 {
			return nil
		}
		if err := p.atom(depth); err != nil {
			return err
		}
		if err := p.quant(); err != nil {
			return err
		}
	}
}

func (p *reParser) atom(depth int) error {
	r := p.src[p.i]
	switch r {
	case '^', '$':
		return fmt.Errorf("%q is not allowed; the expression always matches the whole string", r)
	case '*', '+', '?', '{':
		return fmt.Errorf("%q has nothing to repeat at %d", r, p.i)
	case ')':
		return fmt.Errorf("unbalanced ) at %d", p.i)
	case ']', '}':
		return fmt.Errorf("%q must be escaped at %d", r, p.i)
	case '.':
		p.i++
		return nil
	case '[':
		return p.class()
	case '(':
		p.i++
		if strings.HasPrefix(string(p.src[p.i:]), "?:") {
			p.i += 2
		} else if r, ok := p.peek(); ok && r == '?' {
			return fmt.Errorf("group syntax (?%s is not allowed", string(p.src[p.i+1:min(p.i+2, len(p.src))]))
		}
		if err := p.alt(depth + 1); err != nil {
			return err
		}
		if r, ok := p.peek(); !ok || r != ')' {
			return errors.New("missing )")
		}
		p.i++
		return nil
	case '\\':
		return p.escape(false)
	}
	p.i++
	return nil
}

// escape reads a backslash sequence: a class shorthand, a word boundary outside classes, or escaped ASCII punctuation.
func (p *reParser) escape(inClass bool) error {
	p.i++
	r, ok := p.peek()
	if !ok {
		return errors.New("trailing backslash")
	}
	p.i++
	switch {
	case strings.ContainsRune("dDwWsS", r):
		return nil
	case strings.ContainsRune("bB", r) && !inClass:
		return nil
	case r < 0x80 && (r >= '!' && r <= '/' || r >= ':' && r <= '@' || r >= '[' && r <= '`' || r >= '{' && r <= '~'):
		return nil
	}
	return fmt.Errorf("escape \\%c is not allowed", r)
}

func (p *reParser) class() error {
	p.i++ // [
	if r, ok := p.peek(); ok && r == '^' {
		p.i++
	}
	items := 0
	for {
		r, ok := p.peek()
		if !ok {
			return errors.New("missing ]")
		}
		switch r {
		case ']':
			if items == 0 {
				return errors.New("empty character class")
			}
			p.i++
			return nil
		case '[':
			return errors.New("[ inside a class must be escaped")
		}
		single, err := p.classAtom()
		if err != nil {
			return err
		}
		items++
		if r, ok := p.peek(); ok && r == '-' && p.i+1 < len(p.src) && p.src[p.i+1] != ']' {
			p.i++
			lo := single
			hi, err := p.classAtom()
			if err != nil {
				return err
			}
			if lo < 0 || hi < 0 || lo > hi {
				return errors.New("bad range in character class")
			}
		}
	}
}

// classAtom reads one class member and returns its rune, or -1 for a shorthand such as \d.
func (p *reParser) classAtom() (rune, error) {
	r := p.src[p.i]
	if r != '\\' {
		p.i++
		return r, nil
	}
	if err := p.escape(true); err != nil {
		return 0, err
	}
	if e := p.src[p.i-1]; strings.ContainsRune("dDwWsS", e) {
		return -1, nil
	}
	return p.src[p.i-1], nil
}

func (p *reParser) quant() error {
	r, ok := p.peek()
	if !ok {
		return nil
	}
	switch r {
	case '*', '+', '?':
		p.i++
	case '{':
		end := slices.Index(p.src[p.i:], '}')
		if end < 0 {
			return errors.New("{ must start a repetition or be escaped")
		}
		body := string(p.src[p.i+1 : p.i+end])
		lo, hi, found := strings.Cut(body, ",")
		m, err := strconv.Atoi(lo)
		if err != nil || m < 0 || m > 1000 {
			return fmt.Errorf("bad repetition {%s}", body)
		}
		if found && hi != "" {
			n, err := strconv.Atoi(hi)
			if err != nil || n < m || n > 1000 {
				return fmt.Errorf("bad repetition {%s}", body)
			}
		}
		p.i += end + 1
	default:
		return nil
	}
	if r, ok := p.peek(); ok && r == '?' {
		p.i++ // lazy
	}
	if r, ok := p.peek(); ok && (r == '*' || r == '+' || r == '?' || r == '{') {
		return errors.New("nested repetition")
	}
	return nil
}
