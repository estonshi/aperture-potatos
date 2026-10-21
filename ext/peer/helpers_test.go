package peer_test

import (
	"context"
	"encoding/json"
	"iter"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/ext/peer"
	"github.com/estonshi/aperture-potatos/internal/determ"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
	"github.com/estonshi/aperture-potatos/store"
)

// round is what a fake peer does with one message: its events, after an optional gate the test opens, and an optional hold until canceled.
type round struct {
	events []peer.Event
	gate   chan struct{} // the events after the first wait for it
	start  time.Duration // the first event waits this long
	delay  time.Duration // the events after the first wait this long
	hold   bool          // after the events, wait for a cancel and then report canceled
	err    error         // ends the round instead of the events
}

// fill puts what the peer received into its replies, so a case's expected files show the message, context and task it was sent.
func fill(ev peer.Event, msg peer.Message) peer.Event {
	r := strings.NewReplacer("{{text}}", msgText(msg), "{{context_id}}", msg.ContextID, "{{task_id}}", msg.TaskID)
	fix := func(parts []peer.Part) []peer.Part {
		out := append([]peer.Part(nil), parts...)
		for i := range out {
			out[i].Text = r.Replace(out[i].Text)
		}
		return out
	}
	if ev.Artifact != nil {
		a := *ev.Artifact
		a.Parts = fix(a.Parts)
		ev.Artifact = &a
	}
	if ev.Message != nil {
		m := *ev.Message
		m.Parts = fix(m.Parts)
		ev.Message = &m
	}
	return ev
}

// fake is a scripted peer that records what it was sent.
type fake struct {
	card   peer.Card
	mu     sync.Mutex
	rounds []round
	sent   []peer.Message
	cancel []string
	subs   map[string][]peer.Event
	subErr error       // the next Subscribe fails with it, once
	stop   chan string // a cancel's task id reaches the round that holds
}

func newFake(name string, rounds ...round) *fake {
	return &fake{card: peer.Card{Name: name, Description: "Translates text.", Streaming: true}, rounds: rounds, subs: map[string][]peer.Event{}, stop: make(chan string, 4)}
}

func (f *fake) Card() peer.Card { return f.card }

func (f *fake) Send(ctx context.Context, msg peer.Message, _ peer.SendOptions) iter.Seq2[peer.Event, error] {
	f.mu.Lock()
	f.sent = append(f.sent, msg)
	var r round
	if len(f.rounds) > 0 {
		r, f.rounds = f.rounds[0], f.rounds[1:]
	}
	f.mu.Unlock()
	return func(yield func(peer.Event, error) bool) {
		if r.err != nil {
			yield(peer.Event{}, r.err)
			return
		}
		if r.start > 0 && !sleepCtx(ctx, r.start) {
			yield(peer.Event{}, ctx.Err())
			return
		}
		for i, ev := range r.events {
			if i == 1 && r.gate != nil {
				select {
				case <-r.gate:
				case <-ctx.Done():
					yield(peer.Event{}, ctx.Err())
					return
				}
			}
			if i == 1 && r.delay > 0 {
				tm := time.NewTimer(r.delay)
				select {
				case <-tm.C:
				case id := <-f.stop:
					tm.Stop()
					yield(peer.Event{Kind: peer.EventStatus, TaskID: id, State: peer.StateCanceled}, nil)
					return
				case <-ctx.Done():
					tm.Stop()
					yield(peer.Event{}, ctx.Err())
					return
				}
			}
			if !yield(fill(ev, msg), nil) {
				return
			}
		}
		if !r.hold {
			return
		}
		select {
		case id := <-f.stop:
			yield(peer.Event{Kind: peer.EventStatus, TaskID: id, State: peer.StateCanceled}, nil)
		case <-ctx.Done():
			yield(peer.Event{}, ctx.Err())
		}
	}
}

func (f *fake) Subscribe(_ context.Context, taskID string) iter.Seq2[peer.Event, error] {
	f.mu.Lock()
	evs, ok := f.subs[taskID]
	fail := f.subErr
	f.subErr = nil
	f.mu.Unlock()
	return func(yield func(peer.Event, error) bool) {
		if fail != nil {
			yield(peer.Event{}, fail)
			return
		}
		if !ok {
			yield(peer.Event{}, peer.ErrUnknownTask)
			return
		}
		for _, ev := range evs {
			if !yield(ev, nil) {
				return
			}
		}
	}
}

func (f *fake) Cancel(_ context.Context, taskID string) error {
	f.mu.Lock()
	f.cancel = append(f.cancel, taskID)
	f.mu.Unlock()
	select {
	case f.stop <- taskID:
	default:
	}
	return nil
}

func (f *fake) messages() []peer.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]peer.Message(nil), f.sent...)
}

func (f *fake) canceled() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cancel...)
}

func msgText(m peer.Message) string {
	s := ""
	for _, p := range m.Parts {
		s += p.Text
	}
	return s
}

func agentMsg(text string) *peer.Message {
	return &peer.Message{Role: "agent", Parts: []peer.Part{{Text: text}}}
}

// completed is a whole exchange: the task, its reply in two chunks, and its end.
func completed(taskID, ctxID, a, b string) round {
	return round{events: []peer.Event{
		{Kind: peer.EventTask, TaskID: taskID, ContextID: ctxID, State: peer.StateSubmitted},
		{Kind: peer.EventArtifact, TaskID: taskID, ContextID: ctxID, Artifact: &peer.Artifact{ID: "a1", Parts: []peer.Part{{Text: a}}}},
		{Kind: peer.EventArtifact, TaskID: taskID, ContextID: ctxID, Artifact: &peer.Artifact{ID: "a1", Parts: []peer.Part{{Text: b}}}, Append: true, LastChunk: true},
		{Kind: peer.EventStatus, TaskID: taskID, ContextID: ctxID, State: peer.StateCompleted},
	}}
}

