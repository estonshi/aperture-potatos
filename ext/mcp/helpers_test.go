package mcp_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/ext/mcp"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
	"github.com/estonshi/aperture-potatos/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

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

// TestMain doubles as a stdio MCP server, so a test can start this binary as one.
func TestMain(m *testing.M) {
	if os.Getenv("MCP_TEST_STDIO_SERVER") == "1" {
		srv := sdk.NewServer(&sdk.Implementation{Name: "stdio", Version: "1"}, &sdk.ServerOptions{Instructions: "A server over stdio."})
		srv.AddTool(&sdk.Tool{Name: "env", Description: "Report the token from the environment.", InputSchema: objectSchema}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return textResult("token=" + os.Getenv("STDIO_TOKEN")), nil
		})
		_ = srv.Run(context.Background(), &sdk.StdioTransport{})
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// use installs the extension at agent scope and closes its connections when the test ends.
func use(t *testing.T, o mcp.Options) kernel.Option {
	e := mcp.New(o)
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	return kernel.Use(e)
}

func newAgent(t *testing.T, sp *providertest.ScriptProvider, opts ...kernel.Option) *kernel.Agent {
	t.Helper()
	base := []kernel.Option{
		kernel.WithNickname("Tater"),
		kernel.WithModel(provider.ModelRef{Provider: sp, Model: "m"}),
		kernel.WithStore(store.NewMemory()),
		kernel.WithPanicOnInternalError(true),
		kernel.WithLogger(slog.New(slog.NewTextHandler(testWriter{t}, nil))),
	}
	ag, err := kernel.New(append(base, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ag.Close(context.Background()) })
	return ag
}

func ask(t *testing.T, s *kernel.Session, text string) kernel.TurnResult {
	t.Helper()
	rc, err := s.Send(kernel.Text(text))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	res, err := rc.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func toolResults(s *kernel.Session) map[string]message.ToolResultBlock {
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

func systemText(req *provider.Request) string {
	var parts []string
	for _, b := range req.System {
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, "\n")
}

func toolNames(req *provider.Request) []string {
	var out []string
	for _, d := range req.Tools {
		out = append(out, d.Name)
	}
	return out
}

func toolDef(req *provider.Request, name string) *message.ToolDef {
	for i := range req.Tools {
		if req.Tools[i].Name == name {
			return &req.Tools[i]
		}
	}
	return nil
}

// recorder keeps a session's events.
type recorder struct {
	mu  sync.Mutex
	evs []event.Event
}

func record(t *testing.T, s *kernel.Session) *recorder {
	r := &recorder{}
	if _, err := s.SubscribeFunc(func(_ context.Context, ev event.Event) error {
		r.mu.Lock()
		r.evs = append(r.evs, ev)
		r.mu.Unlock()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *recorder) of(typ event.Type) []event.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []event.Event
	for _, ev := range r.evs {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

// eventually polls cond for up to five seconds, since connections and notifications run on their own goroutines.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// testServer is an MCP server over Streamable HTTP with the tools a test adds.
type testServer struct {
	srv  *sdk.Server
	http *httptest.Server
}

func newTestServer(t *testing.T, instructions string, wrap func(http.Handler) http.Handler) *testServer {
	t.Helper()
	srv := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "1"}, &sdk.ServerOptions{Instructions: instructions,
		Capabilities: &sdk.ServerCapabilities{Tools: &sdk.ToolCapabilities{ListChanged: true}}})
	var h http.Handler = sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, nil)
	if wrap != nil {
		h = wrap(h)
	}
	ts := &testServer{srv: srv, http: httptest.NewServer(h)}
	t.Cleanup(ts.http.Close)
	return ts
}

func (ts *testServer) URL() string { return ts.http.URL }

var objectSchema = json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`)

func (ts *testServer) add(name, desc string, h sdk.ToolHandler) {
	ts.srv.AddTool(&sdk.Tool{Name: name, Description: desc, InputSchema: objectSchema}, h)
}

func textResult(s string) *sdk.CallToolResult {
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: s}}}
}

func argText(req *sdk.CallToolRequest) string {
	var in struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(req.Params.Arguments, &in)
	return in.Text
}

// testWriter sends the agent's log to the test's log, so a failure shows what the extension warned about.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}
