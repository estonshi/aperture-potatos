package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/estonshi/aperture-potatos/ext/mcp"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"
)

// waitFor is eventually with a longer budget, for paths that go through the reconnect backoff.
func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestStdio: a stdio server is started as a command, with its credentials in the command's environment.
func TestStdio(t *testing.T) {
	cmd := func() *exec.Cmd {
		c := exec.Command(os.Args[0])
		c.Env = append(os.Environ(), "MCP_TEST_STDIO_SERVER=1", "STDIO_TOKEN=sesame")
		return c
	}
	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, calls(call{"s1", "mcp__local__env", `{"text":""}`}), reply("ok"))
	ag := newAgent(t, sp, use(t, mcp.Options{Servers: []mcp.Server{{Name: "local", Command: cmd}}}))
	s, err := ag.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ask(t, s, "go")
	if r := toolResults(s)["s1"]; r.IsError || resultText(r) != "token=sesame" {
		t.Fatalf("result %+v %q", r, resultText(r))
	}
	if sys := systemText(sp.Requests()[0]); !strings.Contains(sys, "A server over stdio.") {
		t.Fatalf("catalog: %s", sys)
	}
}

// flakyListener drops every connection while down, which looks to a client like a server that went away and came back.
type flakyListener struct {
	net.Listener
	mu    sync.Mutex
	down  bool
	conns []net.Conn
}

func (l *flakyListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		l.mu.Lock()
		if l.down {
			l.mu.Unlock()
			_ = c.Close()
			continue
		}
		l.conns = append(l.conns, c)
		l.mu.Unlock()
		return c, nil
	}
}

func (l *flakyListener) setDown(down bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.down = down
	if down {
		for _, c := range l.conns {
			_ = c.Close()
		}
		l.conns = nil
	}
}

// TestReconnect: a server that goes away and comes back is reconnected, the session is told both times, and calls work again.
func TestReconnect(t *testing.T) {
	srv := sdk.NewServer(&sdk.Implementation{Name: "t", Version: "1"}, nil)
	srv.AddTool(&sdk.Tool{Name: "echo", Description: "Repeat.", InputSchema: objectSchema}, func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return textResult(argText(req)), nil
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fl := &flakyListener{Listener: ln}
	hs := httptest.NewUnstartedServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, nil))
	hs.Listener = fl
	hs.Start()
	t.Cleanup(func() {
		fl.setDown(true)
		hs.Close()
	})
	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, reply("a"), calls(call{"r1", "mcp__t__echo", `{"text":"x"}`}), reply("b"),
		calls(call{"r2", "mcp__t__echo", `{"text":"back"}`}), reply("c"))
	ag := newAgent(t, sp, use(t, mcp.Options{Servers: []mcp.Server{{Name: "t", URL: hs.URL}}}))
	s, err := ag.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	rec := record(t, s)
	ask(t, s, "first")
	fl.setDown(true)
	ask(t, s, "while down")
	if r := toolResults(s)["r1"]; !strings.Contains(resultText(r), "cannot be reached") {
		t.Fatalf("down: %q", resultText(r))
	}
	fl.setDown(false)
	waitFor(t, "the reconnection", 20*time.Second, func() bool {
		evs := rec.of("mcp.server_status")
		return len(evs) > 0 && evs[len(evs)-1].Data.(*mcp.ServerStatusEvent).State == "ready"
	})
	ask(t, s, "after")
	if r := toolResults(s)["r2"]; r.IsError || resultText(r) != "back" {
		t.Fatalf("after: %+v %q", r, resultText(r))
	}
	b, _ := json.Marshal(sp.Requests()[3].Messages)
	if !strings.Contains(string(b), "MCP server t cannot be reached right now.") || !strings.Contains(string(b), "MCP server t is reachable again.") {
		t.Fatalf("notices: %s", b)
	}
}

