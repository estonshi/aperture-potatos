package skills

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var errNoFrontmatter = errors.New("skills: SKILL.md does not start with a --- frontmatter block")

// split cuts SKILL.md into its frontmatter and its body.
func split(doc []byte) (raw []byte, body string, err error) {
	doc = bytes.TrimPrefix(doc, []byte("\ufeff"))
	lines := strings.SplitAfter(string(doc), "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], " \t\r\n") != "---" {
		return nil, "", errNoFrontmatter
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], " \t\r\n") == "---" {
			return []byte(strings.Join(lines[1:i], "")), strings.TrimSpace(strings.Join(lines[i+1:], "")), nil
		}
	}
	return nil, "", errNoFrontmatter
}

type line struct {
	indent int
	text   string // without the indent
}

// parseFrontmatter is the default parser: key: value pairs, quoted strings, block scalars, and one level of nested maps or lists.
func parseFrontmatter(raw []byte) (Frontmatter, error) {
	var lines []line
	for _, l := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		t := strings.TrimLeft(l, " ")
		if strings.HasPrefix(t, "\t") {
			return Frontmatter{}, fmt.Errorf("skills: tab indentation in frontmatter")
		}
		lines = append(lines, line{indent: len(l) - len(t), text: strings.TrimRight(t, " ")})
	}
	fm := Frontmatter{UserInvocable: true, ModelInvocable: true}
	for i := 0; i < len(lines); {
		l := lines[i]
		i++
		if l.text == "" || strings.HasPrefix(l.text, "#") {
			continue
		}
		if l.indent > 0 {
			return Frontmatter{}, fmt.Errorf("skills: unexpected indentation: %q", l.text)
		}
		key, rest, ok := strings.Cut(l.text, ":")
		if !ok || key == "" || strings.ContainsAny(key, " \"'") {
			return Frontmatter{}, fmt.Errorf("skills: not a key: %q", l.text)
		}
		rest = strings.TrimSpace(rest)
		var nested []line
		for i < len(lines) && (lines[i].text == "" || lines[i].indent > 0) {
			nested = append(nested, lines[i])
			i++
		}
		v, err := value(rest, nested)
		if err != nil {
			return Frontmatter{}, fmt.Errorf("skills: %s: %w", key, err)
		}
		if err := fm.set(key, v); err != nil {
			return Frontmatter{}, err
		}
	}
	return fm, nil
}

// value reads a scalar, a block scalar, a map or a list from the rest of a key's line and the indented lines below it.
func value(rest string, nested []line) (any, error) {
	switch rest {
	case "|", "|-", "|+", ">", ">-", ">+":
		return block(rest[0] == '>', nested), nil
	case "":
		return collection(nested)
	}
	if len(trimBlank(nested)) > 0 {
		if rest[0] == '"' || rest[0] == '\'' {
			return nil, errors.New("indented lines after a quoted value")
		}
		for _, l := range nested { // a plain scalar may continue on indented lines, which fold with single spaces
			if l.text != "" {
				rest += " " + l.text
			}
		}
	}
	return scalar(rest)
}

func trimBlank(ls []line) []line {
	for len(ls) > 0 && ls[len(ls)-1].text == "" {
		ls = ls[:len(ls)-1]
	}
	for len(ls) > 0 && ls[0].text == "" {
		ls = ls[1:]
	}
	return ls
}

// block keeps a literal block's line breaks and folds a folded block's lines with spaces; trailing line breaks are dropped either way.
func block(folded bool, nested []line) string {
	nested = trimBlank(nested)
	if len(nested) == 0 {
		return ""
	}
	base := nested[0].indent
	var b strings.Builder
	for i, l := range nested {
		text := ""
		if l.text != "" {
			text = strings.Repeat(" ", max(l.indent-base, 0)) + l.text
		}
		if i > 0 {
			switch {
			case !folded, text == "":
				b.WriteByte('\n')
			case nested[i-1].text != "": // after a blank line its line break already separates them
				b.WriteByte(' ')
			}
		}
		b.WriteString(text)
	}
	return b.String()
}

