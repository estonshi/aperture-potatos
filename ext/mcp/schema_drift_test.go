package mcp_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/estonshi/aperture-potatos/ext/mcp"
	"github.com/estonshi/aperture-potatos/internal/schemadrift"
)

func TestSchemaDrift(t *testing.T) {
	for _, c := range mcp.Payloads() {
		schemadrift.Check(t, reflect.TypeOf(c.New()), "ext/mcp/events.schema.json#/$defs/"+strings.ReplaceAll(string(c.Type), ".", "_"))
	}
	if got := len(schemadrift.Defs(t, "ext/mcp/events.schema.json")); got != len(mcp.Payloads()) {
		t.Errorf("events.schema.json has %d definitions for %d events", got, len(mcp.Payloads()))
	}
	schemadrift.Check(t, reflect.TypeFor[mcp.ElicitInput](), "ext/mcp/hooks.schema.json#/$defs/mcp_elicitation_input")
	schemadrift.Check(t, reflect.TypeFor[mcp.ElicitOutput](), "ext/mcp/hooks.schema.json#/$defs/mcp_elicitation_output")
}
