package kernel

import (
	"context"
	"fmt"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/internal/tokens"
	"github.com/estonshi/aperture-potatos/internal/workers"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/spec"
)

type phase string

const (
	phaseInitializing phase = "initializing"
	phaseIdle         phase = "idle"
	phaseRunning      phase = "running"
	phaseMaintenance  phase = "maintenance"
	phaseClosing      phase = "closing"
	phaseClosed       phase = "closed"
)

type opMsg struct {
	fn   func(*state)
	done chan struct{} // nil for fire-and-forget posts
}

type inputMsg struct{ item *queueItem }

type commandMsg struct{ qc *queuedCommand }

type turnExitMsg struct {
	turnID string
	out    turnOutcome
}

type commandExitMsg struct {
	qc      *queuedCommand
	res     CommandResult
	err     error
	blocked bool
}

type initDoneMsg struct{ err error }

type closeMsg struct {
	done   chan error // nil when the kernel closes the session itself
	reason string
}

type closeReadyMsg struct{ leaked []workers.Info }

func (*opMsg) isMsg()          {}
func (*inputMsg) isMsg()       {}
func (*commandMsg) isMsg()     {}
func (*turnExitMsg) isMsg()    {}
func (*commandExitMsg) isMsg() {}
func (*initDoneMsg) isMsg()    {}
func (*closeMsg) isMsg()       {}
func (*closeReadyMsg) isMsg()  {}

type turnState struct {
	id         string
	started    time.Time
	step       int
	humanInput bool      // a human, command or agent item reached this turn, so it is not an automatic turn
	preempted  bool      // an interrupt item or InterruptFirst command canceled it, so its own inputs are kept for the next turn
	dropInputs bool      // InterruptAndDrop canceled it, so its uncommitted human, command and agent items are discarded
	view       *mainView // a side chat's main part for this turn, which the turn's side calls send too
}

// state is owned by the actor; the current epoch's history, stableLen and the executed-call ledger live here too.
type state struct {
	phase     phase
	turn      *turnState
	queue     []*queueItem // seq order
	seq       uint64
	autoTurns int
	version   uint64

	history   []message.Message // the current epoch; history[0] is its system line
	stableLen int               // longest prefix that is valid for the API, which side calls may read
	epoch     int
	lineSeq   int64
	pending   []message.Message // committed lines the store does not have yet
	executed  map[string]*message.ToolResultBlock
	system    []provider.SystemBlock

	usage   UsageReport
	context ContextUsage
	ledger  callLedger
	callSeq int64       // seq of the last call booked
	calls   []ModelCall // booked calls the store does not have yet
	writing bool        // a calls writer is running
	policy  ContextPolicy
	meter   tokens.Estimate

	mainTokens int64 // a side chat's main part as its latest request or turn carried it, which the estimate adds and a report subtracts
	mainLines  int

	pins            []Pin // oldest first, one per key
	compactFailures int   // consecutive failed automatic compactions
	autoCompactOff  bool  // MaxConsecutiveFailures reached, so this session no longer compacts on its own

	model         modelState
	pendingSwitch *modelState
	nextReasoning *provider.ReasoningParams // a pre_model_request hook's reasoning for the next turn only
	preemptCancel func()                    // set by preempt, run once the preempting input is announced
	learned       map[provider.ModelInfo]*learned
	fallbackEpoch int // the epoch whose reasoning an API refused to replay; later epochs start over

	tasks      taskTable
	owned      *childSet         // ChildOf: turns of its own wait for the owning task to run
	deniedStop *string           // a reviewer said deny_and_stop in the running turn, which fails the owning task once it ends
	lastTurn   TurnResult        // what WaitQuiet returns
	quiet      []chan TurnResult // WaitQuiet callers

	commands     map[*queuedCommand]context.CancelFunc
	runNowWait   []*queuedCommand // RunNow commands that arrived while initializing
	closeWaiters []chan error
	closeReason  string
}

// exec runs fn on the actor and waits for it; it is the only way a worker touches session state.
func (s *Session) exec(fn func(*state)) error {
	o := &opMsg{fn: fn, done: make(chan struct{})}
	if err := s.mb.push(o, false); err != nil {
		return err
	}
	select {
	case <-o.done:
		return nil
	case <-s.done:
		return ErrSessionClosed
	}
}

// post is exec without waiting, for callers that must never block on the actor.
func (s *Session) post(fn func(*state)) { _ = s.mb.push(&opMsg{fn: fn}, false) }

