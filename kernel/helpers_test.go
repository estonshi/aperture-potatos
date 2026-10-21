package kernel_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/internal/determ"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
	"github.com/estonshi/aperture-potatos/store"
)

var usage = message.Usage{InputTokens: 100, OutputTokens: 10}

func producer() *message.Producer {
	return &message.Producer{API: string(provider.APIAnthropicMessages), Endpoint: "script.invalid", Model: "m"}
}

// textReply scripts one attempt that answers with text.
func textReply(text string) []providertest.Step {
	u := usage
	m := &message.Message{Role: message.RoleAssistant, Origin: message.Origin{Kind: message.OriginModel}, Content: message.Content{&message.TextBlock{Text: text}}, Producer: producer(), Usage: &u, StopReason: string(provider.StopEndTurn)}
	return []providertest.Step{
		{Event: provider.StreamEvent{Kind: provider.SEStart, ResponseID: "resp"}},
		{Event: provider.StreamEvent{Kind: provider.SEBlockStart, Index: 0, BlockType: message.TypeText}},
		{Event: provider.StreamEvent{Kind: provider.SETextDelta, Index: 0, Channel: "text", Text: text}},
		{Event: provider.StreamEvent{Kind: provider.SEBlockStop, Index: 0, Block: &message.TextBlock{Text: text}}},
		{Event: provider.StreamEvent{Kind: provider.SEEnd, Message: m, Stop: &provider.Stop{Reason: provider.StopEndTurn, Raw: "end_turn"}, Usage: &u}},
	}
}

type call struct {
	id, name, input string
}

// toolReply scripts one attempt that asks for the given calls.
func toolReply(calls ...call) []providertest.Step {
	u := usage
	steps := []providertest.Step{{Event: provider.StreamEvent{Kind: provider.SEStart, ResponseID: "resp"}}}
	m := &message.Message{Role: message.RoleAssistant, Origin: message.Origin{Kind: message.OriginModel}, Producer: producer(), Usage: &u, StopReason: string(provider.StopToolUse)}
	for i, c := range calls {
		b := &message.ToolCallBlock{ID: c.id, Name: c.name, Input: json.RawMessage(c.input)}
		m.Content = append(m.Content, b)
		steps = append(steps,
			providertest.Step{Event: provider.StreamEvent{Kind: provider.SEBlockStart, Index: i, BlockType: message.TypeToolCall, ToolCallID: c.id, ToolName: c.name}},
			providertest.Step{Event: provider.StreamEvent{Kind: provider.SEToolArgsDelta, Index: i, ToolCallID: c.id, Text: c.input}},
			providertest.Step{Event: provider.StreamEvent{Kind: provider.SEBlockStop, Index: i, Block: b}},
		)
	}
	return append(steps, providertest.Step{Event: provider.StreamEvent{Kind: provider.SEEnd, Message: m, Stop: &provider.Stop{Reason: provider.StopToolUse, Raw: "tool_use"}, Usage: &u}})
}

// recorder collects every event of a session through a lossless handler that never blocks.
type recorder struct {
	mu     sync.Mutex
	events []event.Event
}

func (r *recorder) handle(_ context.Context, ev event.Event) error {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
	return nil
}

func (r *recorder) all() []event.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]event.Event(nil), r.events...)
}

func (r *recorder) types() []string {
	var out []string
	for _, ev := range r.all() {
		out = append(out, string(ev.Type))
	}
	return out
}

func (r *recorder) count(typ event.Type) int {
	n := 0
	for _, ev := range r.all() {
		if ev.Type == typ {
			n++
		}
	}
	return n
}

// echoTool returns its input as text and counts its runs per call id.
type echoTool struct {
	name  string
	delay time.Duration
	mu    sync.Mutex
	runs  map[string]int
}

func (e *echoTool) Spec() kernel.ToolSpec {
	return kernel.ToolSpec{Name: e.name, Description: "Echo the input.", InputSchema: json.RawMessage(`{"type":"object"}`), Concurrency: kernel.Serial}
}

func (e *echoTool) Call(ctx context.Context, tc *kernel.ToolContext, input json.RawMessage) (kernel.ToolResult, error) {
	e.mu.Lock()
	if e.runs == nil {
		e.runs = map[string]int{}
	}
	e.runs[tc.CallID()]++
	e.mu.Unlock()
	if e.delay > 0 {
		select {
		case <-time.After(e.delay):
		case <-ctx.Done():
			return kernel.ToolResult{}, ctx.Err()
		}
	}
	return kernel.TextResult(string(input)), nil
}

func (e *echoTool) runsOf(id string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.runs[id]
}

type harness struct {
	t     *testing.T
	sp    *providertest.ScriptProvider
	store store.SessionStore
	ag    *kernel.Agent
	sess  *kernel.Session
	rec   *recorder
}

