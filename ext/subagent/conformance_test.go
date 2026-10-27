package subagent_test

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/estonshi/aperture-potatos/conformance"
	"github.com/estonshi/aperture-potatos/ext/subagent"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/std"
)

var update = flag.Bool("update", false, "rewrite expected files from the Go reference implementation")

// caseModel names a model of one of the case's providers.
type caseModel struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type caseDef struct {
	Name         string     `json:"name"`
	Description  string     `json:"description"`
	Instructions string     `json:"instructions"`
	Tools        []string   `json:"tools"`
	DenyTools    []string   `json:"deny_tools"`
	Model        *caseModel `json:"model"`
	FixedModel   bool       `json:"fixed_model"`
	MaxSteps     int        `json:"max_steps"`
	AskTimeoutMS int64      `json:"ask_timeout_ms"`
}

type caseOptions struct {
	Defs          []caseDef            `json:"defs"`
	MaxConcurrent int                  `json:"max_concurrent"`
	Models        []string             `json:"models"`
	Efforts       []string             `json:"efforts"`
	ResolveModel  map[string]caseModel `json:"resolve_model"`
}

// build makes this extension from the case's options and takes any other one from std, so a case that combines them skips once that one is deleted.
func build(name string, raw json.RawMessage, models map[string]provider.ModelRef) (kernel.Extension, error) {
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
	ref := func(m caseModel) (*provider.ModelRef, error) {
		r, ok := models[m.Provider]
		if !ok {
			return nil, fmt.Errorf("the case has no provider %q", m.Provider)
		}
		r.Model = m.Model
		return &r, nil
	}
	opts := subagent.Options{MaxConcurrent: o.MaxConcurrent, Models: o.Models, Efforts: o.Efforts}
	if len(o.Models) > 0 {
		opts.ResolveModel = func(alias string) (*provider.ModelRef, error) {
			m, ok := o.ResolveModel[alias]
			if !ok {
				return nil, fmt.Errorf("the case resolves no model %q", alias)
			}
			return ref(m)
		}
	}
	for _, d := range o.Defs {
		def := subagent.Def{Name: d.Name, Description: d.Description, Instructions: d.Instructions, Tools: d.Tools, DenyTools: d.DenyTools,
			FixedModel: d.FixedModel, MaxSteps: d.MaxSteps, AskTimeout: time.Duration(d.AskTimeoutMS) * time.Millisecond}
		if d.Model != nil {
			model, err := ref(*d.Model)
			if err != nil {
				return nil, err
			}
			def.Model = model
		}
		opts.Defs = append(opts.Defs, def)
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
			res := conformance.RunLoopWithModels(t, dir, build)
			if t.Failed() {
				return
			}
			if err := res.Expected(dir, *update); err != nil {
				t.Fatal(err)
			}
		})
	}
}