// TestRefreshAfterRestart: a stored grant whose access token expired is refreshed with its refresh token, without asking the user.
func TestRefreshAfterRestart(t *testing.T) {
	o := newOAuthServer(t)
	store := &memTokens{m: map[string]*mcp.Grant{"sec": {
		Token:    (&oauth2.Token{AccessToken: "stale", RefreshToken: "r1", TokenType: "Bearer", Expiry: time.Now().Add(-time.Hour)}),
		TokenURL: o.base + "/token", ClientID: "client-1"}}}
	var asked atomic.Int32
	authorize := func(context.Context, string) (string, string, string, error) {
		asked.Add(1)
		return "", "", "", context.Canceled
	}
	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, calls(call{"f1", "mcp__sec__echo", `{"text":"x"}`}), reply("ok"))
	ext := mcp.New(mcp.Options{Servers: []mcp.Server{{Name: "sec", URL: o.base + "/mcp",
		OAuth: &mcp.OAuth{ClientID: "client-1", RedirectURL: "http://127.0.0.1/cb", Authorize: authorize, Tokens: store, AllowInsecure: true}}}})
	t.Cleanup(func() { _ = ext.Close(context.Background()) })
	ag := newAgent(t, sp)
	s, err := ag.NewSession(t.Context(), kernel.SessionUse(ext))
	if err != nil {
		t.Fatal(err)
	}
	ask(t, s, "go")
	if r := toolResults(s)["f1"]; r.IsError || resultText(r) != "secure:x" {
		t.Fatalf("result %+v %q", r, resultText(r))
	}
	if asked.Load() != 0 || o.refreshes.Load() != 1 {
		t.Fatalf("asked %d, refreshes %d", asked.Load(), o.refreshes.Load())
	}
	if g := store.m["sec"]; g.Token.AccessToken != "tok-1" {
		t.Fatalf("the refreshed token was not stored: %+v", g.Token.AccessToken == "stale")
	}
	if k := store.keys[len(store.keys)-1]; k.Server != "sec" || k.Resource != o.base+"/mcp" || k.Session != s.Info().SessionID {
		t.Fatalf("a session install keys its grant by server, resource and session: %+v", k)
	}
}

// TestBothScopes: an agent install and a session install stand side by side, and the session's discovery tools reach the agent's servers too.
func TestBothScopes(t *testing.T) {
	shared := newTestServer(t, "Shared server.", nil)
	shared.add("ping", "Ping.", func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return textResult("pong"), nil
	})
	own := newTestServer(t, "Own server.", nil)
	own.add("mine", "Mine.", func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return textResult("yours"), nil
	})
	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages,
		calls(call{"b1", "mcp_call_tool", `{"name":"mcp__shared__ping","arguments":{"text":""}}`}, call{"b2", "mcp_call_tool", `{"name":"mcp__own__mine","arguments":{"text":""}}`}),
		reply("ok"))
	ag := newAgent(t, sp, use(t, mcp.Options{Servers: []mcp.Server{{Name: "shared", URL: shared.URL()}}}))
	sessionExt := mcp.New(mcp.Options{Discovery: mcp.Search, Servers: []mcp.Server{{Name: "own", URL: own.URL()}}})
	t.Cleanup(func() { _ = sessionExt.Close(context.Background()) })
	s, err := ag.NewSession(t.Context(), kernel.SessionUse(sessionExt))
	if err != nil {
		t.Fatal(err)
	}
	ask(t, s, "go")
	req := sp.Requests()[0]
	sys := systemText(req)
	if !strings.Contains(sys, "Shared server.") || !strings.Contains(sys, "Own server.") {
		t.Fatalf("both catalogs: %s", sys)
	}
	if names := toolNames(req); !slices.Contains(names, "mcp__shared__ping") || !slices.Contains(names, "mcp_call_tool") || slices.Contains(names, "mcp__own__mine") {
		t.Fatalf("tools %v", names)
	}
	res := toolResults(s)
	if resultText(res["b1"]) != "pong" || resultText(res["b2"]) != "yours" {
		t.Fatalf("results %q %q", resultText(res["b1"]), resultText(res["b2"]))
	}
}

