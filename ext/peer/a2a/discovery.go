package a2a

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/estonshi/aperture-potatos/ext/peer"
)

const (
	v03 = "0.3"
	v10 = "1.0"
)

// candidate is one interface a card offers, in the client's terms.
type candidate struct {
	url     string
	binding Binding
	version string
	tenant  string
	order   int
}

type cardJSON struct {
	Name                string `json:"name"`
	Description         string `json:"description"`
	Version             string `json:"version"`
	URL                 string `json:"url"`
	ProtocolVersion     string `json:"protocolVersion"`
	PreferredTransport  string `json:"preferredTransport"`
	AdditionalInterface []struct {
		URL       string `json:"url"`
		Transport string `json:"transport"`
	} `json:"additionalInterfaces"`
	SupportedInterfaces []struct {
		URL             string `json:"url"`
		ProtocolBinding string `json:"protocolBinding"`
		ProtocolVersion string `json:"protocolVersion"`
		Tenant          string `json:"tenant"`
	} `json:"supportedInterfaces"`
	Capabilities struct {
		Streaming bool `json:"streaming"`
	} `json:"capabilities"`
	Skills []struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Tags        []string `json:"tags"`
		Examples    []string `json:"examples"`
	} `json:"skills"`
}

// parseCard reads a card of either version: the v1.0 interface list and the v0.3 top-level url with its additional interfaces.
func parseCard(raw []byte) (peer.Card, []candidate, error) {
	var c cardJSON
	if err := json.Unmarshal(raw, &c); err != nil {
		return peer.Card{}, nil, fmt.Errorf("a2a: the agent card does not parse: %w", err)
	}
	card := peer.Card{Name: c.Name, Description: c.Description, Version: c.Version, Streaming: c.Capabilities.Streaming}
	for _, s := range c.Skills {
		card.Skills = append(card.Skills, peer.CardSkill{ID: s.ID, Name: s.Name, Description: s.Description, Tags: s.Tags, Examples: s.Examples})
	}
	var out []candidate
	add := func(url, binding, version, tenant string) {
		v, ok := majorMinor(version)
		b := Binding(strings.ToUpper(strings.TrimSpace(binding)))
		if !ok || url == "" || b != BindingJSONRPC && b != BindingHTTPJSON {
			return
		}
		cd := candidate{url: url, binding: b, version: v, tenant: tenant, order: len(out)}
		if !slices.ContainsFunc(out, func(x candidate) bool {
			return x.url == cd.url && x.binding == cd.binding && x.version == cd.version && x.tenant == cd.tenant
		}) {
			out = append(out, cd)
		}
	}
	for _, s := range c.SupportedInterfaces {
		v := s.ProtocolVersion
		if v == "" {
			v = v10 // the list is v1.0's shape
		}
		add(s.URL, s.ProtocolBinding, v, s.Tenant)
	}
	if c.URL != "" {
		v := c.ProtocolVersion
		if v == "" {
			v = v03
		}
		t := c.PreferredTransport
		if t == "" {
			t = string(BindingJSONRPC)
		}
		add(c.URL, t, v, "")
		for _, a := range c.AdditionalInterface {
			add(a.URL, a.Transport, v, "")
		}
	}
	return card, out, nil
}

// majorMinor maps a card's version to the one the client speaks: 1.x to 1.0, 0.3.x and 0.2.x to 0.3.
func majorMinor(v string) (string, bool) {
	parts := strings.Split(strings.TrimSpace(v), ".")
	if len(parts) < 2 {
		return "", false
	}
	switch {
	case parts[0] == "1":
		return v10, true
	case parts[0] == "0" && (parts[1] == "3" || parts[1] == "2"):
		return v03, true
	}
	return "", false
}

// choose filters by the caller's options, then prefers 1.0 to 0.3 and JSON-RPC to HTTP+JSON, keeping the card's order otherwise.
func choose(cands []candidate, version string, binding Binding) (candidate, error) {
	var keep []candidate
	for _, c := range cands {
		if version != "" && c.version != version || binding != "" && c.binding != binding {
			continue
		}
		keep = append(keep, c)
	}
	if len(keep) == 0 {
		return candidate{}, ErrNoSupportedInterface
	}
	rank := func(c candidate) int {
		r := 0
		if c.version != v10 {
			r += 2
		}
		if c.binding != BindingJSONRPC {
			r++
		}
		return r
	}
	slices.SortStableFunc(keep, func(a, b candidate) int {
		if d := rank(a) - rank(b); d != 0 {
			return d
		}
		return a.order - b.order
	})
	return keep[0], nil
}
