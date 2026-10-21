package caps

import (
	"encoding/json"
	"testing"

	"github.com/estonshi/aperture-potatos/provider"
)

func TestMaxOutput(t *testing.T) {
	cases := []struct {
		cfg, known, want int
		adjusted         bool
	}{{0, 0, 64000, false}, {0, 32000, 32000, true}, {100000, 0, 100000, false}, {8000, 32000, 8000, false}}
	for _, c := range cases {
		n, adj := MaxOutput(provider.Params{MaxOutputTokens: c.cfg}, provider.Capabilities{MaxOutputTokens: c.known})
		if n != c.want || adj != c.adjusted {
			t.Errorf("%+v: got %d %v", c, n, adj)
		}
	}
}

func TestMapEffort(t *testing.T) {
	deepseek := []string{"low", "high", "max"}
	for in, want := range map[string]string{"minimal": "low", "medium": "high", "xhigh": "high", "high": "high", "max": "max", "none": "low"} {
		if got, _ := MapEffort(in, deepseek); got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}
	if got, adj := MapEffort("medium", nil); got != "medium" || adj {
		t.Fatal("unknown efforts never rewrite")
	}
}

func TestStrictOK(t *testing.T) {
	ok := `{"type":"object","properties":{"a":{"type":"object","properties":{"b":{"type":"string"}},"required":["b"],"additionalProperties":false}},"required":["a"],"additionalProperties":false}`
	notClosed := `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`
	notRequired := `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}},"required":["a"],"additionalProperties":false}`
	for s, want := range map[string]bool{ok: true, notClosed: false, notRequired: false, `not json`: false} {
		if StrictOK(json.RawMessage(s)) != want {
			t.Errorf("%s: want %v", s, want)
		}
	}
}