// TestNamesAndFilters: names are mapped and hashed when too long, a mapped name already taken is skipped, and the allow and deny lists hold.
func TestNamesAndFilters(t *testing.T) {
	ts := newTestServer(t, "", nil)
	ok := func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) { return textResult("ok"), nil }
	long := strings.Repeat("x", 80)
	for _, n := range []string{"a.b", "a_b", long, "hidden", "skipped"} {
		ts.add(n, "Tool "+n+".", ok)
	}
	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, reply("hi"))
	ag := newAgent(t, sp, use(t, mcp.Options{Servers: []mcp.Server{{Name: "t", URL: ts.URL(), Tools: []string{"a.b", "a_b", long, "hidden"}, DenyTools: []string{"hidden"}}}}))
	s, err := ag.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ask(t, s, "go")
	var names []string
	for _, n := range toolNames(sp.Requests()[0]) {
		if strings.HasPrefix(n, "mcp__") {
			names = append(names, n)
		}
	}
	if len(names) != 2 || names[0] != "mcp__t__a_b" {
		t.Fatalf("names %v", names)
	}
	if h := names[1]; len(h) != 64 || !strings.HasPrefix(h, "mcp__t__xxxx") || h[57] != '_' {
		t.Fatalf("hashed name %q (%d)", h, len(h))
	}
}

// TestServerNameCase: a server named as the host's own configuration names it keeps its capitals in tool names, and the commands its prompts become are lower-cased.
func TestServerNameCase(t *testing.T) {
	ts := promptServer(t)
	ts.add("echo", "Echo.", okTool)
	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, reply("hi"))
	ag := newAgent(t, sp, use(t, mcp.Options{Servers: []mcp.Server{{Name: "claude_ai_Gmail", URL: ts.URL(), Prompts: true}}}))
	s, err := ag.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ask(t, s, "go")
	if !slices.Contains(toolNames(sp.Requests()[0]), "mcp__claude_ai_Gmail__echo") {
		t.Fatalf("tool names %v", toolNames(sp.Requests()[0]))
	}
	if got := commandNames(s); !slices.Equal(got, []string{"mcp__claude_ai_gmail__few_shot", "mcp__claude_ai_gmail__review_code"}) {
		t.Fatalf("commands %v", got)
	}
}

// TestElicitationURL: a URL request reaches the hook in url mode when it is https, and is declined without asking when it is not.
func TestElicitationURL(t *testing.T) {
	ts := newTestServer(t, "", nil)
	ts.add("link", "Ask through a page.", func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		res, err := req.Session.Elicit(ctx, &sdk.ElicitParams{Mode: "url", Message: "Connect your account.", URL: argText(req), ElicitationID: "e-1"})
		if err != nil {
			return nil, err
		}
		return textResult(res.Action), nil
	})
	var seen []mcp.ElicitInput
	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages,
		calls(call{"u1", "mcp__t__link", `{"text":"https://accounts.example/connect"}`}), calls(call{"u2", "mcp__t__link", `{"text":"javascript:alert(1)"}`}), reply("ok"))
	ag := newAgent(t, sp, use(t, mcp.Options{Servers: []mcp.Server{{Name: "t", URL: ts.URL()}}}),
		kernel.WithHook(mcp.Elicitation, "user", func(_ context.Context, in mcp.ElicitInput) (mcp.ElicitOutput, error) {
			seen = append(seen, in)
			return mcp.ElicitOutput{Action: "accept"}, nil
		}))
	s, err := ag.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ask(t, s, "go")
	res := toolResults(s)
	if resultText(res["u1"]) != "accept" || resultText(res["u2"]) != "decline" {
		t.Fatalf("results %q %q", resultText(res["u1"]), resultText(res["u2"]))
	}
	if len(seen) != 1 || seen[0].Mode != "url" || seen[0].URL != "https://accounts.example/connect" {
		t.Fatalf("hook saw %+v", seen)
	}
}

