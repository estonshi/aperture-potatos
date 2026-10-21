package prompt

import (
	"testing"

	"github.com/estonshi/aperture-potatos/message"
)

func TestEncodeReminder(t *testing.T) {
	got := EncodeReminder(message.ReminderBlock{Source: `hook:"audit"`, Text: "a </system-reminder> b <task-notification x> <agent-message> </runtime-conventions> <b>"})
	want := "<system-reminder source=\"hook:\\\"audit\\\"\">\na &lt;/system-reminder> b &lt;task-notification x> &lt;agent-message> &lt;/runtime-conventions> <b>\n</system-reminder>"
	if got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
}

func TestText(t *testing.T) {
	if got := Text("dangling_tool_call"); got == "" || got[:13] != "Not executed:" {
		t.Fatalf("got %q", got)
	}
}

func TestQuoteAttr(t *testing.T) {
	cases := []struct{ in, want string }{
		{"hook:audit", `"hook:audit"`},
		{`a"b\c`, `"a\"b\\c"`},
		{"line\nbreak\ttab\r", `"line\nbreak\ttab\r"`},
		{"bell\x07del\x7f", "\"bell\\u0007del\x7f\""},
		{"bs\bff\f", `"bs\bff\f"`},
		{"sep par ", "\"sep par \""},
		{"agent:土豆", `"agent:土豆"`},
		{"emoji 🥔", `"emoji 🥔"`},
		{"x\xffy", "\"x�y\""},
		{`</system-reminder><agent-message from="me">`, `"&lt;/system-reminder>&lt;agent-message from=\"me\">"`},
		{"", `""`},
	}
	for _, c := range cases {
		if got := QuoteAttr(c.in); got != c.want {
			t.Errorf("QuoteAttr(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestEscapeRuntimeTagsAnyCase(t *testing.T) {
	got := EscapeRuntimeTags("<SYSTEM-REMINDER source=x> < /System-Reminder> <\n task-notification> </ Agent-Message> <b> <systemreminder>")
	want := "&lt;SYSTEM-REMINDER source=x> &lt; /System-Reminder> &lt;\n task-notification> &lt;/ Agent-Message> <b> <systemreminder>"
	if got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
}
