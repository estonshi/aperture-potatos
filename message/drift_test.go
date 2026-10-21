package message

import (
	"reflect"
	"strings"
	"testing"

	"github.com/estonshi/aperture-potatos/internal/schemadrift"
)

func TestSchemaDrift(t *testing.T) {
	schemadrift.Check(t, reflect.TypeFor[Message](), "message.schema.json")
	schemadrift.Check(t, reflect.TypeFor[Usage](), "common.schema.json#/$defs/usage")
	blocks := map[string]reflect.Type{
		"text": reflect.TypeFor[TextBlock](), "refusal": reflect.TypeFor[RefusalBlock](), "image": reflect.TypeFor[ImageBlock](),
		"document": reflect.TypeFor[DocumentBlock](), "reasoning": reflect.TypeFor[ReasoningBlock](), "tool_call": reflect.TypeFor[ToolCallBlock](),
		"tool_result": reflect.TypeFor[ToolResultBlock](), "reminder": reflect.TypeFor[ReminderBlock](), "opaque": reflect.TypeFor[OpaqueBlock](),
		"hosted_call": reflect.TypeFor[HostedCallBlock](), "hosted_result": reflect.TypeFor[HostedResultBlock](),
	}
	for name, typ := range blocks {
		schemadrift.Check(t, typ, "message.schema.json#/$defs/"+name+"_block", "type")
	}
	for _, def := range schemadrift.Defs(t, "message.schema.json") {
		if name, ok := strings.CutSuffix(def, "_block"); ok && blocks[name] == nil {
			t.Errorf("schema defines block %q with no Go type", name)
		}
	}
}
