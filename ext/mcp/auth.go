package mcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

const (
	maxAuthFailures = 3 // the spec asks clients to bound repeated authorization attempts
	authWait        = 10 * time.Minute
)

// OAuth runs the MCP authorization flow (OAuth 2.1) for one HTTP server; the SDK does discovery, PKCE, resource indicators and iss checks.
type OAuth struct {
	ClientMetadataURL      string // a Client ID Metadata Document URL, which the spec prefers
	ClientID, ClientSecret string // a pre-registered client
	DynamicRegistration    bool   // RFC 7591, deprecated by the spec and kept as a fallback
	ClientName             string // shown by the authorization server when registering dynamically
	RedirectURL            string // required; the host serves the callback, see CallbackAuthorizer and LoopbackAuthorizer

	// Authorize puts authURL in front of the user and returns what the authorization server sent to RedirectURL.
	Authorize func(ctx context.Context, authURL string) (code, state, iss string, err error)

	Tokens        TokenStore              // keeps grants across restarts; the host stores them securely
	Scopes        func([]string) []string // narrows or extends the scopes discovered from the server
	OfflineAccess bool                    // ask for a refresh token when the server offers offline_access
	HTTPClient    *http.Client            // for discovery, registration and token requests; replaces the SSRF guard, e.g. with an egress proxy
	AllowInsecure bool                    // development only: http and private addresses in OAuth URLs
}

// Grant is what a TokenStore keeps: the token and what refreshing it needs.
type Grant struct {
	Token        *oauth2.Token
	TokenURL     string
	ClientID     string
	ClientSecret string
}

// TokenKey names a grant: a store keys by every field it cares about, so users and resources never share a token.
type TokenKey struct {
	Server   string            // Server.Name
	Resource string            // Server.URL, the resource the token was issued for (RFC 8707), so a server moved to another URL starts over
	Session  string            // the session's id when the extension is installed on one session; empty at agent scope, where every session shares the grant
	Labels   map[string]string // that session's labels, such as user_id, for a store that keys by user rather than by session
}

type TokenStore interface {
	Load(ctx context.Context, key TokenKey) (*Grant, error) // nil, nil when there is none
	Save(ctx context.Context, key TokenKey, g *Grant) error
}

func (o *OAuth) validate() error {
	switch {
	case o.RedirectURL == "":
		return errors.New("OAuth.RedirectURL is required")
	case o.Authorize == nil:
		return errors.New("OAuth.Authorize is required")
	case o.ClientMetadataURL == "" && o.ClientID == "" && !o.DynamicRegistration:
		return errors.New("OAuth needs ClientMetadataURL, ClientID or DynamicRegistration")
	}
	return nil
}

var errAuthStopped = errors.New("mcp: authorization failed too many times")

// authGate is the SDK's authorization-code handler with the host's side added: URL checks, events, the retry bound and token storage.
type authGate struct {
	c   *conn
	o   *OAuth
	h   *auth.AuthorizationCodeHandler
	err error
	key TokenKey

	mu      sync.Mutex
	fails   int
	pending bool
}

func newAuthGate(c *conn, o *OAuth) *authGate {
	g := &authGate{c: c, o: o, key: TokenKey{Server: c.srv.Name, Resource: c.srv.URL}}
	if s := c.in.session; s != nil {
		info := s.Info()
		g.key.Session, g.key.Labels = info.SessionID, info.Labels
	}
	client := o.HTTPClient
	if client == nil {
		client = guardedClient(o.AllowInsecure)
	}
	cfg := &auth.AuthorizationCodeHandlerConfig{
		RedirectURL:              o.RedirectURL,
		AuthorizationCodeFetcher: g.fetch,
		ScopeFilter:              o.Scopes,
		RequestRefreshToken:      o.OfflineAccess,
		Client:                   client,
		NewTokenSource:           g.newTokenSource,
	}
	if o.ClientMetadataURL != "" {
		cfg.ClientIDMetadataDocumentConfig = &auth.ClientIDMetadataDocumentConfig{URL: o.ClientMetadataURL}
	}
	if o.ClientID != "" {
		cc := &oauthex.ClientCredentials{ClientID: o.ClientID}
		if o.ClientSecret != "" {
			cc.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: o.ClientSecret}
		}
		cfg.PreregisteredClient = cc
	}
	if o.DynamicRegistration {
		grants := []string{"authorization_code"}
		if o.OfflineAccess {
			grants = append(grants, "refresh_token")
		}
		cfg.DynamicClientRegistrationConfig = &auth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{
			RedirectURIs: []string{o.RedirectURL}, GrantTypes: grants, ClientName: o.ClientName, TokenEndpointAuthMethod: "none"}}
	}
	if o.Tokens != nil {
		if gr, err := o.Tokens.Load(c.ctx, g.key); err == nil && gr != nil && gr.Token != nil {
			cfg.InitialTokenSource = g.restore(gr, client)
		}
	}
	g.h, g.err = auth.NewAuthorizationCodeHandler(cfg)
	return g
}