func (s *Session) envelope(p event.Payload, turnID string, step, attempt int) *event.Event {
	return &event.Event{Type: p.EventType(), Time: s.agent.cfg.clock.Now(), SessionID: s.id, Agent: s.nick(), TurnID: turnID, Step: step, Attempt: attempt, Data: p}
}

func (s *Session) publish(p event.Payload) { s.bus.Publish(s.envelope(p, "", 0, 0)) }

func (s *Session) publishAt(p event.Payload, turnID string, step, attempt int) {
	s.bus.Publish(s.envelope(p, turnID, step, attempt))
}

// publishUnder publishes an event a task caused; it happens outside any turn, and the hop names the call that started the task.
func (s *Session) publishUnder(p event.Payload, path []event.Hop) {
	ev := s.envelope(p, "", 0, 0)
	ev.Path = path
	s.bus.Publish(ev)
}

func (s *Session) run() {
	defer close(s.done)
	for range s.mb.ready {
		if !s.drain() {
			return
		}
	}
}

// drain handles every queued message and then looks for work; false means the actor is done.
func (s *Session) drain() (alive bool) {
	defer func() {
		if r := recover(); r != nil {
			s.crash(r)
			alive = false
		}
	}()
	for {
		m, ok := s.mb.pop()
		if !ok {
			break
		}
		s.handle(m)
		if s.st.phase == phaseClosed {
			s.mb.close()
			return false
		}
	}
	s.maybeStartWork()
	s.checkQuiet()
	s.publishSnapshot() // a fallback; closures that change snapshot fields publish it themselves before their events
	return true
}

func (s *Session) handle(m msg) {
	switch m := m.(type) {
	case *opMsg:
		m.fn(&s.st)
		if m.done != nil {
			close(m.done)
		}
	case *inputMsg:
		s.enqueue(m.item)
	case *commandMsg:
		s.onCommand(m.qc)
	case *turnExitMsg:
		s.onTurnExit(m)
	case *commandExitMsg:
		s.onCommandExit(m)
	case *initDoneMsg:
		s.onInitDone(m)
	case *closeMsg:
		s.onClose(m)
	case *closeReadyMsg:
		s.onCloseReady(m)
	}
	s.st.version++
}

func (s *Session) publishSnapshot() {
	st := &s.st
	sn := &Snapshot{SessionID: s.id, Phase: string(st.phase), Epoch: st.epoch, Model: st.model.info, QueuedInputs: len(st.queue), Usage: st.usage.clone(), Context: st.context, Version: st.version}
	if st.turn != nil {
		sn.TurnID, sn.Step = st.turn.id, st.turn.step
	}
	s.snap.Store(sn)
}

// setPhase publishes the snapshot before session.state_changed, so a reader woken by the event never sees the old phase.
func (s *Session) setPhase(p phase) {
	from := s.st.phase
	s.st.phase = p
	s.publishSnapshot()
	if from != p {
		s.publish(&event.SessionStateChanged{From: string(from), To: string(p)})
	}
}

func (s *Session) maxAuto() int { return s.limits.MaxConsecutiveAutoTurns }

func (s *Session) maybeStartWork() {
	if s.st.phase != phaseIdle {
		return
	}
	plan, qc := s.st.nextWork(s.maxAuto())
	switch {
	case qc != nil:
		s.startCommand(qc) // phase → maintenance
	case plan != nil:
		s.startTurn(plan)
	}
}

func (s *Session) startTurn(plan *turnPlan) {
	st := &s.st
	plan.id = s.agent.cfg.ids.NewID("turn")
	plan.model, plan.switchTo, st.pendingSwitch = st.model, st.pendingSwitch, nil
	if r := st.nextReasoning; r != nil {
		plan.model.ref.Params.Reasoning, st.nextReasoning = *r, nil
		if plan.switchTo != nil {
			sw := *plan.switchTo
			sw.ref.Params.Reasoning = *r
			plan.switchTo = &sw
		}
	}
	if plan.humanInput {
		st.autoTurns = 0
	}
	st.ledger.open(plan.id)
	ctx, cancel := context.WithCancelCause(s.ctx)
	s.ctl.set(plan.id, cancel)
	st.turn = &turnState{id: plan.id, started: s.agent.cfg.clock.Now(), humanInput: plan.humanInput}
	s.setPhase(phaseRunning) // after st.turn, so the published snapshot already carries this turn id
	s.publishAt(&event.TurnStarted{TurnID: plan.id, Trigger: string(plan.trigger), InputIDs: plan.inputIDs()}, plan.id, 0, 0)
	s.wk.Go("turn "+plan.id, func() {
		out := s.runTurn(ctx, plan)
		cancel(nil)
		_ = s.mb.push(&turnExitMsg{turnID: plan.id, out: out}, false)
	})
}

