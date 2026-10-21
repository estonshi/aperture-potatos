package kernel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/internal/prompt"
	"github.com/estonshi/aperture-potatos/message"
)

// Tasks is a session's task manager; tools, hooks, other tasks and the host all reach the same one.
type Tasks struct {
	s    *Session
	snap atomic.Pointer[taskSnap]
}

// taskSnap is the manager's published view, replaced on every change, so Get and List never ask the actor.
type taskSnap struct {
	list  []TaskInfo
	index map[string]int
	inbox []chan TaskInstruction // the current attempt's inbox of each entry, read for PendingInstructions
}

func (sn *taskSnap) info(i int) TaskInfo {
	in := sn.list[i]
	if !in.Status.Terminal() {
		in.PendingInstructions = len(sn.inbox[i])
	}
	return in
}

// Tasks returns the manager the task tools use, so the host and the model see one task set.
func (s *Session) Tasks() *Tasks { return s.tasks }

// Get reads the last published state without waiting on the session; ok is false for an unknown id or one List no longer keeps.
func (t *Tasks) Get(id string) (TaskInfo, bool) {
	sn := t.snap.Load()
	if sn == nil {
		return TaskInfo{}, false
	}
	i, ok := sn.index[id]
	if !ok {
		return TaskInfo{}, false
	}
	return sn.info(i), true
}

// List is in creation order: unfinished tasks and the most recent finished ones.
func (t *Tasks) List() []TaskInfo {
	sn := t.snap.Load()
	if sn == nil {
		return nil
	}
	out := make([]TaskInfo, len(sn.list))
	for i := range sn.list {
		out[i] = sn.info(i)
	}
	return out
}

// taskTable is owned by the actor.
type taskTable struct {
	byID      map[string]*taskEntry
	order     []*taskEntry
	reserved  int                 // slots held by starts and migrations in flight
	notifying int                 // task_notify runs not back yet, which WaitQuiet waits for
	byCall    map[string][]string // tool call id -> tasks it started, continued, restarted or stopped, until its result line commits
	waits     map[string]string   // a task whose Run waits on another task of this session -> that task
}

func (tt *taskTable) live() int {
	n := 0
	for _, e := range tt.order {
		if !e.info.Status.Terminal() {
			n++
		}
	}
	return n
}

// startedIn reports an unfinished attempt started by a call of the turn, whose calls still count toward it.
func (tt *taskTable) startedIn(turnID string) bool {
	return slices.ContainsFunc(tt.order, func(e *taskEntry) bool { return !e.info.Status.Terminal() && e.info.TurnID == turnID })
}

func (tt *taskTable) reserve(limit int) bool {
	if limit >= 0 && tt.live()+tt.reserved >= limit {
		return false
	}
	tt.reserved++
	return true
}

// cycle reports whether waiter waiting on target would close a loop of tasks waiting on each other.
func (tt *taskTable) cycle(waiter, target string) bool {
	for cur, n := target, 0; n <= len(tt.waits); n++ {
		if cur == waiter {
			return true
		}
		next, ok := tt.waits[cur]
		if !ok {
			return false
		}
		cur = next
	}
	return true
}

type taskEntry struct {
	id        string
	kind      TaskKind // resolved when the current attempt was made; zero for a restored task whose kind is gone
	spec      TaskSpec // the current attempt's spec, without Continue
	info      TaskInfo
	cur       *taskAttempt
	migrating bool              // a restart or continuation holds the lock
	held      []TaskInstruction // sent while the lock is held
	approved  bool              // the current input passed the gate in this process
	waiters   []chan struct{}   // Wait callers, woken on every change to re-check
	fg        []*fgWaiter       // TaskTool calls waiting in the foreground
	noteVer   uint64            // bumped by every notification started
	sentVer   uint64            // the newest one queued, so an older hook that finishes late is dropped
	floorVer  uint64            // notifications up to it were withdrawn
	kids      *childSet         // the ChildOf sessions it owns, across attempts
}

func (e *taskEntry) view() TaskInfo {
	in := e.info
	if !in.Status.Terminal() {
		in.PendingInstructions = len(e.cur.inbox)
	}
	return in
}

// stopped is what Wait waits for: an end, or a question the attempt still wants answered.
func (e *taskEntry) stopped() bool {
	return e.info.Status.Terminal() || e.info.Status == TaskWaitingInput && !e.cur.stopReq
}

// fgWaiter is one TaskTool call waiting in the foreground; the actor sets res and closes ch together.
type fgWaiter struct {
	ch   chan struct{}
	res  *ToolResult
	call string // the start tool call waiting, which BackgroundCall names
}

type taskAttempt struct {
	n        int
	rt       *taskRuntime
	inst     Task
	ctx      context.Context
	cancel   context.CancelCauseFunc
	inbox    chan TaskInstruction
	cont     *TaskContinuation
	started  time.Time
	begun    bool // task.started is out and Run was called
	stopReq  bool // Stop, Close, the foreground call's cancel, or a restart asked it to end
	silent   bool // the model asked for that stop, or a restart replaced it, so no notification
	reason   string
	graced   bool // the abandon timer is set
	given    bool // abandoned after the grace period
	settled  bool // Run returned or was abandoned: the outcome is fixed
	finished bool
	status   TaskStatus
	result   *TaskResult
	errText  string
	unread   []TaskInstruction // left in the inbox when a restart replaced it
	holding  []TaskInstruction // taken from the inbox and not yet acted on, as the kind last reported
	denied   string            // a reviewer in a child session said deny_and_stop, so it fails with this text
}

var errTaskStopped = errors.New("task stopped")

// taskKind finds a kind: the session's registration shadows the agent's.
func (s *Session) taskKind(name string) (TaskKind, bool) {
	if name == ToolExecKind {
		return s.toolExecKind(), true
	}
	if r, ok := s.reg.state().taskKinds[name]; ok {
		return r.kind, true
	}
	if r, ok := s.outer().taskKinds[name]; ok {
		return r.kind, true
	}
	return TaskKind{}, false
}

// callOf is the tool call of this session that ctx carries, and its turn, so a task is recorded against the result line of that call.
func (s *Session) callOf(ctx context.Context) (callID, turnID string) {
	m, ok := ctx.Value(markerKey{}).(marker)
	if !ok || m.kind != "tool" || len(m.chain) == 0 || m.chain[len(m.chain)-1] != s.id {
		return "", ""
	}
	if sc := scopeOf(ctx); sc != nil {
		turnID = sc.turnID
	}
	if v, ok := s.movable.Load(m.call); ok && v.(*movableCall).mv.Moved() {
		return "", turnID // a moved call's line is already committed, so what it starts is the task's doing, not the model's
	}
	return m.call, turnID
}

func (s *Session) noteCall(st *state, callID, id string) {
	if callID != "" && !slices.Contains(st.tasks.byCall[callID], id) {
		st.tasks.byCall[callID] = append(st.tasks.byCall[callID], id)
	}
}

// ---- starting ----

// Start starts a task from the host through the task_start hook, and returns once it is registered rather than when it ends, even for a foreground spec.
func (t *Tasks) Start(ctx context.Context, spec TaskSpec) (TaskInfo, error) {
	if spec.Continue != nil {
		return TaskInfo{}, errors.New("TaskSpec.Continue is set only by a continuation")
	}
	info, _, err := t.start(ctx, spec, false)
	return info, err
}

