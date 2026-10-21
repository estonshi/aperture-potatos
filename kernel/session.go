package kernel

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/estonshi/aperture-potatos/internal/bus"
	"github.com/estonshi/aperture-potatos/internal/hookrun"
	"github.com/estonshi/aperture-potatos/internal/workers"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
)

// Session is one conversation, run by its own actor goroutine so at most one turn runs at a time; its methods are safe for concurrent use.
type Session struct {
	id           string
	source       string // new | resume | fork, as session.started reports it
	agent        *Agent
	chain        []string                          // session ids from the root to this session; drives re-entrancy detection
	nickname     atomic.Pointer[string]            // the agent's name when the current epoch's system was rendered
	renderedName string                            // the name the last renderSystem used, which systemBlocks makes current; both run for the history-token holder, in that order
	labels       atomic.Pointer[map[string]string] // replaced whole by SetLabels, so a reader never sees half a merge
	labelsMu     sync.Mutex                        // one SetLabels at a time, so concurrent merges keep every key
	inputMode    InputMode
	sessInstr    *string
	limits       Limits
	exts         []string

	ctx       context.Context // canceled by Close; parent of every worker context
	stop      context.CancelFunc
	mb        *mailbox
	bus       *bus.Bus
	ctl       turnControl
	snap      atomic.Pointer[Snapshot]
	queued    atomic.Int64 // human, command and agent items from Send to delivery, so a full queue fails Send without asking the actor
	wk        *workers.Set
	done      chan struct{} // closed when the actor goroutine returns
	ready     chan error    // init outcome, read once by NewSession
	reg       *registry
	receipts  receiptSet
	jitter    func(time.Duration) time.Duration
	noticed   sync.Map // model notices already announced, so an adjustment repeated on every request is announced once
	rules     []compiledRule
	state     stateStore
	stored    storedLines   // the store's lines, cached on the first history search
	clearGen  atomic.Uint64 // counts /clear, so an async hook that answers a cleared context is dropped
	asyncPool *hookrun.Pool
	parent    *Session  // a side chat's main session, whose current context every one of its requests carries
	sideName  string    // the name a side chat is held by under parent
	owner     *childSet // ChildOf: the task that owns this session
	outerView viewCache
	effView   viewCache
	allowed   map[string]bool // ForkTools: the only tools the gate lets run; nil allows all
	sides     sideChats
	tasks     *Tasks
	frozen    atomic.Pointer[frozenSet] // the tools the model was last sent
	movable   sync.Map                  // call id -> *movableCall, the running calls BackgroundCall can hand to a task
	st        state                     // owned by the actor goroutine; never touched elsewhere
}

func (s *Session) stateStore() *stateStore { return &s.state }

// SessionInfo is what WithInstructionsFunc and extensions can see about a session.
type SessionInfo struct {
	SessionID       string
	ParentSessionID string
	Agent           string // nickname of the owning agent
	Labels          map[string]string
	Epoch           int
	SideChat        string // name under its main session for a side chat, so an extension can tell one apart; empty for any other session
}

// Info is the session's identity and labels; it never blocks, so a hook or tool may call it.
func (s *Session) Info() SessionInfo {
	return SessionInfo{SessionID: s.id, ParentSessionID: s.parentID(), Agent: s.nick(), Labels: maps.Clone(s.labelMap()), Epoch: s.snap.Load().Epoch, SideChat: s.sideName}
}

func (s *Session) nick() string { return *s.nickname.Load() }

func (s *Session) parentID() string {
	if p := s.Parent(); p != nil {
		return p.id
	}
	return ""
}

// Snapshot is the session's state as last published, for status displays and health checks.
type Snapshot struct {
	SessionID    string
	Phase        string
	TurnID       string
	Step         int
	Epoch        int
	Model        provider.ModelInfo
	QueuedInputs int
	RunningTasks int // unfinished tasks, foreground and background
	Usage        UsageReport
	Context      ContextUsage
	Workers      []WorkerInfo
	Version      uint64
}

// Snapshot is a lock-free read of the last published state.
func (s *Session) Snapshot() Snapshot {
	sn := *s.snap.Load()
	if ts := s.tasks.snap.Load(); ts != nil {
		for _, info := range ts.list {
			if !info.Status.Terminal() {
				sn.RunningTasks++
			}
		}
	}
	now := time.Now()
	for _, w := range s.wk.Live() {
		sn.Workers = append(sn.Workers, WorkerInfo{Name: w.Name, Age: now.Sub(w.Started)})
	}
	return sn
}

// TurnTrigger is the origin kind that started a turn; turns fed only by reminder inputs count toward MaxConsecutiveAutoTurns.
type TurnTrigger string

const (
	TurnHuman    TurnTrigger = "human"    // a person's input
	TurnCommand  TurnTrigger = "command"  // a prompt command's expanded input
	TurnAgent    TurnTrigger = "agent"    // another agent, such as a subagent's prompt or a peer
	TurnTask     TurnTrigger = "task"     // a task notification
	TurnReminder TurnTrigger = "reminder" // only reminders, which count as an automatic turn
)

