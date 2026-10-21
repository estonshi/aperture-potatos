package usagecmd_test

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/estonshi/aperture-potatos/conformance"
	"github.com/estonshi/aperture-potatos/ext/cmd/usagecmd"
	"github.com/estonshi/aperture-potatos/kernel"
)

var update = flag.Bool("update", false, "rewrite expected files from the Go reference implementation")

func build(name string, _ json.RawMessage) (kernel.Extension, error) {
	if name != usagecmd.Name {
		return nil, fmt.Errorf("unknown extension %q", name)
	}
	return usagecmd.New(), nil
}

// TestConformance runs conformance/loop/ext/usagecmd, the cases this extension owns and deletes with itself.
func TestConformance(t *testing.T) {
	root := filepath.Join("..", "..", "..", "conformance", "loop", "ext", "usagecmd")
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