// start is Start with the gate's verdict on the input; a foreground spec comes back with the waiter registered before the task can end.
func (t *Tasks) start(ctx context.Context, spec TaskSpec, approved bool) (TaskInfo, *fgWaiter, error) {
	return t.startWith(ctx, spec, approved, nil)
}

// startWith takes the first attempt's instance when it is given instead of calling the kind's constructor; such an instance never runs when the start fails.
func (t *Tasks) startWith(ctx context.Context, spec TaskSpec, approved bool, given Task) (TaskInfo, *fgWaiter, error) {
	s := t.s
	kind, ok := s.taskKind(spec.Kind)
	if !ok {
		return TaskInfo{}, nil, fmt.Errorf("unknown task kind %q", spec.Kind)
	}
	if spec.NotifyMode != "" {
		if _, ok := modeRank[spec.NotifyMode]; !ok {
			return TaskInfo{}, nil, ErrInvalidMode
		}
	}
	in, ok := compactJSON(spec.Input)
	if !ok {
		return TaskInfo{}, nil, errors.New("task input is not valid JSON")
	}
	spec.Input = in
	var err error
	if e := s.exec(func(st *state) {
		switch {
		case st.phase == phaseClosing || st.phase == phaseClosed:
			err = ErrSessionClosed
		case !st.tasks.reserve(s.limits.MaxRunningTasks):
			err = ErrTaskLimit
		}
	}); e != nil {
		return TaskInfo{}, nil, ErrSessionClosed
	}
	if err != nil {
		return TaskInfo{}, nil, err
	}
	release := func() { s.post(func(st *state) { st.tasks.reserved-- }) }
	callID, turnID := s.callOf(ctx)
	if spec, err = s.taskStart(ctx, spec, callID, turnID, 1, nil); err != nil {
		release()
		return TaskInfo{}, nil, err
	}
	inst := given
	if inst == nil {
		if inst, err = newTask(kind, spec); err != nil {
			release()
			return TaskInfo{}, nil, err
		}
	}
	var info TaskInfo
	var w *fgWaiter
	if e := s.exec(func(st *state) {
		st.tasks.reserved--
		if st.phase == phaseClosing || st.phase == phaseClosed {
			err = ErrSessionClosed
			return
		}
		info, w = s.registerTask(st, kind, spec, inst, callID, turnID, approved)
	}); e != nil {
		err = ErrSessionClosed
	}
	if err != nil {
		if given == nil {
			s.runDead(inst, "", 1, spec)
		}
		return TaskInfo{}, nil, err
	}
	return info, w, nil
}

// taskStart fires task_start on the caller's goroutine; it may refuse, or rewrite the spec but not its kind.
func (s *Session) taskStart(ctx context.Context, spec TaskSpec, callID, turnID string, attempt int, ins *TaskInstruction) (TaskSpec, error) {
	if len(s.hookEntries(TaskStart.meta)) == 0 {
		return spec, nil
	}
	f := fire(ctx, s, TaskStart, TaskStartInput{HookEnv: s.env(turnID, 0), Spec: spec, CallID: callID, Attempt: attempt, Continuation: ins != nil, Instruction: ins}, nil)
	reason := f.out.Reason
	switch {
	case f.out.Deny:
	case f.in.Spec.Kind != spec.Kind:
		reason = "task_start hook failed (it changed the kind)"
	case spec.Kind == ToolExecKind && !sameInput(f.in.Spec.Input, spec.Input):
		reason = "task_start hook failed (it changed the input of a tool_exec task, which the gate did not see)"
	default:
		out := f.in.Spec
		out.Continue = spec.Continue
		return out, nil
	}
	if reason == "" {
		reason = "refused by a task_start hook"
	}
	return spec, &TaskDeniedError{Text: prompt.Textf("task_not_started", map[string]string{"reason": reason})}
}

func sameInput(a, b json.RawMessage) bool {
	same, err := sameJSON(a, b)
	return err == nil && same
}

// newTask calls the kind's constructor, which is caller code, so a panic is an error like any other.
func newTask(kind TaskKind, spec TaskSpec) (inst Task, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("task kind %s: New panicked: %v", kind.Name, r)
		}
	}()
	inst, err = kind.New(spec)
	if err == nil && inst == nil {
		err = fmt.Errorf("task kind %s: New returned no task", kind.Name)
	}
	return inst, err
}

// runDead gives an instance that will never be an attempt its one Run, with a context that is already canceled and a runtime whose writes do nothing.
func (s *Session) runDead(inst Task, id string, n int, spec TaskSpec) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(ErrSessionClosed)
	_, _ = runTask(ctx, inst, &taskRuntime{s: s, id: id, kind: spec.Kind, spec: spec, n: n, ctx: ctx})
}

func (s *Session) newTaskID(st *state) string {
	for {
		if id := s.agent.cfg.ids.NewID("task"); st.tasks.byID[id] == nil {
			return id
		}
	}
}

func (s *Session) registerTask(st *state, kind TaskKind, spec TaskSpec, inst Task, callID, turnID string, approved bool) (TaskInfo, *fgWaiter) {
	id := s.newTaskID(st)
	e := &taskEntry{id: id, kind: kind, spec: spec, approved: approved}
	e.info = TaskInfo{ID: id, Kind: spec.Kind, Title: spec.Title}
	var w *fgWaiter
	if spec.Foreground {
		w = &fgWaiter{ch: make(chan struct{}), call: callID}
		e.fg = []*fgWaiter{w}
	}
	st.tasks.byID[id] = e
	st.tasks.order = append(st.tasks.order, e)
	s.noteCall(st, callID, id)
	e.cur = s.newAttempt(e, 1, inst, callID, turnID, nil)
	s.beginAttempt(st, e)
	return e.view(), w
}

func (s *Session) newAttempt(e *taskEntry, n int, inst Task, callID, turnID string, cont *TaskContinuation) *taskAttempt {
	ctx, cancel := context.WithCancelCause(s.ctx)
	a := &taskAttempt{n: n, inst: inst, ctx: ctx, cancel: cancel, inbox: make(chan TaskInstruction, defaults().Tasks.InboxSize), cont: cont}
	spec := e.spec
	spec.Continue = cont
	if e.kids == nil {
		e.kids = &childSet{owner: s}
	}
	a.rt = &taskRuntime{s: s, id: e.id, kind: spec.Kind, spec: spec, n: n, callID: callID, turnID: turnID, inbox: a.inbox, ctx: ctx, kids: e.kids}
	a.rt.live.Store(true)
	return a
}

