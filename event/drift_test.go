package event

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/estonshi/aperture-potatos/internal/schemadrift"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/spec"
)

func TestSchemaDrift(t *testing.T) {
	schemadrift.Extra(reflect.TypeFor[message.HostedCallBlock](), "type") // written by the block's own MarshalJSON
	schemadrift.Extra(reflect.TypeFor[message.HostedResultBlock](), "type")
	schemadrift.Check(t, reflect.TypeFor[Event](), "event.schema.json")
	schemadrift.Check(t, reflect.TypeFor[Hop](), "common.schema.json#/$defs/hop")
	inGo := map[Type]bool{}
	for _, c := range kernelPayloads {
		inGo[c.Type] = true
		schemadrift.Check(t, reflect.TypeOf(c.New()), "event.schema.json#/$defs/"+strings.ReplaceAll(string(c.Type), ".", "_"))
	}
	var root struct {
		AllOf []struct {
			If struct {
				Properties struct {
					Type struct {
						Const Type `json:"const"`
					} `json:"type"`
				} `json:"properties"`
			} `json:"if"`
		} `json:"allOf"`
	}
	if err := json.Unmarshal([]byte(spec.MustText("schema/event.schema.json")), &root); err != nil {
		t.Fatal(err)
	}
	inSchema := map[Type]bool{}
	for _, a := range root.AllOf {
		inSchema[a.If.Properties.Type.Const] = true
		if !inGo[a.If.Properties.Type.Const] {
			t.Errorf("schema lists kernel event %q with no Go payload", a.If.Properties.Type.Const)
		}
	}
	for ty := range inGo {
		if !inSchema[ty] {
			t.Errorf("Go payload %q is missing from event.schema.json", ty)
		}
	}
}
