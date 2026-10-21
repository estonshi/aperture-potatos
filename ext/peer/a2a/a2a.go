// Package a2a is a client for remote A2A agents, v0.3 and v1.0 over JSON-RPC or HTTP+JSON, seen through the peer.Peer interface.
package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/estonshi/aperture-potatos/ext/peer"
	"github.com/estonshi/aperture-potatos/provider"
)

// Doer carries every request, card fetch included, so auth, signing and proxies live in one place; *http.Client satisfies it.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

type Binding string

const (
	BindingJSONRPC  Binding = "JSONRPC"
	BindingHTTPJSON Binding = "HTTP+JSON"
)

type Option func(*config)

type config struct {
	doer    Doer
	version string
	binding Binding
	headers map[string]string
	hosts   []string
}

func WithDoer(d Doer) Option { return func(c *config) { c.doer = d } }

// WithProtocolVersion keeps only the card's interfaces of "0.3" or "1.0".
func WithProtocolVersion(v string) Option { return func(c *config) { c.version = v } }

func WithBinding(b Binding) Option { return func(c *config) { c.binding = b } }

// WithHeaders adds headers to every request, the card fetch included; A2A-Version, Content-Type and Accept are the client's own.
func WithHeaders(h map[string]string) Option { return func(c *config) { c.headers = h } }

// WithAllowedHosts lets the card's interfaces live on these hosts too, as "host" or "host:port"; without it they must be on the card's own host, since every request carries WithHeaders.
func WithAllowedHosts(hosts ...string) Option { return func(c *config) { c.hosts = hosts } }

var (
	ErrNoSupportedInterface = errors.New("a2a: the agent card offers no usable interface")
	// ErrInterfaceHost reports a card whose interfaces are all on hosts the client may not send its headers to.
	ErrInterfaceHost = errors.New("a2a: the agent card's interfaces are on another host; allow it with WithAllowedHosts")
)

const (
	cardPath       = "/.well-known/agent-card.json"
	requestTimeout = 30 * time.Second
	idleCheck      = 5 * time.Minute
	pollStart      = time.Second
	pollMax        = 30 * time.Second
	maxReconnects  = 3
	maxBody        = 32 << 20
)

// NewRemotePeer fetches the agent card and fixes the interface up front, so Card never does I/O.
func NewRemotePeer(ctx context.Context, baseURL string, opts ...Option) (peer.Peer, error) {
	c := config{doer: provider.DefaultClient}
	for _, o := range opts {
		o(&c)
	}
	raw, err := fetchCard(ctx, c, baseURL)
	if err != nil {
		return nil, err
	}
	card, cands, err := parseCard(raw)
	if err != nil {
		return nil, err
	}
	if cands, err = onAllowedHosts(cands, baseURL, c.hosts); err != nil {
		return nil, err
	}
	pick, err := choose(cands, c.version, c.binding)
	if err != nil {
		return nil, err
	}
	card.URL = pick.url
	r := &remote{cfg: c, card: card, iface: pick}
	if pick.version == v03 {
		r.codec = codec03{proto: pick.binding == BindingHTTPJSON}
	} else {
		r.codec = codec10{}
	}
	if pick.binding == BindingJSONRPC {
		r.wire = &jsonrpc{r: r}
	} else {
		r.wire = &httpjson{r: r}
	}
	return r, nil
}

// onAllowedHosts keeps the interfaces on the card's host or an allowed one, over https when the card came over https, so a card cannot send the client's credentials or requests anywhere else.
func onAllowedHosts(cands []candidate, baseURL string, hosts []string) ([]candidate, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	allowed := func(u *url.URL) bool {
		if base.Scheme == "https" && u.Scheme != "https" || u.Scheme != "https" && u.Scheme != "http" {
			return false
		}
		if strings.EqualFold(u.Host, base.Host) {
			return true
		}
		return slices.ContainsFunc(hosts, func(h string) bool { return strings.EqualFold(h, u.Host) || strings.EqualFold(h, u.Hostname()) })
	}
	var keep []candidate
	for _, c := range cands {
		if u, err := url.Parse(c.url); err == nil && allowed(u) {
			keep = append(keep, c)
		}
	}
	if len(keep) == 0 && len(cands) > 0 {
		return nil, ErrInterfaceHost
	}
	return keep, nil
}

func fetchCard(ctx context.Context, c config, baseURL string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+cardPath, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.doer.Do(req)
	if err != nil {
		return nil, fmt.Errorf("a2a: fetching the agent card: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("a2a: reading the agent card: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("a2a: fetching the agent card: HTTP %d", resp.StatusCode)
	}
	if !json.Valid(body) {
		return nil, errors.New("a2a: the agent card is not valid JSON")
	}
	return body, nil
}

// remote is one agent behind the interface chosen from its card.
type remote struct {
	cfg    config
	card   peer.Card
	iface  candidate
	codec  codec
	wire   wire
	rpcID  atomic.Int64
	getSub atomic.Bool // the peer answered a POST subscribe with 405, so subscribe uses GET
}

func (r *remote) Card() peer.Card         { return r.card }
func (r *remote) ProtocolVersion() string { return r.iface.version }

// headers sets the client's own headers after the caller's, so a caller cannot change the version it speaks.
func (r *remote) headers(req *http.Request, contentType, accept string) {
	for k, v := range r.cfg.headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("A2A-Version", r.iface.version)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", accept)
}
