// Package rewindcmd provides /rewind, which undoes the last turns of the current epoch.
package rewindcmd

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/estonshi/aperture-potatos/kernel"
)

// Name is the extension's name, which std.Without takes.
const Name = "cmd.rewind"

type Options struct {
	Interrupt bool // InterruptFirst instead of WaitIdle, so a runaway turn can be undone at once
}

func New(o Options) kernel.Extension {
	busy := kernel.WaitIdle
	if o.Interrupt {
		busy = kernel.InterruptFirst
	}
	return kernel.ExtensionFunc(Name, func(r kernel.Registrar) error {
		_, err := r.Command(kernel.Command{
			Name:        "rewind",
			Description: "Undo the last n turns (default 1).",
			ArgHint:     "[n]",
			Busy:        busy,
			Run:         run,
		})
		return err
	})
}

func run(ctx context.Context, cc *kernel.CommandContext) (kernel.CommandResult, error) {
	n := 1
	if a := strings.TrimSpace(cc.Args()); a != "" {
		v, err := strconv.Atoi(a)
		if err != nil || v < 1 {
			return kernel.CommandResult{Text: "Usage: /rewind [n] with n >= 1."}, nil
		}
		n = v
	}
	s := cc.Session()
	turns := s.Turns()
	if len(turns) == 0 {
		return kernel.CommandResult{Text: "Nothing to rewind."}, nil
	}
	n = min(n, len(turns))
	rep, err := s.Rewind(ctx, turns[len(turns)-n].ID) // runs inline: this runner holds the history token
	if err != nil {
		return kernel.CommandResult{}, err
	}
	unit := "turns"
	if n == 1 {
		unit = "turn"
	}
	return kernel.CommandResult{Text: fmt.Sprintf("Rewound %d %s (epoch %d).", n, unit, rep.Epoch), Data: rep}, nil
}
