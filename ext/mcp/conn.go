package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	firstBackoff = time.Second
	maxBackoff   = 5 * time.Minute
)

// conn is one server's connection for one install, reconnecting until the install closes.
type conn struct {
	in      *install
	srv     Server
	client  *sdk.Client
	oauth   auth.OAuthHandler // kept across reconnects, so a token outlives the connection
	gate    *authGate         // nil unless the extension runs the OAuth flow
	ctx     context.Context
	cancel  context.CancelFunc
	first   chan struct{} // closed once the first attempt ended either way
	once    sync.Once
	refresh chan struct{}
	stopped chan struct{}

	mu           sync.Mutex
	cs           *sdk.ClientSession
	state        string
	known        bool // tools were listed at least once
	instructions string
	tools        []*toolEntry
	prompts      []*promptEntry // only when the host enabled them

	calls sync.Map // progress token → *callState
}

// callState is one call in flight, which progress notices and elicitations are routed to.
type callState struct {
	tc   *kernel.ToolContext
	tool *toolEntry
}

type callKey struct{}

// toolEntry is one server tool under the name the model uses.
type toolEntry struct {
	name    string // mcp__<server>__<tool>
	mcpName string
	server  string
	title   string
	desc    string // full description, sanitized
	c       *conn
	spec    kernel.ToolSpec
	summary string
	input   json.RawMessage
	output  json.RawMessage

	once     sync.Once
	inSchema *jsonschema.Resolved
	outCheck *jsonschema.Resolved
}

func newConn(in *install, s Server) *conn {
	c := &conn{in: in, srv: s, first: make(chan struct{}), refresh: make(chan struct{}, 1), stopped: make(chan struct{})}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	opts := &sdk.ClientOptions{
		Capabilities:                &sdk.ClientCapabilities{Elicitation: &sdk.ElicitationCapabilities{Form: &sdk.FormElicitationCapabilities{}, URL: &sdk.URLElicitationCapabilities{}}},
		ElicitationHandler:          c.elicit,
		ToolListChangedHandler:      func(context.Context, *sdk.ToolListChangedRequest) { c.poke() },
		ProgressNotificationHandler: c.progress,
	}
	if s.Prompts {
		opts.PromptListChangedHandler = func(context.Context, *sdk.PromptListChangedRequest) { c.poke() } // the SDK subscribes only when a handler is set
	}
	c.client = sdk.NewClient(&sdk.Implementation{Name: "potatos-mcp", Version: "0.2.0"}, opts)
	switch {
	case s.OAuthHandler != nil:
		c.oauth = s.OAuthHandler
	case s.OAuth != nil:
		c.gate = newAuthGate(c, s.OAuth)
		c.oauth = c.gate
	}
	return c
}

func (c *conn) poke() {
	select {
	case c.refresh <- struct{}{}:
	default:
	}
}

