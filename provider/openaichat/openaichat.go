// Package openaichat adapts OpenAI Chat Completions, and the many servers compatible with it, to the provider SPI over raw HTTP and SSE.
package openaichat

import (
	"context"
	"errors"
	"io"
	"iter"
	"net/http"
	"strings"

	"github.com/estonshi/aperture-potatos/internal/sse"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/internal/httpx"
	"github.com/estonshi/aperture-potatos/provider/internal/modellist"
)

const (
	api            = provider.APIOpenAIChat
	defaultBaseURL = "https://api.openai.com/v1"
	officialHost   = "api.openai.com" // fields only OpenAI's own endpoint accepts are sent there alone
)

// Config configures a Provider; an empty APIKey sends no authorization header, for a local server that needs none.
type Config struct {
	APIKey  string
	BaseURL string        // "" means https://api.openai.com/v1; it includes the version path, as compatible servers such as LiteLLM, vLLM and OpenRouter expect
	Doer    provider.Doer // nil means provider.DefaultClient, which follows no redirect to another host; stream timeouts are the adapter's, so it needs no client timeout
}

// Provider speaks Chat Completions and resolves capabilities from the endpoint's model listing.
type Provider struct {
	cfg      Config
	base     string
	endpoint string
	models   *modellist.List
}

// New makes no request; the model listing is fetched the first time capabilities are resolved.
func New(cfg Config) *Provider {
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = defaultBaseURL
	}
	if cfg.Doer == nil {
		cfg.Doer = provider.DefaultClient
	}
	p := &Provider{cfg: cfg, base: base, endpoint: httpx.Endpoint(base)}
	p.models = &modellist.List{API: api, URL: base + "/models", Doer: cfg.Doer, Headers: func() http.Header { return p.headers(nil) }}
	return p
}

// API reports provider.APIOpenAIChat.
func (p *Provider) API() provider.API { return api }

// Endpoint is the base URL's lowercased host, which tags native content so it is replayed only to the endpoint that produced it.
func (p *Provider) Endpoint() string { return p.endpoint }

// ResolveCapabilities reads the endpoint's model listing once and answers every model from it.
func (p *Provider) ResolveCapabilities(ctx context.Context, m provider.ModelInfo) (provider.Capabilities, error) {
	return p.models.Resolve(ctx, m.Model)
}

// Stream sends one /chat/completions request and yields its events, with the capability notices for anything it had to adjust first.
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
		resp, err := httpx.PostStream(ctx, p.cfg.Doer, api, p.base+"/chat/completions", r.header, r.body, req)
		if err != nil {
			yield(provider.StreamEvent{}, err)
			return
		}
		defer resp.Body.Close()
		d := newDecoder(req, p.endpoint, resp.Header, r.inputEstimate)
		sr := sse.NewReader(resp.Body)
		for !d.done {
			ev, err := sr.Next()
			if err != nil {
				if errors.Is(err, io.EOF) && ctx.Err() == nil {
					yield(provider.StreamEvent{}, httpx.Incomplete(api, d.requestID))
				} else {
					yield(provider.StreamEvent{}, httpx.TransportError(ctx, api, err))
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

func (p *Provider) headers(extra http.Header) http.Header {
	h := http.Header{}
	h.Set("content-type", "application/json")
	h.Set("accept", "text/event-stream")
	if p.cfg.APIKey != "" {
		h.Set("authorization", "Bearer "+p.cfg.APIKey)
	}
	for k, vs := range extra {
		h[http.CanonicalHeaderKey(k)] = append([]string(nil), vs...)
	}
	return h
}
