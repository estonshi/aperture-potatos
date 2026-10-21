package mcp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/ext/mcp"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
	"github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// oauthServer is an MCP server behind bearer tokens plus the authorization server that issues them.
type oauthServer struct {
	base      string
	tokens    atomic.Int32
	refreshes atomic.Int32
	srv       *sdk.Server
}

func newOAuthServer(t *testing.T) *oauthServer {
	o := &oauthServer{}
	o.srv = sdk.NewServer(&sdk.Implementation{Name: "sec", Version: "1"}, nil)
	o.srv.AddTool(&sdk.Tool{Name: "echo", Description: "Repeat.", InputSchema: objectSchema}, func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return textResult("secure:" + argText(req)), nil
	})
	mux := http.NewServeMux()
	verify := func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		if token != "tok-1" {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{Expiration: time.Now().Add(time.Hour)}, nil
	}
	h := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return o.srv }, nil)
	mux.Handle("/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.RequireBearerToken(verify, &auth.RequireBearerTokenOptions{ResourceMetadataURL: o.base + "/.well-known/oauth-protected-resource/mcp"})(h).ServeHTTP(w, r)
	}))
	mux.Handle("/.well-known/oauth-protected-resource/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{Resource: o.base + "/mcp", AuthorizationServers: []string{o.base}}).ServeHTTP(w, r)
	}))
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": o.base, "authorization_endpoint": o.base + "/authorize", "token_endpoint": o.base + "/token",
			"response_types_supported": []string{"code"}, "code_challenge_methods_supported": []string{"S256"}, "grant_types_supported": []string{"authorization_code", "refresh_token"}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") == "refresh_token" && r.Form.Get("refresh_token") == "r1" {
			o.refreshes.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-1", "token_type": "Bearer", "expires_in": 3600, "refresh_token": "r1"})
			return
		}
		if r.Form.Get("code") != "c1" || r.Form.Get("code_verifier") == "" || r.Form.Get("resource") != o.base+"/mcp" {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		o.tokens.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-1", "token_type": "Bearer", "expires_in": 3600, "refresh_token": "r1"})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	o.base = ts.URL
	return o
}

type memTokens struct {
	mu   sync.Mutex
	m    map[string]*mcp.Grant
	keys []mcp.TokenKey
}

func (s *memTokens) Load(_ context.Context, k mcp.TokenKey) (*mcp.Grant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[k.Server], nil
}

func (s *memTokens) Save(_ context.Context, k mcp.TokenKey, g *mcp.Grant) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]*mcp.Grant{}
	}
	s.m[k.Server] = g
	s.keys = append(s.keys, k)
	return nil
}

// sessionRecorder catches a session's events from its creation, which an install at session scope publishes before NewSession returns.
type sessionRecorder struct {
	mu  sync.Mutex
	evs []event.Event
}

func (r *sessionRecorder) handle(_ context.Context, ev event.Event) error {
	r.mu.Lock()
	r.evs = append(r.evs, ev)
	r.mu.Unlock()
	return nil
}

func (r *sessionRecorder) all() []event.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]event.Event(nil), r.evs...)
}

// TestOAuthFlow: a 401 starts the authorization-code flow with PKCE and the resource indicator, the token is stored, and a second session reuses it without asking the user.
func TestOAuthFlow(t *testing.T) {
	o := newOAuthServer(t)
	store := &memTokens{}
	var asked atomic.Int32
	authorize := func(_ context.Context, authURL string) (string, string, string, error) {
		asked.Add(1)
		u, _ := url.Parse(authURL)
		q := u.Query()
		if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" || q.Get("resource") != o.base+"/mcp" || q.Get("client_id") != "client-1" {
			t.Errorf("authorization URL %s", authURL)
		}
		return "c1", q.Get("state"), "", nil
	}
	server := func() mcp.Server {
		return mcp.Server{Name: "sec", URL: o.base + "/mcp", OAuth: &mcp.OAuth{ClientID: "client-1", RedirectURL: "http://127.0.0.1/cb", Authorize: authorize, Tokens: store, AllowInsecure: true}}
	}
	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, calls(call{"a1", "mcp__sec__echo", `{"text":"x"}`}), reply("ok"), calls(call{"a2", "mcp__sec__echo", `{"text":"y"}`}), reply("ok"))
	ag := newAgent(t, sp)
	rec := &sessionRecorder{}
	first := mcp.New(mcp.Options{Servers: []mcp.Server{server()}})
	t.Cleanup(func() { _ = first.Close(context.Background()) })
	s1, err := ag.NewSession(t.Context(), kernel.SessionUse(first), kernel.WithSessionFunc(rec.handle))
	if err != nil {
		t.Fatal(err)
	}
	ask(t, s1, "go")
	if r := toolResults(s1)["a1"]; r.IsError || resultText(r) != "secure:x" {
		t.Fatalf("result %+v %q", r, resultText(r))
	}
	if asked.Load() != 1 || o.tokens.Load() != 1 {
		t.Fatalf("asked %d, tokens %d", asked.Load(), o.tokens.Load())
	}
	g := store.m["sec"]
	if g == nil || g.Token.AccessToken != "tok-1" || g.TokenURL != o.base+"/token" || g.ClientID != "client-1" {
		t.Fatalf("stored grant %+v", g)
	}
	var waiting bool
	for _, ev := range rec.all() {
		b, _ := json.Marshal(ev)
		if strings.Contains(string(b), "tok-1") || strings.Contains(string(b), `"c1"`) || strings.Contains(string(b), `"r1"`) {
			t.Fatalf("a credential in an event: %s", b)
		}
		if a, ok := ev.Data.(*mcp.AuthRequiredEvent); ok && a.State == "waiting" && strings.HasPrefix(a.AuthURL, o.base+"/authorize?") {
			waiting = true
		}
	}
	if !waiting {
		t.Fatal("no mcp.auth_required{waiting} with the link")
	}
	second := mcp.New(mcp.Options{Servers: []mcp.Server{server()}})
	t.Cleanup(func() { _ = second.Close(context.Background()) })
	s2, err := ag.NewSession(t.Context(), kernel.SessionUse(second))
	if err != nil {
		t.Fatal(err)
	}
	ask(t, s2, "again")
	if r := toolResults(s2)["a2"]; r.IsError || resultText(r) != "secure:y" {
		t.Fatalf("second result %+v %q", r, resultText(r))
	}
	if asked.Load() != 1 {
		t.Fatalf("the stored grant was not reused: asked %d times", asked.Load())
	}
}

