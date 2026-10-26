package kernel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/rand/v2"
	"slices"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/internal/bus"
	"github.com/estonshi/aperture-potatos/internal/hookrun"
	"github.com/estonshi/aperture-potatos/internal/prompt"
	"github.com/estonshi/aperture-potatos/internal/workers"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/store"
)

// Agent holds the configuration and agent-scope registrations that every session it opens shares; it is safe for concurrent use.
type Agent struct {
	cfg   config
	reg   *registry
	exts  []string
	caps  capCache
	state stateStore

	base    *Agent     // the agent this one was derived from, whose registrations it layers its own over
	derived derivation // what Derive does to them
	views   viewCache

	mu       sync.Mutex
	nickname string
	open     map[string]*Session
	children []*Agent // agents derived from this one, for rule checks on agent-scope changes
	sinks    []*agentSink
	closers  []func(context.Context) error
	closed   bool
}

type config struct {
	nickname         string
	description      string
	instructions     string
	instructionsFunc func(context.Context, SessionInfo) (string, error)
	model            *provider.ModelRef
	store            store.SessionStore
	labels           store.LabelStore // the store's, guarded apart, or nil when it keeps no labels
	storePolicy      StorePolicy
	retry            provider.RetryPolicy
	limits           Limits
	autoContinue     bool
	clock            Clock
	ids              IDGenerator
	logger           *slog.Logger
	panicOnInternal  bool
	inputMode        InputMode
	rawEvents        bool
	exts             []Extension
	app              []func(Registrar) error // option-form registrations, installed as the "app" extension in call order
	sinks            []sinkSpec
	capDecls         []capDecl
	resolver         provider.CapabilityResolver
	resolverSet      bool // WithCapabilityResolver was given, so the providers' own resolvers are not asked, even when it is nil
	rules            []ApprovalRule
	requestHeaders   func(RequestInfo) map[string]string
	captureHeaders   []string
	costSource       CostSource
	context          ContextPolicy
	tokenCounter     provider.TokenCounter
	texts            prompt.Overrides
	anomaly          AnomalyConfig
}

// Option configures an Agent in New.
type Option func(*config)

// defaultNickname names an agent created without WithNickname, because the identity section always carries a name.
const defaultNickname = "Assistant"

// WithNickname sets the name the identity section gives the model, at most 64 characters; it defaults to "Assistant".
func WithNickname(name string) Option { return func(c *config) { c.nickname = name } }

// WithDescription adds a line about the agent to the identity section, and is what a peer card advertises.
func WithDescription(text string) Option { return func(c *config) { c.description = text } }

// WithInstructions sets the developer section of the system prompt, unless WithInstructionsFunc or SessionInstructions overrides it.
func WithInstructions(text string) Option { return func(c *config) { c.instructions = text } }

// WithInstructionsFunc computes the developer section at every epoch start, off the actor.
func WithInstructionsFunc(fn func(ctx context.Context, info SessionInfo) (string, error)) Option {
	return func(c *config) { c.instructionsFunc = fn }
}

// WithModel sets the model every session starts with, and New fails without it.
func WithModel(ref provider.ModelRef) Option { return func(c *config) { c.model = &ref } }

// WithStore sets where sessions are persisted, so they can be resumed; without it they live in memory and end with the process.
func WithStore(st store.SessionStore) Option { return func(c *config) { c.store = st } }

// WithStorePolicy chooses what a failed store write does; the default is StoreWarn.
func WithStorePolicy(p StorePolicy) Option { return func(c *config) { c.storePolicy = p } }

// WithRetryPolicy bounds how retryable model call errors are retried; zero fields keep the defaults.
func WithRetryPolicy(p provider.RetryPolicy) Option { return func(c *config) { c.retry = p } }

// WithLimits bounds steps, timeouts, queues and other per-session work; zero fields keep the defaults.
func WithLimits(l Limits) Option { return func(c *config) { c.limits = l } }

// WithAutoContinueTruncatedText lets a reply cut off by the output limit continue once instead of ending the turn.
func WithAutoContinueTruncatedText(on bool) Option { return func(c *config) { c.autoContinue = on } }

// WithLogger receives the kernel's and extensions' warnings, which are discarded without it.
func WithLogger(l *slog.Logger) Option { return func(c *config) { c.logger = l } }

// WithClock replaces the clock that stamps events and turns, so tests get stable timestamps.
func WithClock(c Clock) Option { return func(cfg *config) { cfg.clock = c } }

