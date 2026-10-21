package mcp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/ext/mcp"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func deferredNames(req *provider.Request) []string {
	var out []string
	for _, d := range req.Tools {
		if d.Deferred {
			out = append(out, d.Name)
		}
	}
	return out
}

func hasToolSearch(req *provider.Request) bool {
	return slices.ContainsFunc(req.HostedTools, func(h message.HostedTool) bool { return h.ToolSearch != nil && h.Name == "tool_search" })
}

func okTool(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
	return textResult("ok"), nil
}

// TestNativeMode: every server tool goes as a deferred definition beside the neutral tool search, with no discovery tools, and the catalog says to search.
func TestNativeMode(t *testing.T) {
	ts := newTestServer(t, "", nil)
	ts.add("alpha", "Alpha.", okTool)
	ts.add("beta", "Beta.", okTool)
	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, reply("hi"))
	ag := newAgent(t, sp, use(t, mcp.Options{Discovery: mcp.Native, Servers: []mcp.Server{{Name: "t", URL: ts.URL()}}}))
	s, err := ag.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ask(t, s, "go")
	req := sp.Requests()[0]
	if got := deferredNames(req); !slices.Equal(got, []string{"mcp__t__alpha", "mcp__t__beta"}) || !hasToolSearch(req) {
		t.Fatalf("deferred %v, hosted %+v", got, req.HostedTools)
	}
	if slices.Contains(toolNames(req), "mcp_call_tool") || slices.Contains(toolNames(req), "mcp_search_tools") {
		t.Fatalf("discovery tools sent in native mode: %v", toolNames(req))
	}
	sys := systemText(req)
	if !strings.Contains(sys, "are not loaded up front. Search for the ones you need with the tool search") || !strings.Contains(sys, `<server name="t">`) || strings.Contains(sys, "<tools>") {
		t.Fatalf("native catalog lists the server, even without instructions, and no tools: %s", sys)
	}
}

// TestAutoSwitchesToNative: with NativeSearch, growing past the ratio defers the tools and announces it, and shrinking below half loads them again.
func TestAutoSwitchesToNative(t *testing.T) {
	ts := newTestServer(t, "", nil)
	ts.add("small", "Small.", okTool)
	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, reply("a"), reply("b"), reply("c"))
	ag := newAgent(t, sp, use(t, mcp.Options{NativeSearch: true, ContextWindow: 1000, DiscoveryRatio: 0.1, Servers: []mcp.Server{{Name: "t", URL: ts.URL()}}}))
	s, err := ag.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	rec := record(t, s)
	ask(t, s, "first")
	big := []string{"one", "two", "three"}
	for _, n := range big {
		ts.add(n, strings.Repeat("A long description of "+n+". ", 10), okTool)
	}
	eventually(t, "the switch to native", func() bool {
		return slices.ContainsFunc(rec.of("registry.changed"), func(ev event.Event) bool {
			d, _ := ev.Data.(*event.RegistryChanged)
			return d != nil && d.Kind == string(kernel.KindHostedTool) && d.Op == "add"
		})
	})
	ask(t, s, "second")
	ts.srv.RemoveTools(big...)
	eventually(t, "the switch back", func() bool {
		return slices.ContainsFunc(rec.of("registry.changed"), func(ev event.Event) bool {
			d, _ := ev.Data.(*event.RegistryChanged)
			return d != nil && d.Kind == string(kernel.KindHostedTool) && d.Op == "remove"
		})
	})
	ask(t, s, "third")
	reqs := sp.Requests()
	if len(deferredNames(reqs[0])) != 0 || hasToolSearch(reqs[0]) {
		t.Fatalf("first request is direct: %v %+v", deferredNames(reqs[0]), reqs[0].HostedTools)
	}
	if got := deferredNames(reqs[1]); len(got) != 4 || !hasToolSearch(reqs[1]) {
		t.Fatalf("second request is native: %v %+v", got, reqs[1].HostedTools)
	}
	last := reqs[1].Messages[len(reqs[1].Messages)-1]
	if !strings.Contains(last.Text()+reminders(last), "MCP server tools are no longer loaded up front.") {
		t.Fatalf("the switch is announced with the next turn: %+v", last.Content)
	}
	if len(deferredNames(reqs[2])) != 0 || hasToolSearch(reqs[2]) {
		t.Fatalf("third request is direct again: %v %+v", deferredNames(reqs[2]), reqs[2].HostedTools)
	}
}