// restore rebuilds a stored grant's token source, refreshing when the stored grant says how.
func (g *authGate) restore(gr *Grant, client *http.Client) oauth2.TokenSource {
	if gr.TokenURL == "" || gr.Token.RefreshToken == "" {
		return oauth2.StaticTokenSource(gr.Token)
	}
	cfg := &oauth2.Config{ClientID: gr.ClientID, ClientSecret: gr.ClientSecret, Endpoint: oauth2.Endpoint{TokenURL: gr.TokenURL}}
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, client)
	return &savingSource{g: g, cfg: cfg, base: cfg.TokenSource(ctx, gr.Token), last: gr.Token.AccessToken}
}

func (g *authGate) newTokenSource(ctx context.Context, cfg *oauth2.Config, tok *oauth2.Token) (oauth2.TokenSource, error) {
	s := &savingSource{g: g, cfg: cfg, base: cfg.TokenSource(ctx, tok)}
	s.save(tok)
	return s, nil
}

// savingSource stores every new token it hands out, so a refreshed grant survives a restart.
type savingSource struct {
	g    *authGate
	cfg  *oauth2.Config
	base oauth2.TokenSource

	mu   sync.Mutex
	last string
}

func (s *savingSource) Token() (*oauth2.Token, error) {
	t, err := s.base.Token()
	if err != nil {
		return nil, err
	}
	s.save(t)
	return t, nil
}

func (s *savingSource) save(t *oauth2.Token) {
	s.mu.Lock()
	fresh := t.AccessToken != s.last
	s.last = t.AccessToken
	s.mu.Unlock()
	if !fresh || s.g.o.Tokens == nil {
		return
	}
	gr := &Grant{Token: t, TokenURL: s.cfg.Endpoint.TokenURL, ClientID: s.cfg.ClientID, ClientSecret: s.cfg.ClientSecret}
	if err := s.g.o.Tokens.Save(context.WithoutCancel(s.g.c.ctx), s.g.key, gr); err != nil {
		s.g.c.in.r.Agent().Logger().Warn("mcp: token not saved", "server", s.g.c.srv.Name, "error", err) // the error text is the store's, never the token
	}
}

func (g *authGate) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	if g.err != nil {
		return nil, g.err
	}
	return g.h.TokenSource(ctx)
}

func (g *authGate) Authorize(ctx context.Context, req *http.Request, resp *http.Response) error {
	if g.err != nil {
		return g.err
	}
	if g.blocked() {
		return errAuthStopped
	}
	err := g.h.Authorize(ctx, req, resp)
	g.mu.Lock()
	if err != nil && ctx.Err() == nil {
		g.fails++
	} else if err == nil {
		g.fails = 0
	}
	stopped := g.fails >= maxAuthFailures
	g.mu.Unlock()
	if err != nil && stopped {
		g.c.in.emit(&AuthRequiredEvent{Server: g.c.srv.Name, State: "failed"})
	}
	return err
}

func (g *authGate) blocked() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.fails >= maxAuthFailures
}

func (g *authGate) waiting() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pending
}

// fetch hands the authorization URL to the host once it passes the scheme check, and waits for the user.
func (g *authGate) fetch(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
	if err := checkAuthURL(args.URL, g.o.AllowInsecure); err != nil {
		return nil, err
	}
	g.mu.Lock()
	g.pending = true
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.pending = false
		g.mu.Unlock()
	}()
	g.c.in.emit(&AuthRequiredEvent{Server: g.c.srv.Name, State: "waiting", AuthURL: args.URL})
	code, state, iss, err := g.o.Authorize(ctx, args.URL)
	if err != nil {
		return nil, err
	}
	return &auth.AuthorizationResult{Code: code, State: state, Iss: iss}, nil
}

// checkAuthURL lets only https through, and http when the host opted out for development, so no javascript:, data: or file: URL reaches a browser.
func checkAuthURL(raw string, allowInsecure bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("mcp: malformed authorization URL")
	}
	if u.Scheme == "https" || u.Scheme == "http" && allowInsecure {
		return nil
	}
	return fmt.Errorf("mcp: authorization URL scheme %q refused", u.Scheme)
}

