//go:build live

package openaichat

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
)

// Live smoke tests run against a real endpoint: OPENAI_BASE_URL (with its version path), OPENAI_API_KEY and OPENAI_MODEL.
// OPENAI_REASONING_EXTRA, a JSON object, is the vendor switch that turns reasoning on, merged into the body through Params.Extra.

func live(t *testing.T) (*Provider, string) {
	t.Helper()
	model := os.Getenv("OPENAI_MODEL")
	if model == "" || os.Getenv("OPENAI_API_KEY") == "" {
		t.Skip("set OPENAI_API_KEY and OPENAI_MODEL (and OPENAI_BASE_URL for a gateway)")
	}
	return New(Config{APIKey: os.Getenv("OPENAI_API_KEY"), BaseURL: os.Getenv("OPENAI_BASE_URL")}), model
}

func run(t *testing.T, p *Provider, req *provider.Request) *message.Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var started bool
	for ev, err := range p.Stream(ctx, req) {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		switch ev.Kind {
		case provider.SENotice:
			t.Logf("notice %s %s", ev.Notice.Kind, ev.Notice.Detail)
		case provider.SEStart:
			started = true
			t.Logf("response_id=%s request_id=%s", ev.ResponseID, ev.RequestID)
		case provider.SEEnd:
			if !started {
				t.Fatal("end without start")
			}
			if ev.Usage.Estimated {
				t.Errorf("usage not reported: %+v", ev.Usage)
			}
			b, _ := json.Marshal(ev.Message.Content)
			u, _ := json.Marshal(ev.Usage)
			n, _ := json.Marshal(ev.Message.Native)
			t.Logf("stop=%s usage=%s content=%s native=%s", ev.Stop.Raw, u, b, n)
			return ev.Message
		}
	}
	t.Fatal("stream ended without an end event")
	return nil
}

var weather = message.ToolDef{
	Name:        "get_weather",
	Description: "Get the current weather for a city.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}`),
}

// nonce keeps a gateway that caches identical requests from answering from its cache.
func nonce() string { return " (request " + time.Now().Format(time.RFC3339Nano) + ")" }

func TestLiveText(t *testing.T) {
	p, model := live(t)
	m := run(t, p, &provider.Request{Model: model, Params: provider.Params{MaxOutputTokens: 256},
		System:   []provider.SystemBlock{{Slot: "core", Text: "Answer with one word."}},
		Messages: []message.Message{message.UserText("Reply with the single word: pong" + nonce())}})
	if m.Text() == "" || m.StopReason != string(provider.StopEndTurn) {
		t.Fatalf("text %q stop %s", m.Text(), m.StopReason)
	}
}

// roundTrip asks for the weather, answers the call, and checks the model finishes; with reasoning on the second request replays it.
func roundTrip(t *testing.T, params provider.Params, wantReasoning bool) {
	p, model := live(t)
	history := []message.Message{message.UserText("What is the weather in Osaka right now? Use the tool." + nonce())}
	req := &provider.Request{Model: model, Tools: []message.ToolDef{weather}, Params: params, Messages: history}
	first := run(t, p, req)
	calls := first.ToolCalls()
	if len(calls) == 0 || first.StopReason != string(provider.StopToolUse) {
		t.Fatalf("no tool call: stop %s", first.StopReason)
	}
	var reasoning *message.ReasoningBlock
	for _, b := range first.Content {
		if r, ok := b.(*message.ReasoningBlock); ok {
			reasoning = r
		}
	}
	if wantReasoning && (reasoning == nil || reasoning.Text == "" || reasoning.Native == nil) {
		t.Fatalf("reasoning was requested but no replayable reasoning arrived: %+v", reasoning)
	}
	var results message.Content
	for _, c := range calls {
		results = append(results, &message.ToolResultBlock{CallID: c.ID, Name: c.Name, Outcome: message.OutcomeSuccess, Content: message.Content{&message.TextBlock{Text: "Sunny, 24°C"}}})
	}
	history = append(history, *first, message.Message{Role: message.RoleTool, Origin: message.Origin{Kind: message.OriginTool}, Content: results})
	req.Messages = history
	if wantReasoning {
		r, _ := p.render(req)
		var body struct {
			Messages []map[string]json.RawMessage `json:"messages"`
		}
		_ = json.Unmarshal(r.body, &body)
		if _, ok := body.Messages[1]["reasoning_content"]; !ok {
			t.Fatalf("the replayed assistant message lost its reasoning: %s", r.body)
		}
	}
	second := run(t, p, req)
	if second.Text() == "" {
		t.Fatal("no final answer after the tool result")
	}
}

func TestLiveTools(t *testing.T) {
	roundTrip(t, provider.Params{MaxOutputTokens: 1024}, false)
}

func TestLiveReasoningToolsReplay(t *testing.T) {
	extra := os.Getenv("OPENAI_REASONING_EXTRA")
	if extra == "" {
		t.Skip("set OPENAI_REASONING_EXTRA to the endpoint's reasoning switch")
	}
	params := provider.Params{MaxOutputTokens: 8192}
	if err := json.Unmarshal([]byte(extra), &params.Extra); err != nil {
		t.Fatal(err)
	}
	roundTrip(t, params, true)
}

func TestLiveModels(t *testing.T) {
	p, model := live(t)
	c, err := p.ResolveCapabilities(context.Background(), provider.ModelInfo{API: api, Endpoint: p.Endpoint(), Model: model})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: context_window=%d max_output_tokens=%d", model, c.ContextWindow, c.MaxOutputTokens)
}

// TestLiveExplicitCache: with explicit caching declared, the second request over the same long prefix reads what the first one wrote.
func TestLiveExplicitCache(t *testing.T) {
	p, model := live(t)
	long := ""
	for i := range 140 {
		long += "Entry " + time.Duration(i).String() + ": the archive of the northern observatory records each night's sky conditions, instruments and observers. "
	}
	nonce := time.Now().Format(time.RFC3339Nano)
	var usages []*message.Usage
	for _, q := range []string{"Reply with the word one.", "Reply with the word two."} {
		req := &provider.Request{Model: model, System: []provider.SystemBlock{{Slot: "core", Text: "Reference " + nonce + ".\n" + long}},
			Messages: []message.Message{{ID: "msg_" + q[len(q)-4:len(q)-1], Role: message.RoleUser, Content: message.Content{&message.TextBlock{Text: q}}}},
			Params:   provider.Params{MaxOutputTokens: 16}, Caps: provider.Capabilities{PromptCache: provider.CacheCaps{Explicit: true}}}
		usages = append(usages, run(t, p, req).Usage)
		time.Sleep(3 * time.Second)
	}
	if usages[0].CacheWriteTokens == 0 || usages[1].CacheReadTokens == 0 {
		t.Fatalf("first wrote %d, second read %d", usages[0].CacheWriteTokens, usages[1].CacheReadTokens)
	}
}
