package kernel

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/estonshi/aperture-potatos/message"
)

type hostedReg struct {
	id   uint64
	tool message.HostedTool
}

// WithHostedTool registers a tool the provider runs, such as web search, at agent scope.
func WithHostedTool(t message.HostedTool) Option {
	return func(c *config) {
		c.app = append(c.app, func(r Registrar) error { _, err := r.HostedTool(t); return err })
	}
}

func validHosted(t message.HostedTool) error {
	set := 0
	for _, on := range []bool{t.WebSearch != nil, t.ToolSearch != nil, len(t.Raw) > 0} {
		if on {
			set++
		}
	}
	switch {
	case !toolName.MatchString(t.Name):
		return fmt.Errorf("%w: hosted tool %q", ErrInvalidName, t.Name)
	case set != 1:
		return fmt.Errorf("hosted tool %q: set exactly one of WebSearch, ToolSearch and Raw", t.Name)
	case t.ToolSearch != nil && t.Name != "tool_search":
		return fmt.Errorf("hosted tool %q: the neutral tool search is named tool_search", t.Name)
	}
	if ws := t.WebSearch; ws != nil {
		switch {
		case t.Name != "web_search":
			return fmt.Errorf("hosted tool %q: the neutral web search is named web_search", t.Name)
		case len(ws.AllowedDomains) > 0 && len(ws.BlockedDomains) > 0:
			return fmt.Errorf("hosted tool %q: set at most one of AllowedDomains and BlockedDomains", t.Name)
		case ws.MaxUses < 0:
			return fmt.Errorf("hosted tool %q: negative MaxUses", t.Name)
		case !slices.Contains([]string{"", "low", "medium", "high"}, ws.ContextSize):
			return fmt.Errorf("hosted tool %q: ContextSize %q is not low, medium or high", t.Name, ws.ContextSize)
		}
	}
	for api, def := range t.Raw {
		var obj map[string]json.RawMessage
		if json.Unmarshal(def, &obj) != nil || obj == nil {
			return fmt.Errorf("hosted tool %q: the %s definition is not a JSON object", t.Name, api)
		}
	}
	return nil
}

func (r *registry) addHosted(t message.HostedTool) (*Registration, error) {
	if err := validHosted(t); err != nil {
		return nil, err
	}
	name := t.Name
	id, err := r.update(func(st *regState) error {
		if _, ok := st.hosted[name]; ok {
			return fmt.Errorf("%w: hosted tool %q", ErrDuplicate, name)
		}
		if _, ok := st.tools[name]; ok {
			return fmt.Errorf("%w: hosted tool %q is a tool's name", ErrDuplicate, name)
		}
		st.hosted[name] = &hostedReg{id: r.next + 1, tool: t}
		return nil
	})
	if err != nil {
		return nil, err
	}
	r.changed(KindHostedTool, name, "add")
	return &Registration{remove: func() {
		if _, err := r.update(func(st *regState) error {
			if cur, ok := st.hosted[name]; !ok || cur.id != id {
				return errGone
			}
			delete(st.hosted, name)
			return nil
		}); err == nil {
			r.changed(KindHostedTool, name, "remove")
		}
	}}, nil
}

// HostedTools lists the visible provider-run tools, session scope shadowing agent scope, sorted by name.
func (s *Session) HostedTools() []message.HostedTool {
	byName := map[string]message.HostedTool{}
	sess := s.reg.state()
	for n, h := range s.outer().hosted {
		if !sess.hides(KindHostedTool, n) {
			byName[n] = h.tool
		}
	}
	for n, h := range sess.hosted {
		byName[n] = h.tool
	}
	out := make([]message.HostedTool, 0, len(byName))
	for _, n := range slices.Sorted(maps.Keys(byName)) {
		out = append(out, byName[n])
	}
	return out
}

// freezeHosted reads the effective hosted tools in name order, leaving out any whose name a function tool of the frozen set already takes.
func (s *Session) freezeHosted(defs []message.ToolDef) []message.HostedTool {
	byName := map[string]message.HostedTool{}
	sess := s.reg.state()
	for n, h := range s.outer().hosted {
		if !sess.hides(KindHostedTool, n) {
			byName[n] = h.tool
		}
	}
	for n, h := range sess.hosted {
		byName[n] = h.tool
	}
	var out []message.HostedTool
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		if slices.ContainsFunc(defs, func(d message.ToolDef) bool { return d.Name == name }) {
			s.agent.cfg.logger.Error("hosted tool left out of this freeze, a tool has its name", "tool", name)
			continue
		}
		out = append(out, byName[name])
	}
	return out
}

// awaitsHostedResult reports a hosted call in the latest assistant line that has no result yet, which the API runs only if the next user message holds nothing but tool results (runtime.md §4.6).
func awaitsHostedResult(history []message.Message) bool {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role != message.RoleAssistant {
			continue
		}
		open := map[string]bool{}
		for _, b := range history[i].Content {
			switch v := b.(type) {
			case *message.HostedCallBlock:
				open[v.ID] = true
			case *message.HostedResultBlock:
				delete(open, v.CallID)
			}
		}
		return len(open) > 0
	}
	return false
}