// beginAttempt moves the current attempt to running: the snapshot, tasks.snapshot and task.started go out before Run is called.
func (s *Session) beginAttempt(st *state, e *taskEntry) {
	a := e.cur
	a.begun, a.started = true, s.agent.cfg.clock.Now()
	e.info.Status, e.info.Attempt, e.info.StartedAt, e.info.EndedAt = TaskRunning, a.n, a.started, time.Time{}
	e.info.CallID, e.info.TurnID, e.info.Foreground = a.rt.callID, a.rt.turnID, len(e.fg) > 0
	e.info.Progress, e.info.Question, e.info.Result, e.info.Error, e.info.UnprocessedInstructions, e.info.Abandoned = TaskProgress{}, "", nil, "", nil, false
	if a.cont == nil {
		e.info.State = nil // a restart starts from nothing; a continuation keeps the state it resumes from
	}
	specJSON, _ := json.Marshal(e.spec)
	detail := event.TaskDetail{Spec: specJSON}
	if a.cont != nil {
		detail.State = a.cont.State
	}
	s.taskChanged(st, e, &event.TaskStarted{TaskID: e.id, Kind: e.spec.Kind, Title: e.spec.Title, Foreground: e.info.Foreground, Attempt: a.n, CallID: a.rt.callID, Continuation: a.cont != nil, Detail: detail})
	if a.stopReq {
		s.graceTimer(e.id, a) // stopped while it waited for the attempt it replaced
	}
	e.kids.bind(a.rt)
	ctx := withHookScope(withMarker(a.ctx, marker{chain: s.chain, kind: "task", tasks: []string{e.id}}), &hookScope{turnID: a.rt.turnID})
	s.wk.Go("task "+e.id+" #"+strconv.Itoa(a.n), func() {
		res, err := runTask(ctx, a.inst, a.rt)
		fixed := false
		_ = s.exec(func(st *state) { fixed = s.settleAttempt(st, a, res, err) })
		if fixed {
			e.kids.release(s.limits.CloseTimeout)
		}
		s.post(func(st *state) { s.finishAttempt(st, e.id, a) })
	})
}

// runTask runs caller code, so a panic becomes a failure with its stack.
func runTask(ctx context.Context, inst Task, rt *taskRuntime) (res TaskResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v\n%s", r, debug.Stack())
		}
	}()
	return inst.Run(ctx, rt)
}

// settleAttempt fixes the outcome as Run hands it over; the kernel's own record decides, so an attempt asked to stop is canceled whatever Run returned; false means it was abandoned already.
func (s *Session) settleAttempt(st *state, a *taskAttempt, res TaskResult, err error) bool {
	if a.settled {
		return false // abandoned: the late return is dropped
	}
	a.settled = true
	a.rt.live.Store(false)
	switch {
	case a.denied != "": // set only while no stop was asked, so the reviewer came first
		a.status, a.errText = TaskFailed, a.denied
	case a.stopReq:
		a.status, a.errText = TaskCanceled, a.reason
	case err != nil:
		a.status, a.errText = TaskFailed, err.Error()
	default:
		a.status = TaskSucceeded
	}
	if res.Summary == "" {
		res.Summary = message.Message{Content: res.Content}.Text()
	}
	res.Data, _ = compactJSON(res.Data) // invalid data is dropped, because events and hooks carry it as JSON
	// what a reviewer stopped counts for nothing, so the model reads the reviewer's reason
	if (res.Summary != "" || len(res.Content) > 0 || len(res.Data) > 0) && a.denied == "" {
		a.result = &res
	}
	a.cancel(nil)
	return true
}

// finishAttempt publishes the end: a replaced attempt ends silently and lets its successor start, the current one reaches its waiters and, unless they saw it, the model.
func (s *Session) finishAttempt(st *state, id string, a *taskAttempt) {
	e := st.tasks.byID[id]
	if e == nil || a.finished || !a.settled {
		return
	}
	a.finished = true
	unread := append(append(slices.Clone(a.holding), a.unread...), drain(a.inbox)...)
	fin := &event.TaskFinished{TaskID: id, Status: string(a.status), Attempt: a.n, IsError: a.status != TaskSucceeded, DurationMs: s.agent.cfg.clock.Now().Sub(a.started).Milliseconds()}
	if a.result != nil {
		fin.Summary = a.result.Summary
	}
	for _, in := range unread {
		fin.UnprocessedInstructions = append(fin.UnprocessedInstructions, eventInstruction(in))
	}
	if e.cur != a {
		s.publish(fin)
		if !e.cur.begun {
			s.beginAttempt(st, e)
		}
		return
	}
	e.info.Status, e.info.EndedAt, e.info.Result, e.info.Error, e.info.Question, e.info.Abandoned = a.status, s.agent.cfg.clock.Now(), a.result, a.errText, "", a.given
	e.info.UnprocessedInstructions, e.info.Foreground = unread, false
	fg := e.fg
	e.fg = nil
	for _, w := range fg {
		r := finishedResult(e.view())
		w.res = &r
	}
	s.taskChanged(st, e, fin)
	for _, w := range fg {
		close(w.ch)
	}
	if len(fg) == 0 && (a.status != TaskCanceled || !a.silent) {
		s.notifyTask(st, e, true)
	}
	s.evictFinished(st)
}

// abandon ends an attempt that did not return within the grace period after a stop; its goroutine stays counted as leaked.
func (s *Session) graceTimer(id string, a *taskAttempt) {
	grace := s.limits.ToolCancelGrace
	if a.graced || !a.begun || grace < 0 {
		return
	}
	a.graced = true
	time.AfterFunc(grace, func() {
		s.post(func(st *state) {
			if a.settled {
				return
			}
			a.settled, a.given, a.status, a.errText = true, true, TaskCanceled, a.reason
			a.rt.live.Store(false)
			a.rt.kids.running.Store(false)
			s.publish(&event.Error{Code: "worker_leaked", Message: "task " + id + " did not stop within " + grace.String() + " and was abandoned"})
			s.finishAttempt(st, id, a)
		})
	})
}

func drain(ch chan TaskInstruction) []TaskInstruction {
	var out []TaskInstruction
	for {
		select {
		case in := <-ch:
			out = append(out, in)
		default:
			return out
		}
	}
}

func eventInstruction(in TaskInstruction) event.Instruction {
	return event.Instruction{ID: in.ID, Text: in.Text, Data: in.Data, Mode: string(in.Mode), Answer: in.Answer}
}

// taskChanged publishes a lifecycle change in the O3 order: the snapshot, tasks.snapshot, the change's own events, then the waiters.
func (s *Session) taskChanged(st *state, e *taskEntry, evs ...event.Payload) {
	s.publishTasks(st)
	s.publish(tasksSnapshot(st))
	for _, p := range evs {
		s.publish(p)
	}
	for _, ch := range e.waiters {
		close(ch)
	}
	e.waiters = nil
}

func (s *Session) publishTasks(st *state) {
	sn := &taskSnap{index: make(map[string]int, len(st.tasks.order))}
	for i, e := range st.tasks.order {
		sn.list = append(sn.list, e.info)
		sn.inbox = append(sn.inbox, e.cur.inbox)
		sn.index[e.id] = i
	}
	s.tasks.snap.Store(sn)
}

func tasksSnapshot(st *state) *event.TasksSnapshot {
	ev := &event.TasksSnapshot{}
	for _, e := range st.tasks.order {
		ev.Tasks = append(ev.Tasks, event.TaskSummary{TaskID: e.id, Kind: e.info.Kind, Title: e.info.Title, Status: string(e.info.Status), Attempt: e.info.Attempt, StartedAt: e.info.StartedAt})
	}
	return ev
}

// evictFinished keeps the most recent finished tasks; an evicted task is gone for Get, Send and Restart.
func (s *Session) evictFinished(st *state) {
	keep := defaults().Tasks.KeepFinished
	n := 0
	for _, e := range st.tasks.order {
		if e.info.Status.Terminal() && !e.migrating {
			n++
		}
	}
	if n <= keep {
		return
	}
	var out []*taskEntry
	for _, e := range st.tasks.order {
		if n > keep && e.info.Status.Terminal() && !e.migrating {
			delete(st.tasks.byID, e.id)
			s.closeChildren(e)
			n--
			continue
		}
		out = append(out, e)
	}
	st.tasks.order = out
	s.publishTasks(st)
	s.publish(tasksSnapshot(st))
}

