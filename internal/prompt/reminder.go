// Package prompt holds the runtime's text encoding rules, shared by the kernel and the provider adapters so both write the same bytes.
package prompt

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/spec"
)

// EncodeReminder renders a reminder as tagged text; runtime tags inside the body are escaped first.
func EncodeReminder(r message.ReminderBlock) string {
	return "<system-reminder source=" + QuoteAttr(r.Source) + ">\n" + EscapeRuntimeTags(r.Text) + "\n</system-reminder>"
}

// QuoteAttr writes a tag attribute value as a JSON string by RFC 8785's rules after escaping runtime tags, so every binding produces the same bytes and a value can neither close its quotes nor forge a tag.
func QuoteAttr(v string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range EscapeRuntimeTags(v) { // ranging over a string turns each invalid UTF-8 byte into U+FFFD
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// EscapeRuntimeTags neutralizes opening and closing runtime tags in any letter case and with blanks after the bracket, so foreign text can neither forge a tag nor end one early.
func EscapeRuntimeTags(s string) string {
	if !strings.Contains(s, "<") {
		return s
	}
	return runtimeTags.ReplaceAllString(s, "&lt;$1")
}

// runtimeTagNames reads spec/data/runtime_tags.json, so every binding escapes exactly the same tags.
var runtimeTagNames = sync.OnceValue(func() []string {
	var doc struct {
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal([]byte(spec.MustText("data/runtime_tags.json")), &doc); err != nil {
		panic(err)
	}
	return doc.Tags
})

// runtimeTags is built from the same list.
var runtimeTags = func() *regexp.Regexp {
	names := make([]string, 0, len(runtimeTagNames()))
	for _, t := range runtimeTagNames() {
		names = append(names, regexp.QuoteMeta(t))
	}
	return regexp.MustCompile(`(?i)<(\s*/?\s*(?:` + strings.Join(names, "|") + `))`)
}()