// collection reads one level of `key: value` or `- item` lines.
func collection(nested []line) (any, error) {
	nested = trimBlank(nested)
	if len(nested) == 0 {
		return "", nil
	}
	if strings.HasPrefix(nested[0].text, "- ") || nested[0].text == "-" {
		var out []any
		for _, l := range nested {
			if l.text == "" {
				continue
			}
			item, ok := strings.CutPrefix(l.text, "-")
			if !ok {
				return nil, fmt.Errorf("not a list item: %q", l.text)
			}
			v, err := scalar(strings.TrimSpace(item))
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	}
	out := map[string]any{}
	for _, l := range nested {
		if l.text == "" || strings.HasPrefix(l.text, "#") {
			continue
		}
		k, v, ok := strings.Cut(l.text, ":")
		if !ok || k == "" {
			return nil, fmt.Errorf("not a key: %q", l.text)
		}
		s, err := scalar(strings.TrimSpace(v))
		if err != nil {
			return nil, err
		}
		out[unquote(strings.TrimSpace(k))] = s
	}
	return out, nil
}

// scalar reads a quoted or plain value; a plain one keeps every colon in it and loses a trailing comment.
func scalar(s string) (string, error) {
	switch {
	case s == "":
		return "", nil
	case s[0] == '"':
		end := closing(s)
		if end < 0 {
			return "", errors.New("unterminated double-quoted value")
		}
		v, err := strconv.Unquote(s[:end+1])
		if err != nil {
			return "", fmt.Errorf("bad double-quoted value: %w", err)
		}
		return v, nil
	case s[0] == '\'':
		for i := 1; i < len(s); i++ {
			if s[i] == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				return strings.ReplaceAll(s[1:i], "''", "'"), nil
			}
		}
		return "", errors.New("unterminated single-quoted value")
	}
	if i := strings.Index(s, " #"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s, nil
}

// closing finds the quote that ends a double-quoted value, skipping escaped ones.
func closing(s string) int {
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}
	return -1
}

func unquote(s string) string {
	if v, err := scalar(s); err == nil {
		return v
	}
	return s
}

func (fm *Frontmatter) set(key string, v any) error {
	str := func() (string, error) {
		s, ok := v.(string)
		if !ok {
			return "", fmt.Errorf("skills: %s is not a string", key)
		}
		return s, nil
	}
	flag := func() (bool, error) {
		s, err := str()
		if err != nil {
			return false, err
		}
		switch strings.ToLower(s) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
		return false, fmt.Errorf("skills: %s is not true or false", key)
	}
	var err error
	switch key {
	case "name":
		fm.Name, err = str()
	case "description":
		fm.Description, err = str()
	case "license":
		fm.License, err = str()
	case "compatibility":
		fm.Compatibility, err = str()
	case "metadata":
		m, ok := v.(map[string]any)
		if !ok {
			if s, isStr := v.(string); isStr && s == "" {
				return nil
			}
			return errors.New("skills: metadata is not a map")
		}
		fm.Metadata = map[string]string{}
		for k, x := range m {
			fm.Metadata[k], _ = x.(string)
		}
	case "allowed-tools":
		fm.AllowedTools, err = toolList(v)
	case "user-invocable":
		fm.UserInvocable, err = flag()
	case "disable-model-invocation":
		var off bool
		off, err = flag()
		fm.ModelInvocable = !off
	default:
		if fm.Extra == nil {
			fm.Extra = map[string]any{}
		}
		fm.Extra[key] = v
	}
	return err
}

// toolList takes the spec's space-separated string, also split at commas outside parentheses, or a list.
func toolList(v any) ([]string, error) {
	switch x := v.(type) {
	case []any:
		var out []string
		for _, t := range x {
			if s, _ := t.(string); s != "" {
				out = append(out, s)
			}
		}
		return out, nil
	case string:
		var out []string
		var cur strings.Builder
		depth := 0
		flush := func() {
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		}
		for _, r := range x {
			switch {
			case r == '(':
				depth++
			case r == ')' && depth > 0:
				depth--
			case depth == 0 && (r == ' ' || r == ','):
				flush()
				continue
			}
			cur.WriteRune(r)
		}
		flush()
		return out, nil
	}
	return nil, errors.New("skills: allowed-tools is not a string or a list")
}