// ---- stopping ----

// modelCall is the call id when ctx is a tool call of this session's own loop, the one case where a stop or restart needs no notification.
func (s *Session) modelCall(ctx context.Context) (string, bool) {
	id, _ := s.callOf(ctx)
	return id, id != ""
}

// Stop asks the task to stop and returns without waiting for it; stopping an ended task does nothing.
func (t *Tasks) Stop(ctx context.Context, id, reason string) error {
	s := t.s
	callID, model := s.modelCall(ctx)
	if reason == "" {
		reason = "stopped"
	}
	var err error
	if e := s.exec(func(st *state) {
		e := st.tasks.byID[id]
		if e == nil {
			err = ErrTaskNotFound
			return
		}
		s.noteCall(st, callID, id)
		if !e.info.Status.Terminal() {
			s.stopAttempt(id, e.cur, reason, model)
		}
	}); e != nil {
		return ErrSessionClosed
	}
	return err
}

func (s *Session) stopAttempt(id string, a *taskAttempt, reason string, silent bool) {
	if a.settled || a.stopReq {
		return
	}
	a.stopReq, a.silent, a.reason = true, silent, reason
	a.cancel(errTaskStopped)
	s.graceTimer(id, a)
}

// stopAll stops every unfinished task as a session policy does, so each one notifies.
func (s *Session) stopAll(st *state, reason string) {
	for _, e := range st.tasks.order {
		if !e.info.Status.Terminal() {
			s.stopAttempt(e.id, e.cur, reason, false)
		}
	}
}

// ---- instructions and continuation ----

// Send gives a running task an instruction, answers one waiting for input, or continues an ended one whose kind allows it.
func (t *Tasks) Send(ctx context.Context, id string, in TaskInstruction) error {
	s := t.s
	if in.ID == "" {
		in.ID = s.agent.cfg.ids.NewID("ins")
	}
	if in.Mode == "" {
		in.Mode = ModeNextLoop
	}
	if _, ok := modeRank[in.Mode]; !ok {
		return ErrInvalidMode
	}
	data, ok := compactJSON(in.Data)
	if !ok {
		return errors.New("instruction data is not valid JSON")
	}
	in.Data, in.Answer = data, false
	callID, turnID := s.callOf(ctx)
	var err error
	var cont bool
	var kind TaskKind
	var spec TaskSpec
	var saved json.RawMessage
	var n int
	if e := s.exec(func(st *state) {
		e := st.tasks.byID[id]
		switch {
		case e == nil:
			err = ErrTaskNotFound
		case !e.info.Status.Terminal():
			if !e.kind.Caps.Instructions {
				err = ErrTaskNoInstructions
				break
			}
			var answered bool
			answered, err = s.acceptInstruction(e, in)
			s.publish(instructionEvent(id, in, err))
			s.instructed(st, e, answered, err)
			return
		case !e.kind.Caps.Continue:
			err = ErrTaskNotContinuable
		case e.migrating:
			err = ErrTaskBusy
		case st.phase == phaseClosing || st.phase == phaseClosed:
			err = ErrSessionClosed
		case !st.tasks.reserve(s.limits.MaxRunningTasks):
			err = ErrTaskLimit
		default:
			e.migrating, cont = true, true
			kind, spec, saved, n = e.kind, e.spec, e.info.State, e.info.Attempt
		}
		if !cont {
			s.publish(instructionEvent(id, in, err))
		}
	}); e != nil {
		return ErrSessionClosed
	}
	if !cont {
		return err
	}
	err = s.continueTask(ctx, id, kind, spec, saved, n, in, callID, turnID)
	if err != nil {
		_ = s.exec(func(*state) { s.publish(instructionEvent(id, in, err)) })
	}
	return err
}

func instructionEvent(id string, in TaskInstruction, err error) *event.TaskInstruction {
	ev := &event.TaskInstruction{TaskID: id, InstructionID: in.ID, Mode: string(in.Mode), Text: in.Text, Accepted: err == nil}
	if err != nil {
		ev.Reason = err.Error()
	}
	return ev
}

// acceptInstruction puts an instruction in the inbox of a running attempt; the one accepted while the attempt waits for input is its answer, and answered reports that.
func (s *Session) acceptInstruction(e *taskEntry, in TaskInstruction) (answered bool, err error) {
	if e.migrating {
		if len(e.held)+len(e.cur.inbox) >= defaults().Tasks.InboxSize {
			return false, ErrQueueFull
		}
		e.held = append(e.held, in) // a restart may replace the attempt, so the answer waits to see which one it reaches
		return false, nil
	}
	answered = e.info.Status == TaskWaitingInput && !e.cur.stopReq
	in.Answer = answered
	select {
	case e.cur.inbox <- in:
	default:
		return false, ErrQueueFull
	}
	if answered {
		e.info.Status, e.info.Question = TaskRunning, ""
	}
	return answered, nil
}

// instructed publishes what accepting an instruction changed, after the task.instruction that caused it.
func (s *Session) instructed(st *state, e *taskEntry, answered bool, err error) {
	switch {
	case answered:
		s.taskChanged(st, e, &event.TaskStatusChanged{TaskID: e.id, From: string(TaskWaitingInput), To: string(TaskRunning)})
	case err == nil:
		s.publishTasks(st)
	}
}

func (s *Session) continueTask(ctx context.Context, id string, kind TaskKind, spec TaskSpec, saved json.RawMessage, n int, in TaskInstruction, callID, turnID string) error {
	fail := func(err error) error {
		s.post(func(st *state) {
			if e := st.tasks.byID[id]; e != nil {
				s.unlock(st, e, true)
			}
		})
		return err
	}
	cont := &TaskContinuation{State: saved, Instruction: in}
	spec.Continue = cont
	spec, err := s.taskStart(ctx, spec, callID, turnID, n+1, &in)
	if err != nil {
		return fail(err)
	}
	spec.Continue = cont
	inst, err := newTask(kind, spec)
	if err != nil {
		return fail(err)
	}
	return s.migrate(id, kind, spec, inst, callID, turnID, cont, false, true, instructionEvent(id, in, nil))
}

// migrate switches the task to its new attempt, or gives the new instance its one canceled Run when the session or the kind has gone meanwhile.
func (s *Session) migrate(id string, kind TaskKind, spec TaskSpec, inst Task, callID, turnID string, cont *TaskContinuation, approved, slot bool, pre ...event.Payload) error {
	var err error
	if e := s.exec(func(st *state) {
		e := st.tasks.byID[id]
		cur, ok := s.taskKind(kind.Name)
		switch {
		case e == nil:
			err = ErrTaskNotFound
			return
		case st.phase == phaseClosing || st.phase == phaseClosed:
			err = ErrSessionClosed
		case !ok || cont != nil && !cur.Caps.Continue || cont == nil && !cur.Caps.Restart:
			err = fmt.Errorf("task kind %q no longer allows this", kind.Name)
		}
		if err != nil {
			s.unlock(st, e, slot)
			return
		}
		for _, p := range pre {
			s.publish(p)
		}
		s.switchAttempt(st, e, cur, spec, inst, callID, turnID, cont, approved, slot)
	}); e != nil {
		err = ErrSessionClosed
	}
	if err != nil {
		s.runDead(inst, id, 0, spec)
	}
	return err
}