func reply(text string) []providertest.Step {
	u := message.Usage{InputTokens: 10, OutputTokens: 2}
	m := &message.Message{Role: message.RoleAssistant, Origin: message.Origin{Kind: message.OriginModel}, Content: message.Content{&message.TextBlock{Text: text}},
		Producer: &message.Producer{API: string(provider.APIAnthropicMessages), Endpoint: "script.invalid", Model: "m"}, Usage: &u, StopReason: string(provider.StopEndTurn)}
	return []providertest.Step{
		{Event: provider.StreamEvent{Kind: provider.SEStart, ResponseID: "resp"}},
		{Event: provider.StreamEvent{Kind: provider.SEBlockStart, Index: 0, BlockType: message.TypeText}},
		{Event: provider.StreamEvent{Kind: provider.SETextDelta, Index: 0, Channel: "text", Text: text}},
		{Event: provider.StreamEvent{Kind: provider.SEBlockStop, Index: 0, Block: &message.TextBlock{Text: text}}},
		{Event: provider.StreamEvent{Kind: provider.SEEnd, Message: m, Stop: &provider.Stop{Reason: provider.StopEndTurn, Raw: "end_turn"}, Usage: &u}},
	}
}

type call struct {
	id    string
	name  string
	input string
}

func calls(cs ...call) []providertest.Step {
	u := message.Usage{InputTokens: 10, OutputTokens: 2}
	var content message.Content
	steps := []providertest.Step{{Event: provider.StreamEvent{Kind: provider.SEStart, ResponseID: "resp"}}}
	for i, c := range cs {
		b := &message.ToolCallBlock{ID: c.id, Name: c.name, Input: json.RawMessage(c.input)}
		content = append(content, b)
		steps = append(steps,
			providertest.Step{Event: provider.StreamEvent{Kind: provider.SEBlockStart, Index: i, BlockType: message.TypeToolCall, ToolCallID: c.id, ToolName: c.name}},
			providertest.Step{Event: provider.StreamEvent{Kind: provider.SEToolArgsDelta, Index: i, ToolCallID: c.id, Text: c.input}},
			providertest.Step{Event: provider.StreamEvent{Kind: provider.SEBlockStop, Index: i, Block: b}})
	}
	m := &message.Message{Role: message.RoleAssistant, Origin: message.Origin{Kind: message.OriginModel}, Content: content,
		Producer: &message.Producer{API: string(provider.APIAnthropicMessages), Endpoint: "script.invalid", Model: "m"}, Usage: &u, StopReason: string(provider.StopToolUse)}
	return append(steps, providertest.Step{Event: provider.StreamEvent{Kind: provider.SEEnd, Message: m, Stop: &provider.Stop{Reason: provider.StopToolUse, Raw: "tool_use"}, Usage: &u}})
}

var noWake = kernel.WithHook(kernel.TaskNotify, "no_wake", func(context.Context, kernel.TaskNotifyInput) (kernel.TaskNotifyOutput, error) {
	return kernel.TaskNotifyOutput{NoWake: true}, nil
})

func newAgent(t *testing.T, name string, st store.SessionStore, sp *providertest.ScriptProvider, opts ...kernel.Option) *kernel.Agent {
	t.Helper()
	base := []kernel.Option{
		kernel.WithNickname(name),
		kernel.WithModel(provider.ModelRef{Provider: sp, Model: "m"}),
		kernel.WithStore(st),
		kernel.WithClock(determ.NewStepClock(time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), time.Millisecond)),
		kernel.WithIDGenerator(determ.NewSequentialIDs()),
		kernel.WithPanicOnInternalError(true),
	}
	ag, err := kernel.New(append(base, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return ag
}

// recorder keeps a session's events.
type recorder struct {
	mu  sync.Mutex
	evs []event.Event
}

func (r *recorder) handle(_ context.Context, ev event.Event) error {
	r.mu.Lock()
	r.evs = append(r.evs, ev)
	r.mu.Unlock()
	return nil
}

func (r *recorder) all() []event.Event {
	synctest.Wait() // the recorder's pump delivers the events asynchronously
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]event.Event(nil), r.evs...)
}

// toolResults are the texts of the results of a session's tool calls, by call id.
func toolResults(t *testing.T, s *kernel.Session) map[string]message.ToolResultBlock {
	t.Helper()
	out := map[string]message.ToolResultBlock{}
	for _, m := range s.Messages() {
		for _, b := range m.Content {
			if r, ok := b.(*message.ToolResultBlock); ok {
				out[r.CallID] = *r
			}
		}
	}
	return out
}

func resultText(r message.ToolResultBlock) string { return message.Message{Content: r.Content}.Text() }

func ask(t *testing.T, s *kernel.Session, text string) kernel.TurnResult {
	t.Helper()
	rc, err := s.Send(kernel.Text(text))
	if err != nil {
		t.Fatal(err)
	}
	res, err := rc.Wait(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// prefixed keeps a second agent's ids apart from the first's, as distinct random ids would be.
type prefixed struct {
	p string
	g *determ.SequentialIDs
}

func (x prefixed) NewID(prefix string) string { return x.p + x.g.NewID(prefix) }

func otherIDs(p string) kernel.Option {
	return kernel.WithIDGenerator(prefixed{p, determ.NewSequentialIDs()})
}

func stepClock() *determ.StepClock {
	return determ.NewStepClock(time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), time.Millisecond)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-tm.C:
		return true
	case <-ctx.Done():
		return false
	}
}
