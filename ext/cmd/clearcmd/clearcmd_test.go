package clearcmd_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/estonshi/aperture-potatos/ext/cmd/clearcmd"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
)

func TestInterruptClearsARunningTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, []providertest.Step{{Event: provider.StreamEvent{Kind: provider.SEStart}}, {Hold: -1}})
		ag, err := kernel.New(kernel.WithModel(provider.ModelRef{Provider: sp, Model: "m"}), kernel.WithCapabilityResolver(nil), kernel.Use(clearcmd.New(clearcmd.Options{Interrupt: true})))
		if err != nil {
			t.Fatal(err)
		}
		defer ag.Close(context.Background())
		sess, err := ag.NewSession(ctx)
		if err != nil {
			t.Fatal(err)
		}
		turn, _ := sess.Send(kernel.Text("go on forever"))
		synctest.Wait()
		rc, err := sess.Command("clear", "")
		if err != nil {
			t.Fatal(err)
		}
		res, err := rc.Wait(ctx)
		if err != nil || res.Command.Text != "Context cleared (epoch 2)." {
			t.Fatalf("%v %+v", err, res.Command)
		}
		tr, _ := turn.Wait(ctx)
		var ie *kernel.InterruptError
		if !errors.As(tr.Err, &ie) {
			t.Fatalf("the running turn is interrupted first: %v", tr.Err)
		}
	})
}
