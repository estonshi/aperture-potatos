package compact_test

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/estonshi/aperture-potatos/conformance"
	"github.com/estonshi/aperture-potatos/ext/compact"
	"github.com/estonshi/aperture-potatos/kernel"
)

var update = flag.Bool("update", false, "rewrite expected files from the Go reference implementation")

// caseOptions is Options as a case writes it, per spec/ext/compact/conformance.md.
type caseOptions struct {
	DropCurrentTurn bool   `json:"drop_current_turn"`
	SummaryPrompt   string `json:"summary_prompt"`
	Prune           *struct {
		KeepRecent int `json:"keep_recent"`
		MinTokens  int `json:"min_tokens"`
	} `json:"prune"`
}

func build(name string, raw json.RawMessage) (kernel.Extension, error) {
	if name != "compact" {
		return nil, fmt.Errorf("unknown extension %q", name)
	}
	var co caseOptions
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &co); err != nil {
			return nil, err
		}
	}
	o := compact.Options{DropCurrentTurn: co.DropCurrentTurn, SummaryPrompt: co.SummaryPrompt}
	if p := co.Prune; p != nil {
		o.Prune = &compact.PruneOptions{KeepRecent: p.KeepRecent, MinTokens: p.MinTokens}
	}
	return compact.New(o), nil
}

// TestConformance runs conformance/loop/ext/compact, the cases this extension owns and deletes with itself.
func TestConformance(t *testing.T) {
	root := filepath.Join("..", "..", "conformance", "loop", "ext", "compact")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Skip("no cases")
	}
	for _, e := range entries {
		dir := filepath.Join(root, e.Name())
		t.Run(e.Name(), func(t *testing.T) {
			res := conformance.RunLoopWith(t, dir, build)
			if t.Failed() {
				return
			}
			if err := res.Expected(dir, *update); err != nil {
				t.Fatal(err)
			}
		})
	}
}