func (s *Session) onTurnExit(m *turnExitMsg) {
	st := &s.st
	s.ctl.clear(m.turnID)
	if !st.turn.humanInput {
		st.autoTurns++ // only reminder items reached this turn
	}
	st.turn = nil
	st.usage.Turns++
	if st.phase == phaseRunning {
		s.setPhase(phaseIdle) // publishes the snapshot first, so a reader woken by turn.completed never sees a stale phase
	}
	s.publishAt(m.out.completed(m.turnID), m.turnID, 0, 0)
	if r := st.deniedStop; r != nil {
		st.deniedStop = nil
		s.ownerDenied(*r)
	}
	if m.out.note != nil {
		s.enqueue(m.out.note) // a reminder that never wakes, so the next turn learns what was cut off
	}
	res := m.out.result(m.turnID)
	st.lastTurn = res
	for _, r := range m.out.receipts {
		r.resolve(res) // Receipt.Wait returns through this out-of-band channel, never via the event stream
	}
}

// init renders the first epoch's system line off the actor, because instructions may come from caller code.
func (s *Session) init() {
	ctx, cancel := bounded(s.ctx, s.limits.InitTimeout)
	defer cancel()
	_ = s.mb.push(&initDoneMsg{err: s.initModel(ctx)}, false)
}

// initModel resolves the model before the first epoch, because a prefix-bound model decides how that epoch freezes its tools.
func (s *Session) initModel(ctx context.Context) error {
	defs, _ := s.freezeTools()
	ms, err := s.prepareModel(ctx, *s.agent.cfg.model, len(defs))
	if err != nil {
		return err
	}
	if err := s.exec(func(st *state) {
		st.model = ms
		st.setWindow()
		s.publishSnapshot()
	}); err != nil {
		return err
	}
	return s.openEpoch(ctx)
}

func (s *Session) openEpoch(ctx context.Context) error {
	defs, tools := s.freezeTools()
	parts, err := s.renderSystem(ctx, tools, 1)
	if err != nil {
		return err
	}
	line := &message.Message{
		Role:        message.RoleSystem,
		Origin:      message.Origin{Kind: message.OriginSystem},
		Content:     systemContent(parts),
		Boundary:    &message.Boundary{Reason: "new", Session: &message.SessionMeta{Agent: s.nick(), Labels: s.labelMap(), SpecVersion: specVersion()}},
		Tools:       defs,
		HostedTools: s.freezeHosted(defs),
	}
	if s.owner != nil {
		line.Boundary.Session.ParentSessionID = s.owner.owner.id // so Resume reopens it only under ChildOf
	}
	if err := s.exec(func(st *state) {
		st.epoch = 1
		st.system = s.systemBlocks(parts)
		s.appendLine(st, line, "")
		s.publishCommitted(st, line, nil)
		s.publish(systemRendered(1, parts))
	}); err != nil {
		return err
	}
	if err := s.epochHead(ctx, "new"); err != nil {
		return err
	}
	return s.saveLabels(ctx)
}

// epochHead fires session_start and, when its hooks give context, writes it as the epoch's head line right after the system line, the two in one batch (RULE-STORE-008).
func (s *Session) epochHead(ctx context.Context, source string) error {
	sink := &hookSink{}
	var model provider.ModelInfo
	_ = s.exec(func(st *state) { model = st.model.info })
	fire(withHookScope(ctx, &hookScope{sink: sink}), s, SessionStart, SessionStartInput{HookEnv: s.env("", 0), Source: source, Model: model}, nil)
	notes := sink.take()
	var unsynced []message.Message
	if err := s.exec(func(st *state) {
		if len(notes) > 0 {
			head, info := delivery(nil, nil, notes)
			head.Origin = message.Origin{Kind: message.OriginEpochHead}
			s.appendLine(st, head, "")
			s.publishCommitted(st, head, &lineEvents{delivery: &info})
		}
		unsynced = st.unsynced()
	}); err != nil {
		return err
	}
	return s.persist(ctx, unsynced)
}