// unlock releases a migration that failed; what was sent meanwhile reaches the attempt that stays.
func (s *Session) unlock(st *state, e *taskEntry, slot bool) {
	e.migrating = false
	if slot {
		st.tasks.reserved--
	}
	held := e.held
	e.held = nil
	for _, in := range held {
		if e.info.Status.Terminal() {
			e.cur.unread = append(e.cur.unread, in)
			continue
		}
		answered, err := s.acceptInstruction(e, in)
		if err != nil {
			e.cur.unread = append(e.cur.unread, in)
		}
		s.instructed(st, e, answered, err)
	}
}

// switchAttempt makes the new attempt current; a replaced attempt that is still running is canceled without a notice and the new one starts once it has exited.
func (s *Session) switchAttempt(st *state, e *taskEntry, kind TaskKind, spec TaskSpec, inst Task, callID, turnID string, cont *TaskContinuation, approved, slot bool) {
	e.migrating = false
	if slot {
		st.tasks.reserved--
	}
	old := e.cur
	spec.Continue = nil
	e.kind, e.spec = kind, spec
	if cont == nil {
		e.approved = approved
		e.floorVer = e.noteVer // what the replaced attempt said no longer applies
		s.dropNotes(e.id, "withdrawn")
		s.closeChildren(e) // a restart starts from nothing
	}
	a := s.newAttempt(e, old.n+1, inst, callID, turnID, cont)
	for _, in := range e.held {
		select {
		case a.inbox <- in:
		default:
			a.unread = append(a.unread, in)
		}
	}
	e.held = nil
	e.cur = a
	s.noteCall(st, callID, e.id)
	switch {
	case !old.begun:
		old.settled, old.finished = true, true // replaced before it started, so it never counts as an attempt; its instance still gets its one Run
		s.wk.Go("task "+e.id+" #"+strconv.Itoa(old.n)+" replaced", func() { s.runDead(old.inst, e.id, old.n, old.rt.spec) })
	case !e.info.Status.Terminal():
		old.unread = append(old.unread, drain(old.inbox)...)
		s.stopAttempt(e.id, old, "restarted", true)
	}
	if old.finished && old.begun {
		s.beginAttempt(st, e)
		return
	}
	e.info.Status, e.info.Attempt, e.info.Foreground, e.info.Question = TaskPending, a.n, len(e.fg) > 0, ""
	s.taskChanged(st, e)
}

// ---- restart ----

// Restart replaces the current attempt with a new one, with input replacing the spec's input when non-nil, and sends it to approval when new input is given or the task was never approved.
func (t *Tasks) Restart(ctx context.Context, id string, input json.RawMessage) error {
	s := t.s
	callID, turnID := s.callOf(ctx)
	var err error
	var kind TaskKind
	var spec TaskSpec
	var approved, slot bool
	var n int
	if e := s.exec(func(st *state) {
		e := st.tasks.byID[id]
		switch {
		case e == nil:
			err = ErrTaskNotFound
		case !e.kind.Caps.Restart:
			err = ErrTaskNotRestartable
		case e.migrating:
			err = ErrTaskBusy
		case st.phase == phaseClosing || st.phase == phaseClosed:
			err = ErrSessionClosed
		case e.info.Status.Terminal() && !st.tasks.reserve(s.limits.MaxRunningTasks):
			err = ErrTaskLimit
		default:
			slot = e.info.Status.Terminal()
			e.migrating = true
			kind, spec, approved, n = e.kind, e.spec, e.approved, e.info.Attempt
		}
	}); e != nil {
		return ErrSessionClosed
	}
	if err != nil {
		return err
	}
	fail := func(err error) error {
		s.post(func(st *state) {
			if e := st.tasks.byID[id]; e != nil {
				s.unlock(st, e, slot)
			}
		})
		return err
	}
	if input != nil {
		if spec.Input, err = restartInput(spec, input); err != nil {
			return fail(err)
		}
	}
	if input != nil || !approved {
		in, err := s.gateRestart(ctx, spec.Kind, spec.Input, callID, turnID)
		if err != nil {
			return fail(err)
		}
		spec.Input = in
	}
	spec, err = s.taskStart(ctx, spec, callID, turnID, n+1, nil)
	if err != nil {
		return fail(err)
	}
	inst, err := newTask(kind, spec)
	if err != nil {
		return fail(err)
	}
	return s.migrate(id, kind, spec, inst, callID, turnID, nil, true, slot)
}

// gateRestart reviews a restart's input as a background call of the kind's start tool, so a restart never runs an input nobody approved.
func (s *Session) gateRestart(ctx context.Context, kind string, input json.RawMessage, callID, turnID string) (json.RawMessage, error) {
	name, view := s.restartView(kind, input)
	view, err := withKeyLast(view, "background", json.RawMessage("true"))
	if err == nil {
		view, err = normalizeInput(view)
	}
	if err != nil {
		return nil, &TaskDeniedError{Text: prompt.Textf("invalid_input_detail", map[string]string{"detail": "the input is not a JSON object"})}
	}
	v := decide(s.rules, ToolCallView{CallID: callID, Name: name, Input: view})
	if v.deny != nil {
		return nil, &TaskDeniedError{Text: prompt.Textf("denied_by_rule", map[string]string{"id": v.deny.ID, "reason": v.deny.Reason})}
	}
	if v.action == RuleAsk {
		rs := reviewScope{env: s.env(turnID, 0), publish: s.publish, hooks: func(id string) *hookScope { return &hookScope{turnID: turnID, callID: id} }}
		outs, _ := s.review(ctx, rs, []askCall{{call: message.ToolCallBlock{ID: callID, Name: name, Input: view}, matched: v.matched}})
		if !outs[0].approved {
			return nil, &TaskDeniedError{Text: message.Message{Content: outs[0].result.Content}.Text()}
		}
		view = outs[0].input
	}
	_, rest, err := takeBackground(view)
	if err == nil && kind == ToolExecKind {
		rest = toolExecSpec(name, rest)
	}
	return rest, err
}

// withKeyLast sets key as the object's last member, replacing an earlier one.
func withKeyLast(obj json.RawMessage, key string, value json.RawMessage) (json.RawMessage, error) {
	keys, values, err := objectEntries(obj)
	if err != nil {
		return nil, err
	}
	var ks []string
	var vs []json.RawMessage
	for i, k := range keys {
		if k != key {
			ks, vs = append(ks, k), append(vs, values[i])
		}
	}
	return writeObject(append(ks, key), append(vs, value)), nil
}

// ---- inspecting ----

