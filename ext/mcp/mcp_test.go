package mcp_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/estonshi/aperture-potatos/ext/mcp"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestDirectTools: each server tool is a tool of its own, the catalog carries the server's instructions, and every kind of result reaches the model as the mapping says.
func TestDirectTools(t *testing.T) {
	ts := newTestServer(t, "Use echo to repeat text.", nil)
	ts.add("echo", "Repeat text.\nMore detail here.", func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return textResult(argText(req)), nil
	})
	ts.add("pic", "Draw.", func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{
			&sdk.ImageContent{MIMEType: "image/png", Data: []byte("png")},
			&sdk.TextContent{Text: "for the user only", Annotations: &sdk.Annotations{Audience: []sdk.Role{"user"}}},
			&sdk.ResourceLink{URI: "file:///a.txt", Name: "a.txt", MIMEType: "text/plain"},
			&sdk.AudioContent{MIMEType: "audio/wav", Data: []byte("wav")},
		}}, nil
	})
	ts.srv.AddTool(&sdk.Tool{Name: "data", Description: "Data.", InputSchema: objectSchema,
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`)},
		func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			if argText(req) == "bad" {
				return &sdk.CallToolResult{StructuredContent: map[string]any{"n": "x"}}, nil
			}
			return &sdk.CallToolResult{StructuredContent: map[string]any{"n": 3}}, nil
		})
	ts.add("oops", "Fails.", func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: "date must be in the future"}}}, nil
	})
	ts.srv.AddTool(&sdk.Tool{Name: "weird.name/x", Description: "Odd.", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) { return textResult("ok"), nil })

	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages,
		calls(call{"c1", "mcp__t__echo", `{"text":"hi"}`}, call{"c2", "mcp__t__pic", `{"text":""}`}, call{"c3", "mcp__t__data", `{"text":""}`},
			call{"c4", "mcp__t__data", `{"text":"bad"}`}, call{"c5", "mcp__t__oops", `{"text":""}`}, call{"c6", "mcp__t__weird_name_x", `{}`}),
		reply("done"))
	ag := newAgent(t, sp, use(t, mcp.Options{Servers: []mcp.Server{{Name: "t", URL: ts.URL()}}}))
	s, err := ag.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ask(t, s, "go")
	req := sp.Requests()[0]
	echo := toolDef(req, "mcp__t__echo")
	if echo == nil || echo.Description != "Repeat text.\nMore detail here." || !strings.Contains(string(echo.InputSchema), `"required":["text"]`) {
		t.Fatalf("echo definition %+v in %v", echo, toolNames(req))
	}
	if slices.Contains(toolNames(req), "mcp_call_tool") {
		t.Fatal("a few small tools went to discovery mode")
	}
	sys := systemText(req)
	if !strings.Contains(sys, "<server name=\"t\">\n<instructions>Use echo to repeat text.</instructions>\n</server>") || strings.Contains(sys, "<tools>") {
		t.Fatalf("catalog: %s", sys)
	}
	res := toolResults(s)
	if r := res["c1"]; r.IsError || resultText(r) != "hi" {
		t.Fatalf("echo %+v", r)
	}
	pic := res["c2"].Content
	if len(pic) != 3 {
		t.Fatalf("pic content %d blocks: %+v", len(pic), pic)
	}
	if img, ok := pic[0].(*message.ImageBlock); !ok || img.MediaType != "image/png" || string(img.Data) != "png" {
		t.Fatalf("image %+v", pic[0])
	}
	if got := (message.Message{Content: pic[1:]}).Text(); !strings.Contains(got, "Linked resource: a.txt <file:///a.txt> (text/plain)") || !strings.Contains(got, "[Audio (audio/wav) omitted") {
		t.Fatalf("link and audio: %q", got)
	}
	if r := res["c3"]; r.IsError || resultText(r) != `{"n":3}` {
		t.Fatalf("structured %+v %q", r, resultText(r))
	}
	if r := res["c4"]; !r.IsError || !strings.Contains(resultText(r), "does not match its declared output schema") {
		t.Fatalf("bad output %+v %q", r, resultText(r))
	}
	if r := res["c5"]; !r.IsError || resultText(r) != "date must be in the future" {
		t.Fatalf("tool error %+v", r)
	}
	if r := res["c6"]; r.IsError || resultText(r) != "ok" {
		t.Fatalf("mapped name %+v", r)
	}
}

// TestSearchMode: discovery registers only the two meta tools, the catalog lists every tool by name and summary, and calls run through mcp_call_tool with their arguments checked.
func TestSearchMode(t *testing.T) {
	ts := newTestServer(t, "", nil)
	ts.add("echo", "Repeat text.\nMore detail here.", func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return textResult("echo:" + argText(req)), nil
	})
	ts.add("shout", "Say it loud.", func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return textResult(strings.ToUpper(argText(req))), nil
	})
	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages,
		calls(call{"s1", "mcp_search_tools", `{"names":["mcp__t__echo"]}`}, call{"s2", "mcp_search_tools", `{"query":"loud"}`}, call{"s3", "mcp_search_tools", `{}`}),
		calls(call{"k1", "mcp_call_tool", `{"name":"mcp__t__echo","arguments":{"text":"yo"}}`}, call{"k2", "mcp_call_tool", `{"name":"mcp__t__echo","arguments":{"text":5}}`},
			call{"k3", "mcp_call_tool", `{"name":"mcp__t__nope","arguments":{}}`}),
		reply("done"))
	ag := newAgent(t, sp, use(t, mcp.Options{Discovery: mcp.Search, Servers: []mcp.Server{{Name: "t", URL: ts.URL()}}}))
	s, err := ag.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ask(t, s, "go")
	reqs := sp.Requests()
	if names := toolNames(reqs[0]); !slices.Contains(names, "mcp_search_tools") || !slices.Contains(names, "mcp_call_tool") || slices.Contains(names, "mcp__t__echo") {
		t.Fatalf("tools %v", names)
	}
	if d := toolDef(reqs[0], "mcp_search_tools"); d == nil || !strings.Contains(string(d.InputSchema), `"enum":["t"]`) {
		t.Fatalf("search tool %+v", d)
	}
	sys := systemText(reqs[0])
	if !strings.Contains(sys, "<tools>\n- mcp__t__echo: Repeat text.\n- mcp__t__shout: Say it loud.\n</tools>") || !strings.Contains(sys, "read its definition with mcp_search_tools") {
		t.Fatalf("catalog: %s", sys)
	}
	for i := 1; i < len(reqs); i++ {
		if !slices.Equal(toolNames(reqs[i]), toolNames(reqs[0])) {
			t.Fatalf("tools changed between requests: %v / %v", toolNames(reqs[0]), toolNames(reqs[i]))
		}
	}
	res := toolResults(s)
	if got := resultText(res["s1"]); !strings.HasPrefix(got, "Server t:\n{\"name\":\"mcp__t__echo\"") || !strings.Contains(got, `"input_schema":{`) {
		t.Fatalf("names lookup %q", got)
	}
	if got := resultText(res["s2"]); got != "Server t:\nmcp__t__shout\nSay it loud." {
		t.Fatalf("keyword search %q", got)
	}
	if r := res["s3"]; !r.IsError || resultText(r) != "Give query, names, or both." {
		t.Fatalf("empty search %+v", r)
	}
	if r := res["k1"]; r.IsError || resultText(r) != "echo:yo" {
		t.Fatalf("call %+v", r)
	}
	if r := res["k2"]; !r.IsError || !strings.HasPrefix(resultText(r), "Invalid arguments for mcp__t__echo:") {
		t.Fatalf("bad arguments %+v %q", r, resultText(r))
	}
	if r := res["k3"]; !r.IsError || !strings.HasPrefix(resultText(r), "Unknown tool mcp__t__nope.") {
		t.Fatalf("unknown %+v", r)
	}
}

// TestAutoSwitchesToSearch: definitions past DiscoveryRatio of the window go to discovery, and the catalog falls back to names alone past its own budget.
func TestAutoSwitchesToSearch(t *testing.T) {
	ts := newTestServer(t, "", nil)
	for _, n := range []string{"alpha", "beta", "gamma"} {
		ts.add(n, strings.Repeat("A long description of "+n+". ", 10), func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return textResult("ok"), nil
		})
	}
	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, reply("hi"))
	ag := newAgent(t, sp, use(t, mcp.Options{ContextWindow: 1000, CatalogRatio: 0.01, Servers: []mcp.Server{{Name: "t", URL: ts.URL()}}}))
	s, err := ag.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ask(t, s, "go")
	req := sp.Requests()[0]
	if !slices.Contains(toolNames(req), "mcp_call_tool") {
		t.Fatalf("tools %v", toolNames(req))
	}
	if sys := systemText(req); !strings.Contains(sys, "<tools>\n- mcp__t__alpha\n- mcp__t__beta\n- mcp__t__gamma\n</tools>") {
		t.Fatalf("names-only catalog: %s", sys)
	}
}

// TestListChanged: a tool the server adds later is registered for the next turn.
func TestListChanged(t *testing.T) {
	ts := newTestServer(t, "", nil)
	ts.add("one", "One.", func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) { return textResult("1"), nil })
	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, reply("a"), reply("b"))
	ag := newAgent(t, sp, use(t, mcp.Options{Servers: []mcp.Server{{Name: "t", URL: ts.URL()}}}))
	s, err := ag.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	rec := record(t, s)
	ask(t, s, "first")
	ts.add("two", "Two.", func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) { return textResult("2"), nil })
	eventually(t, "the new tool's registration", func() bool { return len(rec.of("registry.changed")) > 0 })
	ask(t, s, "second")
	reqs := sp.Requests()
	if slices.Contains(toolNames(reqs[0]), "mcp__t__two") || !slices.Contains(toolNames(reqs[1]), "mcp__t__two") {
		t.Fatalf("before %v after %v", toolNames(reqs[0]), toolNames(reqs[1]))
	}
}

// TestProgressAndInterrupt: a server's progress notices become tool.progress, and an interrupt cancels the call on the server.
func TestProgressAndInterrupt(t *testing.T) {
	ts := newTestServer(t, "", nil)
	var canceled atomic.Bool
	started := make(chan struct{})
	ts.add("slow", "Slow.", func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		_ = req.Session.NotifyProgress(ctx, &sdk.ProgressNotificationParams{ProgressToken: req.Params.GetProgressToken(), Message: "half", Progress: 1, Total: 2})
		close(started)
		<-ctx.Done()
		canceled.Store(true)
		return nil, ctx.Err()
	})
	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, calls(call{"p1", "mcp__t__slow", `{"text":""}`}), reply("stopped"))
	ag := newAgent(t, sp, use(t, mcp.Options{Servers: []mcp.Server{{Name: "t", URL: ts.URL()}}}))
	s, err := ag.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	rec := record(t, s)
	rc, err := s.Send(kernel.Text("go"))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the call never reached the server")
	}
	eventually(t, "tool.progress", func() bool { return len(rec.of("tool.progress")) == 1 })
	s.Interrupt("user")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := rc.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the server-side cancel", canceled.Load)
}

// TestUnreachableServer: a server that never answers does not stop New, and the session is told nothing about tools it never had.
func TestUnreachableServer(t *testing.T) {
	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, reply("hi"))
	start := time.Now()
	ag := newAgent(t, sp, use(t, mcp.Options{ConnectTimeout: 300 * time.Millisecond, Servers: []mcp.Server{{Name: "down", URL: "http://127.0.0.1:1/mcp"}}}))
	if time.Since(start) > 5*time.Second {
		t.Fatal("New waited on the unreachable server")
	}
	s, err := ag.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ask(t, s, "go")
	if sys := systemText(sp.Requests()[0]); strings.Contains(sys, "mcp_servers") {
		t.Fatalf("catalog for a server never reached: %s", sys)
	}
}

func TestInvalidOptions(t *testing.T) {
	for name, s := range map[string]mcp.Server{
		"bad name":    {Name: "Bad", URL: "http://x"},
		"no target":   {Name: "a"},
		"stdio oauth": {Name: "a", Command: nil, URL: "", OAuth: &mcp.OAuth{}},
	} {
		sp := providertest.NewScriptProvider(provider.APIAnthropicMessages)
		_, err := kernel.New(kernel.WithModel(provider.ModelRef{Provider: sp, Model: "m"}), use(t, mcp.Options{Servers: []mcp.Server{s}}))
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestApprovalRulesMirror: a rule on a server tool gets a twin on mcp_call_tool with the same verdict on the same input.
func TestApprovalRulesMirror(t *testing.T) {
	rules := mcp.ApprovalRules(
		kernel.ApprovalRule{ID: "gh", Tool: "mcp__github__*", Action: kernel.RuleAsk, When: map[string]kernel.Condition{"repo": {Prefix: "secret/"}}},
		kernel.ApprovalRule{ID: "other", Tool: "write_file", Action: kernel.RuleDeny},
	)
	if len(rules) != 3 {
		t.Fatalf("rules %+v", rules)
	}
	m := rules[2]
	if m.ID != "gh.via_call_tool" || m.Tool != "mcp_call_tool" || m.Action != kernel.RuleAsk || m.When["name"].Glob != "mcp__github__*" || m.When["arguments.repo"].Prefix != "secret/" {
		t.Fatalf("mirror %+v", m)
	}
}

// TestServerGoesAway: when the server stops, its tools stay registered, calls say it cannot be reached, and the session is told.
func TestServerGoesAway(t *testing.T) {
	ts := newTestServer(t, "", nil)
	ts.add("echo", "Repeat.", func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return textResult(argText(req)), nil
	})
	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, reply("a"), calls(call{"g1", "mcp__t__echo", `{"text":"x"}`}), reply("b"), reply("c"))
	ag := newAgent(t, sp, use(t, mcp.Options{Servers: []mcp.Server{{Name: "t", URL: ts.URL()}}}))
	s, err := ag.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	rec := record(t, s)
	ask(t, s, "first")
	ts.http.CloseClientConnections()
	ts.http.Close()
	ask(t, s, "second")
	eventually(t, "the failed state", func() bool {
		for _, ev := range rec.of("mcp.server_status") {
			if ev.Data.(*mcp.ServerStatusEvent).State == "failed" {
				return true
			}
		}
		return false
	})
	reqs := sp.Requests()
	if !slices.Contains(toolNames(reqs[1]), "mcp__t__echo") {
		t.Fatalf("the tool left with the server: %v", toolNames(reqs[1]))
	}
	if r := toolResults(s)["g1"]; !r.IsError || resultText(r) != "mcp__t__echo did not run: the t server cannot be reached right now." {
		t.Fatalf("result %+v %q", r, resultText(r))
	}
	ask(t, s, "third")
	reqs = sp.Requests()
	b, _ := json.Marshal(reqs[len(reqs)-1].Messages)
	if !strings.Contains(string(b), "MCP server t cannot be reached right now.") {
		t.Fatalf("the session was not told: %s", b)
	}
}

// TestSearchModeAnnouncesChanges: in discovery mode a tool added mid-epoch is announced with its summary, since the catalog of this epoch does not list it.
func TestSearchModeAnnouncesChanges(t *testing.T) {
	ts := newTestServer(t, "", nil)
	ts.add("one", "One.", func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) { return textResult("1"), nil })
	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, reply("a"), reply("b"))
	ag := newAgent(t, sp, use(t, mcp.Options{Discovery: mcp.Search, Servers: []mcp.Server{{Name: "t", URL: ts.URL()}}}))
	s, err := ag.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ask(t, s, "first")
	ts.add("two", "Two things.", func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) { return textResult("2"), nil })
	eventually(t, "the queued announcement", func() bool { return s.Snapshot().QueuedInputs > 0 })
	ask(t, s, "second")
	reqs := sp.Requests()
	b, _ := json.Marshal(reqs[1].Messages)
	if !strings.Contains(string(b), `MCP server t changed its tools.\nAdded:\n- mcp__t__two: Two things.`) {
		t.Fatalf("no announcement: %s", b)
	}
	if !slices.Equal(toolNames(reqs[0]), toolNames(reqs[1])) {
		t.Fatalf("the tool array changed in discovery mode: %v / %v", toolNames(reqs[0]), toolNames(reqs[1]))
	}
}
