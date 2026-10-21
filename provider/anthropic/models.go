package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/url"
	"slices"

	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/internal/httpx"
)

var effortLevels = []string{"low", "medium", "high", "xhigh", "max"}

// ResolveCapabilities reads the Models API; only fields that map onto Capabilities are taken, and every other field stays unknown.
func (p *Provider) ResolveCapabilities(ctx context.Context, m provider.ModelInfo) (provider.Capabilities, error) {
	resp, err := httpx.Get(ctx, p.cfg.Doer, api, p.base+"/v1/models/"+url.PathEscape(m.Model), p.headers(nil, nil))
	if err != nil {
		return provider.Capabilities{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return provider.Capabilities{}, httpx.TransportError(ctx, api, err)
	}
	return parseModel(b)
}

type supported struct {
	Supported bool `json:"supported"`
}

func parseModel(b []byte) (provider.Capabilities, error) {
	var m struct {
		MaxInputTokens int                        `json:"max_input_tokens"`
		MaxTokens      int                        `json:"max_tokens"`
		Capabilities   map[string]json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return provider.Capabilities{}, &provider.Error{Class: provider.ClassTransient, API: api, Code: "protocol_error", Message: "models response is not JSON: " + err.Error()}
	}
	c := provider.Capabilities{ContextWindow: m.MaxInputTokens, MaxOutputTokens: m.MaxTokens}
	var thinking struct {
		Types map[string]supported `json:"types"`
	}
	if json.Unmarshal(m.Capabilities["thinking"], &thinking) == nil {
		for _, mode := range []string{"adaptive", "enabled"} {
			if thinking.Types[mode].Supported {
				c.Reasoning.Modes = append(c.Reasoning.Modes, mode)
			}
		}
	}
	var effort map[string]json.RawMessage
	if json.Unmarshal(m.Capabilities["effort"], &effort) == nil {
		for _, level := range effortLevels {
			var s supported
			if json.Unmarshal(effort[level], &s) == nil && s.Supported && !slices.Contains(c.Reasoning.Efforts, level) {
				c.Reasoning.Efforts = append(c.Reasoning.Efforts, level)
			}
		}
	}
	var server struct {
		WebSearch *supported `json:"web_search"`
	}
	if json.Unmarshal(m.Capabilities["server_tools"], &server) == nil && server.WebSearch != nil {
		c.NoWebSearch = !server.WebSearch.Supported
	}
	return c, nil
}
