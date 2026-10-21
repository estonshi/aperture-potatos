// Package forkcmd provides /fork, which copies the context into a new stored session the host resumes to continue there.
package forkcmd

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/estonshi/aperture-potatos/kernel"
)

// Name is the extension's name, which std.Without takes.
const Name = "cmd.fork"

// Forked is /fork's structured result.
type Forked struct {
	SessionID string `json:"session_id"`
}

func New() kernel.Extension {
	return kernel.ExtensionFunc(Name, func(r kernel.Registrar) error {
		_, err := r.Command(kernel.Command{
			Name:        "fork",
			Description: "Copy the conversation, or all but its last n turns, into a new session.",
			ArgHint:     "[n]",
			Busy:        kernel.RunNow,
			Run:         run,
		})
		return err
	})
}

func run(ctx context.Context, cc *kernel.CommandContext) (kernel.CommandResult, error) {
	s := cc.Session()
	if s.Info().SideChat != "" { // turning a side chat into a session of its own is the host's call, through Fork
		return kernel.CommandResult{Text: "/fork is not available in a side chat."}, nil
	}
	turns := s.Turns()
	var at string
	if tid := s.Snapshot().TurnID; len(turns) > 0 && turns[len(turns)-1].ID == tid {
		at, turns = tid, turns[:len(turns)-1] // the running turn's input is stable, but a fork of a question without its answer is not what /fork means
	}
	if a := strings.TrimSpace(cc.Args()); a != "" {
		n, err := strconv.Atoi(a)
		if err != nil || n < 1 {
			return kernel.CommandResult{Text: "Usage: /fork [n] with n >= 1."}, nil
		}
		if len(turns) > 0 {
			at = turns[len(turns)-min(n, len(turns))].ID
		}
	}
	var opts []kernel.ForkOption
	if at != "" {
		opts = append(opts, kernel.ForkAt(at))
	}
	f, err := s.Fork(ctx, opts...)
	if err != nil {
		return kernel.CommandResult{}, err
	}
	id := f.Info().SessionID
	if err := f.Close(ctx); err != nil { // the fork is already stored; the host resumes it where it wants it
		return kernel.CommandResult{}, err
	}
	return kernel.CommandResult{Text: fmt.Sprintf("Forked to %s; resume it to continue there.", id), Data: Forked{SessionID: id}}, nil
}
