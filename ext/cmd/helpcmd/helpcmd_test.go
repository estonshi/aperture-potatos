package helpcmd_test

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/estonshi/aperture-potatos/ext/cmd/helpcmd"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
)

func TestHelpListsVisibleCommands(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		noop := func(context.Context, *kernel.CommandContext) (kernel.CommandResult, error) {
			return kernel.CommandResult{}, nil
		}
		ag, err := kernel.New(
			kernel.WithModel(provider.ModelRef{Provider: providertest.NewScriptProvider(provider.APIAnthropicMessages), Model: "m"}),
			kernel.Use(helpcmd.New()),
			kernel.WithCommand(kernel.Command{Name: "trip", Description: "Plan a trip.", ArgHint: "<city>", Run: noop}),
			kernel.WithCommand(kernel.Command{Name: "secret", Hidden: true, Run: noop}),
		)
		if err != nil {
			t.Fatal(err)
		}
		defer ag.Close(context.Background())
		sess, err := ag.NewSession(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		rc, err := sess.Send(kernel.Text("/help"))
		if err != nil {
			t.Fatal(err)
		}
		res, err := rc.Wait(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		want := "/help  List the available commands. (runs while busy)\n/trip <city>  Plan a trip."
		if res.Command == nil || res.Command.Text != want {
			t.Fatalf("got %q, want %q", res.Command.Text, want)
		}
		if cmds, ok := res.Command.Data.([]kernel.CommandInfo); !ok || len(cmds) != 2 {
			t.Fatalf("data %#v", res.Command.Data)
		}
	})
}
