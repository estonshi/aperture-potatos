package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"slices"
	"time"

	"github.com/estonshi/aperture-potatos/ext/peer"
)

// rpcTenant is the tenant JSON-RPC carries in its params; HTTP+JSON carries it in the path.
func (r *remote) rpcTenant() string {
	if r.iface.binding == BindingJSONRPC {
		return r.iface.tenant
	}
	return ""
}

// Send streams when the card says it can, and otherwise sends without waiting and polls, since a blocking send holds one request open for the whole task.
func (r *remote) Send(ctx context.Context, msg peer.Message, opt peer.SendOptions) iter.Seq2[peer.Event, error] {
	return func(yield func(peer.Event, error) bool) {
		t := &tracker{r: r, yield: yield}
		if r.card.Streaming {
			fr, err := r.wire.stream(ctx, opStream, "", r.codec.send(msg, true, opt.HistoryLength, r.rpcTenant()))
			if err != nil {
				t.fail(err)
				return
			}
			t.follow(ctx, fr)
			return
		}
		raw, err := r.wire.unary(ctx, opSend, "", r.codec.send(msg, false, opt.HistoryLength, r.rpcTenant()))
		if err != nil {
			t.fail(err)
			return
		}
		o, err := r.codec.decode(raw)
		if err != nil {
			t.fail(err)
			return
		}
		if t.take(o) && !t.ended {
			t.poll(ctx)
		}
	}
}

// Subscribe starts from GetTask, so its first event is always the task as it is now, then follows the task to the end of its round.
func (r *remote) Subscribe(ctx context.Context, taskID string) iter.Seq2[peer.Event, error] {
	return func(yield func(peer.Event, error) bool) {
		t := &tracker{r: r, yield: yield, taskID: taskID}
		snap, err := t.get(ctx)
		if err != nil {
			t.fail(err)
			return
		}
		if !t.take(snap) || t.ended {
			return
		}
		if !r.card.Streaming {
			t.poll(ctx)
			return
		}
		fr, err := r.wire.stream(ctx, opSubscribe, taskID, r.codec.task(taskID, -1, r.rpcTenant()))
		if err != nil {
			fr = &oneFrame{done: true} // a broken subscribe is a broken stream: follow checks and reconnects
			if !retryable(ctx, err) {
				t.fail(err)
				return
			}
		}
		t.follow(ctx, fr)
	}
}

// Cancel is best effort; a task the peer cannot cancel has ended already.
func (r *remote) Cancel(ctx context.Context, taskID string) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	_, err := r.wire.unary(ctx, opCancel, taskID, r.codec.task(taskID, -1, r.rpcTenant()))
	if err == nil || notCancelable(err) {
		return nil
	}
	return err
}

// tracker follows one exchange: what the peer has said so far, so a snapshot taken after a gap adds only what is new.
type tracker struct {
	r       *remote
	yield   func(peer.Event, error) bool
	stopped bool // the consumer stopped, or an error was handed over
	first   bool // a first result arrived, so the peer has the message
	ended   bool
	taskID  string
	ctxID   string
	state   peer.TaskState
	status  string
	parts   map[string][]peer.Part
}

func (t *tracker) emit(ev peer.Event) bool {
	if t.stopped {
		return false
	}
	if !t.yield(ev, nil) {
		t.stopped = true
	}
	return !t.stopped
}

func (t *tracker) fail(err error) {
	if !t.stopped {
		t.stopped = true
		t.yield(peer.Event{}, err)
	}
}

func (t *tracker) ids(o object) {
	if o.taskID != "" {
		t.taskID = o.taskID
	}
	if o.contextID != "" {
		t.ctxID = o.contextID
	}
}

// take hands one result or stream event to the consumer; false means the consumer stopped.
func (t *tracker) take(o object) bool {
	if t.parts == nil {
		t.parts = map[string][]peer.Part{}
	}
	t.ids(o)
	switch o.kind {
	case objMessage:
		t.first, t.ended = true, true
		return t.emit(peer.Event{Kind: peer.EventMessage, TaskID: o.taskID, ContextID: o.contextID, Message: o.message})
	case objTask:
		if t.first {
			return t.reconcile(o)
		}
		t.first = true
		t.state, t.status = o.state, text(o.message)
		if !t.emit(peer.Event{Kind: peer.EventTask, TaskID: t.taskID, ContextID: t.ctxID, State: o.state, Message: o.message}) {
			return false
		}
		for _, a := range o.artifacts {
			t.parts[a.ID] = slices.Clip(a.Parts)
			if !t.emit(peer.Event{Kind: peer.EventArtifact, TaskID: t.taskID, ContextID: t.ctxID, Artifact: &a}) {
				return false
			}
		}
		if o.state.EndsRound() {
			t.ended = true
			return t.emit(peer.Event{Kind: peer.EventStatus, TaskID: t.taskID, ContextID: t.ctxID, State: o.state, Message: o.message})
		}
		return true
	case objStatus:
		t.first = true
		t.state, t.status = o.state, text(o.message)
		t.ended = o.state.EndsRound()
		return t.emit(peer.Event{Kind: peer.EventStatus, TaskID: t.taskID, ContextID: t.ctxID, State: o.state, Message: o.message})
	case objArtifact:
		t.first = true
		a := o.artifact
		if o.append {
			t.parts[a.ID] = append(t.parts[a.ID], a.Parts...)
		} else {
			t.parts[a.ID] = slices.Clip(a.Parts) // clipped, so the first append copies instead of writing into the emitted event's array
		}
		return t.emit(peer.Event{Kind: peer.EventArtifact, TaskID: t.taskID, ContextID: t.ctxID, Artifact: &a, Append: o.append, LastChunk: o.lastChunk})
	}
	return true
}

