package peer

import (
	"context"
	"iter"
	"slices"
	"sync"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
)

const (
	defaultIdleClose   = 30 * time.Minute
	defaultMaxSessions = 64
	keepFinished       = 1024 // finished exchanges Subscribe still knows
	replyArtifact      = "reply"
)

type LocalOption func(*local)

// IdleClose closes a conversation's session after it was idle this long; a later message resumes it when the agent has a store.
func IdleClose(d time.Duration) LocalOption { return func(l *local) { l.idle = d } }

// MaxSessions bounds the open conversations; the least recently used idle one is closed first.
func MaxSessions(n int) LocalOption { return func(l *local) { l.max = n } }

// Local exposes another agent of this process as a peer without serialization; each context is one of its sessions.
func Local(ag *kernel.Agent, opts ...LocalOption) Peer {
	l := &local{ag: ag, idle: defaultIdleClose, max: defaultMaxSessions, open: map[string]*lsess{}, tasks: map[string]*lexch{}}
	for _, o := range opts {
		o(l)
	}
	l.card = Card{Name: ag.Nickname(), Description: ag.Description(), Streaming: true}
	return l
}

type local struct {
	ag      *kernel.Agent
	card    Card
	idle    time.Duration
	max     int
	opening sync.Mutex // one session opened at a time, so a context is never resumed twice
	mu      sync.Mutex
	open    map[string]*lsess // context id -> its session
	tasks   map[string]*lexch // input id -> its exchange
	done    []string          // finished exchanges, oldest first
	tick    uint64
}

type lsess struct {
	s     *kernel.Session
	busy  int
	used  uint64
	timer *time.Timer
}

// lexch is one message's exchange, which Subscribe and Cancel find by its input id.
type lexch struct {
	mu      sync.Mutex
	ctxID   string
	sess    *kernel.Session
	turn    string
	cancel  bool
	state   TaskState
	text    string
	message string
	done    chan struct{}
}

func (l *local) Card() Card { return l.card }

func (l *local) Send(ctx context.Context, msg Message, _ SendOptions) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		ls, err := l.acquire(ctx, msg.ContextID)
		if err != nil {
			yield(Event{}, err)
			return
		}
		defer l.release(ls)
		l.exchange(ctx, ls.s, msg, yield)
	}
}

// exchange hands the message to the session as its own turn and turns that turn's events into peer events, relaying them to the task that drives it.
func (l *local) exchange(ctx context.Context, s *kernel.Session, msg Message, yield func(Event, error) bool) {
	rt := runtimeFrom(ctx)
	ch := make(chan event.Event, 16)
	sub, err := s.Subscribe(ch, kernel.WithStallTimeout(-1))
	if err != nil {
		yield(Event{}, err)
		return
	}
	defer sub.Unsubscribe()
	from := "peer"
	if rt != nil {
		from = "peer:" + rt.Session().Info().Agent
	}
	in := kernel.Input{Text: partsText(msg.Parts), Origin: message.Origin{Kind: message.OriginAgent, Source: from}}
	if c, ok := msg.Metadata[chainKey]; ok {
		in.Metadata = map[string]string{chainKey: string(c)}
	}
	rc, err := s.Send(in, kernel.WithMode(kernel.ModeQueue)) // one message is one turn, never merged into a running one
	if err != nil {
		yield(Event{}, err)
		return
	}
	id := rc.InputID()
	ctxID := s.Info().SessionID
	x := &lexch{ctxID: ctxID, sess: s, state: StateSubmitted, done: make(chan struct{})}
	l.mu.Lock()
	l.tasks[id] = x
	l.mu.Unlock()
	finish := func(res kernel.TurnResult) []Event {
		evs := l.finish(id, x, res)
		if rt != nil && res.TurnID != "" {
			if calls, err := s.TurnCalls(res.TurnID); err == nil {
				rt.Session().RecordCalls("peer", rt.TurnID(), calls.Calls)
			}
		}
		return evs
	}
	// a caller that stops reading leaves the turn running, so its end is still recorded for Subscribe
	abandon := func() {
		go func() {
			res, _ := rc.Wait(context.Background())
			finish(res)
		}()
	}
	if !yield(Event{Kind: EventTask, TaskID: id, ContextID: ctxID, State: StateSubmitted}, nil) {
		abandon()
		return
	}
	turn := ""
	fresh := true
	done := rc.Done()
	for {
		select {
		case ev := <-ch:
			if d, ok := ev.Data.(*event.TurnStarted); ok && turn == "" && len(ev.Path) == 0 && slices.Contains(d.InputIDs, id) {
				turn = d.TurnID
				x.mu.Lock()
				x.turn, x.state = turn, StateWorking
				stop := x.cancel
				x.mu.Unlock()
				if stop {
					s.InterruptTurn(turn, "peer cancel")
				}
				if !yield(Event{Kind: EventStatus, TaskID: id, ContextID: ctxID, State: StateWorking}, nil) {
					abandon()
					return
				}
			}
			if turn == "" || other(ev, turn) {
				continue
			}
			if rt != nil {
				if err := rt.Relay(ctx, ev); err != nil {
					abandon()
					yield(Event{}, err)
					return
				}
			}
			switch d := ev.Data.(type) {
			case *event.StepStarted:
				if len(ev.Path) == 0 {
					fresh = true
				}
			case *event.MessageDelta:
				if len(ev.Path) > 0 || d.Channel != "text" || d.Text == "" {
					continue
				}
				x.mu.Lock()
				if fresh {
					x.text = ""
				}
				x.text += d.Text
				x.mu.Unlock()
				ok := yield(Event{Kind: EventArtifact, TaskID: id, ContextID: ctxID, Artifact: &Artifact{ID: replyArtifact, Parts: []Part{{Text: d.Text}}}, Append: !fresh}, nil)
				fresh = false
				if !ok {
					abandon()
					return
				}
			case *event.TurnCompleted:
				if len(ev.Path) > 0 || d.TurnID != turn {
					continue
				}
				select {
				case <-rc.Done():
				case <-ctx.Done():
					abandon()
					yield(Event{}, ctx.Err())
					return
				}
				res, _ := rc.Wait(context.Background()) // done already; the task's context would count as waiting on its own chain when ids repeat
				for _, out := range finish(res) {
					if !yield(out, nil) {
						return
					}
				}
				return
			}
		case <-done:
			done = nil
			res, _ := rc.Wait(context.Background())
			if turn != "" || res.TurnID != "" {
				continue // a turn ran, so its events from turn.started to turn.completed are still on their way
			}
			for _, out := range finish(res) {
				if !yield(out, nil) {
					return
				}
			}
			return
		case <-ctx.Done():
			l.cancelExchange(x)
			abandon()
			yield(Event{}, ctx.Err())
			return
		}
	}
}

