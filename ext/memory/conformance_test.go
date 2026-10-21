package memory_test

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/estonshi/aperture-potatos/conformance"
	"github.com/estonshi/aperture-potatos/ext/memory"
	"github.com/estonshi/aperture-potatos/kernel"
)

var update = flag.Bool("update", false, "rewrite expected files from the Go reference implementation")

// caseOptions holds a case's instruction files and the memories its store starts with, so a case is one script.
type caseOptions struct {
	Instructions map[string]string `json:"instructions"`
	Order        []string          `json:"order"` // instruction files in load order
	Items        []struct {
		User    string `json:"user"`
		Project string `json:"project"`
		Content string `json:"content"`
	} `json:"items"`
	ConsolidateOn []string `json:"consolidate_on"`
	NoTools       bool     `json:"no_tools"`
	SharedWrites  bool     `json:"shared_writes"`
}

func build(name string, raw json.RawMessage) (kernel.Extension, error) {
	if name != memory.Name {
		return nil, fmt.Errorf("unknown extension %q", name)
	}
	var o caseOptions
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &o); err != nil {
			return nil, err
		}
	}
	opts := memory.Options{Store: memory.NewInMemory(), ConsolidateOn: o.ConsolidateOn, NoTools: o.NoTools, SharedWrites: o.SharedWrites}
	var adds []memory.Change
	for _, it := range o.Items {
		adds = append(adds, memory.Change{Op: "add", Content: &it.Content, User: &it.User, Project: &it.Project})
	}
	if _, err := opts.Store.Apply(context.Background(), memory.Scope{}, adds); err != nil {
		return nil, err
	}
	if len(o.Order) > 0 {
		fsys := fstest.MapFS{}
		for p, content := range o.Instructions {
			fsys[p] = &fstest.MapFile{Data: []byte(content)}
		}
		opts.Instructions = memory.Files(fsys, o.Order...)
	}
	return memory.New(opts), nil
}

// TestConformance runs conformance/loop/ext/memory, the cases this extension owns and deletes with itself.
func TestConformance(t *testing.T) {
	root := filepath.Join("..", "..", "conformance", "loop", "ext", "memory")
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
