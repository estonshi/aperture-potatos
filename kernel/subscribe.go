package kernel

import (
	"context"
	"errors"
	"iter"
	"sync"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/internal/bus"
)

// SubscribeOption configures one subscription.
type SubscribeOption func(*subscribeConfig)

type subscribeConfig struct {
	policy      bus.Policy
	highWater   int
	stall       time.Duration
	types       []string
	maxDepth    *int
	native      bool
	closeOnDone bool
}

// WithDropDeltas drops delta events above the high water mark instead of holding the model stream back, for a display that can skip frames.
func WithDropDeltas() SubscribeOption { return func(c *subscribeConfig) { c.policy = bus.DropDeltas } }

// WithBuffer sets the high water mark of the subscription queue, and raises its hard limit to at least twice that.
func WithBuffer(highWater int) SubscribeOption {
	return func(c *subscribeConfig) { c.highWater = highWater }
}

// WithStallTimeout sets how long a lossless subscriber may hold producers back without consuming; d < 0 disables detaching.
func WithStallTimeout(d time.Duration) SubscribeOption {
	return func(c *subscribeConfig) { c.stall = d }
}

// WithTypes keeps events whose type starts with one of the prefixes, e.g. "message.", "tool.", "turn.".
func WithTypes(prefixes ...string) SubscribeOption {
	return func(c *subscribeConfig) { c.types = append(c.types, prefixes...) }
}

// WithMaxDepth keeps events nested at most n hops deep; 0 means the session's own loop only.
func WithMaxDepth(n int) SubscribeOption { return func(c *subscribeConfig) { c.maxDepth = &n } }

// WithNative keeps the native provider data in message.completed.
func WithNative() SubscribeOption { return func(c *subscribeConfig) { c.native = true } }

// WithCloseOnDone closes the channel after the last event; only for a channel with no other writer.
func WithCloseOnDone() SubscribeOption { return func(c *subscribeConfig) { c.closeOnDone = true } }

func newSubscribeConfig(opts []SubscribeOption) subscribeConfig {
	var c subscribeConfig
	for _, o := range opts {
		o(&c)
	}
	return c
}

func (c subscribeConfig) options() bus.Options {
	d := defaults().Subscription
	o := bus.Options{Policy: c.policy, HighWater: d.HighWater, HardLimit: d.HardLimit, StallTimeout: ms(d.StallTimeout), Types: c.types, MaxDepth: -1, Native: c.native}
	if c.highWater > 0 {
		o.HighWater = c.highWater
		o.HardLimit = max(o.HardLimit, 2*c.highWater)
	}
	if c.stall != 0 {
		o.StallTimeout = c.stall
	}
	if c.maxDepth != nil {
		o.MaxDepth = *c.maxDepth
	}
	return o
}

// Subscription is the handle of one sink; Unsubscribe never blocks, Done reports when the sink will never be touched again.
type Subscription struct {
	unsub   func()
	done    <-chan struct{}
	err     func() error
	dropped func() uint64
}

// Unsubscribe detaches the sink and discards its queued events; a handler call already running finishes, which Done reports.
func (s *Subscription) Unsubscribe() { s.unsub() }

// Done is closed once the sink will never be sent another event or called again.
func (s *Subscription) Done() <-chan struct{} { return s.done }

// Err is nil after Unsubscribe; otherwise ErrSubscriberStalled, ErrSubscriberOverflow, ErrChannelClosed, a handler error, or ErrSessionClosed.
func (s *Subscription) Err() error { return s.err() }

// Dropped counts the delta events WithDropDeltas has skipped so far.
func (s *Subscription) Dropped() uint64 { return s.dropped() }

func (s *Session) wrap(sub *bus.Sub) *Subscription {
	return &Subscription{
		unsub:   func() { s.bus.Unsubscribe(sub) },
		done:    sub.Done(),
		err:     func() error { return sessionErr(sub.Err()) },
		dropped: sub.Dropped,
	}
}

func sessionErr(err error) error {
	if errors.Is(err, bus.ErrClosed) {
		return ErrSessionClosed
	}
	return err
}

// sinkSpec is a channel or a function waiting to be attached to a session.
type sinkSpec struct {
	ch   chan<- event.Event
	fn   func(context.Context, event.Event) error
	opts subscribeConfig
}

// attachSink subscribes and starts the pump; closeOnDone is left to agent-level sinks, which close only after every session's pump.
func (s *Session) attachSink(sk sinkSpec) *bus.Sub {
	sub := s.bus.Subscribe(sk.opts.options())
	if sk.fn != nil {
		go bus.PumpFunc(s.handlerContext(sk.opts), s.bus, sub, sk.fn)
	} else {
		go bus.PumpChan(context.Background(), s.bus, sub, sk.ch, sk.opts.closeOnDone)
	}
	return sub
}

