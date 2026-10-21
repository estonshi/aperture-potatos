package forkcmd_test

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/estonshi/aperture-potatos/ext/cmd/forkcmd"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
	"github.com/estonshi/aperture-potatos/store"
)

func reply(text string) []providertest.Step {
	u := message.Usage{InputTokens: 100, OutputTokens: 10}
	m := &message.Message{Role: message.RoleAssistant, Origin: message.Origin{Kind: message.OriginModel}, Content: message.Content{&message.TextBlock{Text: text}}, Usage: &u, StopReason: string(provider.StopEndTurn)}
	return []providertest.Step{
		{Event: provider.StreamEvent{Kind: provider.SEStart, ResponseID: "resp"}},
		{Event: provider.StreamEvent{Kind: provider.SEBlockStart, Index: 0, BlockType: message.TypeText}},
		{Event: provider.StreamEvent{Kind: provider.SETextDelta, Index: 0, Channel: "text", Text: text}},
		{Event: provider.StreamEvent{Kind: provider.SEBlockStop, Index: 0, Block: &message.TextBlock{Text: text}}},
		{Event: provider.StreamEvent{Kind: provider.SEEnd, Message: m, Stop: &provider.Stop{Reason: provider.StopEndTurn, Raw: "end_turn"}, Usage: &u}},
	}
}

func TestForkBeforeTheLastTurns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		st := store.NewMemory()
		sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, reply("one"), reply("two"))
		ag, err := kernel.New(kernel.WithModel(provider.ModelRef{Provider: sp, Model: "m"}), kernel.WithCapabilityResolver(nil), kernel.WithStore(st), kernel.Use(forkcmd.New()))
		if err != nil {
			t.Fatal(err)
		}
		defer ag.Close(context.Background())
		sess, err := ag.NewSession(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, text := range []string{"first", "second"} {
			rc, _ := sess.Send(kernel.Text(text))
			if res, err := rc.Wait(ctx); err != nil || res.Err != nil {
				t.Fatal(err, res.Err)
			}
		}
		rc, err := sess.Command("fork", "1")
		if err != nil {
			t.Fatal(err)
		}
		res, err := rc.Wait(ctx)
		if err != nil {
			t.Fatal(err)
		}
		f, ok := res.Command.Data.(forkcmd.Forked)
		if !ok {
			t.Fatalf("data %+v", res.Command.Data)
		}
		back, err := ag.Resume(ctx, f.SessionID)
		if err != nil {
			t.Fatalf("the fork resumes: %v", err)
		}
		defer back.Close(context.Background())
		lines, _ := st.Load(ctx, f.SessionID)
		var texts []string
		for _, m := range store.Current(lines)[1:] {
			texts = append(texts, m.Text())
		}
		if len(texts) != 2 || texts[0] != "first" || texts[1] != "one" {
			t.Fatalf("/fork 1 copies what came before the last turn: %q", texts)
		}
	})
}
