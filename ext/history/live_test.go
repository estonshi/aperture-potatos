//go:build live

package history_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/estonshi/aperture-potatos/ext/history"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/openaichat"
)

// heldTool answers once the test lets it, so a side chat can be asked while the main session waits on it.
type heldTool struct {
	started, release chan struct{}
}

func (h *heldTool) Spec() kernel.ToolSpec {
	return kernel.ToolSpec{Name: "lookup", Description: "Look up the current weather in a city.", InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`)}
}

func (h *heldTool) Call(ctx context.Context, _ *kernel.ToolContext, _ json.RawMessage) (kernel.ToolResult, error) {
	close(h.started)
	select {
	case <-h.release:
	case <-ctx.Done():
		return kernel.ToolResult{}, ctx.Err()
	}
	return kernel.TextResult("Sunny, 24C."), nil
}

// TestLiveSideChat runs a side chat through OPENAI_BASE_URL, OPENAI_API_KEY and OPENAI_MODEL with thinking on: every turn carries the main session's current context, a call still running included, and parent search is refused.
func TestLiveSideChat(t *testing.T) {
	model := os.Getenv("OPENAI_MODEL")
	if model == "" || os.Getenv("OPENAI_API_KEY") == "" {
		t.Skip("set OPENAI_API_KEY and OPENAI_MODEL (and OPENAI_BASE_URL for a gateway)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	p := openaichat.New(openaichat.Config{APIKey: os.Getenv("OPENAI_API_KEY"), BaseURL: os.Getenv("OPENAI_BASE_URL")})
	held := &heldTool{started: make(chan struct{}), release: make(chan struct{})}
	params := provider.Params{MaxOutputTokens: 2048, Extra: map[string]any{"extra_body": map[string]any{"thinking": map[string]any{"type": "enabled"}}}}
	ag, err := kernel.New(
		kernel.WithModel(provider.ModelRef{Provider: p, Model: model, Params: params}),
		kernel.WithCapabilityResolver(nil),
		kernel.WithInstructions(fmt.Sprintf("Answer in one short sentence. (run %d)", time.Now().UnixNano())), // the gateway answers a repeated request from its cache
		kernel.WithTool(held),
		kernel.Use(history.New()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close(context.Background())
	main, err := ag.NewSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ask := func(s *kernel.Session, text string) string {
		t.Helper()
		r, err := s.Send(kernel.Input{Text: text})
		if err != nil {
			t.Fatal(err)
		}
		res, err := r.Wait(ctx)
		if err != nil || res.Err != nil {
			t.Fatalf("turn: %v %v", err, res.Err)
		}
		t.Logf("%s: %q -> %q (%d steps, stop %s, cache read %d of %d, output %d, reasoning %d)", s.Info().SessionID, text, res.FinalText, res.Steps, res.StopReason, res.Usage.CacheReadTokens, res.Usage.InputTokens, res.Usage.OutputTokens, res.Usage.ReasoningTokens)
		return res.FinalText
	}
	ask(main, "Remember this: the secret word is PERSIMMON.")
	side, err := main.SideChat(ctx, "notes", kernel.ForkTools("search_history"))
	if err != nil {
		t.Fatal(err)
	}
	ask(main, "Also remember: the meeting moved to room 42.")
	if got := ask(side, "What is the secret word, and which room did the meeting move to?"); !strings.Contains(strings.ToUpper(got), "PERSIMMON") || !strings.Contains(got, "42") {
		t.Fatalf("the side chat does not see the main session as it is now: %q", got)
	}
	rc, err := main.Send(kernel.Input{Text: "Use the lookup tool for the weather in Paris, then tell me what it said."})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-held.started:
	case <-ctx.Done():
		t.Fatal("the main session never called lookup")
	}
	if got := ask(side, "Which tool is the main conversation waiting on right now?"); !strings.Contains(strings.ToLower(got), "lookup") {
		t.Fatalf("the side chat does not see the running call: %q", got)
	}
	close(held.release)
	if res, err := rc.Wait(ctx); err != nil || res.Err != nil {
		t.Fatalf("main turn: %v %v", err, res.Err)
	}
	if got := ask(side, "What did the lookup return?"); !strings.Contains(strings.ToLower(got), "sunny") && !strings.Contains(got, "24") {
		t.Fatalf("the side chat does not see the finished call: %q", got)
	}
	ask(side, "Call search_history with parent set to true and the query \"room\", then tell me what it said.")
	if b := main.Snapshot().Usage.Buckets["side"]; b.InputTokens == 0 {
		t.Fatalf("the side chat's calls count in the main session's side bucket: %+v", main.Snapshot().Usage.Buckets)
	}
	t.Logf("side chat total: %+v", side.Snapshot().Usage.Total)
}
