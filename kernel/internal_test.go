package kernel

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
)

func reply(text string) []providertest.Step {
	u := message.Usage{InputTokens: 1, OutputTokens: 1}
	m := &message.Message{Role: message.RoleAssistant, Origin: message.Origin{Kind: message.OriginModel}, Content: message.Content{&message.TextBlock{Text: text}}, Usage: &u}
	return []providertest.Step{
		{Event: provider.StreamEvent{Kind: provider.SEStart}},
		{Event: provider.StreamEvent{Kind: provider.SEEnd, Message: m, Stop: &provider.Stop{Reason: provider.StopEndTurn}, Usage: &u}},
	}
}

func TestInternalPanicClosesTheSession(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := providertest.NewScriptProvider(provider.APIAnthropicMessages)
		ag, err := New(WithModel(provider.ModelRef{Provider: sp, Model: "m"}))
		if err != nil {
			t.Fatal(err)
		}
		var last []event.Type
		s, err := ag.NewSession(t.Context(), WithSessionFunc(func(_ context.Context, ev event.Event) error {
			last = append(last, ev.Type)
			return nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		pending, _ := s.Send(Text("never answered"), NoWake())
		s.post(func(*state) { panic("kernel bug") })
		<-s.done
		synctest.Wait()
		if res, _ := pending.Wait(t.Context()); !errors.Is(res.Err, ErrSessionClosed) {
			t.Fatalf("pending receipt %+v", res)
		}
		if _, err := s.Send(Text("x")); !errors.Is(err, ErrSessionClosed) {
			t.Fatalf("send after the crash: %v", err)
		}
		n := len(last)
		if n < 2 || last[n-2] != event.TypeError || last[n-1] != event.TypeSessionClosed {
			t.Fatalf("last events %v", last[max(0, n-3):])
		}
		if err := ag.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

// kernelFrames lists goroutines that run code of this module, other than the caller's; a closed agent must leave none.
func kernelFrames() []string {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	var out []string
	for i, g := range strings.Split(string(buf[:n]), "\n\n") {
		if i == 0 {
			continue // the goroutine taking the dump
		}
		if strings.Contains(g, "aperture-potatos.") && !strings.Contains(g, "testing.tRunner") {
			out = append(out, g)
		}
	}
	return out
}

// noLeaks polls briefly, because a pump may still be delivering its last event when Close returns.
func noLeaks(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		left := kernelFrames()
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines left:\n%s", len(left), strings.Join(left, "\n\n"))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestRealTimeLeavesNothingBehind runs outside a synctest bubble, so the leak check is the runtime's own view.
func TestRealTimeLeavesNothingBehind(t *testing.T) {
	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, reply("one"), reply("two"))
	ag, err := New(WithModel(provider.ModelRef{Provider: sp, Model: "m"}))
	if err != nil {
		t.Fatal(err)
	}
	s, err := ag.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan event.Event, 1024)
	if _, err := s.Subscribe(ch, WithCloseOnDone()); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"a", "b"} {
		rc, _ := s.Send(Text(q))
		if res, err := rc.Wait(t.Context()); err != nil || res.Outcome != OutcomeCompleted {
			t.Fatalf("%+v %v", res, err)
		}
	}
	if err := ag.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	if w := s.wk.Live(); len(w) != 0 {
		t.Fatalf("workers left: %v", w)
	}
	noLeaks(t)
}