// run connects, lists, and waits for the connection to end, then starts over with backoff.
func (c *conn) run() {
	defer close(c.stopped)
	backoff := firstBackoff
	for c.ctx.Err() == nil {
		c.setState("connecting", nil)
		var cs *sdk.ClientSession
		tr, err := c.transport()
		if err == nil {
			cs, err = c.client.Connect(c.ctx, tr, &sdk.ClientSessionOptions{ProtocolVersion: c.srv.ProtocolVersion})
		}
		if err != nil {
			c.in.r.Agent().Logger().Warn("mcp: connection failed", "server", c.srv.Name, "error", err)
			c.setState("failed", err)
			c.once.Do(func() { close(c.first) })
			if !sleep(c.ctx, backoff) {
				return
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		backoff = firstBackoff
		c.mu.Lock()
		back := c.known // listed before, so this is a reconnection the sessions were told about
		c.mu.Unlock()
		ttl, err := c.list(cs)
		if err != nil {
			_ = cs.Close()
			c.setState("failed", err)
			c.once.Do(func() { close(c.first) })
			if !sleep(c.ctx, backoff) {
				return
			}
			continue
		}
		c.mu.Lock()
		c.cs = cs
		c.mu.Unlock()
		c.setState("ready", nil)
		c.once.Do(func() { close(c.first) })
		c.in.reconcile()
		if back && c.in.isReady() {
			c.in.say(text("reconnected", "name", c.srv.Name))
		}
		c.serve(cs, ttl)
	}
}

// serve keeps a live connection's tool list current until it ends.
func (c *conn) serve(cs *sdk.ClientSession, ttl time.Duration) {
	done := make(chan struct{})
	go func() {
		_ = cs.Wait()
		close(done)
	}()
	for {
		var tick <-chan time.Time
		if ttl > 0 {
			tick = time.After(ttl)
		}
		select {
		case <-c.ctx.Done():
			_ = cs.Close()
			return
		case <-done:
			c.lost(cs)
			return
		case <-c.refresh:
		case <-tick:
		}
		next, err := c.list(cs)
		if err != nil {
			if errors.Is(err, sdk.ErrConnectionClosed) {
				c.lost(cs)
				return
			}
			continue
		}
		ttl = next
		c.in.reconcile()
	}
}

// lost marks a connection gone; the tools stay registered and their calls fail until run reconnects.
func (c *conn) lost(cs *sdk.ClientSession) {
	c.mu.Lock()
	if c.cs != cs {
		c.mu.Unlock()
		return
	}
	c.cs = nil
	c.mu.Unlock()
	_ = cs.Close()
	c.setState("failed", sdk.ErrConnectionClosed)
	if c.in.isReady() {
		c.in.say(text("disconnected", "name", c.srv.Name))
	}
}

func (c *conn) close() {
	c.cancel()
	c.mu.Lock()
	cs := c.cs
	c.cs = nil
	c.mu.Unlock()
	if cs != nil {
		_ = cs.Close()
	}
	<-c.stopped
	c.setState("closed", nil)
}

func (c *conn) current() *sdk.ClientSession {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cs
}

func (c *conn) snapshot() (string, []*toolEntry, []*promptEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.instructions, c.tools, c.prompts, c.known
}

func (c *conn) setState(state string, err error) {
	c.mu.Lock()
	changed := c.state != state
	c.state = state
	n := len(c.tools)
	c.mu.Unlock()
	if !changed {
		return
	}
	ev := &ServerStatusEvent{Server: c.srv.Name, State: state, Tools: n}
	if err != nil {
		ev.Error = errorClass(err)
	}
	c.in.emit(ev)
}

func (c *conn) transport() (tr sdk.Transport, err error) {
	if c.srv.Command == nil {
		return &sdk.StreamableClientTransport{Endpoint: c.srv.URL, HTTPClient: withHeaders(c.srv.HTTPClient, c.srv.Headers), OAuthHandler: c.oauth}, nil
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("mcp server %q: Command panicked: %v", c.srv.Name, r) // the host's factory runs on the reconnect goroutine, which nothing else recovers
		}
	}()
	cmd := c.srv.Command()
	if cmd == nil {
		return nil, fmt.Errorf("mcp server %q: Command returned no command", c.srv.Name)
	}
	return &sdk.CommandTransport{Command: cmd}, nil
}

// list reads every page of the server's tools and keeps the valid ones; the ttl is when to list again.
func (c *conn) list(cs *sdk.ClientSession) (time.Duration, error) {
	var all []*sdk.Tool
	cursor, ttl := "", time.Duration(0)
	for {
		res, err := cs.ListTools(c.ctx, &sdk.ListToolsParams{Cursor: cursor})
		if err != nil {
			return 0, err
		}
		if cursor == "" && res.TTLMs > 0 {
			ttl = time.Duration(res.TTLMs) * time.Millisecond
		}
		all = append(all, res.Tools...)
		if res.NextCursor == "" || res.NextCursor == cursor {
			break
		}
		cursor = res.NextCursor
	}
	var entries []*toolEntry
	for _, t := range all {
		if te, ok := c.entry(t); ok {
			entries = append(entries, te)
		}
	}
	instr := ""
	if r := cs.InitializeResult(); r != nil {
		instr = clip(kernel.SanitizeForeign(r.Instructions), maxInstructions)
	}
	prompts, pttl := c.listPrompts(cs)
	if pttl > 0 && (ttl == 0 || pttl < ttl) {
		ttl = pttl
	}
	c.mu.Lock()
	c.tools, c.prompts, c.instructions, c.known = entries, prompts, instr, true
	c.mu.Unlock()
	return ttl, nil
}

var notToolChar = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// toolName maps a server's tool name onto the kernel's ^[a-zA-Z0-9_-]{1,64}$, hashing the tail of a name too long to keep.
func toolName(server, tool string) string {
	n := "mcp__" + server + "__" + notToolChar.ReplaceAllString(tool, "_")
	if len(n) <= 64 {
		return n
	}
	sum := sha256.Sum256([]byte(tool))
	return n[:57] + "_" + hex.EncodeToString(sum[:])[:6]
}

func (c *conn) entry(t *sdk.Tool) (*toolEntry, bool) {
	log := c.in.r.Agent().Logger()
	if t == nil || t.Name == "" {
		return nil, false
	}
	if len(c.srv.Tools) > 0 && !slices.Contains(c.srv.Tools, t.Name) || slices.Contains(c.srv.DenyTools, t.Name) {
		return nil, false
	}
	input, err := json.Marshal(t.InputSchema)
	var probe struct {
		Type string `json:"type"`
	}
	if err != nil || json.Unmarshal(input, &probe) != nil || probe.Type != "object" {
		log.Warn("mcp: tool skipped, its input schema is not an object schema", "server", c.srv.Name, "tool", t.Name)
		return nil, false
	}
	var output json.RawMessage
	if t.OutputSchema != nil {
		output, _ = json.Marshal(t.OutputSchema)
	}
	title := t.Title
	if title == "" && t.Annotations != nil {
		title = t.Annotations.Title
	}
	desc := kernel.SanitizeForeign(t.Description)
	full := desc
	if title != "" {
		full = kernel.SanitizeForeign(title) + "\n" + desc
	}
	te := &toolEntry{name: toolName(c.srv.Name, t.Name), mcpName: t.Name, server: c.srv.Name, title: title, desc: desc, c: c, input: input, output: output}
	te.summary = summary(desc, title)
	conc := kernel.Serial
	if c.srv.Trusted && t.Annotations != nil && t.Annotations.ReadOnlyHint {
		conc = kernel.Parallel
	}
	te.spec = kernel.ToolSpec{Name: te.name, Description: strings.TrimSpace(full), InputSchema: input, Concurrency: conc, Timeout: c.srv.Timeout, Background: c.srv.Background}
	return te, true
}

// summary is the first line of a description, or the title, short enough for one catalog line.
func summary(desc, title string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(desc), "\n")
	if line = strings.TrimSpace(line); line == "" {
		line = strings.TrimSpace(title)
	}
	return clip(line, maxSummary)
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// call runs one tool call; a canceled ctx returns its error, which the kernel reports as an interrupt or a timeout.
func (c *conn) call(ctx context.Context, tc *kernel.ToolContext, te *toolEntry, args json.RawMessage) (kernel.ToolResult, error) {
	cs := c.current()
	if cs == nil {
		if c.gate != nil && c.gate.waiting() {
			return errResult(text("sign_in", "tool", te.name, "server", c.srv.Name)), nil
		}
		return errResult(text("unavailable", "tool", te.name, "server", c.srv.Name)), nil
	}
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	p := &sdk.CallToolParams{Name: te.mcpName, Arguments: args}
	st := &callState{tc: tc, tool: te}
	token := tc.CallID()
	p.SetProgressToken(token)
	c.calls.Store(token, st)
	defer c.calls.Delete(token)
	res, err := cs.CallTool(context.WithValue(ctx, callKey{}, st), p)
	if err != nil {
		if ctx.Err() != nil {
			return kernel.ToolResult{}, ctx.Err()
		}
		return c.failure(cs, te, err), nil
	}
	return c.in.convert(res, te), nil
}

func (c *conn) failure(cs *sdk.ClientSession, te *toolEntry, err error) kernel.ToolResult {
	c.in.r.Agent().Logger().Warn("mcp: call failed", "server", c.srv.Name, "tool", te.mcpName, "error", err)
	if c.gate != nil && c.gate.blocked() {
		return errResult(text("sign_in", "tool", te.name, "server", c.srv.Name))
	}
	if unreachable(err) {
		c.lost(cs)
		return errResult(text("unavailable", "tool", te.name, "server", c.srv.Name))
	}
	return errResult(text("server_error", "tool", te.name, "server", c.srv.Name, "kind", errorClass(err)))
}

// unreachable says the request never got an answer from the server, as opposed to the server answering with an error.
func unreachable(err error) bool {
	var ue *url.Error
	var oe *net.OpError
	return errors.Is(err, sdk.ErrConnectionClosed) || errors.As(err, &ue) || errors.As(err, &oe) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)
}

