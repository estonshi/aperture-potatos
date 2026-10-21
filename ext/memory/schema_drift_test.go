package memory_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/estonshi/aperture-potatos/ext/memory"
	"github.com/estonshi/aperture-potatos/internal/schemadrift"
)

func TestSchemaDrift(t *testing.T) {
	for _, c := range memory.Payloads() {
		schemadrift.Check(t, reflect.TypeOf(c.New()), "ext/memory/events.schema.json#/$defs/"+strings.ReplaceAll(string(c.Type), ".", "_"))
	}
	if got := len(schemadrift.Defs(t, "ext/memory/events.schema.json")); got != len(memory.Payloads())+1 { // +1: change, shared by memory.saved
		t.Errorf("events.schema.json has %d definitions for %d events", got, len(memory.Payloads()))
	}
}
