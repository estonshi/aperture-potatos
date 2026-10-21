package kernel_test

import (
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/provider/providertest"
)

func TestSimpleTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{textReply("pong")})
		rc, err := h.sess.Send(kernel.Text("ping"))
		if err != nil {
			t.Fatal(err)
		}
		res, err := rc.Wait(t.Context())
		if err != nil || res.Outcome != kernel.OutcomeCompleted || res.FinalText != "pong" {
			t.Fatalf("result %+v, %v", res, err)
		}
		h.close()
		if got := roles(h.lines()); got != "system,user,assistant" {
			t.Fatalf("lines %s", got)
		}
		types := strings.Join(h.rec.types(), " ")
		want := "message.completed system.rendered session.started session.state_changed input.accepted session.state_changed turn.started message.completed step.started message.started message.delta message.completed step.completed usage.updated session.state_changed turn.completed session.state_changed session.closed"
		if types != want {
			t.Fatalf("events\n got %s\nwant %s", types, want)
		}
	})
}

func TestMultiStepToolLoop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		echo := &echoTool{name: "echo"}
		h := newHarness(t, [][]providertest.Step{
			toolReply(call{"toolu_1", "echo", `{"text":"a"}`}),
			toolReply(call{"toolu_2", "echo", `{"text":"b"}`}, call{"toolu_3", "echo", `{"text":"c"}`}),
			textReply("done"),
		}, kernel.WithTool(echo))
		rc, err := h.sess.Send(kernel.Text("go"))
		if err != nil {
			t.Fatal(err)
		}
		res, _ := rc.Wait(t.Context())
		if res.Outcome != kernel.OutcomeCompleted || res.Steps != 3 {
			t.Fatalf("result %+v", res)
		}
		h.close()
		lines := h.lines()
		if got := roles(lines); got != "system,user,assistant,tool,assistant,tool,assistant" {
			t.Fatalf("lines %s", got)
		}
		if err := pairedHistory(lines); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"toolu_1", "toolu_2", "toolu_3"} {
			if n := echo.runsOf(id); n != 1 {
				t.Fatalf("%s ran %d times", id, n)
			}
		}
		if n := h.rec.count(event.TypeToolCompleted); n != 3 {
			t.Fatalf("tool.completed %d", n)
		}
		reqs := h.sp.Requests()
		if len(reqs) != 3 || len(reqs[2].Messages) != 5 || len(reqs[0].Tools) != 1 {
			t.Fatalf("requests %d, last has %d messages", len(reqs), len(reqs[len(reqs)-1].Messages))
		}
	})
}

func TestInterruptDuringTool(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		echo := &echoTool{name: "echo", delay: time.Hour}
		h := newHarness(t, [][]providertest.Step{
			toolReply(call{"toolu_1", "echo", `{}`}, call{"toolu_2", "echo", `{}`}),
		}, kernel.WithTool(echo))
		rc, _ := h.sess.Send(kernel.Text("go"))
		synctest.Wait()
		if !h.sess.Interrupt("stop") {
			t.Fatal("no turn to interrupt")
		}
		res, _ := rc.Wait(t.Context())
		if res.Outcome != kernel.OutcomeInterrupted {
			t.Fatalf("result %+v", res)
		}
		synctest.Wait()
		if q := h.sess.Snapshot().QueuedInputs; q != 1 {
			t.Fatalf("queued %d, want the interruption note", q)
		}
		h.close()
		lines := h.lines()
		if err := pairedHistory(lines); err != nil {
			t.Fatal(err)
		}
		if echo.runsOf("toolu_2") != 0 {
			t.Fatal("a call after the interrupt ran")
		}
	})
}