func newHarness(t *testing.T, scripts [][]providertest.Step, opts ...kernel.Option) *harness {
	t.Helper()
	h := &harness{t: t, sp: providertest.NewScriptProvider(provider.APIAnthropicMessages, scripts...), store: store.NewMemory(), rec: &recorder{}}
	base := []kernel.Option{
		kernel.WithNickname("Tater"),
		kernel.WithModel(provider.ModelRef{Provider: h.sp, Model: "m"}),
		kernel.WithStore(h.store),
		kernel.WithClock(determ.NewStepClock(time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC), time.Millisecond)),
		kernel.WithIDGenerator(determ.NewSequentialIDs()),
		kernel.WithPanicOnInternalError(true),
	}
	ag, err := kernel.New(append(base, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	h.ag = ag
	sess, err := ag.NewSession(t.Context(), kernel.WithSessionFunc(h.rec.handle))
	if err != nil {
		t.Fatal(err)
	}
	h.sess = sess
	return h
}

// close waits for the recorder too, because Close never waits for a caller's pump.
func (h *harness) close() {
	h.t.Helper()
	if err := h.ag.Close(context.Background()); err != nil {
		h.t.Fatal(err)
	}
	synctest.Wait()
	if w := h.sess.Snapshot().Workers; len(w) != 0 {
		h.t.Fatalf("workers left after close: %v", w) // every goroutine the session started has an owner and has exited (I7)
	}
}

func (h *harness) lines() []message.Message {
	h.t.Helper()
	msgs, err := h.store.Load(context.Background(), h.sess.Info().SessionID)
	if err != nil {
		h.t.Fatal(err)
	}
	return msgs
}

func roles(msgs []message.Message) string {
	var out []string
	for _, m := range msgs {
		out = append(out, string(m.Role))
	}
	return strings.Join(out, ",")
}

// pairedHistory reports the first tool call without exactly one result in the next tool line (I1).
func pairedHistory(msgs []message.Message) error {
	for i, m := range msgs {
		calls := m.ToolCalls()
		if m.Role != message.RoleAssistant || len(calls) == 0 {
			continue
		}
		if i+1 >= len(msgs) || msgs[i+1].Role != message.RoleTool {
			return fmt.Errorf("line %d: calls without a tool line", m.Seq)
		}
		got := map[string]int{}
		for _, b := range msgs[i+1].Content {
			if r, ok := b.(*message.ToolResultBlock); ok {
				got[r.CallID]++
			}
		}
		for _, c := range calls {
			if got[c.ID] != 1 {
				return fmt.Errorf("line %d: call %s has %d results", m.Seq, c.ID, got[c.ID])
			}
		}
	}
	return nil
}

func command(name string, busy kernel.BusyPolicy, text string) kernel.Command {
	return kernel.Command{Name: name, Busy: busy, Run: func(context.Context, *kernel.CommandContext) (kernel.CommandResult, error) {
		return kernel.CommandResult{Text: text}, nil
	}}
}

// resumeAgent opens an agent on st whose model answers scripts in order.
func resumeAgent(t *testing.T, st store.SessionStore, scripts ...[]providertest.Step) *kernel.Agent {
	t.Helper()
	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, scripts...)
	ag, err := kernel.New(
		kernel.WithNickname("Tater"),
		kernel.WithModel(provider.ModelRef{Provider: sp, Model: "m"}),
		kernel.WithStore(st),
		kernel.WithClock(determ.NewStepClock(time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC), time.Millisecond)),
		kernel.WithIDGenerator(determ.NewSequentialIDs()),
		kernel.WithPanicOnInternalError(true),
		kernel.WithTool(&echoTool{name: "echo"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return ag
}

// keepTurnCompactor summarizes with the model and keeps the running turn, like the default strategy.
type keepTurnCompactor struct{ calls int }

func (c *keepTurnCompactor) Compact(ctx context.Context, req kernel.CompactRequest) (kernel.CompactResult, error) {
	c.calls++
	sum, _, err := req.Summarize(ctx, nil, "Summarize the conversation.")
	if err != nil {
		return kernel.CompactResult{}, err
	}
	var keep []message.Message
	if req.CurrentTurn >= 0 {
		keep = req.History[req.CurrentTurn:]
	}
	return kernel.CompactResult{Summary: sum, Replace: keep, Strategy: "test"}, nil
}

func ask(t *testing.T, s *kernel.Session, text string) kernel.TurnResult {
	t.Helper()
	r, err := s.Send(kernel.Input{Text: text})
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Wait(context.Background())
	if err != nil || res.Err != nil {
		t.Fatalf("turn %q: %v %v", text, err, res.Err)
	}
	return res
}

// wire is a request as the provider sees it, without what changes per call.
func wire(t *testing.T, r *provider.Request) string {
	t.Helper()
	b, err := json.Marshal(struct {
		System   []provider.SystemBlock
		Tools    []message.ToolDef
		Messages []message.Message
	}{r.System, r.Tools, nil})
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + messagesJSON(t, r.Messages)
}

// text is what a content's text blocks say.
func text(c message.Content) string { return message.Message{Content: c}.Text() }

// messagesJSON leaves out the line identity, which no adapter sends.
func messagesJSON(t *testing.T, msgs []message.Message) string {
	t.Helper()
	out := make([]message.Message, len(msgs))
	for i, m := range msgs {
		m.ID, m.SessionID, m.Seq, m.Epoch, m.TurnID, m.CopyOf, m.CreatedAt = "", "", 0, 0, "", "", time.Time{}
		out[i] = m
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func messagesJSONAny(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