// other reports an event of another turn, such as a background task a different turn started.
func other(ev event.Event, turn string) bool {
	if ev.TurnID != "" && ev.TurnID != turn {
		return true
	}
	return len(ev.Path) > 0 && ev.Path[0].TurnID != "" && ev.Path[0].TurnID != turn
}

// finish fixes the exchange's end from the turn's result, once; the reply artifact is the turn's last text.
func (l *local) finish(id string, x *lexch, res kernel.TurnResult) []Event {
	state, note := StateCompleted, ""
	switch res.Outcome {
	case kernel.OutcomeCompleted:
	case kernel.OutcomeError:
		state = StateFailed
		if res.Err != nil {
			note = res.Err.Error()
		}
	case kernel.OutcomeInterrupted, kernel.OutcomeCanceled:
		state = StateCanceled
	case kernel.OutcomeDropped:
		state = StateRejected
	default:
		note = text("outcome", "outcome", string(res.Outcome))
	}
	x.mu.Lock()
	select {
	case <-x.done:
		x.mu.Unlock()
		return nil
	default:
	}
	x.state, x.text, x.message = state, res.FinalText, note
	close(x.done)
	ctxID := x.ctxID
	x.mu.Unlock()
	l.mu.Lock()
	l.done = append(l.done, id)
	if len(l.done) > keepFinished {
		delete(l.tasks, l.done[0])
		l.done = l.done[1:]
	}
	l.mu.Unlock()
	var out []Event
	if state != StateRejected {
		out = append(out, Event{Kind: EventArtifact, TaskID: id, ContextID: ctxID, Artifact: &Artifact{ID: replyArtifact, Parts: []Part{{Text: res.FinalText}}}, LastChunk: true})
	}
	return append(out, statusEvent(id, ctxID, state, note))
}

func statusEvent(id, ctxID string, state TaskState, note string) Event {
	ev := Event{Kind: EventStatus, TaskID: id, ContextID: ctxID, State: state}
	if note != "" {
		ev.Message = &Message{Role: "agent", Parts: []Part{{Text: note}}}
	}
	return ev
}

