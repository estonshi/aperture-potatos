package kernel

import "testing"

func TestMatchGlob(t *testing.T) {
	for _, c := range []struct {
		pattern, name string
		want          bool
	}{
		{"claude-opus-5-5*", "claude-opus-5-5", true},
		{"claude-opus-5-5*", "claude-opus-5-5-20260901", true},
		{"*", "anything/with/slashes", true},
		{"openai/*", "openai/gpt-oss-120b", true},
		{"gpt-?", "gpt-5", true},
		{"gpt-?", "gpt-55", false},
		{"*-mini", "o5-mini", true},
		{"*-mini", "o5-mini-high", false},
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "axxbyy", false},
		{"", "", true},
		{"", "x", false},
		{"模型*", "模型-大", true},
	} {
		if got := matchGlob(c.pattern, c.name); got != c.want {
			t.Errorf("matchGlob(%q, %q) = %v", c.pattern, c.name, got)
		}
	}
}