// Inspect asks a running attempt whose kind can inspect for its live state, on the caller's goroutine, and keeps the answer as its latest progress; otherwise it is Get.
func (t *Tasks) Inspect(ctx context.Context, id string) (TaskInfo, error) {
	s := t.s
	var info TaskInfo
	var a *taskAttempt
	var insp TaskInspector
	var err error
	if e := s.exec(func(st *state) {
		e := st.tasks.byID[id]
		if e == nil {
			err = ErrTaskNotFound
			return
		}
		info = e.view()
		if e.kind.Caps.Inspect && !e.info.Status.Terminal() && e.cur.begun && !e.cur.settled {
			insp, _ = e.cur.inst.(TaskInspector)
			a = e.cur
		}
	}); e != nil {
		return TaskInfo{}, ErrSessionClosed
	}
	if err != nil || insp == nil {
		return info, err
	}
	p, err := inspectTask(ctx, insp)
	if err != nil {
		return info, err
	}
	p.Data, _ = compactJSON(p.Data)
	if err := s.bus.WaitWritable(ctx); err != nil { // task.progress is bulk, so the credit is waited for here and not on the actor
		return info, err
	}
	_ = s.exec(func(st *state) {
		e := st.tasks.byID[id]
		if e == nil {
			return
		}
		if e.cur == a && !a.settled {
			e.info.Progress = p
			s.publishTasks(st)
			s.publish(&event.TaskProgress{TaskID: id, Text: p.Text, Percent: p.Percent, Data: p.Data})
		}
		info = e.view()
	})
	return info, nil
}

// inspectTask runs caller code, so a panic becomes an error with its stack.
func inspectTask(ctx context.Context, insp TaskInspector) (p TaskProgress, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("task Inspect panicked: %v\n%s", r, debug.Stack())
		}
	}()
	return insp.Inspect(ctx)
}

// ---- waiting ----

// Wait returns once the task ends or asks for input, following it across attempts.
func (t *Tasks) Wait(ctx context.Context, id string) (TaskInfo, error) {
	s := t.s
	if reentrantTask(ctx, s.chain, id) {
		return TaskInfo{}, ErrReentrantWait
	}
	waiter := ""
	if m, ok := ctx.Value(markerKey{}).(marker); ok && m.kind == "task" && len(m.tasks) > 0 && m.chain[len(m.chain)-1] == s.id {
		waiter = m.tasks[len(m.tasks)-1]
	}
	defer func() {
		if waiter != "" {
			s.post(func(st *state) {
				if st.tasks.waits[waiter] == id {
					delete(st.tasks.waits, waiter)
				}
			})
		}
	}()
	for {
		var info TaskInfo
		var ch chan struct{}
		var err error
		if e := s.exec(func(st *state) {
			e := st.tasks.byID[id]
			switch {
			case e == nil:
				err = ErrTaskNotFound
				return
			case e.hosts(ctx), waiter != "" && st.tasks.cycle(waiter, id):
				err = ErrReentrantWait
				return
			}
			info = e.view()
			if !e.stopped() {
				if waiter != "" {
					st.tasks.waits[waiter] = id
				}
				ch = make(chan struct{})
				e.waiters = append(e.waiters, ch)
			}
		}); e != nil {
			return TaskInfo{}, ErrSessionClosed
		}
		if err != nil || ch == nil {
			return info, err
		}
		select {
		case <-ch:
		case <-ctx.Done():
			info, _ := t.Get(id)
			return info, context.Cause(ctx)
		case <-s.done:
			return info, ErrSessionClosed
		}
	}
}

// reentrantTask reports whether waiting on a task whose session has ownerChain could end up waiting on the caller, through ancestry or backpressure.
func reentrantTask(ctx context.Context, ownerChain []string, task string) bool {
	m, ok := ctx.Value(markerKey{}).(marker)
	return ok && (slices.Contains(m.tasks, task) || m.block && len(m.chain) > 0 && slices.Contains(ownerChain, m.chain[len(m.chain)-1]))
}

// foreground waits in the TaskTool call: the result, the question, or the call's own end, which moves the task to the background on a timeout and stops it otherwise.
func (t *Tasks) foreground(ctx context.Context, info TaskInfo, w *fgWaiter) (ToolResult, error) {
	s := t.s
	select {
	case <-w.ch:
		return *w.res, nil
	case <-ctx.Done():
	}
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
	var res *ToolResult
	var settled bool
	if e := s.exec(func(st *state) {
		if w.res != nil {
			res = w.res
			return
		}
		e := st.tasks.byID[info.ID]
		if e == nil {
			return
		}
		if !timedOut && e.cur.settled {
			settled = true // the outcome is fixed, and its finish hands the real result over
			return
		}
		e.fg = slices.DeleteFunc(e.fg, func(x *fgWaiter) bool { return x == w })
		if timedOut {
			e.info.Foreground = len(e.fg) > 0
			s.publishTasks(st)
			r := startedResult(e.view(), true)
			res = &r
			return
		}
		s.stopAttempt(e.id, e.cur, "the call that started it was interrupted", true)
	}); e != nil {
		return ToolResult{}, ctx.Err()
	}
	if settled {
		<-w.ch
		return *w.res, nil
	}
	if res != nil {
		return *res, nil
	}
	return ToolResult{}, ctx.Err()
}

// detach moves a start tool's call off its task as the call's timeout does: the call returns task_started with moved and the task notifies when it ends.
func (t *Tasks) detach(callID string) (TaskInfo, error) {
	s := t.s
	var info TaskInfo
	found := false
	if err := s.exec(func(st *state) {
		for _, e := range st.tasks.order {
			i := slices.IndexFunc(e.fg, func(w *fgWaiter) bool { return w.call == callID })
			if i < 0 || e.cur.settled {
				continue
			}
			w := e.fg[i]
			e.fg = slices.Delete(e.fg, i, i+1)
			e.info.Foreground = len(e.fg) > 0
			s.publishTasks(st)
			info, found = e.view(), true
			r := startedResult(info, true)
			w.res = &r
			close(w.ch)
			return
		}
	}); err != nil {
		return TaskInfo{}, ErrSessionClosed
	}
	if !found {
		return TaskInfo{}, ErrNotMovable
	}
	return info, nil
}

// WaitQuiet returns only when nothing in the session can move on its own, so a task that owns a child session never reports while work it caused is still running.
func (s *Session) WaitQuiet(ctx context.Context) (TurnResult, error) {
	if reentrant(ctx, s.id) {
		return TurnResult{}, ErrReentrantWait
	}
	ch := make(chan TurnResult, 1)
	if err := s.exec(func(st *state) { st.quiet = append(st.quiet, ch) }); err != nil {
		return TurnResult{}, ErrSessionClosed
	}
	select {
	case r := <-ch:
		return r, nil
	case <-ctx.Done():
		s.post(func(st *state) {
			st.quiet = slices.DeleteFunc(st.quiet, func(c chan TurnResult) bool { return c == ch })
		})
		return TurnResult{}, context.Cause(ctx)
	case <-s.done:
		return TurnResult{}, ErrSessionClosed
	}
}

// checkQuiet runs after the actor has looked for work, so an idle phase already means no queued item can start a turn.
func (s *Session) checkQuiet() {
	st := &s.st
	if len(st.quiet) == 0 || st.phase != phaseIdle || len(st.commands) > 0 || st.tasks.live() > 0 || st.tasks.reserved > 0 || st.tasks.notifying > 0 {
		return
	}
	for _, ch := range st.quiet {
		ch <- st.lastTurn
	}
	st.quiet = nil
}

// ---- notifications ----

// taskNote is what a queued notification carries: which task it reports on and in what state.
type taskNote struct {
	task    string
	kind    string
	status  TaskStatus
	version uint64
}