func (s *Session) handlerContext(c subscribeConfig) context.Context {
	return withMarker(context.Background(), marker{chain: s.chain, kind: "handler", block: c.policy == bus.Block})
}

// Subscribe registers a caller-owned channel; its buffer is only a latency cushion because the subscription queue does the buffering.
func (s *Session) Subscribe(ch chan<- event.Event, opts ...SubscribeOption) (*Subscription, error) {
	if s.snap.Load().Phase == string(phaseClosed) {
		return nil, ErrSessionClosed
	}
	return s.wrap(s.attachSink(sinkSpec{ch: ch, opts: newSubscribeConfig(opts)})), nil
}

// SubscribeFunc calls fn sequentially on a dedicated goroutine; returning an error ends the subscription.
func (s *Session) SubscribeFunc(fn func(ctx context.Context, ev event.Event) error, opts ...SubscribeOption) (*Subscription, error) {
	if s.snap.Load().Phase == string(phaseClosed) {
		return nil, ErrSessionClosed
	}
	return s.wrap(s.attachSink(sinkSpec{fn: fn, opts: newSubscribeConfig(opts)})), nil
}

// WithSessionSink attaches ch when the session is created, so session.started is never missed.
func WithSessionSink(ch chan<- event.Event, opts ...SubscribeOption) SessionOption {
	return func(c *sessionConfig) { c.sinks = append(c.sinks, sinkSpec{ch: ch, opts: newSubscribeConfig(opts)}) }
}

// WithSessionFunc is WithSessionSink with a callback, so fn sees session.started too.
func WithSessionFunc(fn func(ctx context.Context, ev event.Event) error, opts ...SubscribeOption) SessionOption {
	return func(c *sessionConfig) { c.sinks = append(c.sinks, sinkSpec{fn: fn, opts: newSubscribeConfig(opts)}) }
}

// Events is a single-use iterator consumed on the caller's goroutine; breaking out unsubscribes.
func (s *Session) Events(ctx context.Context, opts ...SubscribeOption) iter.Seq2[event.Event, error] {
	c := newSubscribeConfig(opts)
	return func(yield func(event.Event, error) bool) {
		sub := s.bus.Subscribe(c.options())
		defer sub.Finish()
		defer s.bus.Unsubscribe(sub)
		for {
			ev, ok := sub.Pop(ctx, s.bus)
			if !ok {
				if ctx.Err() != nil {
					yield(event.Event{}, context.Cause(ctx))
				} else if err := sub.Err(); err != nil && !errors.Is(err, bus.ErrClosed) {
					yield(event.Event{}, err)
				}
				return
			}
			if !yield(*ev, nil) {
				return
			}
		}
	}
}

// Run sends one input and yields the events of the turn that consumes it until turn.completed; breaking out or canceling ctx only stops iterating, never the turn.
// For a command it yields the command's events until command.completed.
func (s *Session) Run(ctx context.Context, in Input, opts ...SendOption) iter.Seq2[event.Event, error] {
	return func(yield func(event.Event, error) bool) {
		if reentrant(ctx, s.id) {
			yield(event.Event{}, ErrReentrantWait)
			return
		}
		sub := s.bus.Subscribe(subscribeConfig{}.options())
		defer sub.Finish()
		defer s.bus.Unsubscribe(sub)
		rc, err := s.Send(in, opts...)
		if err != nil {
			yield(event.Event{}, err)
			return
		}
		id := rc.InputID()
		var turnID string
		for {
			ev, ok := sub.Pop(ctx, s.bus)
			if !ok {
				if ctx.Err() != nil {
					yield(event.Event{}, context.Cause(ctx))
				} else {
					yield(event.Event{}, sessionErr(sub.Err()))
				}
				return
			}
			switch d := ev.Data.(type) {
			case *event.TurnStarted:
				if turnID == "" && contains(d.InputIDs, id) {
					turnID = d.TurnID
				}
			case *event.InputInjected:
				if turnID == "" && contains(d.InputIDs, id) {
					turnID = d.TurnID
				}
			case *event.InputDropped:
				if d.InputID == id {
					yield(*ev, nil)
					return
				}
			case *event.CommandStarted:
				if d.CommandID == id && !yield(*ev, nil) {
					return
				}
				continue
			case *event.CommandOutput:
				if d.CommandID == id && !yield(*ev, nil) {
					return
				}
				continue
			case *event.CommandCompleted:
				if d.CommandID == id {
					yield(*ev, nil)
					return
				}
				continue
			}
			if turnID == "" || ev.TurnID != turnID {
				continue
			}
			if !yield(*ev, nil) {
				return
			}
			if d, ok := ev.Data.(*event.TurnCompleted); ok && d.TurnID == turnID {
				return
			}
		}
	}
}