// errorClass names a failure without the server's own text, which never reaches the model or the events.
func errorClass(err error) string {
	var we *jsonrpc.Error
	switch {
	case errors.As(err, &we) && we.Code == jsonrpc.CodeInvalidParams:
		return "invalid request"
	case errors.As(err, &we) && we.Code == jsonrpc.CodeMethodNotFound:
		return "unknown tool"
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.Is(err, sdk.ErrConnectionClosed):
		return "connection closed"
	}
	return "server error"
}

func (c *conn) progress(_ context.Context, req *sdk.ProgressNotificationClientRequest) {
	if req == nil || req.Params == nil {
		return
	}
	v, ok := c.calls.Load(fmt.Sprint(req.Params.ProgressToken))
	if !ok {
		return
	}
	data := map[string]any{"progress": req.Params.Progress}
	if req.Params.Total > 0 {
		data["total"] = req.Params.Total
	}
	v.(*callState).tc.Progress(req.Params.Message, data)
}

func (in *install) isReady() bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.ready
}

func errResult(s string) kernel.ToolResult { return kernel.ErrorResult("%s", s) }

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// headerTransport adds the server's static headers to every request.
type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (h *headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range h.headers {
		r.Header.Set(k, v)
	}
	return h.base.RoundTrip(r)
}

func withHeaders(c *http.Client, headers map[string]string) *http.Client {
	if c == nil {
		c = provider.DefaultClient
	}
	if len(headers) == 0 {
		return c
	}
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	out := *c
	out.Transport = &headerTransport{base: base, headers: headers}
	return &out
}
