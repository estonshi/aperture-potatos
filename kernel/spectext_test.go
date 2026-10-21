package kernel_test

import (
	"testing"

	"github.com/estonshi/aperture-potatos/kernel"
)

func TestSpecTexts(t *testing.T) {
	text := kernel.SpecTexts("prompts/reminders.md")
	got := text("interrupted_note", "partial", "yes", "calls", "{{partial}} and {{#calls}}x{{/calls}}")
	want := "The previous turn was interrupted before it finished. Its last reply was cut off. These tool calls did not complete: {{partial}} and {{#calls}}x{{/calls}}. Do not assume the interrupted work was done."
	if got != want {
		t.Fatalf("a value was expanded again:\n got %q\nwant %q", got, want)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("a missing section did not panic")
		}
	}()
	text("no_such_section")
}

func TestRenderTemplateRejectsMalformed(t *testing.T) {
	if _, err := kernel.RenderTemplate("{{#a}}open", nil); err == nil {
		t.Fatal("an unclosed section rendered")
	}
}