func reminders(m message.Message) string {
	var out []string
	for _, b := range m.Content {
		if r, ok := b.(*message.ReminderBlock); ok {
			out = append(out, r.Text)
		}
	}
	return strings.Join(out, "\n")
}

// promptServer is a test server that offers prompts besides tools.
func promptServer(t *testing.T) *testServer {
	t.Helper()
	srv := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "1"}, &sdk.ServerOptions{
		Capabilities: &sdk.ServerCapabilities{Tools: &sdk.ToolCapabilities{ListChanged: true}, Prompts: &sdk.PromptCapabilities{ListChanged: true}}})
	srv.AddPrompt(&sdk.Prompt{Name: "Review.Code", Title: "Review code", Arguments: []*sdk.PromptArgument{{Name: "code", Required: true}, {Name: "lang"}}},
		func(_ context.Context, req *sdk.GetPromptRequest) (*sdk.GetPromptResult, error) {
			a := req.Params.Arguments
			return &sdk.GetPromptResult{Messages: []*sdk.PromptMessage{{Role: "user", Content: &sdk.TextContent{Text: "Review this " + a["lang"] + " code: " + a["code"]}}}}, nil
		})
	srv.AddPrompt(&sdk.Prompt{Name: "few_shot", Description: "Answer like the example.\nMore detail."},
		func(context.Context, *sdk.GetPromptRequest) (*sdk.GetPromptResult, error) {
			return &sdk.GetPromptResult{Messages: []*sdk.PromptMessage{
				{Role: "user", Content: &sdk.TextContent{Text: "What is 2+2?"}},
				{Role: "assistant", Content: &sdk.TextContent{Text: "4."}},
				{Role: "user", Content: &sdk.ImageContent{MIMEType: "image/png", Data: []byte("png")}},
				{Role: "user", Content: &sdk.TextContent{Text: "What is in the picture?"}},
			}}, nil
		})
	ts := &testServer{srv: srv, http: httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, nil))}
	t.Cleanup(ts.http.Close)
	return ts
}

func commandNames(s *kernel.Session) []string {
	var out []string
	for _, c := range s.Commands() {
		if strings.HasPrefix(c.Name, "mcp__") {
			out = append(out, c.Name)
		}
	}
	return out
}

// TestPromptsOffByDefault: a server's prompts become commands only when the host enables them.
func TestPromptsOffByDefault(t *testing.T) {
	ts := promptServer(t)
	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages)
	ag := newAgent(t, sp, use(t, mcp.Options{Servers: []mcp.Server{{Name: "t", URL: ts.URL()}}}))
	s, err := ag.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := commandNames(s); len(got) != 0 {
		t.Fatalf("commands without Prompts: %v", got)
	}
}

// runCommand invokes a command and waits for its command.completed, returning its error text.
func runCommand(t *testing.T, s *kernel.Session, rec *recorder, name, args string) string {
	t.Helper()
	before := len(rec.of("command.completed"))
	if _, err := s.Command(name, args); err != nil {
		t.Fatal(err)
	}
	var done *event.CommandCompleted
	eventually(t, "command "+name, func() bool {
		evs := rec.of("command.completed")
		if len(evs) <= before {
			return false
		}
		done, _ = evs[len(evs)-1].Data.(*event.CommandCompleted)
		return done != nil
	})
	return done.Error
}

