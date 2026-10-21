package prompt

import (
	"fmt"
	"strings"
	"sync"

	"github.com/estonshi/aperture-potatos/spec"
)

var texts = sync.OnceValue(func() map[string]string { return Sections(spec.MustText("prompts/reminders.md")) })

// Sections splits a text file into its level-two sections, key to body with surrounding whitespace removed; text before the first heading is ignored.
func Sections(doc string) map[string]string {
	out := map[string]string{}
	var key string
	var body []string
	flush := func() {
		if key != "" {
			out[key] = strings.TrimSpace(strings.Join(body, "\n"))
		}
	}
	for line := range strings.SplitSeq(doc, "\n") {
		if k, ok := strings.CutPrefix(line, "## "); ok {
			flush()
			key, body = strings.TrimSpace(k), nil
			continue
		}
		body = append(body, line)
	}
	flush()
	return out
}

// Text returns a fixed runtime text from spec/prompts/reminders.md; an unknown key is a build defect, so it panics.
func Text(key string) string {
	t, ok := texts()[key]
	if !ok {
		panic(fmt.Sprintf("prompt: no text %q in spec/prompts/reminders.md", key))
	}
	return t
}

// Textf renders a reminders.md text as a template; a malformed template is a build defect, so it panics.
func Textf(key string, vars map[string]string) string {
	return mustRender(key, Text(key), vars)
}

// File returns spec/prompts/<name>.md without its final newline, which files end with but templates do not contain.
func File(name string) string {
	return strings.TrimSuffix(spec.MustText("prompts/"+name+".md"), "\n")
}

// Filef renders a prompt file as a template.
func Filef(name string, vars map[string]string) string {
	return mustRender(name, File(name), vars)
}

func mustRender(name, tmpl string, vars map[string]string) string {
	s, err := Mustache(tmpl, vars)
	if err != nil {
		panic(fmt.Sprintf("prompt: template %s: %v", name, err))
	}
	return s
}
