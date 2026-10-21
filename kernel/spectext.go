package kernel

import (
	"fmt"

	"github.com/estonshi/aperture-potatos/internal/prompt"
	"github.com/estonshi/aperture-potatos/spec"
)

// RenderTemplate renders tmpl in the template language every binding shares (spec/runtime.md §7): values go in verbatim and are never expanded again.
func RenderTemplate(tmpl string, vars map[string]string) (string, error) {
	return prompt.Mustache(tmpl, vars)
}

// SpecTexts reads a spec file of "## key" sections, such as an extension's texts.md, and returns a renderer taking name-value pairs; every section is checked here, so a malformed template or a missing key is a build defect and panics.
func SpecTexts(path string) func(key string, vars ...string) string {
	sections := prompt.Sections(spec.MustText(path))
	for key, body := range sections {
		if _, err := prompt.Mustache(body, nil); err != nil {
			panic(fmt.Sprintf("%s section %s: %v", path, key, err))
		}
	}
	return func(key string, vars ...string) string {
		body, ok := sections[key]
		if !ok {
			panic(fmt.Sprintf("%s has no section %s", path, key))
		}
		m := make(map[string]string, len(vars)/2)
		for i := 0; i+1 < len(vars); i += 2 {
			m[vars[i]] = vars[i+1]
		}
		s, _ := prompt.Mustache(body, m) // checked above, and values cannot make a template malformed
		return s
	}
}
