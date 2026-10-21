// Package clearcmd provides /clear, which starts a new epoch without the old lines.
package clearcmd

import (
	"context"
	"fmt"

	"github.com/estonshi/aperture-potatos/kernel"
)

// Name is the extension's name, which std.Without takes.
const Name = "cmd.clear"

type Options struct {
	Interrupt bool // InterruptFirst instead of WaitIdle, so a runaway turn can be cleared at once
}

func New(o Options) kernel.Extension {
	busy := kernel.WaitIdle
	if o.Interrupt {
		busy = kernel.InterruptFirst
	}
	return kernel.ExtensionFunc(Name, func(r kernel.Registrar) error {
		_, err := r.Command(kernel.Command{
			Name:        "clear",
			Description: "Start over with an empty context.",
			Busy:        busy,
			Run:         run,
		})
		return err
	})
}

func run(ctx context.Context, cc *kernel.CommandContext) (kernel.CommandResult, error) {
	s := cc.Session()
	if err := s.Clear(ctx); err != nil { // runs inline: this runner holds the history token
		return kernel.CommandResult{}, err
	}
	return kernel.CommandResult{Text: fmt.Sprintf("Context cleared (epoch %d).", s.Info().Epoch)}, nil
}
