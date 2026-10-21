package skills_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/estonshi/aperture-potatos/ext/skills"
	"github.com/estonshi/aperture-potatos/internal/schemadrift"
)

func TestSchemaDrift(t *testing.T) {
	for _, c := range skills.Payloads() {
		schemadrift.Check(t, reflect.TypeOf(c.New()), "ext/skills/events.schema.json#/$defs/"+strings.ReplaceAll(string(c.Type), ".", "_"))
	}
	if got := len(schemadrift.Defs(t, "ext/skills/events.schema.json")); got != len(skills.Payloads()) {
		t.Errorf("events.schema.json has %d definitions for %d events", got, len(skills.Payloads()))
	}
	schemadrift.Check(t, reflect.TypeFor[skills.ActivateInput](), "ext/skills/hooks.schema.json#/$defs/skill_activate_input")
	schemadrift.Check(t, reflect.TypeFor[skills.ActivateOutput](), "ext/skills/hooks.schema.json#/$defs/skill_activate_output")
}