// TestPromptCommands: each prompt is a command with its hint; typed words fill the arguments in order, a lone user message goes as it is, and several are labeled by role with images as parts.
func TestPromptCommands(t *testing.T) {
	ts := promptServer(t)
	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, reply("a"), reply("b"))
	ag := newAgent(t, sp, use(t, mcp.Options{Servers: []mcp.Server{{Name: "t", URL: ts.URL(), Prompts: true}}}))
	s, err := ag.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	rec := record(t, s)
	if got := commandNames(s); !slices.Equal(got, []string{"mcp__t__few_shot", "mcp__t__review_code"}) {
		t.Fatalf("commands %v", got)
	}
	for _, c := range s.Commands() {
		switch c.Name {
		case "mcp__t__review_code":
			if c.ArgHint != "<code> [lang]" || c.Description != "Review code" {
				t.Fatalf("review command %+v", c)
			}
		case "mcp__t__few_shot":
			if c.Description != "Answer like the example." {
				t.Fatalf("few-shot command %+v", c)
			}
		}
	}
	if e := runCommand(t, s, rec, "mcp__t__review_code", ""); !strings.Contains(e, "needs code") || !strings.Contains(e, "usage: /mcp__t__review_code <code> [lang]") {
		t.Fatalf("missing argument: %q", e)
	}
	if e := runCommand(t, s, rec, "mcp__t__review_code", `"x = 1" go with care`); e != "" {
		t.Fatal(e)
	}
	eventually(t, "the first prompt's turn", func() bool { return len(sp.Requests()) == 1 })
	first := sp.Requests()[0].Messages
	if got := first[len(first)-1].Text(); got != "Review this go with care code: x = 1" {
		t.Fatalf("lone user message %q", got)
	}
	eventually(t, "the session to settle", func() bool { return s.Snapshot().Phase == "idle" })
	if e := runCommand(t, s, rec, "mcp__t__few_shot", ""); e != "" {
		t.Fatal(e)
	}
	eventually(t, "the second prompt's turn", func() bool { return len(sp.Requests()) == 2 })
	msgs := sp.Requests()[1].Messages
	last := msgs[len(msgs)-1]
	want := "User:\nWhat is 2+2?\n\nAssistant:\n4.\n\nUser:\n\nUser:\nWhat is in the picture?"
	if got := last.Text(); got != want {
		t.Fatalf("labeled prompt\n got %q\nwant %q", got, want)
	}
	if !slices.ContainsFunc(last.Content, func(b message.Block) bool {
		im, ok := b.(*message.ImageBlock)
		return ok && im.MediaType == "image/png"
	}) || last.Origin.Kind != message.OriginCommand {
		t.Fatalf("image part and command origin: %+v %+v", last.Content, last.Origin)
	}
}

// TestPromptListChanged: a prompt the server adds becomes a command, and one it removes goes away.
func TestPromptListChanged(t *testing.T) {
	ts := promptServer(t)
	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages)
	ag := newAgent(t, sp, use(t, mcp.Options{Servers: []mcp.Server{{Name: "t", URL: ts.URL(), Prompts: true}}}))
	s, err := ag.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ts.srv.AddPrompt(&sdk.Prompt{Name: "summarize"}, func(context.Context, *sdk.GetPromptRequest) (*sdk.GetPromptResult, error) {
		return &sdk.GetPromptResult{Messages: []*sdk.PromptMessage{{Role: "user", Content: &sdk.TextContent{Text: "Summarize."}}}}, nil
	})
	eventually(t, "the new command", func() bool { return slices.Contains(commandNames(s), "mcp__t__summarize") })
	ts.srv.RemovePrompts("few_shot")
	eventually(t, "the removed command", func() bool { return !slices.Contains(commandNames(s), "mcp__t__few_shot") })
}

// TestPromptServerGone: a command whose server went away fails for the host and sends nothing to the model.
func TestPromptServerGone(t *testing.T) {
	ts := promptServer(t)
	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages)
	ag := newAgent(t, sp, use(t, mcp.Options{Servers: []mcp.Server{{Name: "t", URL: ts.URL(), Prompts: true}}}))
	s, err := ag.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	rec := record(t, s)
	ts.http.CloseClientConnections()
	ts.http.Close()
	if e := runCommand(t, s, rec, "mcp__t__few_shot", ""); e == "" {
		t.Fatal("a command whose server is gone succeeded")
	}
	if !slices.Contains(commandNames(s), "mcp__t__few_shot") {
		t.Fatal("the command went away with the connection")
	}
	if len(sp.Requests()) != 0 {
		t.Fatal("a failed prompt reached the model")
	}
}
