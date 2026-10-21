package subagent_test

import (
	"reflect"
	"testing"

	"github.com/estonshi/aperture-potatos/ext/subagent"
	"github.com/estonshi/aperture-potatos/internal/schemadrift"
)

func TestSchemaDrift(t *testing.T) {
	for typ, def := range map[reflect.Type]string{
		reflect.TypeFor[subagent.StartInput]():  "subagent_start_input",
		reflect.TypeFor[subagent.StartOutput](): "subagent_start_output",
		reflect.TypeFor[subagent.StopInput]():   "subagent_stop_input",
		reflect.TypeFor[subagent.StopOutput]():  "subagent_stop_output",
	} {
		schemadrift.Check(t, typ, "ext/subagent/hooks.schema.json#/$defs/"+def)
	}
}
