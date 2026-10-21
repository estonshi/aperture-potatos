// Package compactcmd provides /compact, a manual model summary of the context with the arguments as extra instructions.
package compactcmd

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/estonshi/aperture-potatos/kernel"
)

// Name is the extension's name, which std.Without takes.
const Name = "cmd.compact"

func New() kernel.Extension {
	return kernel.ExtensionFunc(Name, func(r kernel.Registrar) error {
		_, err := r.Command(kernel.Command{
			Name:        "compact",
			Description: "Summarize the conversation to free context.",
			ArgHint:     "[instructions]",
			Busy:        kernel.WaitIdle,
			Run:         run,
		})
		return err
	})
}

func run(ctx context.Context, cc *kernel.CommandContext) (kernel.CommandResult, error) {
	rep, err := cc.Session().Compact(ctx, strings.TrimSpace(cc.Args())) // runs inline: this runner holds the history token
	switch {
	case errors.Is(err, kernel.ErrNothingToCompact):
		return kernel.CommandResult{Text: "Not enough messages to compact."}, nil
	case errors.Is(err, kernel.ErrNoCompactor):
		return kernel.CommandResult{Text: "No compaction strategy installed."}, nil
	case err != nil:
		return kernel.CommandResult{}, err
	}
	return kernel.CommandResult{Text: fmt.Sprintf("Compacted: %s → %s tokens (epoch %d).", tokens(rep.PreTokens), tokens(rep.PostTokens), rep.Epoch), Data: rep}, nil
}

// tokens writes a token count the way the command texts do: exact below 1000, then in thousands.
func tokens(n int64) string {
	switch {
	case n < 1000:
		return fmt.Sprint(n)
	case n < 9950: // one decimal until it would round to 10.0k
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1000), ".0") + "k"
	}
	return fmt.Sprintf("%dk", (n+500)/1000)
}
