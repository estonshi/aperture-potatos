// Package modellist resolves capabilities from an OpenAI-style GET {base}/models listing, which both OpenAI adapters share.
package modellist

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"

	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/internal/httpx"
)

// windowFields and outputFields are the spec/runtime.md field table, read in order on the entry and then on its model_info.
var (
	windowFields = []string{"max_input_tokens", "context_window", "context_length", "max_model_len"}
	outputFields = []string{"max_output_tokens", "max_tokens", "max_completion_tokens"}
)

// List fetches the listing once per endpoint and answers every model from it; a failed fetch is not kept, so the next call tries again.
type List struct {
	API     provider.API
	URL     string
	Doer    provider.Doer
	Headers func() http.Header

	mu     sync.Mutex
	models map[string]provider.Capabilities
}

// Resolve returns the listed model's capabilities, or all zero (unknown) for a model the listing does not name.
func (l *List) Resolve(ctx context.Context, model string) (provider.Capabilities, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.models == nil {
		m, err := l.fetch(ctx)
		if err != nil {
			return provider.Capabilities{}, err
		}
		l.models = m
	}
	return l.models[model], nil
}

func (l *List) fetch(ctx context.Context) (map[string]provider.Capabilities, error) {
	resp, err := httpx.Get(ctx, l.Doer, l.API, l.URL, l.Headers())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, httpx.TransportError(ctx, l.API, err)
	}
	return Parse(l.API, b)
}

// Parse reads a models listing; an entry is named by id, or by model_name when it has no id.
func Parse(api provider.API, b []byte) (map[string]provider.Capabilities, error) {
	var doc struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, &provider.Error{Class: provider.ClassTransient, API: api, Code: "protocol_error", Message: "models response is not JSON: " + err.Error()}
	}
	out := map[string]provider.Capabilities{}
	for _, e := range doc.Data {
		name := str(e["id"])
		if name == "" {
			name = str(e["model_name"])
		}
		if name == "" {
			continue
		}
		var info map[string]json.RawMessage
		_ = json.Unmarshal(e["model_info"], &info)
		out[name] = provider.Capabilities{ContextWindow: first(windowFields, e, info), MaxOutputTokens: first(outputFields, e, info)}
	}
	return out, nil
}

func first(fields []string, objs ...map[string]json.RawMessage) int {
	for _, o := range objs {
		for _, f := range fields {
			var n float64
			if json.Unmarshal(o[f], &n) == nil && n >= 1 && n == float64(int(n)) {
				return int(n)
			}
		}
	}
	return 0
}

func str(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}
