//go:build live

package anthropic

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
)

// Live smoke tests run against a real endpoint: ANTHROPIC_BASE_URL (a direct API or a LiteLLM proxy), ANTHROPIC_API_KEY and ANTHROPIC_MODEL.
// ANTHROPIC_THINKING_BUDGET, when set, asks for the manual thinking form that older models need.

func live(t *testing.T) (*Provider, string) {
	t.Helper()
	model := os.Getenv("ANTHROPIC_MODEL")
	if model == "" || os.Getenv("ANTHROPIC_API_KEY") == "" {
		t.Skip("set ANTHROPIC_API_KEY and ANTHROPIC_MODEL (and ANTHROPIC_BASE_URL for a gateway)")
	}
	return New(Config{APIKey: os.Getenv("ANTHROPIC_API_KEY"), BaseURL: os.Getenv("ANTHROPIC_BASE_URL")}), model
}

func run(t *testing.T, p *Provider, req *provider.Request) *message.Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
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
			if ev.ResponseID == "" {
				t.Error("no response id")
			}
		case provider.SEEnd:
			if !started {
				t.Fatal("end without start")
			}
			if ev.Usage.InputTokens == 0 || ev.Usage.OutputTokens == 0 {
				t.Errorf("usage not reported: %+v", ev.Usage)
			}
			b, _ := json.Marshal(ev.Message.Content)
			u, _ := json.Marshal(ev.Usage)
			t.Logf("stop=%s usage=%s content=%s", ev.Stop.Raw, u, b)
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

func TestLiveText(t *testing.T) {
	p, model := live(t)
	m := run(t, p, &provider.Request{Model: model, Params: provider.Params{MaxOutputTokens: 256},
		System:   []provider.SystemBlock{{Slot: "core", Text: "Answer with one word."}},
		Messages: []message.Message{message.UserText("Reply with the single word: pong")}})
	if m.Text() == "" || m.StopReason != string(provider.StopEndTurn) {
		t.Fatalf("text %q stop %s", m.Text(), m.StopReason)
	}
}

// roundTrip asks for the weather, answers the call, and checks the model finishes; with reasoning on it also proves signed thinking replays.
func roundTrip(t *testing.T, params provider.Params, wantReasoning bool) {
	p, model := live(t)
	history := []message.Message{message.UserText("What is the weather in Osaka right now? Use the tool.")}
	req := &provider.Request{Model: model, Tools: []message.ToolDef{weather}, Params: params, Messages: history}
	first := run(t, p, req)
	calls := first.ToolCalls()
	if len(calls) == 0 || first.StopReason != string(provider.StopToolUse) {
		t.Fatalf("no tool call: stop %s", first.StopReason)
	}
	if wantReasoning && !hasReasoning(first) {
		t.Fatal("reasoning was requested but the reply has no reasoning block, so replay is not exercised")
	}
	var in struct{ City string }
	if err := json.Unmarshal(calls[0].Input, &in); err != nil || in.City == "" {
		t.Fatalf("tool input %s: %v", calls[0].Input, err)
	}
	var results message.Content
	for _, c := range calls {
		results = append(results, &message.ToolResultBlock{CallID: c.ID, Name: c.Name, Outcome: message.OutcomeSuccess, Content: message.Content{&message.TextBlock{Text: "Sunny, 24°C"}}})
	}
	history = append(history, *first, message.Message{Role: message.RoleTool, Origin: message.Origin{Kind: message.OriginTool}, Content: results})
	req.Messages = history
	second := run(t, p, req)
	if second.Text() == "" {
		t.Fatal("no final answer after the tool result")
	}
}

func hasReasoning(m *message.Message) bool {
	for _, b := range m.Content {
		if _, ok := b.(*message.ReasoningBlock); ok {
			return true
		}
	}
	return false
}

func TestLiveTools(t *testing.T) {
	roundTrip(t, provider.Params{MaxOutputTokens: 1024}, false)
}

func TestLiveThinkingTools(t *testing.T) {
	params := provider.Params{MaxOutputTokens: 8192, Reasoning: provider.ReasoningParams{Mode: provider.ReasoningOn}}
	if b, err := strconv.Atoi(os.Getenv("ANTHROPIC_THINKING_BUDGET")); err == nil {
		params.Reasoning.Budget = b
	}
	roundTrip(t, params, true)
}

var webSearch = message.HostedTool{Name: "web_search", WebSearch: &message.WebSearch{MaxUses: 3}}

func hosted(m *message.Message) (calls []*message.HostedCallBlock, results []*message.HostedResultBlock) {
	for _, b := range m.Content {
		switch v := b.(type) {
		case *message.HostedCallBlock:
			calls = append(calls, v)
		case *message.HostedResultBlock:
			results = append(results, v)
		}
	}
	return calls, results
}

