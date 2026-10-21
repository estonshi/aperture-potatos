package prompt

import (
	"strings"
	"testing"
)

func TestMustache(t *testing.T) {
	vars := map[string]string{"name": "Tater", "desc": "Plans trips.", "empty": ""}
	cases := []struct{ tmpl, want string }{
		{"Hi {{name}}.", "Hi Tater."},
		{"{{ name }}", "Tater"},
		{"a{{#desc}}\n{{desc}}{{/desc}}b", "a\nPlans trips.b"},
		{"a{{#empty}}x{{/empty}}b", "ab"},
		{"a{{^empty}}x{{/empty}}b", "axb"},
		{"a{{^desc}}x{{/desc}}b", "ab"},
		{"{{! a note }}x", "x"},
		{"{{missing}}!", "!"},
		{"{{#desc}}{{#name}}{{name}}{{/name}}{{/desc}}", "Tater"},
		{"<b>{{name}}</b> & {{desc}}", "<b>Tater</b> & Plans trips."},
	}
	for _, c := range cases {
		got, err := Mustache(c.tmpl, vars)
		if err != nil || got != c.want {
			t.Errorf("%q: got %q, %v; want %q", c.tmpl, got, err, c.want)
		}
	}
	for _, bad := range []string{"{{#a}}x", "{{#a}}x{{/b}}", "{{a", "{{}}", "{{&a}}", "{{> part}}", "x{{/a}}"} {
		if _, err := Mustache(bad, vars); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestSystemSkeleton(t *testing.T) {
	parts := System(SystemInput{Nickname: "Tater", Instructions: "Be brief."})
	var slots []string
	for _, p := range parts {
		slots = append(slots, p.Slot)
	}
	if got := strings.Join(slots, ","); got != "core,identity,user" {
		t.Fatalf("slots %s", got)
	}
	if parts[1].Text != "<identity>\nYour name is Tater.\n</identity>" {
		t.Fatalf("identity %q", parts[1].Text)
	}
	if parts[2].Text != "# Instructions from the agent developer\n\nBe brief." {
		t.Fatalf("user %q", parts[2].Text)
	}
	if !strings.HasPrefix(parts[0].Text, "<runtime-conventions>\n") || !strings.HasSuffix(parts[0].Text, "</runtime-conventions>") {
		t.Fatalf("core %q", parts[0].Text)
	}
	withDesc := System(SystemInput{Nickname: "Tater", Description: "Plans trips."})
	if len(withDesc) != 2 || withDesc[1].Text != "<identity>\nYour name is Tater.\nPlans trips.\n</identity>" {
		t.Fatalf("with description: %+v", withDesc)
	}
	again := System(SystemInput{Nickname: "Tater", Instructions: "Be brief."})
	for i := range parts {
		if parts[i] != again[i] {
			t.Fatal("rendering is not byte stable")
		}
	}
}

func TestEveryFixedTextRenders(t *testing.T) {
	for key, body := range texts() {
		if _, err := Mustache(body, nil); err != nil {
			t.Errorf("%s: %v", key, err)
		}
	}
	if got := Textf("turn_ended_not_executed", map[string]string{"outcome": "interrupted"}); !strings.Contains(got, "(interrupted)") {
		t.Fatalf("got %q", got)
	}
}
