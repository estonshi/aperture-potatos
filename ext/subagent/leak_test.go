package subagent_test

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/estonshi/aperture-potatos/ext/subagent"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
	"github.com/estonshi/aperture-potatos/store"
)

func callReply(id, name, input string) []providertest.Step {
	u := message.Usage{InputTokens: 10, OutputTokens: 2}
	b := &message.ToolCallBlock{ID: id, Name: name, Input: json.RawMessage(input)}
	m := &message.Message{Role: message.RoleAssistant, Origin: message.Origin{Kind: message.OriginModel}, Content: message.Content{b},
		Producer: &message.Producer{API: string(provider.APIAnthropicMessages), Endpoint: "script.invalid", Model: "m"}, Usage: &u, StopReason: string(provider.StopToolUse)}
	return []providertest.Step{
		{Event: provider.StreamEvent{Kind: provider.SEStart, ResponseID: "resp"}},
		{Event: provider.StreamEvent{Kind: provider.SEBlockStart, Index: 0, BlockType: message.TypeToolCall, ToolCallID: id, ToolName: name}},
		{Event: provider.StreamEvent{Kind: provider.SEToolArgsDelta, Index: 0, ToolCallID: id, Text: input}},
		{Event: provider.StreamEvent{Kind: provider.SEBlockStop, Index: 0, Block: b}},
		{Event: provider.StreamEvent{Kind: provider.SEEnd, Message: m, Stop: &provider.Stop{Reason: provider.StopToolUse, Raw: "tool_use"}, Usage: &u}},
	}
}

// hangTool runs until its call is canceled.
type hangTool struct{ entered chan struct{} }

func (h *hangTool) Spec() kernel.ToolSpec {
	return kernel.ToolSpec{Name: "hang", Description: "Wait.", InputSchema: json.RawMessage(`{"type":"object"}`), Concurrency: kernel.Serial}
}

func (h *hangTool) Call(ctx context.Context, _ *kernel.ToolContext, _ json.RawMessage) (kernel.ToolResult, error) {
	h.entered <- struct{}{}
	<-ctx.Done()
	return kernel.ToolResult{}, ctx.Err()
}

// moduleFrames lists goroutines other than the caller's that run code of this module; a closed agent must leave none.
func moduleFrames() []string {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	var out []string
	for i, g := range strings.Split(string(buf[:n]), "\n\n") {
		if i > 0 && strings.Contains(g, "aperture-potatos/") && !strings.Contains(g, "testing.tRunner") {
			out = append(out, g)
		}
	}
	return out
}

// TestRealTimeLeavesNothingBehind runs a foreground subagent, stops a background one inside a tool call and continues it, all in real time, so the leak check after Close is the runtime's own view.
func TestRealTimeLeavesNothingBehind(t *testing.T) {
	ctx := t.Context()
	childModel := provider.ModelRef{Provider: providertest.NewScriptProvider(provider.APIAnthropicMessages, reply("A"), callReply("c1", "hang", `{}`), reply("resumed")), Model: "m"}
	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages,
		callReply("toolu_1", subagent.Name, `{"agent_type":"general","description":"a","prompt":"a"}`), reply("got A"),
		callReply("toolu_2", subagent.Name, `{"agent_type":"general","description":"b","prompt":"b","background":true}`), reply("started"))
	hang := &hangTool{entered: make(chan struct{}, 1)}
	var kids children
	ag := newAgent(t, store.NewMemory(), sp, kernel.WithTool(hang), kids.hook(),
		kernel.Use(subagent.New(subagent.Options{Defs: []subagent.Def{{Name: "general", Description: "Does anything.", Model: &childModel}}})))
	sess, err := ag.NewSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"foreground", "background"} {
		rc, err := sess.Send(kernel.Text(q))
		if err != nil {
			t.Fatal(err)
		}
		if res, err := rc.Wait(ctx); err != nil || res.Outcome != kernel.OutcomeCompleted {
			t.Fatalf("%s turn: %+v %v", q, res, err)
		}
	}
	<-hang.entered
	bg := "task_02"
	if err := sess.Tasks().Stop(ctx, bg, "enough"); err != nil {
		t.Fatal(err)
	}
	if got, _ := sess.Tasks().Wait(ctx, bg); got.Status != kernel.TaskCanceled {
		t.Fatalf("stopped task %s", got.Status)
	}
	if err := sess.Tasks().Send(ctx, bg, kernel.TaskInstruction{Text: "go on"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for got, _ := sess.Tasks().Get(bg); got.Attempt < 2 || got.Status == kernel.TaskRunning; got, _ = sess.Tasks().Get(bg) {
		if time.Now().After(deadline) {
			t.Fatalf("continuation %s at attempt %d", got.Status, got.Attempt)
		}
		time.Sleep(time.Millisecond)
	}
	if got, _ := sess.Tasks().Get(bg); got.Status != kernel.TaskSucceeded || got.Result.Summary != "resumed" {
		t.Fatalf("continuation %+v", got)
	}
	if err := ag.Close(ctx); err != nil {
		t.Fatal(err)
	}
	kids.mu.Lock()
	seen := kids.seen
	kids.mu.Unlock()
	if len(seen) != 3 { // the foreground child, and the background one before and after it was resumed
		t.Fatalf("%d child sessions", len(seen))
	}
	for _, s := range append(seen, sess) {
		if sn := s.Snapshot(); sn.Phase != "closed" || len(sn.Workers) != 0 {
			t.Fatalf("%s %s with workers %v", s.Info().SessionID, sn.Phase, sn.Workers)
		}
	}
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) { // a pump may still be delivering its last event
		left := moduleFrames()
		if len(left) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines left:\n%s", len(left), strings.Join(left, "\n\n"))
		}
	}
}
