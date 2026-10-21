package subagent_test

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/estonshi/aperture-potatos/conformance"
	"github.com/estonshi/aperture-potatos/ext/subagent"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/std"
)

var update = flag.Bool("update", false, "rewrite expected files from the Go reference implementation")

type caseDef struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Instructions string   `json:"instructions"`
	Tools        []string `json:"tools"`
	DenyTools    []string `json:"deny_tools"`
	MaxSteps     int      `json:"max_steps"`
	AskTimeoutMS int64    `json:"ask_timeout_ms"`
}

type caseOptions struct {
	Defs          []caseDef `json:"defs"`
	MaxConcurrent int       `json:"max_concurrent"`
}

// build makes this extension from the case's options and takes any other one from std, so a case that combines them skips once that one is deleted.
func build(name string, raw json.RawMessage) (kernel.Extension, error) {
	if name != subagent.Name {
		if i := slices.IndexFunc(std.Extensions(), func(e kernel.Extension) bool { return e.Name() == name }); i >= 0 {
			return std.Extensions()[i], nil
		}
		return nil, conformance.ErrExtensionMissing
	}
	var o caseOptions
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &o); err != nil {
			return nil, err
		}
	}
	opts := subagent.Options{MaxConcurrent: o.MaxConcurrent}
	for _, d := range o.Defs {
		opts.Defs = append(opts.Defs, subagent.Def{Name: d.Name, Description: d.Description, Instructions: d.Instructions, Tools: d.Tools, DenyTools: d.DenyTools,
			MaxSteps: d.MaxSteps, AskTimeout: time.Duration(d.AskTimeoutMS) * time.Millisecond})
	}
	return subagent.New(opts), nil
}

// TestConformance runs conformance/loop/ext/subagent, the cases this extension owns and deletes with itself.
func TestConformance(t *testing.T) {
	root := filepath.Join("..", "..", "conformance", "loop", "ext", "subagent")
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