// TestCallbackAuthorizer: the redirect is matched by state, once, and a state nobody waits for is refused.
func TestCallbackAuthorizer(t *testing.T) {
	cb := mcp.NewCallbackAuthorizer()
	type out struct {
		code, state, iss string
		err              error
	}
	done := make(chan out, 1)
	go func() {
		c, s, i, err := cb.Authorize(t.Context(), "https://as.example/authorize?state=s1&client_id=x")
		done <- out{c, s, i, err}
	}()
	time.Sleep(50 * time.Millisecond)
	hit := func(q string) int {
		w := httptest.NewRecorder()
		cb.ServeHTTP(w, httptest.NewRequest("GET", "/cb?"+q, nil))
		return w.Code
	}
	if code := hit("code=x&state=other"); code != http.StatusBadRequest {
		t.Fatalf("unknown state answered %d", code)
	}
	if code := hit("code=c9&state=s1&iss=https%3A%2F%2Fas.example"); code != http.StatusOK {
		t.Fatalf("matching state answered %d", code)
	}
	got := <-done
	if got.err != nil || got.code != "c9" || got.state != "s1" || got.iss != "https://as.example" {
		t.Fatalf("got %+v", got)
	}
	if code := hit("code=c9&state=s1"); code != http.StatusBadRequest {
		t.Fatalf("a used state answered %d", code)
	}
}

// TestLoopbackAuthorizer: the browser opener gets the link, and the redirect to the loopback port completes the sign-in.
func TestLoopbackAuthorizer(t *testing.T) {
	var opened string
	var la *mcp.LoopbackAuthorizer
	la, err := mcp.NewLoopbackAuthorizer(func(u string) error {
		opened = u
		go func() {
			resp, err := http.Get(la.RedirectURL() + "?code=c2&state=s2")
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer la.Close()
	if !strings.HasPrefix(la.RedirectURL(), "http://127.0.0.1:") {
		t.Fatalf("redirect %s", la.RedirectURL())
	}
	code, state, _, err := la.Authorize(t.Context(), "https://as.example/authorize?state=s2")
	if err != nil || code != "c2" || state != "s2" || opened != "https://as.example/authorize?state=s2" {
		t.Fatalf("code %q state %q opened %q err %v", code, state, opened, err)
	}
}

// TestSessionHeaders: a session-scope install sends that session's headers and closes its connection with the session.
func TestSessionHeaders(t *testing.T) {
	var mu sync.Mutex
	var users []string
	deletes := atomic.Int32{}
	ts := newTestServer(t, "", func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			users = append(users, r.Header.Get("X-Acting-User"))
			mu.Unlock()
			if r.Method == http.MethodDelete {
				deletes.Add(1)
			}
			h.ServeHTTP(w, r)
		})
	})
	ts.add("echo", "Repeat.", func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return textResult(argText(req)), nil
	})
	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, calls(call{"h1", "mcp__t__echo", `{"text":"x"}`}), reply("ok"))
	ag := newAgent(t, sp)
	s, err := ag.NewSession(t.Context(), kernel.SessionUse(mcp.New(mcp.Options{Servers: []mcp.Server{{Name: "t", URL: ts.URL(), Headers: map[string]string{"X-Acting-User": "ann@example.com"}}}})))
	if err != nil {
		t.Fatal(err)
	}
	ask(t, s, "go")
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the connection's DELETE", func() bool { return deletes.Load() > 0 })
	mu.Lock()
	defer mu.Unlock()
	for _, u := range users {
		if u != "ann@example.com" {
			t.Fatalf("a request without the session's header: %q", users)
		}
	}
}