// StorePolicy chooses between availability and the guarantee that every tool that ran has a persisted record.
type StorePolicy string

const (
	StoreWarn   StorePolicy = "warn"   // emit error{store_failed}, retry the unsynced lines on the next commit
	StoreStrict StorePolicy = "strict" // end the turn with an error, so an unpersisted call is never executed
)

// Outcome says how a turn ended, or why a receipt resolved without one.
type Outcome string

const (
	OutcomeCompleted   Outcome = "completed"    // the model gave its final answer
	OutcomeMaxSteps    Outcome = "max_steps"    // Limits.MaxSteps ran out
	OutcomeMaxTokens   Outcome = "max_tokens"   // a reply hit the output limit and was not continued
	OutcomeRefused     Outcome = "refused"      // the model refused or a content filter stopped it, and the reply was discarded
	OutcomeHookStopped Outcome = "hook_stopped" // a hook halted the turn, rejected the reply or stopped it after tools
	OutcomeInterrupted Outcome = "interrupted"  // Interrupt or a preempting input ended it
	OutcomeError       Outcome = "error"        // a model call, the store or the kernel failed; see Err
	OutcomeCanceled    Outcome = "canceled"     // Close ended it, or the session closed before the input was used
	OutcomeDropped     Outcome = "dropped"      // the input was dropped before any turn used it
	OutcomeCommand     Outcome = "command"      // the receipt belonged to a command
)

// TurnResult is what a receipt resolves with; Err says why a turn failed, and the turn may still have committed lines before it.
type TurnResult struct {
	TurnID     string              `json:"turn_id,omitempty"`
	Outcome    Outcome             `json:"outcome"`
	StopReason provider.StopReason `json:"stop_reason,omitempty"`
	Steps      int                 `json:"steps"`
	Usage      message.Usage       `json:"usage"`
	FinalText  string              `json:"final_text,omitempty"` // last assistant text of the turn
	Command    *CommandResult      `json:"-"`                    // set when the receipt belonged to a command
	Err        error               `json:"-"`
}

// MarshalJSON writes Err as its message, which is how hooks outside the process see it.
func (r TurnResult) MarshalJSON() ([]byte, error) {
	type plain TurnResult
	v := struct {
		plain
		Error string `json:"error,omitempty"`
	}{plain: plain(r)}
	if r.Err != nil {
		v.Error = r.Err.Error()
	}
	return json.Marshal(v)
}

// Receipt resolves through an out-of-band channel when the turn that consumed the input, or the command, has finished.
type Receipt struct {
	s    *Session
	id   string
	done chan struct{}
	once sync.Once
	res  TurnResult
}

func (s *Session) newReceipt(id string) *Receipt {
	r := &Receipt{s: s, id: id, done: make(chan struct{})}
	s.receipts.add(r)
	return r
}

// InputID is the id of the input or command, as Withdraw, InterruptWith and the input events use it.
func (r *Receipt) InputID() string { return r.id }

// Done is closed once the receipt resolves, for a select beside other channels; Wait then returns at once.
func (r *Receipt) Done() <-chan struct{} { return r.done }

// Wait fails only when waiting fails; how the turn went, errors included, is in the TurnResult.
func (r *Receipt) Wait(ctx context.Context) (TurnResult, error) {
	if reentrant(ctx, r.s.id) {
		return TurnResult{}, ErrReentrantWait
	}
	select {
	case <-r.done:
		return r.res, nil
	case <-ctx.Done():
		return TurnResult{}, context.Cause(ctx)
	}
}

func (r *Receipt) resolve(res TurnResult) {
	if r == nil {
		return
	}
	r.once.Do(func() {
		r.res = res
		r.s.receipts.remove(r)
		close(r.done)
	})
}

// receiptSet lets a crashed session resolve every receipt it still owes, wherever the item behind it was held.
type receiptSet struct {
	mu   sync.Mutex
	open map[*Receipt]struct{}
}

func (rs *receiptSet) add(r *Receipt) {
	rs.mu.Lock()
	if rs.open == nil {
		rs.open = map[*Receipt]struct{}{}
	}
	rs.open[r] = struct{}{}
	rs.mu.Unlock()
}

func (rs *receiptSet) remove(r *Receipt) {
	rs.mu.Lock()
	delete(rs.open, r)
	rs.mu.Unlock()
}

func (rs *receiptSet) sweep(res TurnResult) {
	rs.mu.Lock()
	open := slices.Collect(maps.Keys(rs.open))
	rs.mu.Unlock()
	for _, r := range open {
		r.resolve(res)
	}
}