// notifyTask renders the notification and hands it to task_notify, off the actor when hooks are registered, before it is queued.
func (s *Session) notifyTask(st *state, e *taskEntry, wake bool) {
	if st.phase == phaseClosing || st.phase == phaseClosed {
		return
	}
	e.noteVer++
	note := taskNote{task: e.id, kind: e.info.Kind, status: e.info.Status, version: e.noteVer}
	info := e.view()
	text := s.noteBody(info)
	mode := e.spec.NotifyMode
	if mode == "" {
		mode = ModeNextLoop
	}
	if len(s.hookEntries(TaskNotify.meta)) == 0 {
		s.queueNote(st, note, text, mode, wake)
		return
	}
	st.tasks.notifying++
	s.wk.Go("task notify "+e.id, func() {
		f := fire(s.ctx, s, TaskNotify, TaskNotifyInput{HookEnv: s.env("", 0), Info: info, Text: text}, nil)
		s.post(func(st *state) {
			st.tasks.notifying--
			s.queueNote(st, note, f.in.Text, mode, wake && !f.out.NoWake)
		})
	})
}

// queueNote keeps only the newest undelivered notification of a task, by the order of the changes rather than of their hooks.
func (s *Session) queueNote(st *state, note taskNote, text string, mode InputMode, wake bool) {
	if st.phase == phaseClosing || st.phase == phaseClosed {
		return
	}
	if e := st.tasks.byID[note.task]; e != nil {
		if note.version <= e.floorVer || note.version <= e.sentVer {
			return
		}
		e.sentVer = note.version
	}
	s.dropNotes(note.task, "superseded")
	id := s.agent.cfg.ids.NewID("in")
	origin := message.Origin{Kind: message.OriginTask, Source: note.task}
	s.enqueue(&queueItem{id: id, origin: origin, mode: mode, wake: wake, parts: []*itemPart{{id: id, origin: origin, text: text, note: &note, receipt: s.newReceipt(id)}}})
}

// dropNotes takes a task's undelivered notifications out of the queue.
func (s *Session) dropNotes(task, reason string) {
	s.dropQueued(reason, func(it *queueItem) bool {
		return it.origin.Kind != message.OriginTask || it.parts[0].note == nil || it.parts[0].note.task != task
	})
}

// noteBody is the notification text for the task's current state, and the result of a foreground call that stopped for input.
func (s *Session) noteBody(info TaskInfo) string {
	vars := map[string]string{"task_id": info.ID, "title": info.Title, "attempt": strconv.Itoa(info.Attempt)}
	if f := s.frozen.Load(); f != nil {
		for _, name := range []string{"task_send", "task_restart"} {
			if f.names[name] {
				vars[name] = "yes"
			}
		}
	}
	switch info.Status {
	case TaskWaitingInput:
		vars["input"], vars["question"] = "yes", prompt.Sanitize(info.Question)
		if info.PendingInstructions > 0 {
			vars["unread"] = strconv.Itoa(info.PendingInstructions)
		}
	case TaskSucceeded, TaskFailed, TaskCanceled, TaskLost:
		vars[string(info.Status)] = "yes"
		vars["error"] = prompt.Sanitize(info.Error)
		if r := info.Result; r != nil && r.Summary != "" {
			vars["summary"] = s.foreign(r.Summary, defaults().Tasks.SummaryChars)
		}
		if n := len(info.UnprocessedInstructions); n > 0 {
			vars["unprocessed"] = strconv.Itoa(n)
		}
	}
	return prompt.Filef("task_notification", vars)
}

// foreign sanitizes text from another agent or task and cuts it to max code points with a note.
func (s *Session) foreign(text string, max int) string {
	cut, n := prompt.Truncate(prompt.Sanitize(text), max)
	if n > 0 {
		cut += "\n" + prompt.Textf("foreign_truncated", map[string]string{"count": strconv.Itoa(n)})
	}
	return cut
}

// SanitizeForeign escapes runtime tags, quotes turn markers and truncates text that came from another agent or task.
func SanitizeForeign(s string) string {
	cut, n := prompt.Truncate(prompt.Sanitize(s), defaults().Tasks.ForeignChars)
	if n > 0 {
		cut += "\n" + prompt.Textf("foreign_truncated", map[string]string{"count": strconv.Itoa(n)})
	}
	return cut
}

// noteBlock wraps a delivered notification in its tag; the body is escaped again, since a task_notify hook may have rewritten it.
func noteBlock(p *itemPart) *message.TextBlock {
	n := p.note
	return &message.TextBlock{Text: "<task-notification task_id=" + prompt.QuoteAttr(n.task) + " kind=" + prompt.QuoteAttr(n.kind) + " status=" + prompt.QuoteAttr(string(n.status)) + ">\n" +
		prompt.EscapeRuntimeTags(p.text) + "\n</task-notification>"}
}

// ---- persistence ----

func (e *taskEntry) ref() message.TaskRef {
	spec, _ := json.Marshal(e.spec)
	status := e.info.Status
	if a := e.cur; !status.Terminal() && a.begun && a.stopReq && !a.settled && a.denied == "" {
		status = TaskCanceled // it can only end canceled, and a silent stop leaves no later line to say so
	}
	return message.TaskRef{ID: e.id, Kind: e.info.Kind, Title: e.info.Title, Status: string(status), Attempt: e.info.Attempt, Spec: spec, State: e.info.State}
}

// taskRefs are the references a line carries, with each task's status and state at commit time.
func (s *Session) taskRefs(st *state, ids []string) []message.TaskRef {
	var out []message.TaskRef
	for _, id := range ids {
		if e := st.tasks.byID[id]; e != nil && !slices.ContainsFunc(out, func(r message.TaskRef) bool { return r.ID == id }) {
			out = append(out, e.ref())
		}
	}
	return out
}

// callRefs hands a tool line the tasks its calls started, continued, restarted or stopped.
func (s *Session) callRefs(st *state, line *message.Message) []message.TaskRef {
	var ids []string
	for _, b := range line.Content {
		if r, ok := b.(*message.ToolResultBlock); ok {
			ids = append(ids, st.tasks.byCall[r.CallID]...)
			delete(st.tasks.byCall, r.CallID)
		}
	}
	return s.taskRefs(st, ids)
}

// restoreTasks rebuilds the table from the last reference of each task outside copies; one that was unfinished is lost, and its notice waits for the next input.
func (s *Session) restoreTasks(st *state, lines []message.Message) {
	last := map[string]message.TaskRef{}
	since := map[string]time.Time{} // the first line of each task's last attempt, the closest the file comes to its start
	var order []string
	for _, m := range lines {
		if m.CopyOf != "" {
			continue // the original is in the file, and the copy's status may be older
		}
		for _, r := range m.Tasks {
			prev, ok := last[r.ID]
			if !ok {
				order = append(order, r.ID)
			}
			if !ok || prev.Attempt != r.Attempt {
				since[r.ID] = m.CreatedAt
			}
			last[r.ID] = r
		}
	}
	if len(order) == 0 {
		return
	}
	var lost []*taskEntry
	for _, id := range order {
		r := last[id]
		var spec TaskSpec
		_ = json.Unmarshal(r.Spec, &spec)
		spec.Kind = r.Kind
		kind, _ := s.taskKind(r.Kind)
		e := &taskEntry{id: id, kind: kind, spec: spec}
		e.info = TaskInfo{ID: id, Kind: r.Kind, Title: r.Title, Status: TaskStatus(r.Status), Attempt: r.Attempt, State: r.State, StartedAt: since[id]}
		e.cur = &taskAttempt{n: r.Attempt, begun: true, settled: true, finished: true, status: e.info.Status}
		if !e.info.Status.Terminal() {
			e.info.Status, e.info.Error = TaskLost, "the process restarted while it was running"
			lost = append(lost, e)
		}
		st.tasks.byID[id] = e
		st.tasks.order = append(st.tasks.order, e)
	}
	s.publishTasks(st)
	s.publish(tasksSnapshot(st))
	for _, e := range lost {
		s.publish(&event.TaskFinished{TaskID: e.id, Status: string(TaskLost), Attempt: e.info.Attempt, IsError: true})
		s.notifyTask(st, e, false)
	}
	s.evictFinished(st)
}

