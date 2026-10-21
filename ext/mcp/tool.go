package mcp

import (
	"context"
	"encoding/json"

	"github.com/estonshi/aperture-potatos/kernel"
)

// serverTool is one server tool registered as a tool of its own; Spec reads the current catalog, so a changed definition shows at the next freeze.
type serverTool struct {
	in   *install
	name string
}

func (t *serverTool) Spec() kernel.ToolSpec {
	if te := t.in.cat.Load().byName[t.name]; te != nil {
		spec := te.spec
		spec.Deferred = t.in.native.Load()
		return spec
	}
	return kernel.ToolSpec{Name: t.name, InputSchema: json.RawMessage(`{"type":"object"}`), Concurrency: kernel.Serial}
}

func (t *serverTool) Call(ctx context.Context, tc *kernel.ToolContext, input json.RawMessage) (kernel.ToolResult, error) {
	te := t.in.cat.Load().byName[t.name]
	if te == nil {
		return errResult(text("unknown_tool", "name", t.name)), nil
	}
	return te.c.call(ctx, tc, te, input)
}