// Subscribe gives the exchange's state now, then its end when it is still running.
func (l *local) Subscribe(ctx context.Context, taskID string) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		l.mu.Lock()
		x := l.tasks[taskID]
		l.mu.Unlock()
		if x == nil {
			yield(Event{}, ErrUnknownTask)
			return
		}
		x.mu.Lock()
		snap := Event{Kind: EventTask, TaskID: taskID, ContextID: x.ctxID, State: x.state}
		if x.message != "" {
			snap.Message = &Message{Role: "agent", Parts: []Part{{Text: x.message}}}
		}
		body, ended, note := x.text, x.state.EndsRound(), x.message
		x.mu.Unlock()
		if !yield(snap, nil) {
			return
		}
		if body != "" && !yield(Event{Kind: EventArtifact, TaskID: taskID, ContextID: snap.ContextID, Artifact: &Artifact{ID: replyArtifact, Parts: []Part{{Text: body}}}, LastChunk: ended}, nil) {
			return
		}
		if ended {
			yield(statusEvent(taskID, snap.ContextID, snap.State, note), nil)
			return
		}
		select {
		case <-x.done:
		case <-ctx.Done():
			yield(Event{}, ctx.Err())
			return
		}
		x.mu.Lock()
		state := x.state
		body, note = x.text, x.message
		x.mu.Unlock()
		if state != StateRejected && !yield(Event{Kind: EventArtifact, TaskID: taskID, ContextID: snap.ContextID, Artifact: &Artifact{ID: replyArtifact, Parts: []Part{{Text: body}}}, LastChunk: true}, nil) {
			return
		}
		yield(statusEvent(taskID, snap.ContextID, state, note), nil)
	}
}

// Cancel interrupts the exchange's turn, or the turn as soon as it starts when the message still waits in the queue.
func (l *local) Cancel(_ context.Context, taskID string) error {
	l.mu.Lock()
	x := l.tasks[taskID]
	l.mu.Unlock()
	if x != nil {
		l.cancelExchange(x)
	}
	return nil
}

func (l *local) cancelExchange(x *lexch) {
	x.mu.Lock()
	x.cancel = true
	turn := x.turn
	x.mu.Unlock()
	if turn != "" {
		x.sess.InterruptTurn(turn, "peer cancel")
	}
}

// ---- sessions ----

// acquire finds the context's session, resumes it, or starts a new one whose id is the new context.
func (l *local) acquire(ctx context.Context, contextID string) (*lsess, error) {
	l.opening.Lock()
	defer l.opening.Unlock()
	l.mu.Lock()
	if ls := l.open[contextID]; ls != nil && contextID != "" && !closing(ls.s) {
		l.use(ls)
		l.mu.Unlock()
		return ls, nil
	}
	l.mu.Unlock()
	var s *kernel.Session
	if contextID != "" {
		s, _ = l.ag.Resume(ctx, contextID) // without the stored session, the peer starts a new context and says so
	}
	if s == nil {
		var err error
		if s, err = l.ag.NewSession(ctx); err != nil {
			return nil, err
		}
	}
	ls := &lsess{s: s}
	l.mu.Lock()
	l.evict()
	l.open[s.Info().SessionID] = ls
	l.use(ls)
	l.mu.Unlock()
	return ls, nil
}

func (l *local) use(ls *lsess) {
	ls.busy++
	l.tick++
	ls.used = l.tick
	if ls.timer != nil {
		ls.timer.Stop()
		ls.timer = nil
	}
}

func (l *local) release(ls *lsess) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ls.busy--
	if ls.busy > 0 || l.idle <= 0 {
		return
	}
	ls.timer = time.AfterFunc(l.idle, func() {
		l.mu.Lock()
		if ls.busy > 0 || l.open[ls.s.Info().SessionID] != ls {
			l.mu.Unlock()
			return
		}
		l.drop(ls)
		l.mu.Unlock()
	})
}

// evict closes the least recently used idle session when the limit is reached; busy ones are never closed.
func (l *local) evict() {
	if l.max <= 0 || len(l.open) < l.max {
		return
	}
	var oldest *lsess
	for _, ls := range l.open {
		if ls.busy == 0 && (oldest == nil || ls.used < oldest.used) {
			oldest = ls
		}
	}
	if oldest != nil {
		l.drop(oldest)
	}
}

func (l *local) drop(ls *lsess) {
	if ls.timer != nil {
		ls.timer.Stop()
	}
	delete(l.open, ls.s.Info().SessionID)
	for id, x := range l.tasks {
		if x.sess == ls.s {
			delete(l.tasks, id)
		}
	}
	l.done = slices.DeleteFunc(l.done, func(id string) bool { _, ok := l.tasks[id]; return !ok })
	go func() { _ = ls.s.Close(context.Background()) }()
}

func closing(s *kernel.Session) bool {
	ph := s.Snapshot().Phase
	return ph == "closing" || ph == "closed"
}