// reconcile turns a later snapshot into what it adds: each changed artifact whole, then the status when it moved or the round ended.
func (t *tracker) reconcile(o object) bool {
	for _, a := range o.artifacts {
		if samePart(t.parts[a.ID], a.Parts) {
			continue
		}
		t.parts[a.ID] = slices.Clip(a.Parts)
		if !t.emit(peer.Event{Kind: peer.EventArtifact, TaskID: t.taskID, ContextID: t.ctxID, Artifact: &a}) {
			return false
		}
	}
	msg := text(o.message)
	if o.state == t.state && msg == t.status && !o.state.EndsRound() {
		return true
	}
	t.state, t.status = o.state, msg
	t.ended = o.state.EndsRound()
	return t.emit(peer.Event{Kind: peer.EventStatus, TaskID: t.taskID, ContextID: t.ctxID, State: o.state, Message: o.message})
}

func samePart(a, b []peer.Part) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func text(m *peer.Message) string {
	if m == nil {
		return ""
	}
	var s string
	for _, p := range m.Parts {
		s += p.Text
	}
	return s
}

// follow reads a stream to the end of the round; a stream that breaks first is checked with GetTask and subscribed to again, at most three times.
func (t *tracker) follow(ctx context.Context, fr frames) {
	for broken := 0; ; broken++ {
		err := t.read(ctx, fr)
		fr.close()
		switch {
		case t.stopped || t.ended:
			return
		case ctx.Err() != nil:
			t.fail(ctx.Err())
			return
		case err != nil && !errors.Is(err, io.EOF) && !retryable(ctx, err):
			t.fail(err)
			return
		case !t.first:
			t.fail(errors.New("a2a: the stream closed before the first result; the peer may have the message, so it is not sent again"))
			return
		case broken == maxReconnects:
			t.fail(fmt.Errorf("a2a: the stream broke %d times", broken+1))
			return
		}
		if !sleep(ctx, time.Second<<broken) {
			t.fail(ctx.Err())
			return
		}
		snap, err := t.get(ctx)
		if err != nil {
			t.fail(err)
			return
		}
		if !t.take(snap) || t.ended {
			return
		}
		if !t.r.card.Streaming {
			t.poll(ctx)
			return
		}
		if fr, err = t.r.wire.stream(ctx, opSubscribe, t.taskID, t.r.codec.task(t.taskID, -1, t.r.rpcTenant())); err != nil {
			if !retryable(ctx, err) {
				t.fail(err)
				return
			}
			fr = &oneFrame{done: true}
		}
	}
}

type frame struct {
	raw json.RawMessage
	err error
}

// read takes events until the round ends or the stream does; a quiet stream is checked with GetTask every five minutes, against a half-open connection.
func (t *tracker) read(ctx context.Context, fr frames) error {
	ch := make(chan frame)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			raw, err := fr.next()
			select {
			case ch <- frame{raw, err}:
			case <-done:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	idle := time.NewTimer(idleCheck)
	defer idle.Stop()
	for {
		select {
		case f := <-ch:
			if f.err != nil {
				return f.err
			}
			o, err := t.r.codec.decode(f.raw)
			if err != nil {
				return err
			}
			if !t.take(o) || t.ended {
				return nil
			}
			if o.final {
				return io.EOF // v0.3 ended the interaction without a state that ends the round, so GetTask says where it is
			}
			idle.Reset(idleCheck)
		case <-idle.C:
			if snap, err := t.get(ctx); err == nil && (!t.take(snap) || t.ended) {
				return nil
			}
			idle.Reset(idleCheck)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// poll asks GetTask from one second, doubling to thirty, until the round ends.
func (t *tracker) poll(ctx context.Context) {
	for d := pollStart; !t.ended && !t.stopped; d = min(2*d, pollMax) {
		if !sleep(ctx, d) {
			t.fail(ctx.Err())
			return
		}
		snap, err := t.get(ctx)
		if err != nil {
			t.fail(err)
			return
		}
		if !t.take(snap) {
			return
		}
	}
}

// get is GetTask, tried again after a network or server failure up to three times.
func (t *tracker) get(ctx context.Context) (object, error) {
	for try := 0; ; try++ {
		raw, err := t.r.wire.unary(ctx, opGet, t.taskID, t.r.codec.task(t.taskID, 0, t.r.rpcTenant()))
		if err == nil {
			o, err := t.r.codec.decode(raw)
			if err == nil && o.kind != objTask {
				err = errUndecodable
			}
			return o, err
		}
		if try == maxReconnects || !retryable(ctx, err) {
			return object{}, err
		}
		if !sleep(ctx, time.Second<<try) {
			return object{}, ctx.Err()
		}
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-tm.C:
		return true
	case <-ctx.Done():
		return false
	}
}