// agentSink attaches one caller sink to every current and future session of an agent.
type agentSink struct {
	spec  sinkSpec
	mu    sync.Mutex
	subs  map[*bus.Sub]*Session
	wg    sync.WaitGroup
	ended bool
	done  chan struct{}
	err   error
}

func newAgentSink(sp sinkSpec) *agentSink {
	return &agentSink{spec: sp, subs: map[*bus.Sub]*Session{}, done: make(chan struct{})}
}

func (a *agentSink) attach(s *Session) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ended {
		return
	}
	sp := a.spec
	sp.opts.closeOnDone = false // one channel serves many pumps, so only the last one out may close it
	sub := s.attachSink(sp)
	a.subs[sub] = s
	a.wg.Go(func() {
		<-sub.Done()
		a.mu.Lock()
		if err := sub.Err(); err != nil && !errors.Is(err, bus.ErrClosed) {
			a.err = err
		}
		delete(a.subs, sub)
		a.mu.Unlock()
	})
}

// end stops attaching; with cut the sessions stop delivering at once, otherwise their pumps drain what the closing sessions still hold.
func (a *agentSink) end(cut bool) {
	a.mu.Lock()
	if a.ended {
		a.mu.Unlock()
		return
	}
	a.ended = true
	if cut {
		for sub, s := range a.subs {
			s.bus.Unsubscribe(sub)
		}
	}
	a.mu.Unlock()
	go func() {
		a.wg.Wait()
		if a.spec.ch != nil && a.spec.opts.closeOnDone {
			close(a.spec.ch)
		}
		close(a.done)
	}()
}

func (a *agentSink) subscription(owner *Agent) *Subscription {
	return &Subscription{
		unsub: func() {
			owner.removeSink(a)
			a.end(true)
		},
		done: a.done,
		err: func() error {
			a.mu.Lock()
			defer a.mu.Unlock()
			return a.err
		},
		dropped: func() uint64 {
			a.mu.Lock()
			defer a.mu.Unlock()
			var n uint64
			for sub := range a.subs {
				n += sub.Dropped()
			}
			return n
		},
	}
}

func (a *Agent) addSink(sp sinkSpec) (*Subscription, error) {
	sk := newAgentSink(sp)
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil, ErrAgentClosed
	}
	a.sinks = append(a.sinks, sk)
	sessions := make([]*Session, 0, len(a.open))
	for _, s := range a.open {
		sessions = append(sessions, s)
	}
	a.mu.Unlock()
	for _, s := range sessions {
		sk.attach(s)
	}
	return sk.subscription(a), nil
}

func (a *Agent) removeSink(sk *agentSink) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, x := range a.sinks {
		if x == sk {
			a.sinks = append(a.sinks[:i], a.sinks[i+1:]...)
			return
		}
	}
}

// Subscribe attaches ch to every current and future session of the agent; events carry their session_id.
func (a *Agent) Subscribe(ch chan<- event.Event, opts ...SubscribeOption) (*Subscription, error) {
	return a.addSink(sinkSpec{ch: ch, opts: newSubscribeConfig(opts)})
}

// SubscribeFunc is Subscribe with a callback, which is called sequentially within each session but concurrently across sessions.
func (a *Agent) SubscribeFunc(fn func(ctx context.Context, ev event.Event) error, opts ...SubscribeOption) (*Subscription, error) {
	return a.addSink(sinkSpec{fn: fn, opts: newSubscribeConfig(opts)})
}

// WithEventSink attaches ch to every session from its creation, so session.started is never missed.
func WithEventSink(ch chan<- event.Event, opts ...SubscribeOption) Option {
	return func(c *config) { c.sinks = append(c.sinks, sinkSpec{ch: ch, opts: newSubscribeConfig(opts)}) }
}

type markerKey struct{}

// marker travels in every callback's ctx, so a waiting method can tell it is being called from inside the session chain it would wait on.
type marker struct {
	chain []string // session ids from the root to the session running the callback
	tasks []string // the task whose Run is the caller
	kind  string   // handler | tool | command | hook | task
	call  string   // tool call id
	block bool     // handler of a lossless (Block) subscriber
	token bool     // the caller holds the history token (a WaitIdle or InterruptFirst command runner)
	seq   uint64   // that command's queue seq, which Clear drops older reminders by
}

func withMarker(ctx context.Context, m marker) context.Context {
	return context.WithValue(ctx, markerKey{}, m)
}

// reentrant reports whether waiting on session target from ctx would wait on a session that is running the caller.
func reentrant(ctx context.Context, target string) bool {
	m, ok := ctx.Value(markerKey{}).(marker)
	return ok && contains(m.chain, target) && (m.kind != "handler" || m.block)
}
