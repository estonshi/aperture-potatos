package rewindcmd_test

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/estonshi/aperture-potatos/conformance"
	"github.com/estonshi/aperture-potatos/ext/cmd/rewindcmd"
	"github.com/estonshi/aperture-potatos/kernel"
)

var update = flag.Bool("update", false, "rewrite expected files from the Go reference implementation")

func build(name string, raw json.RawMessage) (kernel.Extension, error) {
	if name != rewindcmd.Name {
		return nil, fmt.Errorf("unknown extension %q", name)
	}
	var o rewindcmd.Options
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &o); err != nil {
			return nil, err
		}
	}
	return rewindcmd.New(o), nil
}

// TestConformance runs conformance/loop/ext/rewindcmd, the cases this extension owns and deletes with itself.
func TestConformance(t *testing.T) {
	root := filepath.Join("..", "..", "..", "conformance", "loop", "ext", "rewindcmd")
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
