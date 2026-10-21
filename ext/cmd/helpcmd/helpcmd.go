// Package helpcmd provides /help, which lists the commands a session can run.
package helpcmd

import (
	"context"
	"strings"

	"github.com/estonshi/aperture-potatos/kernel"
)

// Name is the extension's name, which std.Without takes.
const Name = "cmd.help"

func New() kernel.Extension {
	return kernel.ExtensionFunc(Name, func(r kernel.Registrar) error {
		_, err := r.Command(kernel.Command{
			Name:        "help",
			Description: "List the available commands.",
			Busy:        kernel.RunNow,
			Run:         run,
		})
		return err
	})
}

// run lists the visible commands; Data carries the same list for a UI to build completions from.
func run(_ context.Context, cc *kernel.CommandContext) (kernel.CommandResult, error) {
	cmds := cc.Session().Commands()
	var b strings.Builder
	for i, c := range cmds {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("/" + c.Name)
		if c.ArgHint != "" {
			b.WriteString(" " + c.ArgHint)
		}
		if c.Description != "" {
			b.WriteString("  " + c.Description)
		}
		if c.Busy == kernel.RunNow {
			b.WriteString(" (runs while busy)")
		}
	}
	return kernel.CommandResult{Text: b.String(), Data: cmds}, nil
}