// TestBackgroundAndResources: Background adds the background parameter, and embedded resources and long text map as the table says.
func TestBackgroundAndResources(t *testing.T) {
	ts := newTestServer(t, "", nil)
	ts.add("files", "Files.", func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{
			&sdk.EmbeddedResource{Resource: &sdk.ResourceContents{URI: "file:///notes.txt", MIMEType: "text/plain", Text: strings.Repeat("n", 30)}},
			&sdk.EmbeddedResource{Resource: &sdk.ResourceContents{URI: "file:///r/report.pdf", MIMEType: "application/pdf", Blob: []byte("%PDF")}},
			&sdk.EmbeddedResource{Resource: &sdk.ResourceContents{URI: "file:///a.zip", MIMEType: "application/zip", Blob: []byte("PK")}},
		}}, nil
	})
	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, calls(call{"f1", "mcp__t__files", `{"text":""}`}), reply("ok"))
	ag := newAgent(t, sp, use(t, mcp.Options{MaxResultChars: 20, Servers: []mcp.Server{{Name: "t", URL: ts.URL(), Background: true}}}))
	s, err := ag.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ask(t, s, "go")
	if d := toolDef(sp.Requests()[0], "mcp__t__files"); d == nil || !strings.Contains(string(d.InputSchema), `"background"`) {
		t.Fatalf("definition %+v", d)
	}
	c := toolResults(s)["f1"].Content
	if len(c) != 3 {
		t.Fatalf("content %+v", c)
	}
	if doc, ok := c[1].(*message.DocumentBlock); !ok || doc.Filename != "report.pdf" || doc.MediaType != "application/pdf" {
		t.Fatalf("pdf %+v", c[1])
	}
	text := message.Message{Content: message.Content{c[0], c[2]}}.Text()
	if !strings.HasPrefix(text, strings.Repeat("n", 20)) || !strings.Contains(text, "[Output truncated: showing the first 20 of ") {
		t.Fatalf("text %q", text)
	}
	if strings.Contains(text, "a.zip") {
		t.Fatalf("text past the limit kept: %q", text)
	}
}

// TestMediaChecked: an image of a type the APIs refuse, and an image or PDF over the media limit, become notes instead of blocks that would poison the history.
func TestMediaChecked(t *testing.T) {
	ts := newTestServer(t, "", nil)
	ts.add("pics", "Pictures.", func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{
			&sdk.ImageContent{MIMEType: "image/PNG", Data: []byte("small")},
			&sdk.ImageContent{MIMEType: "image/svg+xml", Data: []byte("<svg/>")},
			&sdk.ImageContent{MIMEType: "image/jpeg", Data: make([]byte, 11)},
			&sdk.EmbeddedResource{Resource: &sdk.ResourceContents{URI: "file:///big.pdf", MIMEType: "application/pdf", Blob: make([]byte, 11)}},
		}}, nil
	})
	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, calls(call{"p1", "mcp__t__pics", `{"text":""}`}), reply("ok"))
	ag := newAgent(t, sp, use(t, mcp.Options{MaxMediaBytes: 10, Servers: []mcp.Server{{Name: "t", URL: ts.URL()}}}))
	s, err := ag.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ask(t, s, "go")
	c := toolResults(s)["p1"].Content
	if len(c) != 4 {
		t.Fatalf("content %+v", c)
	}
	if img, ok := c[0].(*message.ImageBlock); !ok || img.MediaType != "image/PNG" {
		t.Fatalf("a small png stays an image: %+v", c[0])
	}
	want := []string{
		"[Image (image/svg+xml) omitted: images of this type cannot be shown here.]",
		"[image/jpeg content of 11 bytes omitted: it is larger than the 10 bytes that can be shown here.]",
		"[application/pdf content of 11 bytes omitted: it is larger than the 10 bytes that can be shown here.]",
	}
	for i, w := range want {
		if tb, ok := c[i+1].(*message.TextBlock); !ok || tb.Text != w {
			t.Fatalf("block %d: %+v", i+1, c[i+1])
		}
	}
}

