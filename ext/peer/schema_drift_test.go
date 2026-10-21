package peer_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/estonshi/aperture-potatos/ext/peer"
	"github.com/estonshi/aperture-potatos/internal/schemadrift"
)

func TestSchemaDrift(t *testing.T) {
	for _, c := range peer.Payloads() {
		schemadrift.Check(t, reflect.TypeOf(c.New()), "ext/peer/events.schema.json#/$defs/"+strings.ReplaceAll(string(c.Type), ".", "_"))
	}
	if got := len(schemadrift.Defs(t, "ext/peer/events.schema.json")); got != len(peer.Payloads()) {
		t.Errorf("events.schema.json has %d definitions for %d events", got, len(peer.Payloads()))
	}
	schemadrift.Check(t, reflect.TypeFor[peer.AuthInput](), "ext/peer/hooks.schema.json#/$defs/peer_auth_required_input")
	schemadrift.Check(t, reflect.TypeFor[peer.AuthOutput](), "ext/peer/hooks.schema.json#/$defs/peer_auth_required_output")
}
