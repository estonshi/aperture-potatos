package mcp

import (
	"encoding/json"

	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/spec"
)

var text = kernel.SpecTexts("ext/mcp/texts.md")

// metaDefs are the two discovery tools as spec/ext/mcp/tools.json defines them.
var metaDefs = func() map[string]kernel.ToolSpec {
	var defs []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
	}
	if err := json.Unmarshal([]byte(spec.MustText("ext/mcp/tools.json")), &defs); err != nil {
		panic("ext/mcp: tools.json: " + err.Error())
	}
	out := map[string]kernel.ToolSpec{}
	for _, d := range defs {
		out[d.Name] = kernel.ToolSpec{Name: d.Name, Description: d.Description, InputSchema: d.InputSchema}
	}
	return out
}()
