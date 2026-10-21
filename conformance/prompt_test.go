package conformance

import (
	"cmp"
	"path/filepath"
	"slices"
	"testing"

	"github.com/estonshi/aperture-potatos/internal/prompt"
)

type mustacheCase struct {
	Template string            `json:"template"`
	Vars     map[string]string `json:"vars"`
}

type mustacheExpected struct {
	Text  *string `json:"text,omitempty"`
	Error bool    `json:"error,omitempty"`
}

type systemVars struct {
	Nickname     string    `json:"nickname"`
	Description  string    `json:"description,omitempty"`
	Instructions string    `json:"instructions,omitempty"`
	Sections     []section `json:"sections,omitempty"`
}

type section struct {
	Slot  string `json:"slot"`
	Name  string `json:"name"`
	Order int    `json:"order,omitempty"`
	Text  string `json:"text"`
}

type systemBlock struct {
	Slot string `json:"slot"`
	Name string `json:"name"`
	Text string `json:"text"`
}

type systemExpected struct {
	Blocks     []systemBlock `json:"blocks"`
	SystemHash string        `json:"system_hash"`
}

// TestPrompt checks the template language and the system skeleton of runtime.md §7; strings compare exactly, since prompt bytes decide prompt caching.
func TestPrompt(t *testing.T) {
	for _, dir := range promptCases(t, "mustache") {
		t.Run("mustache/"+filepath.Base(dir), func(t *testing.T) {
			var c mustacheCase
			mustRead(t, filepath.Join(dir, "case.json"), &c)
			var got mustacheExpected
			if text, err := prompt.Mustache(c.Template, c.Vars); err != nil {
				got.Error = true
			} else {
				got.Text = &text
			}
			exact(t, dir, "actual.json", got)
		})
	}
	for _, dir := range promptCases(t, "system") {
		t.Run("system/"+filepath.Base(dir), func(t *testing.T) {
			var v systemVars
			mustRead(t, filepath.Join(dir, "vars.json"), &v)
			slotRank := map[string]int{prompt.SlotCapabilities: 0, prompt.SlotCatalog: 1, prompt.SlotEnvironment: 2}
			sections := slices.Clone(v.Sections)
			slices.SortStableFunc(sections, func(a, b section) int {
				return cmp.Or(cmp.Compare(slotRank[a.Slot], slotRank[b.Slot]), cmp.Compare(a.Order, b.Order), cmp.Compare(a.Name, b.Name))
			})
			in := prompt.SystemInput{Nickname: v.Nickname, Description: v.Description, Instructions: v.Instructions}
			for _, s := range sections {
				in.Sections = append(in.Sections, prompt.Part{Slot: s.Slot, Name: s.Name, Text: s.Text})
			}
			var got systemExpected
			var texts []string
			for _, p := range prompt.System(in) {
				got.Blocks = append(got.Blocks, systemBlock(p))
				texts = append(texts, p.Text)
			}
			got.SystemHash = prompt.Hash(texts)
			exact(t, dir, "actual.system.json", got)
		})
	}
}

// promptCases lists one kind's cases and fails when there are none, so a moved directory cannot pass silently.
func promptCases(t *testing.T, kind string) []string {
	t.Helper()
	cases, err := Cases(filepath.Join("prompt", kind))
	if err != nil || len(cases) == 0 {
		t.Fatalf("no prompt/%s cases: %v", kind, err)
	}
	return cases
}

func mustRead(t *testing.T, path string, v any) {
	t.Helper()
	if err := readJSON(path, v); err != nil {
		t.Fatal(err)
	}
}

// exact compares got with the case's expected file as a JSON value, so every string must match byte for byte.
func exact(t *testing.T, dir, name string, got any) {
	t.Helper()
	if err := Verify(dir, *update, func(out string) error { return writeJSON(out, name, got) }); err != nil {
		t.Error(err)
	}
}