// guardedClient is the SSRF guard of the security best practices: https only, and no private, loopback or link-local address at dial time, redirects included.
func guardedClient(allowInsecure bool) *http.Client {
	if allowInsecure {
		return &http.Client{Timeout: 30 * time.Second}
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip, err := netip.ParseAddr(host)
		if err != nil {
			return err
		}
		if blockedIP(ip) {
			return fmt.Errorf("mcp: OAuth request to %s refused", ip)
		}
		return nil
	}}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil // a proxy would be dialed instead of the target; set OAuth.HTTPClient to use an egress proxy
	tr.DialContext = dialer.DialContext
	return &http.Client{Timeout: 30 * time.Second, Transport: httpsOnly{tr}, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("mcp: too many redirects")
		}
		if req.URL.Scheme != "https" {
			return errors.New("mcp: redirect off https refused")
		}
		return nil
	}}
}

type httpsOnly struct{ base http.RoundTripper }

func (h httpsOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "https" {
		return nil, fmt.Errorf("mcp: OAuth request over %s refused", r.URL.Scheme)
	}
	return h.base.RoundTrip(r)
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func blockedIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() || cgnat.Contains(ip)
}

// CallbackAuthorizer completes logins on a server host: mount it at RedirectURL, show the auth_url of mcp.auth_required to the user, and Authorize returns once their browser comes back.
type CallbackAuthorizer struct {
	mu    sync.Mutex
	waits map[string]chan callback
}

type callback struct {
	code, state, iss string
	err              error
}

func NewCallbackAuthorizer() *CallbackAuthorizer {
	return &CallbackAuthorizer{waits: map[string]chan callback{}}
}

// Authorize waits up to ten minutes for the callback whose state matches the one in authURL.
func (a *CallbackAuthorizer) Authorize(ctx context.Context, authURL string) (string, string, string, error) {
	u, err := url.Parse(authURL)
	if err != nil {
		return "", "", "", err
	}
	state := u.Query().Get("state")
	if state == "" {
		return "", "", "", errors.New("mcp: authorization URL has no state")
	}
	ch := make(chan callback, 1)
	a.mu.Lock()
	a.waits[state] = ch
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.waits, state)
		a.mu.Unlock()
	}()
	t := time.NewTimer(authWait)
	defer t.Stop()
	select {
	case cb := <-ch:
		return cb.code, cb.state, cb.iss, cb.err
	case <-ctx.Done():
		return "", "", "", ctx.Err()
	case <-t.C:
		return "", "", "", errors.New("mcp: the user did not finish signing in")
	}
}

// ServeHTTP takes the redirect from the authorization server; a state nobody waits for, or one already used, is refused.
func (a *CallbackAuthorizer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	a.mu.Lock()
	ch := a.waits[state]
	delete(a.waits, state)
	a.mu.Unlock()
	if state == "" || ch == nil {
		http.Error(w, "unknown or expired sign-in", http.StatusBadRequest)
		return
	}
	if q.Get("error") != "" {
		ch <- callback{err: errors.New("mcp: the authorization server refused the sign-in")} // its error text is not shown, since iss is not checked on this path
		http.Error(w, "sign-in failed", http.StatusBadRequest)
		return
	}
	ch <- callback{code: q.Get("code"), state: state, iss: q.Get("iss")}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("Signed in. You can close this window."))
}

// LoopbackAuthorizer completes logins on a desktop or CLI host: it listens on a loopback port and opens the system browser.
type LoopbackAuthorizer struct {
	cb       *CallbackAuthorizer
	srv      *http.Server
	open     func(string) error
	redirect string
}

// NewLoopbackAuthorizer listens on 127.0.0.1 at a free port; open is OpenBrowser when nil.
func NewLoopbackAuthorizer(open func(url string) error) (*LoopbackAuthorizer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	if open == nil {
		open = OpenBrowser
	}
	l := &LoopbackAuthorizer{cb: NewCallbackAuthorizer(), open: open, redirect: "http://" + ln.Addr().String() + "/callback"}
	mux := http.NewServeMux()
	mux.Handle("/callback", l.cb)
	l.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = l.srv.Serve(ln) }()
	return l, nil
}

// RedirectURL is the value for OAuth.RedirectURL.
func (l *LoopbackAuthorizer) RedirectURL() string { return l.redirect }

func (l *LoopbackAuthorizer) Authorize(ctx context.Context, authURL string) (string, string, string, error) {
	if err := l.open(authURL); err != nil {
		return "", "", "", err
	}
	return l.cb.Authorize(ctx, authURL)
}

func (l *LoopbackAuthorizer) Close() error { return l.srv.Close() }

// OpenBrowser opens url with the platform's own opener and the URL as a plain argument, never through a shell.
func OpenBrowser(u string) error {
	if err := checkAuthURL(u, true); err != nil {
		return err
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	return cmd.Start()
}