// TestLiveWebSearch searches, then sends the reply back with a follow-up, which the API accepts only if every hosted block replays verbatim.
func TestLiveWebSearch(t *testing.T) {
	p, model := live(t)
	history := []message.Message{message.UserText("Search the web for the current population of Osaka city and answer in one line.")}
	req := &provider.Request{Model: model, HostedTools: []message.HostedTool{webSearch}, Params: provider.Params{MaxOutputTokens: 2048}, Messages: history}
	first := run(t, p, req)
	calls, results := hosted(first)
	if len(calls) == 0 || len(results) == 0 || len(results[0].Sources) == 0 && results[0].Error == "" {
		t.Fatalf("no search: %d calls, %d results", len(calls), len(results))
	}
	if first.Usage.HostedCalls["web_search"] == 0 {
		t.Errorf("hosted_calls not counted: %+v", first.Usage.HostedCalls)
	}
	req.Messages = append(history, *first, message.UserText("And Kyoto city? Search again, one line."))
	second := run(t, p, req)
	if second.Text() == "" {
		t.Fatal("no answer after replaying the first search")
	}
}

// TestLiveWebSearchMixed asks for a search and a client tool together; whether the API defers the search or runs it at once, a reply to the tool results alone must finish.
func TestLiveWebSearchMixed(t *testing.T) {
	p, model := live(t)
	history := []message.Message{message.UserText("In parallel: search the web for Osaka's population, and get the weather in Osaka with the tool. Call both in one reply.")}
	req := &provider.Request{Model: model, Tools: []message.ToolDef{weather}, HostedTools: []message.HostedTool{webSearch}, Params: provider.Params{MaxOutputTokens: 2048}, Messages: history}
	first := run(t, p, req)
	calls := first.ToolCalls()
	if len(calls) == 0 {
		t.Skip("the model did not call the client tool in the same reply")
	}
	hc, hr := hosted(first)
	t.Logf("first reply: %d hosted calls, %d hosted results (fewer results means the API deferred the search)", len(hc), len(hr))
	var results message.Content
	for _, c := range calls {
		results = append(results, &message.ToolResultBlock{CallID: c.ID, Name: c.Name, Outcome: message.OutcomeSuccess, Content: message.Content{&message.TextBlock{Text: "Sunny, 24°C"}}})
	}
	req.Messages = append(history, *first, message.Message{Role: message.RoleTool, Origin: message.Origin{Kind: message.OriginTool}, Content: results})
	second := run(t, p, req)
	_, late := hosted(second)
	t.Logf("second reply: %d hosted results", len(late))
	if len(hr) < len(hc) && len(late) == 0 {
		t.Error("a deferred search never returned its result")
	}
	if second.Text() == "" {
		t.Fatal("no final answer")
	}
}

// searchable is a catalog the neutral tool search has to look through; only get_weather fits the question asked.
var searchable = []message.ToolDef{
	{Name: "get_weather", Description: "Get the current weather for a city.", InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`), Deferred: true},
	{Name: "create_issue", Description: "Open an issue in the bug tracker.", InputSchema: json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}`), Deferred: true},
	{Name: "send_email", Description: "Send an email to a colleague.", InputSchema: json.RawMessage(`{"type":"object","properties":{"to":{"type":"string"},"body":{"type":"string"}},"required":["to","body"]}`), Deferred: true},
	{Name: "convert_currency", Description: "Convert an amount between currencies.", InputSchema: json.RawMessage(`{"type":"object","properties":{"amount":{"type":"number"},"from":{"type":"string"},"to":{"type":"string"}},"required":["amount","from","to"]}`), Deferred: true},
}

var toolSearch = message.HostedTool{Name: "tool_search", ToolSearch: &message.ToolSearch{}}

// TestLiveToolSearch: the model searches the deferred catalog, calls what it found, and the search replays with the tool result.
func TestLiveToolSearch(t *testing.T) {
	p, model := live(t)
	history := []message.Message{message.UserText("What is the weather in Osaka right now? Find a suitable tool and use it.")}
	req := &provider.Request{Model: model, Tools: searchable, HostedTools: []message.HostedTool{toolSearch}, Params: provider.Params{MaxOutputTokens: 2048}, Messages: history}
	first := run(t, p, req)
	calls, results := hosted(first)
	for _, r := range results {
		t.Logf("search loaded %v (error %q)", r.Tools, r.Error)
	}
	tc := first.ToolCalls()
	if len(calls) == 0 || len(results) == 0 || len(tc) == 0 {
		t.Fatalf("no search and call: %d searches, %d results, %d tool calls", len(calls), len(results), len(tc))
	}
	var out message.Content
	for _, c := range tc {
		out = append(out, &message.ToolResultBlock{CallID: c.ID, Name: c.Name, Outcome: message.OutcomeSuccess, Content: message.Content{&message.TextBlock{Text: "Sunny, 24°C"}}})
	}
	req.Messages = append(history, *first, message.Message{Role: message.RoleTool, Origin: message.Origin{Kind: message.OriginTool}, Content: out})
	second := run(t, p, req)
	if second.Text() == "" {
		t.Fatal("no answer after replaying the search")
	}
	req.Tools = searchable[1:] // the server dropped the tool the search loaded; the replayed reference gets a stand-in
	req.Messages = append(req.Messages, *second, message.UserText("Thanks. Reply with one word."))
	if third := run(t, p, req); third.Text() == "" {
		t.Fatal("no answer once the loaded tool is gone")
	}
}
