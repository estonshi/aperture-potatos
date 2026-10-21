package prompt

import (
	"fmt"
	"strings"
)

// Mustache renders the spec's template subset: {{name}} inserts a value verbatim, {{#name}}…{{/name}} keeps its body when the value is non-empty, {{^name}}…{{/name}} when it is empty, and {{! … }} is dropped.
// There is no standalone-line trimming and no escaping, so every binding produces the same bytes from the same template.
func Mustache(tmpl string, vars map[string]string) (string, error) {
	nodes, rest, err := parse(tmpl, "")
	if err != nil {
		return "", err
	}
	if rest != "" {
		return "", fmt.Errorf("prompt: unexpected text after the template: %q", rest)
	}
	var b strings.Builder
	render(&b, nodes, vars)
	return b.String(), nil
}

type node struct {
	text     string // literal text when name is empty
	name     string
	section  byte // 0 for a variable, '#' or '^' for a section
	children []node
}

// parse reads nodes until the closing tag of section open ("" at the top level) and returns what follows it.
func parse(s, open string) ([]node, string, error) {
	var out []node
	for {
		i := strings.Index(s, "{{")
		if i < 0 {
			if open != "" {
				return nil, "", fmt.Errorf("prompt: section %q is not closed", open)
			}
			if s != "" {
				out = append(out, node{text: s})
			}
			return out, "", nil
		}
		if i > 0 {
			out = append(out, node{text: s[:i]})
		}
		j := strings.Index(s[i:], "}}")
		if j < 0 {
			return nil, "", fmt.Errorf("prompt: unterminated tag at %q", s[i:])
		}
		tag := s[i+2 : i+j]
		s = s[i+j+2:]
		if tag == "" {
			return nil, "", fmt.Errorf("prompt: empty tag")
		}
		switch tag[0] {
		case '!':
		case '#', '^':
			name := strings.TrimSpace(tag[1:])
			children, rest, err := parse(s, name)
			if err != nil {
				return nil, "", err
			}
			out = append(out, node{name: name, section: tag[0], children: children})
			s = rest
		case '/':
			name := strings.TrimSpace(tag[1:])
			if name != open {
				return nil, "", fmt.Errorf("prompt: closing tag %q does not match %q", name, open)
			}
			return out, s, nil
		default:
			name := strings.TrimSpace(tag)
			if strings.ContainsAny(name, "{}&>=") {
				return nil, "", fmt.Errorf("prompt: tag %q is outside the supported subset", tag)
			}
			out = append(out, node{name: name})
		}
	}
}

func render(b *strings.Builder, nodes []node, vars map[string]string) {
	for _, n := range nodes {
		switch {
		case n.name == "":
			b.WriteString(n.text)
		case n.section == 0:
			b.WriteString(vars[n.name])
		case (n.section == '#') == (vars[n.name] != ""):
			render(b, n.children, vars)
		}
	}
}
