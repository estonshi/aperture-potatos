package prompt

import (
	"strings"
	"testing"
)

// TestOverrides: a section key and a prompt file name resolve through the same map, and every other key keeps the built-in text.
func TestOverrides(t *testing.T) {
	o := Overrides{
		"unknown_tool": "No {{name}}.",
		"identity":     "<identity>{{nickname}}</identity>",
	}
	if got := TextFrom(o, "unknown_tool"); got != "No {{name}}." {
		t.Fatalf("text: %q", got)
	}
	if got := TextfFrom(o, "unknown_tool", map[string]string{"name": "x"}); got != "No x." {
		t.Fatalf("textf: %q", got)
	}
	if got := TextFrom(o, "resume_lost"); got != Text("resume_lost") {
		t.Fatalf("fallback: %q", got)
	}
	if got := FileFrom(o, "identity"); got != "<identity>{{nickname}}</identity>" {
		t.Fatalf("file: %q", got)
	}
	if got := FileFrom(o, "user"); got != File("user") {
		t.Fatalf("file fallback: %q", got)
	}
	parts := SystemFrom(o, SystemInput{Nickname: "Tater"})
	if parts[0].Text != File("system.core") || parts[1].Text != "<identity>Tater</identity>" {
		t.Fatalf("system: %+v", parts)
	}
	if err := Check(o); err != nil {
		t.Fatalf("check: %v", err)
	}
}

// TestCheck: a key must name a text or a prompt file, reminders.md included, and an overridden core must keep the runtime tags closed.
func TestCheck(t *testing.T) {
	core := File("system.core")
	if err := Check(Overrides{"system.core": core}); err != nil {
		t.Fatalf("the built-in core must pass: %v", err)
	}
	for _, c := range []struct {
		name  string
		texts Overrides
		want  string
	}{
		{"typo", Overrides{"unknown_toool": "x"}, "no fixed text or prompt file"},
		{"fixed-text table", Overrides{"reminders": "x"}, "no fixed text or prompt file"},
		{"core drops a tag", Overrides{"system.core": "<runtime-conventions></runtime-conventions>"}, "runtime tag"},
		{"core unclosed", Overrides{"system.core": core[:strings.Index(core, "</runtime-conventions>")]}, "closes runtime-conventions"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := Check(c.texts)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
		})
	}
}

// TestPromptNames: the fixed-text table is not offered as a replaceable prompt file.
func TestPromptNames(t *testing.T) {
	names := PromptNames()
	for _, n := range names {
		if n == "reminders" {
			t.Fatal("reminders.md listed as a prompt file")
		}
	}
	if len(names) == 0 {
		t.Fatal("no prompt files")
	}
}
