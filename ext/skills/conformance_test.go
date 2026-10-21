package skills_test

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/estonshi/aperture-potatos/conformance"
	"github.com/estonshi/aperture-potatos/ext/skills"
	"github.com/estonshi/aperture-potatos/kernel"
)

var update = flag.Bool("update", false, "rewrite expected files from the Go reference implementation")

// caseOptions holds the skills of a case inline, path to file content, so a case is one script.
type caseOptions struct {
	Files map[string]string `json:"files"`
}

func build(name string, raw json.RawMessage) (kernel.Extension, error) {
	if name != skills.Name {
		return nil, fmt.Errorf("unknown extension %q", name)
	}
	var o caseOptions
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &o); err != nil {
			return nil, err
		}
	}
	fsys := fstest.MapFS{}
	for p, content := range o.Files {
		fsys[p] = &fstest.MapFile{Data: []byte(content)}
	}
	return skills.New(skills.Options{Sources: []skills.Source{skills.FS(fsys)}}), nil
}

// TestConformance runs conformance/loop/ext/skills, the cases this extension owns and deletes with itself.
func TestConformance(t *testing.T) {
	root := filepath.Join("..", "..", "conformance", "loop", "ext", "skills")
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