func specVersion() string { return strings.TrimSpace(spec.MustText("VERSION")) }

func (s *Session) onInitDone(m *initDoneMsg) {
	if s.st.phase != phaseInitializing {
		return // Close arrived first
	}
	if m.err != nil {
		s.ready <- m.err
		s.onClose(&closeMsg{reason: "init_failed"})
		return
	}
	s.publish(s.started())
	s.warnNoReviewer()
	s.setPhase(phaseIdle)
	s.ready <- nil
	for _, qc := range s.st.runNowWait {
		s.runCommand(qc, false)
	}
	s.st.runNowWait = nil
}

func (s *Session) started() *event.SessionStarted {
	defs, _ := s.freezeTools()
	tools := make([]string, len(defs))
	for i, d := range defs {
		tools[i] = d.Name
	}
	var commands []string
	for _, c := range s.Commands() {
		commands = append(commands, c.Name)
	}
	texts := make([]string, len(s.st.system))
	for i, b := range s.st.system {
		texts[i] = b.Text
	}
	return &event.SessionStarted{
		SpecVersion: specVersion(),
		Source:      s.source,
		Agent:       s.nick(),
		Model:       event.ModelInfo{API: string(s.st.model.info.API), Endpoint: s.st.model.info.Endpoint, Model: s.st.model.info.Model},
		Tools:       tools,
		Commands:    commands,
		Extensions:  s.exts,
		SystemHash:  systemHash(texts),
	}
}

// onClose drops what is queued, interrupts the turn and hands the waiting to a closer, because the actor itself never waits.
func (s *Session) onClose(m *closeMsg) {
	st := &s.st
	if m.done != nil {
		st.closeWaiters = append(st.closeWaiters, m.done)
	}
	if st.phase == phaseClosing || st.phase == phaseClosed {
		return
	}
	st.closeReason = m.reason
	began := st.phase != phaseInitializing // session_end pairs with a session that started
	usage := st.usage.clone()
	s.setPhase(phaseClosing)
	s.dropQueued("session_closing", func(*queueItem) bool { return false })
	for _, qc := range st.runNowWait {
		s.dropCommand(qc)
	}
	st.runNowWait = nil
	s.ctl.interrupt("", "", ErrSessionClosing, false)
	for _, cancel := range st.commands {
		cancel()
	}
	var kids []*childSet
	for _, e := range st.tasks.order {
		if a := e.cur; !e.info.Status.Terminal() && !a.stopReq {
			a.stopReq, a.silent, a.reason = true, true, "the session closed" // their contexts end with the session's, after session_end
		}
		if e.kids != nil {
			kids = append(kids, e.kids)
		}
	}
	s.wk.Go("closer", func() {
		ctx, cancel := bounded(context.Background(), s.limits.CloseTimeout)
		defer cancel()
		s.closeSides(ctx)
		s.wk.WaitPrefix(ctx, "init", "turn ", "cmd ") // the driver repairs history (I1) and exits
		if began {
			s.sessionEnd(ctx, m.reason, usage)
		}
		_ = s.exec(func(*state) {})   // books what closed side chats forwarded, so its writer has started
		s.wk.WaitPrefix(ctx, "calls") // the last calls reach the store before the context they write with ends
		s.stop()                      // cancels s.ctx for everything else
		for _, c := range kids {
			c.closeAll(ctx) // a finished task's children stay open for a continuation until now
		}
		leaked := s.wk.WaitExcept(ctx, "closer")
		_ = s.mb.push(&closeReadyMsg{leaked: leaked}, false)
	})
}

// sessionEnd fires session_end within one budget for all its hooks, then waits for the async ones it started, so ingesting on close is not canceled at once.
func (s *Session) sessionEnd(ctx context.Context, reason string, usage UsageReport) {
	ectx, cancel := bounded(ctx, ms(defaults().Hooks.EndBudget))
	fire(withHookScope(ectx, &hookScope{sink: &hookSink{discard: true}}), s, SessionEnd, SessionEndInput{HookEnv: s.env("", 0), Reason: reason, Usage: usage}, nil)
	cancel()
	s.wk.WaitPrefix(ctx, "hook async session_end ")
}

func (s *Session) onCloseReady(m *closeReadyMsg) {
	s.publish(&event.SessionClosed{Reason: s.st.closeReason, LeakedWorkers: leakedWorkers(m.leaked)})
	s.bus.Close() // pumps drain queued events, then report done
	s.finish(leakError(m.leaked))
}

