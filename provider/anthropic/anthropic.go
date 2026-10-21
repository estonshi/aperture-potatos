// Package anthropic adapts the Anthropic Messages API to the provider SPI over raw HTTP and SSE.
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"net/http"
	"strings"

	"github.com/estonshi/aperture-potatos/internal/sse"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/internal/httpx"
)

const (
	defaultBaseURL = "https://api.anthropic.com"
	apiVersion     = "2023-06-01"
)

// Config configures a Provider; an empty APIKey sends no x-api-key, for a gateway that authenticates another way.
type Config struct {
	APIKey  string
	BaseURL string        // "" means https://api.anthropic.com; a gateway such as LiteLLM goes here
	Doer    provider.Doer // nil means provider.DefaultClient, which follows no redirect to another host; stream timeouts are the adapter's, so it needs no client timeout
	Betas   []string      // sent on every request, before the betas the adapter adds for the features a request uses
}

// Provider speaks the Messages API, and also resolves capabilities from the Models API and counts tokens, so the kernel needs neither configured separately.
type Provider struct {
	cfg      Config
	base     string
	endpoint string
}

// New makes no request, so a wrong key or base URL shows up on the first call.
func New(cfg Config) *Provider {
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = defaultBaseURL
	}
	if cfg.Doer == nil {
		cfg.Doer = provider.DefaultClient
	}
	return &Provider{cfg: cfg, base: base, endpoint: httpx.Endpoint(base)}
}

// API reports provider.APIAnthropicMessages.
func (p *Provider) API() provider.API { return provider.APIAnthropicMessages }

// Endpoint is the base URL's lowercased host, which tags native content so it is replayed only to the endpoint that produced it.
func (p *Provider) Endpoint() string { return p.endpoint }

// Stream sends one /v1/messages request and yields its events, with the capability notices for anything it had to adjust first.
func (p *Provider) Stream(ctx context.Context, req *provider.Request) iter.Seq2[provider.StreamEvent, error] {
	return func(yield func(provider.StreamEvent, error) bool) {
		r, err := p.render(req)
		if err != nil {
			yield(provider.StreamEvent{}, err)
			return
		}
		for _, n := range r.notices {
			if !yield(provider.StreamEvent{Kind: provider.SENotice, Notice: &n}, nil) {
				return
			}
		}
		resp, err := httpx.PostStream(ctx, p.cfg.Doer, provider.APIAnthropicMessages, p.base+"/v1/messages", r.header, r.body, req)
		if err != nil {
			yield(provider.StreamEvent{}, err)
			return
		}
		defer resp.Body.Close()
		d := newDecoder(req, p.endpoint, r.display, resp.Header)
		sr := sse.NewReader(resp.Body)
		for !d.done {
			ev, err := sr.Next()
			if err != nil {
				if errors.Is(err, io.EOF) && ctx.Err() == nil {
					yield(provider.StreamEvent{}, httpx.Incomplete(provider.APIAnthropicMessages, d.requestID))
				} else {
					yield(provider.StreamEvent{}, httpx.TransportError(ctx, provider.APIAnthropicMessages, err))
				}
				return
			}
			out, err := d.handle(ev)
			for _, e := range out {
				if !yield(e, nil) {
					return
				}
			}
			if err != nil {
				yield(provider.StreamEvent{}, err)
				return
			}
		}
	}
}

func (p *Provider) headers(extra http.Header, betas []string) http.Header {
	h := http.Header{}
	h.Set("content-type", "application/json")
	h.Set("accept", "text/event-stream")
	h.Set("anthropic-version", apiVersion)
	if p.cfg.APIKey != "" {
		h.Set("x-api-key", p.cfg.APIKey)
	}
	all := append(append([]string(nil), p.cfg.Betas...), betas...)
	for k, vs := range extra {
		if strings.EqualFold(k, "anthropic-beta") {
			for _, v := range vs {
				all = append(all, strings.Split(v, ",")...)
			}
			continue
		}
		h[http.CanonicalHeaderKey(k)] = append([]string(nil), vs...)
	}
	var uniq []string
	seen := map[string]bool{}
	for _, b := range all {
		if b = strings.TrimSpace(b); b != "" && !seen[b] {
			seen[b] = true
			uniq = append(uniq, b)
		}
	}
	if len(uniq) > 0 {
		h.Set("anthropic-beta", strings.Join(uniq, ","))
	}
	return h
}

// countFields are the request fields count_tokens accepts; the rest (stream, max_tokens, sampling) would be rejected.
var countFields = []string{"model", "messages", "system", "tools", "tool_choice", "thinking"}

// CountTokens asks the free count_tokens endpoint for the input size of req as Stream would send it.
func (p *Provider) CountTokens(ctx context.Context, req *provider.Request) (int64, error) {
	r, err := p.render(req)
	if err != nil {
		return 0, err
	}
	var full map[string]json.RawMessage
	if err := json.Unmarshal(r.body, &full); err != nil {
		return 0, &provider.Error{Class: provider.ClassInvalidRequest, API: provider.APIAnthropicMessages, Message: err.Error()}
	}
	body := map[string]json.RawMessage{}
	for _, k := range countFields {
		if v, ok := full[k]; ok {
			body[k] = v
		}
	}
	b, _ := json.Marshal(body)
	resp, err := httpx.Post(ctx, p.cfg.Doer, provider.APIAnthropicMessages, p.base+"/v1/messages/count_tokens", r.header, b)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var out struct {
		InputTokens int64 `json:"input_tokens"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, httpx.TransportError(ctx, provider.APIAnthropicMessages, err)
	}
	return out.InputTokens, nil
}
