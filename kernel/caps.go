package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/provider"
)

type capDecl struct {
	api  provider.API
	glob string
	caps provider.Capabilities
}

// WithCapabilities declares what the models matching modelGlob on api can do; a declared field beats the resolver, and a later declaration beats an earlier one field by field.
func WithCapabilities(api provider.API, modelGlob string, caps provider.Capabilities) Option {
	return func(c *config) { c.capDecls = append(c.capDecls, capDecl{api: api, glob: modelGlob, caps: caps}) }
}

// WithCapabilityResolver replaces every provider's own resolver for this agent; nil turns resolution off.
func WithCapabilityResolver(r provider.CapabilityResolver) Option {
	return func(c *config) { c.resolver, c.resolverSet = r, true }
}

// capCache keeps each model's resolved capabilities for the agent's lifetime; a failed lookup is never kept, so the next session asks again.
type capCache struct {
	mu sync.Mutex
	m  map[provider.ModelInfo]provider.Capabilities
}

func (c *capCache) get(info provider.ModelInfo) (provider.Capabilities, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[info]
	return v, ok
}

func (c *capCache) put(info provider.ModelInfo, v provider.Capabilities) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[provider.ModelInfo]provider.Capabilities{}
	}
	c.m[info] = v
}

// modelState is the model a turn runs with, fixed when the turn starts (I10).
type modelState struct {
	ref         provider.ModelRef // Params.MaxOutputTokens already holds the effective output cap
	info        provider.ModelInfo
	caps        provider.Capabilities
	windowKnown bool // declared or resolved rather than the default, which is what allows the quarter-window cap
}

func infoOf(ref provider.ModelRef) provider.ModelInfo {
	return provider.ModelInfo{API: ref.Provider.API(), Endpoint: ref.Provider.Endpoint(), Model: ref.Model}
}

func (ms modelState) name() event.ModelName {
	return event.ModelName{API: string(ms.info.API), Model: ms.info.Model}
}

// family is what reasoning replay compares: the resolved family, or the model name.
func (ms modelState) family() string {
	if ms.caps.Family != "" {
		return ms.caps.Family
	}
	return ms.info.Model
}

func (a *Agent) resolverFor(ref provider.ModelRef) provider.CapabilityResolver {
	if a.cfg.resolverSet {
		return a.cfg.resolver
	}
	r, _ := ref.Provider.(provider.CapabilityResolver)
	return r
}

// capsCached reports whether preparing ref needs no lookup, which decides whether SwitchModel waits.
func (a *Agent) capsCached(ref provider.ModelRef) bool {
	if a.resolverFor(ref) == nil {
		return true
	}
	_, ok := a.caps.get(infoOf(ref))
	return ok
}

