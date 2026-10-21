package prompt

import (
	"strings"
	"unicode/utf8"
)

// turnMarkers are rewritten at line starts, because a model may read them as a new turn of the conversation.
var turnMarkers = []string{"Human:", "Assistant:"}

// Sanitize escapes runtime tags and quotes turn markers at line starts in text another agent or task wrote.
func Sanitize(s string) string {
	s = EscapeRuntimeTags(s)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		for _, m := range turnMarkers {
			if rest, ok := strings.CutPrefix(l, m); ok {
				lines[i] = strings.TrimSuffix(m, ":") + " (quoted):" + rest
			}
		}
	}
	return strings.Join(lines, "\n")
}

// Truncate keeps the first max code points and reports how many it dropped.
func Truncate(s string, max int) (string, int) {
	n := utf8.RuneCountInString(s)
	if max < 0 || n <= max {
		return s, 0
	}
	cut := 0
	for i := range s {
		if cut == max {
			return s[:i], n - max
		}
		cut++
	}
	return s, 0
}
