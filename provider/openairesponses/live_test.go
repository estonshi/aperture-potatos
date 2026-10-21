//go:build live

package openairesponses

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
)

// Live smoke tests run against a real endpoint: OPENAI_BASE_URL (with its version path), OPENAI_API_KEY and OPENAI_MODEL.
// OPENAI_EFFORT, when set, asks for reasoning at that effort.

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
	for ev, err := range p.Stream(ctx, req) {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		switch ev.Kind {
		case provider.SENotice:
			t.Logf("notice %s %s", ev.Notice.Kind, ev.Notice.Detail)
		case provider.SEStart:
			t.Logf("response_id=%s request_id=%s", ev.ResponseID, ev.RequestID)
		case provider.SEEnd:
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

func nonce() string { return " (request " + time.Now().Format(time.RFC3339Nano) + ")" }

func params() provider.Params {
	return provider.Params{MaxOutputTokens: 4096, Reasoning: provider.ReasoningParams{Effort: os.Getenv("OPENAI_EFFORT")}}
}

func TestLiveText(t *testing.T) {
	p, model := live(t)
	m := run(t, p, &provider.Request{Model: model, Params: params(),
		System:   []provider.SystemBlock{{Slot: "core", Text: "Answer with one word."}},
		Messages: []message.Message{message.UserText("Reply with the single word: pong" + nonce())}})
	if m.Text() == "" {
		t.Fatalf("text %q stop %s", m.Text(), m.StopReason)
	}
}

func TestLiveTools(t *testing.T) {
	p, model := live(t)
	history := []message.Message{message.UserText("What is the weather in Osaka right now? Use the tool." + nonce())}
	req := &provider.Request{Model: model, Tools: []message.ToolDef{weather}, Params: params(), Messages: history}
	first := run(t, p, req)
	calls := first.ToolCalls()
	if len(calls) == 0 {
		t.Fatalf("no tool call: stop %s", first.StopReason)
	}
	var results message.Content
	for _, c := range calls {
		results = append(results, &message.ToolResultBlock{CallID: c.ID, Name: c.Name, Outcome: message.OutcomeSuccess, Content: message.Content{&message.TextBlock{Text: "Sunny, 24°C"}}})
	}
	req.Messages = append(history, *first, message.Message{Role: message.RoleTool, Origin: message.Origin{Kind: message.OriginTool}, Content: results})
	for _, b := range first.Content {
		if r, ok := b.(*message.ReasoningBlock); ok && r.Kind == message.ReasoningRaw && r.Native != nil {
			rendered, _ := p.render(req)
			if !strings.Contains(string(rendered.body), `"type":"reasoning_text"`) {
				t.Fatalf("raw reasoning from this endpoint was not replayed: %s", rendered.body)
			}
			t.Log("the second request replays the raw reasoning item")
		}
	}
	if second := run(t, p, req); second.Text() == "" {
		t.Fatal("no final answer after the tool result")
	}
}

// TestLiveWebSearch searches with the neutral web search, then replays the reply with a follow-up under store:false; OPENAI_BLOCKED_DOMAIN, when set, also sends a blocked domain.
func TestLiveWebSearch(t *testing.T) {
	p, model := live(t)
	ws := &message.WebSearch{ContextSize: "low"}
	if d := os.Getenv("OPENAI_BLOCKED_DOMAIN"); d != "" {
		ws.BlockedDomains = []string{d}
	}
	history := []message.Message{message.UserText("Search the web for the current population of Osaka city and answer in one line." + nonce())}
	req := &provider.Request{Model: model, HostedTools: []message.HostedTool{{Name: "web_search", WebSearch: ws}}, Params: params(), Messages: history}
	first := run(t, p, req)
	var calls, sources int
	for _, b := range first.Content {
		switch v := b.(type) {
		case *message.HostedCallBlock:
			calls++
		case *message.HostedResultBlock:
			sources += len(v.Sources)
		case *message.TextBlock:
			t.Logf("citations: %d", len(v.Citations))
		}
	}
	if calls == 0 {
		t.Fatal("no web_search_call: the endpoint ignored the hosted tool")
	}
	t.Logf("%d calls, %d sources, hosted_calls %v", calls, sources, first.Usage.HostedCalls)
	req.Messages = append(history, *first, message.UserText("And Kyoto city? One line."))
	if second := run(t, p, req); second.Text() == "" {
		t.Fatal("no answer after replaying the web_search_call items")
	}
}

// TestLiveExplicitCache marks breakpoints with caps.prompt_cache.explicit and checks that a second request sharing the marked prefix reads the cache; only GPT-5.6 and later take marks, so it runs with OPENAI_EXPLICIT_CACHE=1.
func TestLiveExplicitCache(t *testing.T) {
	p, model := live(t)
	if os.Getenv("OPENAI_EXPLICIT_CACHE") != "1" {
		t.Skip("set OPENAI_EXPLICIT_CACHE=1 for a model that takes explicit breakpoints")
	}
	var table strings.Builder
	for i := range 1100 {
		fmt.Fprintf(&table, "Row %d: item %d costs %d yen. ", i, i, i*3)
	}
	lookup := message.ToolDef{Name: "lookup", Description: "Look up the price table.", InputSchema: json.RawMessage(`{"type":"object","properties":{},"required":[],"additionalProperties":false}`)}
	history := []message.Message{
		{ID: "m1", Role: message.RoleUser, Origin: message.Origin{Kind: message.OriginHuman}, Content: message.Content{&message.TextBlock{Text: "Look up the price table, then answer." + nonce()}}},
		{ID: "m2", Role: message.RoleAssistant, Origin: message.Origin{Kind: message.OriginModel}, Producer: &message.Producer{API: string(api)}, Content: message.Content{&message.ToolCallBlock{ID: "call_1", Name: "lookup", Input: json.RawMessage(`{}`)}}},
		{ID: "m3", Role: message.RoleTool, Origin: message.Origin{Kind: message.OriginTool}, Content: message.Content{&message.ToolResultBlock{CallID: "call_1", Name: "lookup", Outcome: message.OutcomeSuccess, Content: message.Content{&message.TextBlock{Text: table.String()}}}}},
	}
	ask := func(q string) *message.Message {
		msgs := append(slices.Clone(history), message.Message{Role: message.RoleUser, Origin: message.Origin{Kind: message.OriginHuman}, Content: message.Content{&message.TextBlock{Text: q}}})
		return run(t, p, &provider.Request{Model: model, Tools: []message.ToolDef{lookup}, Params: provider.Params{MaxOutputTokens: 200}, Messages: msgs, CacheTail: len(history),
			Caps: provider.Capabilities{PromptCache: provider.CacheCaps{Explicit: true}}})
	}
	first := ask("What does item 12 cost? One number.")
	time.Sleep(5 * time.Second)
	second := ask("What does item 40 cost? One number.")
	t.Logf("first wrote %d, second read %d", first.Usage.CacheWriteTokens, second.Usage.CacheReadTokens)
	if second.Usage.CacheReadTokens == 0 {
		t.Fatal("the second request read nothing from the marked prefix")
	}
}

// searchable is a catalog the neutral tool search has to look through; only get_weather fits the question asked.
var searchable = []message.ToolDef{
	{Name: "get_weather", Description: "Get the current weather for a city.", InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`), Deferred: true},
	{Name: "create_issue", Description: "Open an issue in the bug tracker.", InputSchema: json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}`), Deferred: true},
	{Name: "send_email", Description: "Send an email to a colleague.", InputSchema: json.RawMessage(`{"type":"object","properties":{"to":{"type":"string"},"body":{"type":"string"}},"required":["to","body"]}`), Deferred: true},
	{Name: "convert_currency", Description: "Convert an amount between currencies.", InputSchema: json.RawMessage(`{"type":"object","properties":{"amount":{"type":"number"},"from":{"type":"string"},"to":{"type":"string"}},"required":["amount","from","to"]}`), Deferred: true},
}

// TestLiveToolSearch: the model searches the deferred catalog, calls what it found, and both search items replay without ids under store:false.
func TestLiveToolSearch(t *testing.T) {
	p, model := live(t)
	history := []message.Message{message.UserText("What is the weather in Osaka right now? Find a suitable tool and use it.")}
	req := &provider.Request{Model: model, Tools: searchable, HostedTools: []message.HostedTool{{Name: "tool_search", ToolSearch: &message.ToolSearch{}}}, Params: provider.Params{MaxOutputTokens: 2048}, Messages: history}
	first := run(t, p, req)
	var searches, loaded int
	for _, b := range first.Content {
		switch v := b.(type) {
		case *message.HostedCallBlock:
			searches++
		case *message.HostedResultBlock:
			loaded++
			t.Logf("search loaded %v", v.Tools)
		case *message.OpaqueBlock:
			t.Logf("opaque %s", v.Kind)
		}
	}
	tc := first.ToolCalls()
	if searches == 0 || loaded == 0 || len(tc) == 0 {
		t.Fatalf("no search and call: %d searches, %d results, %d tool calls", searches, loaded, len(tc))
	}
	var out message.Content
	for _, c := range tc {
		out = append(out, &message.ToolResultBlock{CallID: c.ID, Name: c.Name, Outcome: message.OutcomeSuccess, Content: message.Content{&message.TextBlock{Text: "Sunny, 24°C"}}})
	}
	req.Messages = append(history, *first, message.Message{Role: message.RoleTool, Origin: message.Origin{Kind: message.OriginTool}, Content: out})
	if second := run(t, p, req); second.Text() == "" {
		t.Fatal("no answer after replaying the search")
	}
}
