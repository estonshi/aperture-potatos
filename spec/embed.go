// Package spec embeds the language-neutral specification, so the Go binding reads the same bytes every other binding ships.
package spec

import (
	"embed"
	"io/fs"
	"slices"
	"strings"
)

//go:embed README.md VERSION CHANGELOG.md runtime.md schema/*.json data/*.json prompts/*.md all:ext
var files embed.FS

// PromptNames lists the prompt files by name, without the .md suffix, sorted; reminders.md is the fixed-text table, not a prompt a host may replace whole.
func PromptNames() []string {
	paths, err := fs.Glob(files, "prompts/*.md")
	if err != nil {
		panic(err)
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		name := strings.TrimSuffix(strings.TrimPrefix(p, "prompts/"), ".md")
		if name != "reminders" {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// ReadFile returns a spec file by its path relative to spec/, e.g. "schema/message.schema.json".
func ReadFile(path string) ([]byte, error) { return files.ReadFile(path) }

// MustText is ReadFile for files the binary cannot run without; a missing one is a build defect, so it panics.
func MustText(path string) string {
	b, err := files.ReadFile(path)
	if err != nil {
		panic(err)
	}
	return string(b)
}
