package provider

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/estonshi/aperture-potatos/internal/schemadrift"
	"github.com/estonshi/aperture-potatos/spec"
)

func TestSchemaDrift(t *testing.T) {
	for typ, def := range map[reflect.Type]string{
		reflect.TypeFor[Request]():      "request",
		reflect.TypeFor[StreamEvent]():  "stream_event",
		reflect.TypeFor[Capabilities](): "capabilities",
		reflect.TypeFor[SystemBlock]():  "system_block",
	} {
		schemadrift.Check(t, typ, "provider.schema.json#/$defs/"+def)
	}
	schemadrift.Check(t, reflect.TypeFor[Error](), "provider.schema.json#/$defs/error", "retry_after_ms")
	schemadrift.Check(t, reflect.TypeFor[ModelInfo](), "common.schema.json#/$defs/model_info")
}

// TestDefaultsMatchSpec keeps the Go constants equal to spec/data/limits.json, the source every binding reads.
func TestDefaultsMatchSpec(t *testing.T) {
	var l struct {
		Provider struct {
			ContextWindowTokens int `json:"context_window_tokens"`
			MaxOutputTokens     int `json:"max_output_tokens"`
		} `json:"provider"`
	}
	if err := json.Unmarshal([]byte(spec.MustText("data/limits.json")), &l); err != nil {
		t.Fatal(err)
	}
	if l.Provider.ContextWindowTokens != DefaultContextWindow || l.Provider.MaxOutputTokens != DefaultMaxOutputTokens {
		t.Fatalf("limits.json %+v, Go %d/%d", l.Provider, DefaultContextWindow, DefaultMaxOutputTokens)
	}
}
