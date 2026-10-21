package kernel_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/store"
	"github.com/estonshi/aperture-potatos/store/jsonl"
)

func TestResumeHealsDanglingCallsAndContinues(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		st := jsonl.New(t.TempDir())
		first := resumeAgent(t, st, toolReply(call{"c1", "echo", `{"a":1}`}), textReply("done"))
		sess, err := first.NewSession(ctx)
		if err != nil {
			t.Fatal(err)
		}
		r, _ := sess.Send(kernel.Input{Text: "go"})
		if _, err := r.Wait(ctx); err != nil {
			t.Fatal(err)
		}
		id := sess.Info().SessionID
		_ = first.Close(ctx)
		lines, _ := st.Load(ctx, id)

		// a crash right after the assistant line: its tool line never reached the file
		crashed := store.NewMemory()
		var cut []message.Message
		for _, m := range lines {
			cut = append(cut, m)
			if m.Role == message.RoleAssistant && len(m.ToolCalls()) > 0 {
				break
			}
		}
		if err := crashed.Append(ctx, id, cut); err != nil {
			t.Fatal(err)
		}
		rec := &recorder{}
		second := resumeAgent(t, crashed, textReply("after restart"))
		back, err := second.Resume(ctx, id, kernel.WithSessionFunc(rec.handle))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := second.Resume(ctx, id); !errors.Is(err, kernel.ErrSessionOpen) {
			t.Fatalf("a second Resume of an open session: %v", err)
		}
		r, _ = back.Send(kernel.Input{Text: "and now?"})
		res, err := r.Wait(ctx)
		if err != nil || res.FinalText != "after restart" {
			t.Fatalf("turn after resume: %v %+v", err, res)
		}
		_ = second.Close(ctx)
		synctest.Wait()

		got, _ := crashed.Load(ctx, id)
		if err := pairedHistory(got); err != nil {
			t.Fatal(err)
		}
		lost := got[len(cut)]
		r0 := lost.Content[0].(*message.ToolResultBlock)
		if lost.Role != message.RoleTool || r0.Outcome != message.OutcomeLost || r0.CallID != "c1" {
			t.Fatalf("the dangling call gets a lost result: %+v", lost)
		}
		if lost.Seq != cut[len(cut)-1].Seq+1 || lost.Epoch != cut[0].Epoch {
			t.Fatalf("the repair continues the file's seq and epoch: seq %d epoch %d", lost.Seq, lost.Epoch)
		}
		var started *event.SessionStarted
		for _, ev := range rec.all() {
			if s, ok := ev.Data.(*event.SessionStarted); ok {
				started = s
			}
		}
		if started == nil || started.Source != "resume" {
			t.Fatalf("session.started{source:resume}: %+v", started)
		}
	})
}

func TestResumeUnknownSession(t *testing.T) {
	ag := resumeAgent(t, store.NewMemory())
	if _, err := ag.Resume(context.Background(), "ses_nope"); !errors.Is(err, kernel.ErrSessionNotFound) {
		t.Fatalf("got %v", err)
	}
}
