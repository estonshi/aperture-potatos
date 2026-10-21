package usagecmd_test

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/estonshi/aperture-potatos/ext/cmd/usagecmd"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
)

func reply(text string, u message.Usage) []providertest.Step {
	m := &message.Message{Role: message.RoleAssistant, Origin: message.Origin{Kind: message.OriginModel}, Content: message.Content{&message.TextBlock{Text: text}}, Usage: &u}
	return []providertest.Step{
		{Event: provider.StreamEvent{Kind: provider.SEStart}},
		{Event: provider.StreamEvent{Kind: provider.SEBlockStart, Index: 0, BlockType: message.TypeText}},
		{Event: provider.StreamEvent{Kind: provider.SEBlockStop, Index: 0, Block: &message.TextBlock{Text: text}}},
		{Event: provider.StreamEvent{Kind: provider.SEEnd, Message: m, Stop: &provider.Stop{Reason: provider.StopEndTurn}, Usage: &u}},
	}
}

func TestUsageReportsTheSnapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, reply("hi", message.Usage{InputTokens: 1000, CacheReadTokens: 250, OutputTokens: 20}))
		ag, err := kernel.New(kernel.WithModel(provider.ModelRef{Provider: sp, Model: "m"}), kernel.WithCapabilityResolver(nil), kernel.Use(usagecmd.New()))
		if err != nil {
			t.Fatal(err)
		}
		defer ag.Close(context.Background())
		sess, err := ag.NewSession(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		rc, _ := sess.Send(kernel.Text("hello"))
		if _, err := rc.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		rc, err = sess.Command("usage", "")
		if err != nil {
			t.Fatal(err)
		}
		res, err := rc.Wait(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		rep, ok := res.Command.Data.(usagecmd.Report)
		if !ok || rep.Usage.Total.InputTokens != 1000 || rep.CacheHitRate != 0.25 || rep.Usage.Turns != 1 {
			t.Fatalf("report %+v", res.Command.Data)
		}
		for _, want := range []string{"Total: 1000 in, 20 out, 250 cache read", "Cache hit rate: 25%", "Turns 1, steps 1"} {
			if !strings.Contains(res.Command.Text, want) {
				t.Fatalf("text %q lacks %q", res.Command.Text, want)
			}
		}
	})
}