// WithIDGenerator replaces how session, turn, message and other ids are minted, for stable test ids or a tenant prefix.
func WithIDGenerator(g IDGenerator) Option { return func(c *config) { c.ids = g } }

// WithPanicOnInternalError makes a kernel bug panic instead of closing the session, so tests see it.
func WithPanicOnInternalError(on bool) Option { return func(c *config) { c.panicOnInternal = on } }

// WithRawProviderEvents also publishes provider.raw events with the provider's wire events.
func WithRawProviderEvents() Option { return func(c *config) { c.rawEvents = true } }

// WithTexts overrides the fixed texts of the runtime and the built-in adapters for every session of the agent: keys are section keys of spec/prompts/reminders.md, or prompt file names such as "system.core". New rejects a key that names neither, and a system.core override that drops a runtime tag.
func WithTexts(overrides map[string]string) Option {
	return func(c *config) { c.texts = prompt.Overrides(maps.Clone(overrides)) }
}

// WithOutputAnomalyDetection changes the repetition detector behind model.anomaly, which is on by default; Disabled turns it off.
func WithOutputAnomalyDetection(c AnomalyConfig) Option { return func(cfg *config) { cfg.anomaly = c } }

// WithTool registers t at agent scope as part of the "app" extension, so New fails on a duplicate or invalid name.
func WithTool(t Tool) Option {
	return func(c *config) { c.app = append(c.app, func(r Registrar) error { _, err := r.Tool(t); return err }) }
}

// WithCommand registers cmd at agent scope as part of the "app" extension, so New fails on a duplicate or invalid name.
func WithCommand(cmd Command) Option {
	return func(c *config) {
		c.app = append(c.app, func(r Registrar) error { _, err := r.Command(cmd); return err })
	}
}

// New validates the options and installs the extensions, but opens no session and calls no model.
func New(opts ...Option) (*Agent, error) {
	cfg := config{nickname: defaultNickname, storePolicy: StoreWarn, clock: SystemClock(), ids: RandomIDs(), inputMode: ModeNextLoop}
	for _, o := range opts {
		o(&cfg)
	}
	switch {
	case cfg.model == nil || cfg.model.Provider == nil:
		return nil, errors.New("no model configured; use WithModel")
	case cfg.nickname == "" || utf8.RuneCountInString(cfg.nickname) > 64:
		return nil, fmt.Errorf("%w: nickname %q", ErrInvalidName, cfg.nickname)
	case cfg.storePolicy != StoreWarn && cfg.storePolicy != StoreStrict:
		return nil, fmt.Errorf("unknown store policy %q", cfg.storePolicy)
	}
	if err := prompt.Check(cfg.texts); err != nil {
		return nil, fmt.Errorf("%w: texts: %v", ErrInvalidName, err)
	}
	if _, ok := modeRank[cfg.inputMode]; !ok {
		return nil, ErrInvalidMode
	}
	if cfg.store == nil {
		cfg.store = store.NewMemory()
	}
	if ls, ok := cfg.store.(store.LabelStore); ok {
		cfg.labels = safeLabels{ls}
	}
	cfg.store = guardStore(cfg.store)
	if cfg.logger == nil {
		cfg.logger = slog.New(slog.DiscardHandler)
	}
	cfg.limits = cfg.limits.resolved()
	cfg.context = cfg.context.resolved()
	cfg.anomaly = cfg.anomaly.resolved()
	cfg.retry = resolvedRetry(cfg.retry)
	rules, err := compileRules(cfg.rules)
	if err != nil {
		return nil, err
	}
	a := &Agent{cfg: cfg, reg: newRegistry(ScopeAgent), nickname: cfg.nickname, open: map[string]*Session{}}
	for _, rule := range rules {
		if _, err := a.reg.addCompiledRule(rule, ScopeAgent); err != nil {
			return nil, err
		}
	}
	names, err := install(&registrar{scope: ScopeAgent, agent: a, reg: a.reg}, cfg.exts, appExtension(cfg.app))
	if err != nil {
		return nil, err
	}
	a.exts = names
	notify := func(c regChange) {
		for _, s := range a.sessions() {
			s.announceRegistry(ScopeAgent, c)
		}
	}
	a.reg.notify.Store(&notify)
	for _, sk := range cfg.sinks {
		a.sinks = append(a.sinks, newAgentSink(sk))
	}
	return a, nil
}