// prepareModel resolves ref's capabilities, applies the output cap, and rejects a model the session could never use, before it is used rather than at a turn boundary.
func (s *Session) prepareModel(ctx context.Context, ref provider.ModelRef, tools int) (modelState, error) {
	a := s.agent
	ms := modelState{ref: ref, info: infoOf(ref)}
	resolved, ok := a.caps.get(ms.info)
	if r := a.resolverFor(ref); r != nil && !ok {
		c, err := callResolver(withMarker(ctx, marker{chain: s.chain, kind: "resolver"}), r, ms.info)
		if err != nil {
			d, _ := json.Marshal(map[string]string{"model": ms.info.Model, "error": err.Error()})
			s.publish(&event.ModelNotice{Kind: "capability_lookup_failed", Detail: d})
		} else {
			a.caps.put(ms.info, c)
			resolved = c
		}
	}
	decls := a.cfg.capDecls
	for i := len(decls) - 1; i >= 0; i-- {
		if decls[i].api == ms.info.API && matchGlob(decls[i].glob, ms.info.Model) {
			fillZero(reflect.ValueOf(&ms.caps).Elem(), reflect.ValueOf(decls[i].caps))
		}
	}
	fillZero(reflect.ValueOf(&ms.caps).Elem(), reflect.ValueOf(resolved))
	ms.windowKnown = ms.caps.ContextWindow > 0
	if !ms.windowKnown {
		ms.caps.ContextWindow = provider.DefaultContextWindow
	}
	configured := ref.Params.MaxOutputTokens
	if configured <= 0 {
		configured = provider.DefaultMaxOutputTokens
	}
	eff := configured
	switch {
	case ms.caps.MaxOutputTokens > 0:
		eff = min(eff, ms.caps.MaxOutputTokens)
	case ms.windowKnown:
		eff = min(eff, ms.caps.ContextWindow/4) // a window alone says nothing about the output cap, so keep a quarter of it for output
	}
	if eff != configured {
		d, _ := json.Marshal(map[string]any{"field": "max_output_tokens", "from": configured, "to": eff})
		s.notice(&event.ModelNotice{Kind: "capability_adjusted", Detail: d}, "", 0, 0)
	}
	ms.ref.Params.MaxOutputTokens = eff
	if ms.caps.NoTools && tools > 0 {
		return ms, fmt.Errorf("%w: %s on %s takes no tools, and the session has %d", ErrCapability, ms.info.Model, ms.info.API, tools)
	}
	if ms.caps.ContextWindow-eff <= 0 {
		return ms, fmt.Errorf("%w: max output %d leaves no room in a %d-token window; set Params.MaxOutputTokens", ErrCapability, eff, ms.caps.ContextWindow)
	}
	if limit := a.cfg.context.MaxContextTokens; limit > 0 && limit-eff <= 0 {
		return ms, fmt.Errorf("%w: max output %d leaves no room in the %d tokens of ContextPolicy.MaxContextTokens; raise it or set Params.MaxOutputTokens", ErrCapability, eff, limit)
	}
	return ms, nil
}

// setWindow points the context gauge at the current model.

// learned is what a model's errors taught this session, so every later request to that model starts from it.
type learned struct {
	maxOutput      int    // max_output_too_large: the limit the error named
	maxTokensField string // max_tokens_field_unsupported: the field name that works
	noSummary      bool   // reasoning_summary_unavailable
	prefixBound    bool   // thinking_signature_bound: the model binds thinking signatures to the prefix though nobody declared it
}

// callResolver runs caller code, so a panic fails the lookup instead of the session.
func callResolver(ctx context.Context, r provider.CapabilityResolver, info provider.ModelInfo) (c provider.Capabilities, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("capability resolver panicked: %v", p)
		}
	}()
	return r.ResolveCapabilities(ctx, info)
}

// fillZero copies every field of src into a zero field of dst, nested structs field by field, so a source only supplies what the ones before it left unknown.
func fillZero(dst, src reflect.Value) {
	for i := range dst.NumField() {
		d, v := dst.Field(i), src.Field(i)
		switch {
		case d.Kind() == reflect.Struct:
			fillZero(d, v)
		case d.Kind() == reflect.Slice && d.Len() == 0, d.IsZero():
			d.Set(v)
		}
	}
}

// matchGlob matches the whole name, where * is any run of characters, slashes included, and ? is one character.
func matchGlob(pattern, name string) bool {
	p, n := []rune(pattern), []rune(name)
	i, j, star, mark := 0, 0, -1, 0
	for j < len(n) {
		switch {
		case i < len(p) && p[i] == '*':
			star, mark = i, j
			i++
		case i < len(p) && (p[i] == '?' || p[i] == n[j]):
			i++
			j++
		case star >= 0:
			mark++
			i, j = star+1, mark
		default:
			return false
		}
	}
	for i < len(p) && p[i] == '*' {
		i++
	}
	return i == len(p)
}

// notice publishes a model notice, announcing each distinct adjustment or hint once per session instead of on every request.
func (s *Session) notice(n *event.ModelNotice, turnID string, step, attempt int) {
	switch n.Kind {
	case "capability_adjusted", "capability_hint", "hosted_tool_unsupported", "hosted_tool_adjusted":
		if _, seen := s.noticed.LoadOrStore(n.Kind+string(n.Detail), true); seen {
			return
		}
	}
	s.publishAt(n, turnID, step, attempt)
}