// Send never blocks; on a full queue it merges the oldest reminders of one source first, so ErrQueueFull hits only human, command and agent inputs.
func (s *Session) Send(in Input, opts ...SendOption) (*Receipt, error) {
	var sc sendConfig
	for _, o := range opts {
		o(&sc)
	}
	origin := in.Origin
	if sc.origin != nil {
		origin = *sc.origin
	}
	if origin.Kind == "" {
		origin.Kind = message.OriginHuman
	}
	switch origin.Kind {
	case message.OriginHuman, message.OriginCommand, message.OriginAgent, message.OriginReminder:
	default:
		return nil, ErrInvalidOrigin
	}
	if origin.Kind == message.OriginHuman && !sc.noParse {
		if rest, ok := strings.CutPrefix(in.Text, "//"); ok {
			in.Text = "/" + rest
		} else if name, args, ok := parseCommand(in.Text, func(n string) bool { _, ok := s.lookupCommand(n); return ok }); ok {
			c, _ := s.lookupCommand(name)
			return s.invoke(c, args)
		}
	}
	if in.Text == "" && len(in.Parts) == 0 {
		return nil, ErrEmptyInput
	}
	mode := sc.mode
	if mode == "" {
		mode = s.inputMode
	}
	if _, ok := modeRank[mode]; !ok {
		return nil, ErrInvalidMode
	}
	id := s.agent.cfg.ids.NewID("in")
	it := &queueItem{id: id, origin: origin, mode: mode, wake: !sc.noWake}
	it.parts = []*itemPart{{id: id, origin: origin, text: in.Text, content: in.Parts, metadata: mergeMeta(maps.Clone(in.Metadata), sc.meta), receipt: s.newReceipt(id)}}
	if humanish(origin) {
		if !s.reserve() {
			it.parts[0].receipt.resolve(TurnResult{Outcome: OutcomeDropped})
			return nil, ErrQueueFull
		}
		it.reserved = true
	}
	if err := s.mb.push(&inputMsg{item: it}, true); err != nil {
		s.release(it)
		it.parts[0].receipt.resolve(TurnResult{Outcome: OutcomeDropped, Err: err})
		return nil, err
	}
	return it.parts[0].receipt, nil
}

// reserve takes a queue slot for an item that Send, not the actor, must refuse when the queue is full.
func (s *Session) reserve() bool {
	limit := s.limits.MaxQueuedInputs
	if limit < 0 {
		return true
	}
	if s.queued.Add(1) > int64(limit) {
		s.queued.Add(-1)
		return false
	}
	return true
}

// Interrupt stops the running turn; turn.interrupt_requested is published before the cancel, so it precedes every event the cancel causes.
func (s *Session) Interrupt(reason string) bool {
	return s.ctl.interrupt("", reason, &InterruptError{Reason: reason}, true)
}

// InterruptTurn is Interrupt for one turn only, so a caller that owns that turn never stops a later one by racing its end.
func (s *Session) InterruptTurn(turnID, reason string) bool {
	if turnID == "" {
		return false
	}
	return s.ctl.interrupt(turnID, reason, &InterruptError{Reason: reason}, true)
}

// InterruptAndDrop also drops queued human, command and agent inputs; reminders stay.
func (s *Session) InterruptAndDrop(reason string) bool {
	id, running := s.ctl.current()
	err := s.mb.push(&opMsg{fn: func(st *state) {
		if st.turn != nil && st.turn.id == id {
			st.turn.dropInputs = true
		}
		s.dropQueued("interrupted", func(it *queueItem) bool { return it.cmd != nil || !humanish(it.origin) })
		if running {
			s.ctl.interrupt(id, reason, &InterruptError{Reason: reason}, true)
		}
	}}, false)
	return err == nil && running
}

// Registrar returns the session-scope registrar, for adding or hiding tools, commands and other registrations at runtime.
func (s *Session) Registrar() Registrar {
	return &registrar{scope: ScopeSession, agent: s.agent, session: s, reg: s.reg}
}

// Commands lists the visible commands, session scope shadowing agent scope, sorted by name.
func (s *Session) Commands() []CommandInfo {
	byName := map[string]Command{}
	sess := s.reg.state()
	for n, c := range s.outer().commands {
		if !sess.hides(KindCommand, n) {
			byName[n] = c.cmd
		}
	}
	for n, c := range sess.commands {
		byName[n] = c.cmd
	}
	var out []CommandInfo
	for _, n := range slices.Sorted(maps.Keys(byName)) {
		if c := byName[n]; !c.Hidden {
			out = append(out, c.info())
		}
	}
	return out
}

func (s *Session) lookupCommand(name string) (Command, bool) {
	sess := s.reg.state()
	if c, ok := sess.command(name); ok {
		return c, true
	}
	c, ok := s.outer().command(name)
	if !ok || sess.hides(KindCommand, c.Name) {
		return Command{}, false
	}
	return c, true
}

// Close interrupts the running turn, waits for the workers within CloseTimeout and ends with session.closed.
func (s *Session) Close(ctx context.Context) error {
	done := make(chan error, 1)
	if err := s.mb.push(&closeMsg{done: done, reason: "closed"}, false); err != nil {
		return nil // already closed
	}
	if reentrant(ctx, s.id) {
		return ErrReentrantWait // closing continues asynchronously
	}
	select {
	case err := <-done:
		return err
	case <-s.done:
		return nil // the session closed itself, as after a failed init, before it read this request
	case <-ctx.Done():
		return context.Cause(ctx) // closing continues asynchronously
	}
}
