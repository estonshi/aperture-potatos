// Package contextcmd provides /context, the current context broken down by source.
package contextcmd

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/estonshi/aperture-potatos/kernel"
)

// Name is the extension's name, which std.Without takes.
const Name = "cmd.context"

func New() kernel.Extension {
	return kernel.ExtensionFunc(Name, func(r kernel.Registrar) error {
		_, err := r.Command(kernel.Command{
			Name:        "context",
			Description: "Show what the context is made of.",
			Busy:        kernel.RunNow,
			Run:         run,
		})
		return err
	})
}

func run(_ context.Context, cc *kernel.CommandContext) (kernel.CommandResult, error) {
	rep := cc.Session().Context()
	return kernel.CommandResult{Text: text(rep), Data: rep}, nil
}

func text(r kernel.ContextReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Model %s, epoch %d\n", r.Model.Model, r.Epoch)
	fmt.Fprintf(&b, "Context: %d / %d tokens (%s), usable %d, compaction at %d", r.Used, r.Window, r.Basis, r.Usable, r.Threshold)
	parts := slices.Clone(r.Parts)
	slices.SortStableFunc(parts, func(a, b kernel.ContextPart) int { return cmp.Compare(b.Tokens, a.Tokens) }) // the largest first, the report's order among equals
	for _, p := range parts {
		label := p.Kind
		if p.Name != "" {
			label += " " + p.Name
		}
		if p.Count > 0 {
			label += fmt.Sprintf(" (%d)", p.Count)
		}
		share := 0.0
		if r.Used > 0 {
			share = 100 * float64(p.Tokens) / float64(r.Used)
		}
		fmt.Fprintf(&b, "\n  %s: %d tokens, %.1f%%", label, p.Tokens, share)
	}
	return b.String()
}