func appExtension(fns []func(Registrar) error) Extension {
	if len(fns) == 0 {
		return nil
	}
	return ExtensionFunc(appName, func(r Registrar) error {
		for _, fn := range fns {
			if err := fn(r); err != nil {
				return err
			}
		}
		return nil
	})
}

// Logger is the logger WithLogger set, so an extension's warnings go where the host sends the agent's.
func (a *Agent) Logger() *slog.Logger { return a.cfg.logger }

// Description is the agent's own description, as WithDescription or Derive set it.
func (a *Agent) Description() string { return a.cfg.description }

// Nickname is the current name, which SetNickname may have changed since New.
func (a *Agent) Nickname() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.nickname
}

// SetNickname applies to sessions created afterwards; an open session keeps the name its epoch was rendered with.
func (a *Agent) SetNickname(name string) error {
	if name == "" || utf8.RuneCountInString(name) > 64 {
		return fmt.Errorf("%w: nickname %q", ErrInvalidName, name)
	}
	a.mu.Lock()
	a.nickname = name
	a.mu.Unlock()
	return nil
}

func (a *Agent) sessions() []*Session {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Collect(maps.Values(a.open))
}

func (a *Agent) register(s *Session) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case a.closed:
		return ErrAgentClosed
	case a.open[s.id] != nil:
		return ErrSessionOpen
	}
	a.open[s.id] = s
	return nil
}

func (a *Agent) unregister(s *Session) {
	a.mu.Lock()
	if a.open[s.id] == s {
		delete(a.open, s.id)
	}
	a.mu.Unlock()
}

// SessionOption configures one session in NewSession or Resume.
type SessionOption func(*sessionConfig)

type sessionConfig struct {
	inputMode    InputMode
	instructions *string
	labels       map[string]string
	exts         []Extension
	sinks        []sinkSpec
	rules        []ApprovalRule
	owner        TaskRuntime // ChildOf
}

// SessionInstructions replaces the agent's developer section for this session.
func SessionInstructions(text string) SessionOption {
	return func(c *sessionConfig) { c.instructions = &text }
}

// WithLabels attaches labels the kernel does not interpret; extensions read them by convention.
func WithLabels(labels map[string]string) SessionOption {
	return func(c *sessionConfig) { c.labels = maps.Clone(labels) }
}

// NewSession returns once the first epoch is written, so a session it returns is ready for input.
func (a *Agent) NewSession(ctx context.Context, opts ...SessionOption) (*Session, error) {
	var sc sessionConfig
	for _, o := range opts {
		o(&sc)
	}
	mode := a.cfg.inputMode
	if sc.inputMode != "" {
		mode = sc.inputMode
	}
	if _, ok := modeRank[mode]; !ok {
		return nil, ErrInvalidMode
	}
	own, err := compileRules(sc.rules)
	if err != nil {
		return nil, err
	}
	s := a.newSession(a.cfg.ids.NewID("ses"), mode, sc)
	for _, rule := range own {
		if _, err := s.reg.addCompiledRule(rule, ScopeSession); err != nil {
			return nil, err
		}
	}
	return a.start(ctx, s, sc, s.init)
}

// start registers s, attaches its sinks and extensions, and returns once init has written or restored its epoch.
func (a *Agent) start(ctx context.Context, s *Session, sc sessionConfig, init func()) (*Session, error) {
	if err := a.register(s); err != nil {
		return nil, err
	}
	if err := checkRuleIDs(s.effective().rules); err != nil {
		a.unregister(s)
		return nil, err
	}
	if sc.owner != nil {
		if err := s.adopt(sc.owner); err != nil {
			a.unregister(s)
			return nil, err
		}
	}
	a.mu.Lock()
	sinks := slices.Clone(a.sinks)
	a.mu.Unlock()
	for _, sk := range sinks {
		sk.attach(s) // before the actor starts, so no event is missed
	}
	for _, sk := range sc.sinks {
		s.attachSink(sk)
	}
	names, err := install(s.Registrar(), sc.exts, nil)
	if err != nil {
		a.unregister(s)
		s.stop()
		s.bus.Close()
		return nil, err
	}
	s.exts = slices.Concat(a.exts, s.exts, names) // a fork's s.exts already holds the source's session extensions
	notify := func(c regChange) { s.announceRegistry(ScopeSession, c) }
	s.reg.notify.Store(&notify)
	go s.run()
	s.wk.Go("init", init)
	select {
	case err := <-s.ready:
		if err != nil {
			return nil, err
		}
		return s, nil
	case <-ctx.Done():
		go func() { _ = s.Close(context.Background()) }()
		return nil, context.Cause(ctx)
	}
}