// TestApprovalThroughGate: a rule on a server tool, mirrored by ApprovalRules, sends the same tool to review when it comes through mcp_call_tool.
func TestApprovalThroughGate(t *testing.T) {
	ts := newTestServer(t, "", nil)
	var ran atomic.Bool
	ts.add("deploy", "Deploy.", func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		ran.Store(true)
		return textResult("deployed"), nil
	})
	var reviewed []string
	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, calls(call{"d1", "mcp_call_tool", `{"name":"mcp__t__deploy","arguments":{"text":"prod"}}`}), reply("ok"))
	ag := newAgent(t, sp, use(t, mcp.Options{Discovery: mcp.Search, Servers: []mcp.Server{{Name: "t", URL: ts.URL()}}}),
		kernel.WithApprovalRules(mcp.ApprovalRules(kernel.ApprovalRule{ID: "deploys", Tool: "mcp__t__deploy", Action: kernel.RuleAsk,
			When: map[string]kernel.Condition{"text": {Eq: json.RawMessage(`"prod"`)}}})...),
		kernel.WithApprover(func(_ context.Context, req kernel.ApprovalRequest) (kernel.ApprovalDecision, error) {
			reviewed = append(reviewed, req.Call.Name)
			return kernel.ApprovalDecision{Verdict: kernel.VerdictDeny, Reason: "not today"}, nil
		}))
	s, err := ag.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ask(t, s, "go")
	if len(reviewed) != 1 || reviewed[0] != "mcp_call_tool" || ran.Load() {
		t.Fatalf("reviewed %v, ran %v", reviewed, ran.Load())
	}
	if r := toolResults(s)["d1"]; r.Outcome != message.OutcomeDenied {
		t.Fatalf("result %+v", r)
	}
}

// TestTTLRefresh: a server that announces no list changes but gives its list a ttl is listed again when the ttl runs out.
func TestTTLRefresh(t *testing.T) {
	srv := sdk.NewServer(&sdk.Implementation{Name: "t", Version: "1"}, &sdk.ServerOptions{
		Capabilities: &sdk.ServerCapabilities{Tools: &sdk.ToolCapabilities{}},
		SetCacheable: func(_ context.Context, _ sdk.Request, c *sdk.Cacheable) { c.TTLMs = 200 },
	})
	srv.AddTool(&sdk.Tool{Name: "one", Description: "One.", InputSchema: objectSchema}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) { return textResult("1"), nil })
	hs := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, &sdk.StreamableHTTPOptions{Stateless: true}))
	t.Cleanup(hs.Close)
	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, reply("a"), reply("b"))
	ag := newAgent(t, sp, use(t, mcp.Options{Servers: []mcp.Server{{Name: "t", URL: hs.URL}}}))
	s, err := ag.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	rec := record(t, s)
	ask(t, s, "first")
	srv.AddTool(&sdk.Tool{Name: "two", Description: "Two.", InputSchema: objectSchema}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) { return textResult("2"), nil })
	waitFor(t, "the ttl refresh", 5*time.Second, func() bool { return len(rec.of("registry.changed")) > 0 })
	ask(t, s, "second")
	if names := toolNames(sp.Requests()[1]); !slices.Contains(names, "mcp__t__two") {
		t.Fatalf("tools %v", names)
	}
}

// TestProtocolVersion: a server that answers server/discover with a null id is unreachable on the newest version and works on an older one.
func TestProtocolVersion(t *testing.T) {
	nullID := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), `"server/discover"`) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32601,"message":"Method server/discover not found"}}`))
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			h.ServeHTTP(w, r)
		})
	}
	ts := newTestServer(t, "", nullID)
	ts.add("echo", "Repeat text.", func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return textResult(argText(req)), nil
	})
	for version, reachable := range map[string]bool{"": false, "2025-11-25": true} {
		sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, reply("ok"))
		ag := newAgent(t, sp, use(t, mcp.Options{ConnectTimeout: 2 * time.Second, Servers: []mcp.Server{{Name: "t", URL: ts.URL(), ProtocolVersion: version}}}))
		s, err := ag.NewSession(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		ask(t, s, "hi")
		if got := slices.Contains(toolNames(sp.Requests()[0]), "mcp__t__echo"); got != reachable {
			t.Errorf("version %q: tools registered %v, want %v", version, got, reachable)
		}
	}
	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages)
	if _, err := kernel.New(kernel.WithModel(provider.ModelRef{Provider: sp, Model: "m"}),
		use(t, mcp.Options{Servers: []mcp.Server{{Name: "t", URL: ts.URL(), ProtocolVersion: "1999-01-01"}}})); err == nil {
		t.Error("an unsupported protocol version was accepted")
	}
}
