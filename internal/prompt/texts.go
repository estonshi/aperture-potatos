package prompt

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/estonshi/aperture-potatos/spec"
)

var texts = sync.OnceValue(func() map[string]string { return Sections(spec.MustText("prompts/reminders.md")) })

// Source supplies a host's overrides of the fixed texts: Text serves a reminders.md key, File a prompt file name, and a missing entry falls back to the built-in text.
type Source interface {
	Text(key string) (string, bool)
	File(name string) (string, bool)
}

// Overrides is a map-backed Source, keyed by reminders.md key or prompt file name; the nil map overrides nothing.
type Overrides map[string]string

// Text implements Source.
func (o Overrides) Text(key string) (string, bool) { v, ok := o[key]; return v, ok }

// File implements Source; file names and text keys share one namespace, which Check keeps unambiguous.
func (o Overrides) File(name string) (string, bool) { return o.Text(name) }

// Keys lists the built-in text keys, sorted.
func Keys() []string { return slices.Sorted(maps.Keys(texts())) }

// PromptNames lists the built-in prompt files by name, sorted.
func PromptNames() []string { return spec.PromptNames() }

// Check reports whether every override names a fixed text or a prompt file, and whether an overridden system.core still carries the runtime tags.
func Check(o Overrides) error {
	names := map[string]bool{}
	for _, n := range PromptNames() {
		names[n] = true
	}
	for _, k := range slices.Sorted(maps.Keys(o)) {
		if _, ok := texts()[k]; !ok && !names[k] {
			return fmt.Errorf("no fixed text or prompt file is named %q", k)
		}
		if k == "system.core" {
			if err := checkCore(o[k]); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkCore refuses an override that drops a concern the model must still be told about, or that opens runtime-conventions without closing it.
func checkCore(text string) error {
	lower := strings.ToLower(text)
	for _, tag := range runtimeTagNames() {
		if !strings.Contains(lower, "<"+strings.ToLower(tag)) {
			return fmt.Errorf("a system.core override no longer mentions the runtime tag %q", tag)
		}
	}
	if !strings.Contains(lower, "</runtime-conventions") {
		return fmt.Errorf("a system.core override no longer closes runtime-conventions")
	}
	return nil
}

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
func Text(key string) string { return TextFrom(nil, key) }

// TextFrom returns the host's override for key when src has one, and the built-in text otherwise.
func TextFrom(src Source, key string) string {
	if src != nil {
		if v, ok := src.Text(key); ok {
			return v
		}
	}
	t, ok := texts()[key]
	if !ok {
		panic(fmt.Sprintf("prompt: no text %q in spec/prompts/reminders.md", key))
	}
	return t
}

// Textf renders a reminders.md text as a template; a malformed template is a build defect, so it panics.
func Textf(key string, vars map[string]string) string { return TextfFrom(nil, key, vars) }

// TextfFrom renders the text key resolves to through src as a template.
func TextfFrom(src Source, key string, vars map[string]string) string {
	return mustRender(key, TextFrom(src, key), vars)
}

// File returns spec/prompts/<name>.md without its final newline, which files end with but templates do not contain.
func File(name string) string { return FileFrom(nil, name) }

// FileFrom returns the host's override for a prompt file when src has one, and the built-in file otherwise.
func FileFrom(src Source, name string) string {
	if src != nil {
		if v, ok := src.File(name); ok {
			return v
		}
	}
	return strings.TrimSuffix(spec.MustText("prompts/"+name+".md"), "\n")
}

// Filef renders a prompt file as a template.
func Filef(name string, vars map[string]string) string { return FilefFrom(nil, name, vars) }

// FilefFrom renders the prompt file name resolves to through src as a template.
func FilefFrom(src Source, name string, vars map[string]string) string {
	return mustRender(name, FileFrom(src, name), vars)
}

func mustRender(name, tmpl string, vars map[string]string) string {
	s, err := Mustache(tmpl, vars)
	if err != nil {
		panic(fmt.Sprintf("prompt: template %s: %v", name, err))
	}
	return s
}