func (a *Agent) newSession(id string, mode InputMode, sc sessionConfig) *Session {
	ctx, stop := context.WithCancel(context.Background())
	maxExt := -1
	if a.cfg.limits.MaxQueuedInputs >= 0 {
		maxExt = a.cfg.limits.MaxQueuedInputs + floodGuard
	}
	s := &Session{
		id:        id,
		source:    "new",
		agent:     a,
		chain:     []string{id},
		inputMode: mode,
		sessInstr: sc.instructions,
		limits:    a.cfg.limits,
		ctx:       ctx,
		stop:      stop,
		mb:        newMailbox(maxExt),
		wk:        workers.New(),
		done:      make(chan struct{}),
		ready:     make(chan error, 1),
		reg:       newRegistry(ScopeSession),
		jitter:    func(d time.Duration) time.Duration { return rand.N(d + 1) },
		asyncPool: hookrun.NewPool(a.cfg.limits.MaxAsyncHooks),
	}
	name := a.Nickname()
	s.nickname.Store(&name)
	labels := sc.labels
	s.labels.Store(&labels)
	s.bus = bus.New(func(p event.Payload) *event.Event { return s.envelope(p, "", 0, 0) })
	s.ctl.announce = s.announceInterrupt
	s.st = state{phase: phaseInitializing, executed: map[string]*message.ToolResultBlock{}, commands: map[*queuedCommand]context.CancelFunc{}, learned: map[provider.ModelInfo]*learned{}}
	s.st.tasks = taskTable{byID: map[string]*taskEntry{}, byCall: map[string][]string{}, waits: map[string]string{}}
	s.st.ledger.busy = s.st.tasks.startedIn
	s.tasks = &Tasks{s: s}
	s.st.model = modelState{ref: *a.cfg.model, info: infoOf(*a.cfg.model), caps: provider.Capabilities{ContextWindow: provider.DefaultContextWindow}}
	s.st.usage = newUsageReport()
	s.st.policy = a.cfg.context
	s.st.context = ContextUsage{Basis: "estimated"}
	s.st.setWindow()
	s.publishSnapshot()
	return s
}

// floodGuard bounds the mailbox above the input queue; reservations and merges bound inputs, so only a caller outpacing the actor meets it.
const floodGuard = 4096

func (s *Session) announceRegistry(scope Scope, c regChange) {
	s.post(func(st *state) {
		effective := "now"
		switch c.kind {
		case KindTool, KindHostedTool:
			effective = "turn"
			if st.model.caps.ThinkingPrefixBound {
				effective = "epoch" // the definitions are frozen with the epoch, so a change waits for the next one
			}
		case KindPromptSection:
			effective = "epoch"
		}
		s.publish(&event.RegistryChanged{Scope: scope.String(), Kind: string(c.kind), Name: c.name, Op: c.op, Effective: effective})
		if c.kind == KindPromptSection && c.section == nil { // a Hide or its undoing, of the agent's section
			if r := s.outer().sections[c.name]; r != nil {
				c.section = &r.ps
			}
		}
		if c.section != nil && c.section.AnnounceChanges && st.phase != phaseInitializing && st.phase != phaseClosing && st.phase != phaseClosed {
			s.announceSection(c)
		}
	})
}

func (a *Agent) stateStore() *stateStore { return &a.state }

// Close closes every open session concurrently, then ends the agent-level subscriptions.
func (a *Agent) Close(ctx context.Context) error {
	a.mu.Lock()
	a.closed = true
	sessions := slices.Collect(maps.Values(a.open))
	sinks, closers := a.sinks, a.closers
	a.sinks, a.closers = nil, nil
	a.mu.Unlock()
	errs := make([]error, len(sessions))
	var wg sync.WaitGroup
	for i, s := range sessions {
		wg.Go(func() { errs[i] = s.Close(ctx) })
	}
	wg.Wait()
	for _, sk := range sinks {
		sk.end(false) // the closed sessions' pumps drain first, so the last event each sink sees is session.closed
	}
	for i := len(closers) - 1; i >= 0; i-- { // the last installed stops first, as deferred calls do
		errs = append(errs, closers[i](ctx))
	}
	return errors.Join(errs...)
}

// OnClose runs fn once Close has closed every session, so an agent-scope extension stops what it started, such as connections and child processes.
func (a *Agent) OnClose(fn func(context.Context) error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closers = append(a.closers, fn)
}