// ---- the runtime a task sees ----

type taskRuntime struct {
	s      *Session
	id     string
	kind   string
	spec   TaskSpec
	n      int
	callID string
	turnID string
	inbox  chan TaskInstruction
	ctx    context.Context
	live   atomic.Bool
	kids   *childSet
}

func (rt *taskRuntime) ID() string                    { return rt.id }
func (rt *taskRuntime) Spec() TaskSpec                { return rt.spec }
func (rt *taskRuntime) Attempt() int                  { return rt.n }
func (rt *taskRuntime) TurnID() string                { return rt.turnID }
func (rt *taskRuntime) Inbox() <-chan TaskInstruction { return rt.inbox }
func (rt *taskRuntime) Session() *Session             { return rt.s }
func (rt *taskRuntime) hop() event.Hop {
	return event.Hop{Kind: "task", ID: rt.id, Name: rt.kind, SessionID: rt.s.id, CallID: rt.callID, TurnID: rt.turnID}
}
func (rt *taskRuntime) current(st *state) *taskEntry {
	if e := st.tasks.byID[rt.id]; e != nil && e.cur.rt == rt && !e.cur.settled {
		return e
	}
	return nil
}

// Report waits for backpressure credit like any bulk event, bounded by the attempt's context.
func (rt *taskRuntime) Report(p TaskProgress) {
	if !rt.live.Load() || rt.s.bus.WaitWritable(rt.ctx) != nil {
		return
	}
	p.Data, _ = compactJSON(p.Data)
	rt.s.post(func(st *state) {
		e := rt.current(st)
		if e == nil {
			return
		}
		e.info.Progress = p
		rt.s.publishTasks(st)
		rt.s.publish(&event.TaskProgress{TaskID: rt.id, Text: p.Text, Percent: p.Percent, Data: p.Data})
	})
}

func (rt *taskRuntime) AskInput(q string) {
	if rt.live.Load() {
		rt.s.post(func(st *state) { rt.s.askInput(st, rt, q) })
	}
}

// askInput moves the attempt to waiting_input; a foreground call gets the question as its result and lets the task go on in the background.
func (s *Session) askInput(st *state, rt *taskRuntime, q string) {
	e := rt.current(st)
	if e == nil || e.cur.stopReq {
		return
	}
	if !e.kind.Caps.Instructions {
		s.publish(&event.Error{Code: "internal", Message: "task " + e.id + " asked for input, but its kind does not accept instructions"})
		return
	}
	from := e.info.Status
	if q == "" {
		if from != TaskWaitingInput {
			return
		}
		e.info.Status, e.info.Question = TaskRunning, ""
		e.floorVer = e.noteVer // the question is gone, and so is its notice
		s.dropNotes(e.id, "withdrawn")
		s.taskChanged(st, e, &event.TaskStatusChanged{TaskID: e.id, From: string(from), To: string(TaskRunning)})
		return
	}
	e.info.Status, e.info.Question = TaskWaitingInput, q
	fg := e.fg
	e.fg = nil
	if len(fg) > 0 {
		e.info.Foreground = false
		for _, w := range fg {
			r := TextResult(s.noteBody(e.view()))
			r.Structured = taskStructured(e.view())
			w.res = &r
		}
	}
	s.taskChanged(st, e, &event.TaskStatusChanged{TaskID: e.id, From: string(from), To: string(TaskWaitingInput), Question: q})
	for _, w := range fg {
		close(w.ch)
	}
	if len(fg) == 0 {
		s.notifyTask(st, e, true)
	}
}

func (rt *taskRuntime) SaveState(saved json.RawMessage) {
	if !rt.live.Load() {
		return
	}
	saved, ok := compactJSON(saved)
	if !ok {
		rt.s.post(func(*state) {
			rt.s.publish(&event.Error{Code: "internal", Message: "task " + rt.id + " saved a state that is not valid JSON; it was not kept"})
		})
		return
	}
	rt.s.post(func(st *state) {
		if e := rt.current(st); e != nil {
			e.info.State = saved
			rt.s.publishTasks(st)
		}
	})
}

// Holding replaces the attempt's report of what it took from the inbox and has not acted on, so an early end lists them as unprocessed.
func (rt *taskRuntime) Holding(ins []TaskInstruction) {
	if !rt.live.Load() {
		return
	}
	ins = slices.Clone(ins)
	rt.s.post(func(st *state) {
		if e := rt.current(st); e != nil {
			e.cur.holding = ins
		}
	})
}

// Emit publishes an extension event under the task's hop, through the actor like Report, so a task's events keep the order it made them in.
func (rt *taskRuntime) Emit(p event.Payload) {
	if !rt.live.Load() {
		return
	}
	rt.s.post(func(*state) { rt.s.publishUnder(p, []event.Hop{rt.hop()}) })
}

// Relay republishes an event from elsewhere under the task's hop; a bulk one first waits for credit, so a slow subscriber here slows the source down.
func (rt *taskRuntime) Relay(ctx context.Context, ev event.Event) error {
	if !rt.live.Load() {
		return nil
	}
	if bp, ok := ev.Data.(event.Bulk); ok && bp.Bulk() {
		if err := rt.s.bus.WaitWritable(ctx); err != nil {
			return err
		}
	}
	hop := rt.hop()
	hop.Seq = ev.Seq
	ev.Path = append([]event.Hop{hop}, ev.Path...)
	rt.s.post(func(*state) { rt.s.bus.Publish(&ev) }) // through the actor like Report, so progress and relayed events stay in order
	return nil
}

// compactJSON checks JSON a task or its caller hands over and strips its whitespace, because it is written to the session file and events as is.
func compactJSON(raw json.RawMessage) (json.RawMessage, bool) {
	if len(raw) == 0 {
		return nil, true
	}
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return nil, false
	}
	return b.Bytes(), true
}

// frozenSet is what the model was last sent: the tool names the notification texts may point to, and each kind's start tool, which a restart is reviewed under.
type frozenSet struct {
	names  map[string]bool
	starts map[string]string
}

func (s *Session) noteFrozen(defs []message.ToolDef, tools map[string]frozenTool) {
	f := &frozenSet{names: map[string]bool{}, starts: map[string]string{}}
	for _, d := range defs {
		f.names[d.Name] = true
	}
	for name, ft := range tools {
		if k := ft.spec.taskKind; k != "" && f.names[name] {
			f.starts[k] = name
		}
	}
	s.frozen.Store(f)
}