// finish ends the session on the actor: closed phase, the agent forgets the id, and every waiter is released.
func (s *Session) finish(closeErr error) {
	s.st.phase = phaseClosed
	s.publishSnapshot()
	s.agent.unregister(s) // only now may the id be opened again, so two writers never overlap
	for _, w := range s.st.closeWaiters {
		w <- closeErr // buffered, never blocks the actor
	}
	s.receipts.sweep(TurnResult{Outcome: OutcomeCanceled, Err: ErrSessionClosed})
}

// crash is the kernel's own bug path: report it, stop everything and close, instead of taking the host process down.
func (s *Session) crash(r any) {
	if s.agent.cfg.panicOnInternal {
		panic(r)
	}
	defer func() { _ = recover() }() // the state may be inconsistent; closing must still finish
	s.agent.cfg.logger.Error("internal error", "session", s.id, "panic", r, "stack", string(debug.Stack()))
	s.publish(&event.Error{Code: "internal", Message: fmt.Sprint(r), Fatal: true})
	s.ctl.interrupt("", "", ErrSessionClosing, false)
	s.stop()
	s.publish(&event.SessionClosed{Reason: "internal_error"})
	s.bus.Close()
	s.mb.close()
	select {
	case s.ready <- ErrSessionClosed:
	default:
	}
	s.finish(ErrSessionClosed)
}

// fatalStore closes the session after a second writer showed up, whatever the store policy.
func (s *Session) fatalStore(err error) {
	s.post(func(st *state) {
		if st.phase == phaseClosing || st.phase == phaseClosed {
			return
		}
		s.publish(&event.Error{Code: "store_failed", Message: err.Error(), Fatal: true})
		s.onClose(&closeMsg{reason: "store_failed"})
	})
}

func leakedWorkers(ws []workers.Info) []event.LeakedWorker {
	var out []event.LeakedWorker
	now := time.Now()
	for _, w := range ws {
		out = append(out, event.LeakedWorker{Name: w.Name, AgeMs: now.Sub(w.Started).Milliseconds()})
	}
	return out
}

func leakError(ws []workers.Info) error {
	if len(ws) == 0 {
		return nil
	}
	e := &LeakError{}
	now := time.Now()
	for _, w := range ws {
		e.Workers = append(e.Workers, WorkerInfo{Name: w.Name, Age: now.Sub(w.Started)})
	}
	return e
}

type turnControl struct {
	mu        sync.Mutex
	turnID    string
	cancel    context.CancelCauseFunc
	requested bool // turn.interrupt_requested is out for this turn
	announce  func(turnID, reason string)
}

func (c *turnControl) set(id string, cancel context.CancelCauseFunc) {
	c.mu.Lock()
	c.turnID, c.cancel, c.requested = id, cancel, false
	c.mu.Unlock()
}

func (c *turnControl) clear(id string) {
	c.mu.Lock()
	if c.turnID == id {
		c.turnID, c.cancel = "", nil
	}
	c.mu.Unlock()
}

func (c *turnControl) current() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.turnID, c.cancel != nil
}

// interrupt cancels the running turn (only turn want, when set); with announce it first publishes turn.interrupt_requested, once per turn, so the event precedes everything the cancel causes.
func (c *turnControl) interrupt(want, reason string, cause error, announce bool) bool {
	stop := c.stopper(want, reason, cause, announce)
	if stop == nil {
		return false
	}
	stop()
	return true
}

// stopper announces the interrupt and returns the cancel, so a caller that must first publish what caused it cancels afterwards.
func (c *turnControl) stopper(want, reason string, cause error, announce bool) func() {
	c.mu.Lock()
	id, cancel := c.turnID, c.cancel
	if cancel == nil || want != "" && want != id {
		c.mu.Unlock()
		return nil
	}
	if announce && !c.requested {
		c.requested = true
		c.announce(id, reason) // under the lock, so a racing interrupt cannot cancel before the event is out
	}
	c.mu.Unlock()
	return func() { cancel(cause) } // idempotent, so racing callers are harmless
}

func (s *Session) announceInterrupt(turnID, reason string) {
	s.publishAt(&event.TurnInterruptRequested{TurnID: turnID, Reason: reason}, turnID, 0, 0)
}

// contains reports whether an id list names id.
func contains(ids []string, id string) bool { return slices.Contains(ids, id) }
