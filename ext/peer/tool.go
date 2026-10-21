package peer

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/spec"
)

// chainKey is the input metadata that carries the agents an exchange passed through, so a cycle is refused across agents and processes.
const chainKey = "agent_call_chain"

// input is a2a_send's input, which is also the a2a task's; Chain is added by the tool and never shown to the model.
type input struct {
	Agent           string   `json:"agent"`
	Message         string   `json:"message"`
	NewConversation bool     `json:"new_conversation,omitempty"`
	Chain           []string `json:"chain,omitempty"`
}

var toolDef = func() kernel.ToolSpec {
	var def struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
	}
	if err := json.Unmarshal([]byte(spec.MustText("ext/peer/a2a_send.json")), &def); err != nil {
		panic(err)
	}
	return kernel.ToolSpec{Name: def.Name, Description: def.Description, InputSchema: def.InputSchema, Concurrency: kernel.Parallel}
}()

// sendTool recomputes the agent enum at every freeze point and checks the name and the call chain when called.
type sendTool struct {
	agent   *kernel.Agent
	session *kernel.Session // set for a session's own tool
}

func toSpec(raw json.RawMessage) (kernel.TaskSpec, error) {
	var in input
	if err := json.Unmarshal(raw, &in); err != nil {
		return kernel.TaskSpec{}, err
	}
	return kernel.TaskSpec{Title: title(in), Input: raw}, nil
}

// start builds the kind's start tool; its schema comes from the spec files, so an error is a build defect.
func start(def kernel.ToolSpec) kernel.Tool {
	t, err := kernel.TaskTool(Kind, def, toSpec)
	if err != nil {
		panic(err)
	}
	return t
}

func title(in input) string {
	msg := []rune(oneLine(in.Message))
	if len(msg) > 60 {
		msg = msg[:60]
	}
	return in.Agent + ": " + string(msg)
}

func (t *sendTool) Spec() kernel.ToolSpec {
	names := map[string]entry{}
	list := peersOf(t.agent)
	if t.session != nil {
		list = slices.Concat(list, peersOf(t.session))
	}
	for _, en := range list {
		names[en.name] = en
	}
	spec := toolDef
	spec.InputSchema = withEnum(spec.InputSchema, sortedNames(names))
	return start(spec).Spec()
}

func (t *sendTool) Call(ctx context.Context, tc *kernel.ToolContext, raw json.RawMessage) (kernel.ToolResult, error) {
	var in input
	_ = json.Unmarshal(raw, &in)
	s := tc.Session()
	v := view(s)
	if _, ok := v[in.Agent]; !ok {
		return kernel.ErrorResult("%s", text("unknown_agent", "agent", in.Agent, "agents", strings.Join(sortedNames(v), ", "))), nil
	}
	chain := withSelf(upstream(tc.InputMetadata()), s.Info().Agent)
	if slices.Contains(chain, in.Agent) {
		return kernel.ErrorResult("%s", cyclic(chain, in.Agent)), nil
	}
	withChain, err := withKey(raw, "chain", chain)
	if err != nil {
		return kernel.ToolResult{}, err
	}
	return start(toolDef).Call(ctx, tc, withChain)
}

// upstream is the chain the turn's input arrived with, a JSON list of agent names.
func upstream(meta map[string]string) []string {
	var chain []string
	_ = json.Unmarshal([]byte(meta[chainKey]), &chain)
	return chain
}

func withSelf(chain []string, self string) []string {
	if len(chain) > 0 && chain[len(chain)-1] == self {
		return chain
	}
	return append(slices.Clone(chain), self)
}

func cyclic(chain []string, target string) string {
	return text("cyclic", "chain", strings.Join(append(slices.Clone(chain), target), "→"), "agent", target)
}

// withKey sets one member of a JSON object, keeping the others as they were.
func withKey(raw json.RawMessage, key string, v any) (json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	m[key] = b
	return json.Marshal(m)
}

// withEnum sets the agent enum; the schema comes from a2a_send.json, so its shape is known.
func withEnum(schema json.RawMessage, names []string) json.RawMessage {
	var s map[string]json.RawMessage
	var props map[string]json.RawMessage
	var ag map[string]json.RawMessage
	if json.Unmarshal(schema, &s) != nil || json.Unmarshal(s["properties"], &props) != nil || json.Unmarshal(props["agent"], &ag) != nil {
		return schema
	}
	if names == nil {
		names = []string{}
	}
	enum, _ := json.Marshal(names)
	ag["enum"] = enum
	props["agent"] = ordered(ag, "type", "enum", "description")
	s["properties"] = ordered(props, "agent", "message", "new_conversation")
	return ordered(s, "type", "properties", "required", "additionalProperties")
}

// ordered writes an object with its members in the given order, so the definition's bytes stay what a2a_send.json says.
func ordered(m map[string]json.RawMessage, keys ...string) json.RawMessage {
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		name, _ := json.Marshal(k)
		b.Write(name)
		b.WriteByte(':')
		b.Write(m[k])
	}
	b.WriteByte('}')
	return json.RawMessage(b.String())
}
