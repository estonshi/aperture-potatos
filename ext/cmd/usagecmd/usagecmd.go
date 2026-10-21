// Package usagecmd provides /usage, a report of the session's token use read from its snapshot, so it never waits on the session.
package usagecmd

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
)

// Name is the extension's name, which std.Without takes.
const Name = "cmd.usage"

// Report is /usage's structured result.
type Report struct {
	Usage        kernel.UsageReport  `json:"usage"`
	Context      kernel.ContextUsage `json:"context"`
	CacheHitRate float64             `json:"cache_hit_rate"` // cache reads over input, 0 before any input
}

func New() kernel.Extension {
	return kernel.ExtensionFunc(Name, func(r kernel.Registrar) error {
		_, err := r.Command(kernel.Command{
			Name:        "usage",
			Description: "Show token usage and context size.",
			Busy:        kernel.RunNow,
			Run:         run,
		})
		return err
	})
}

func run(_ context.Context, cc *kernel.CommandContext) (kernel.CommandResult, error) {
	sn := cc.Session().Snapshot()
	rep := Report{Usage: sn.Usage, Context: sn.Context, CacheHitRate: hitRate(sn.Usage.Total)}
	return kernel.CommandResult{Text: text(rep), Data: rep}, nil
}

// hitRate is cache reads over input, which already counts the cached tokens.
func hitRate(u message.Usage) float64 {
	if u.InputTokens == 0 {
		return 0
	}
	return float64(u.CacheReadTokens) / float64(u.InputTokens)
}

func text(r Report) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	u := r.Usage
	line("Total: %s", tokens(u.Total))
	for _, name := range slices.Sorted(maps.Keys(u.Buckets)) {
		line("  %s: %s", name, tokens(u.Buckets[name]))
	}
	for _, name := range slices.Sorted(maps.Keys(u.ByModel)) {
		line("Model %s: %s", name, tokens(u.ByModel[name]))
	}
	line("Last step: %s", tokens(u.LastStep))
	c := r.Context
	line("Context: %d / %d tokens (%s), compaction at %d", c.UsedTokens, c.WindowTokens, c.Basis, c.ThresholdTokens)
	line("Cache hit rate: %.0f%%", 100*r.CacheHitRate)
	line("Turns %d, steps %d, retries %d, discarded attempts %d, abandoned tools %d", u.Turns, u.Steps, u.Retries, u.Discards, u.Abandons)
	return strings.TrimSuffix(b.String(), "\n")
}

func tokens(u message.Usage) string {
	s := fmt.Sprintf("%d in, %d out", u.InputTokens, u.OutputTokens)
	if u.CacheReadTokens > 0 || u.CacheWriteTokens > 0 {
		s += fmt.Sprintf(", %d cache read, %d cache write", u.CacheReadTokens, u.CacheWriteTokens)
	}
	if u.ReasoningTokens > 0 {
		s += fmt.Sprintf(", %d reasoning", u.ReasoningTokens)
	}
	return s
}
