package kernel_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
)

func TestRunNowCommandDuringTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{toolReply(call{"toolu_1", "echo", `{}`}), textReply("done")},
			kernel.WithTool(&echoTool{name: "echo", delay: time.Second}), kernel.WithCommand(command("status", kernel.RunNow, "ok")))
		rc, _ := h.sess.Send(kernel.Text("go"))
		synctest.Wait()
		crc, err := h.sess.Send(kernel.Text("/status"))
		if err != nil {
			t.Fatal(err)
		}
		res, _ := crc.Wait(t.Context())
		if res.Outcome != kernel.OutcomeCommand || res.Command.Text != "ok" || h.sess.Snapshot().Phase != "running" {
			t.Fatalf("command result %+v while phase %s", res, h.sess.Snapshot().Phase)
		}
		rc.Wait(t.Context())
		h.close()
		for _, ev := range h.rec.all() {
			if d, ok := ev.Data.(*event.CommandStarted); ok && !d.Busy {
				t.Fatal("a command run beside a turn must say busy")
			}
		}
	})
}

func TestRejectBusyWhileRunning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{toolReply(call{"toolu_1", "echo", `{}`}), textReply("done")},
			kernel.WithTool(&echoTool{name: "echo", delay: time.Second}), kernel.WithCommand(command("reset", kernel.RejectBusy, "reset")))
		rc, _ := h.sess.Send(kernel.Text("go"))
		synctest.Wait()
		crc, _ := h.sess.Command("reset", "")
		res, _ := crc.Wait(t.Context())
		if !errors.Is(res.Err, kernel.ErrBusy) {
			t.Fatalf("result %+v", res)
		}
		rc.Wait(t.Context())
		idle, _ := h.sess.Command("reset", "")
		if res, _ := idle.Wait(t.Context()); res.Err != nil || res.Command.Text != "reset" {
			t.Fatalf("idle result %+v", res)
		}
		h.close()
	})
}

func TestInterruptFirstRunsBeforeQueuedInput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var order []string
		clear := kernel.Command{Name: "clear", Busy: kernel.InterruptFirst, Run: func(ctx context.Context, cc *kernel.CommandContext) (kernel.CommandResult, error) {
			order = append(order, "command:"+cc.Session().Snapshot().Phase)
			return kernel.CommandResult{Text: "cleared"}, nil
		}}
		h := newHarness(t, [][]providertest.Step{toolReply(call{"toolu_1", "echo", `{}`}), textReply("after")},
			kernel.WithTool(&echoTool{name: "echo", delay: time.Hour}), kernel.WithCommand(clear))
		first, _ := h.sess.Send(kernel.Text("go"))
		synctest.Wait()
		queued, _ := h.sess.Send(kernel.Text("later"), kernel.WithMode(kernel.ModeQueue))
		crc, _ := h.sess.Command("clear", "")
		if res, _ := first.Wait(t.Context()); res.Outcome != kernel.OutcomeInterrupted {
			t.Fatalf("first turn %+v", res)
		}
		crc.Wait(t.Context())
		if res, _ := queued.Wait(t.Context()); res.Outcome != kernel.OutcomeCompleted || res.FinalText != "after" {
			t.Fatalf("queued turn %+v", res)
		}
		h.close()
		if !slices.Equal(order, []string{"command:maintenance"}) {
			t.Fatalf("order %v", order)
		}
	})
}

func TestCommandOutputAndDropAtClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		talk := kernel.Command{Name: "talk", Busy: kernel.RunNow, Run: func(ctx context.Context, cc *kernel.CommandContext) (kernel.CommandResult, error) {
			cc.Output("step " + cc.Name() + " " + cc.Args())
			return kernel.CommandResult{Text: "done", Data: map[string]int{"n": 1}}, nil
		}}
		h := newHarness(t, [][]providertest.Step{toolReply(call{"toolu_1", "echo", `{}`})}, kernel.WithTool(&echoTool{name: "echo", delay: time.Hour}),
			kernel.WithCommand(talk), kernel.WithCommand(command("later", kernel.WaitIdle, "later")))
		h.sess.Send(kernel.Text("go"))
		synctest.Wait()
		c, _ := h.sess.Command("talk", "now")
		if res, _ := c.Wait(t.Context()); res.Command.Text != "done" {
			t.Fatalf("talk %+v", res)
		}
		queued, _ := h.sess.Command("later", "")
		h.close()
		if res, _ := queued.Wait(t.Context()); !errors.Is(res.Err, kernel.ErrSessionClosed) {
			t.Fatalf("a queued command at close: %+v", res)
		}
		var output, data bool
		for _, ev := range h.rec.all() {
			switch d := ev.Data.(type) {
			case *event.CommandOutput:
				output = d.Text == "step talk now"
			case *event.CommandCompleted:
				data = data || string(d.Data) == `{"n":1}`
			}
		}
		if !output || !data {
			t.Fatalf("output %v data %v", output, data)
		}
		infos := h.sess.Commands()
		if len(infos) != 2 || infos[0].Name != "later" || infos[1].Busy != kernel.RunNow {
			t.Fatalf("commands %+v", infos)
		}
	})
}

func TestRunForACommandAndSessionSink(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ch := make(chan event.Event, 64)
		sp := providertest.NewScriptProvider(provider.APIAnthropicMessages)
		emit := kernel.Command{Name: "note", Busy: kernel.RunNow, Run: func(ctx context.Context, cc *kernel.CommandContext) (kernel.CommandResult, error) {
			cc.Output("noted " + cc.ID())
			return kernel.CommandResult{Text: "ok"}, nil
		}}
		ag, _ := kernel.New(kernel.WithModel(provider.ModelRef{Provider: sp, Model: "m"}), kernel.WithCommand(emit))
		s, err := ag.NewSession(t.Context(), kernel.WithSessionSink(ch, kernel.WithCloseOnDone(), kernel.WithMaxDepth(0), kernel.WithTypes("command.", "session.closed")))
		if err != nil {
			t.Fatal(err)
		}
		var types []event.Type
		for ev, err := range s.Run(t.Context(), kernel.Text("/note")) {
			if err != nil {
				t.Fatal(err)
			}
			types = append(types, ev.Type)
		}
		if len(types) != 3 || types[0] != event.TypeCommandStarted || types[2] != event.TypeCommandCompleted {
			t.Fatalf("run of a command: %v", types)
		}
		ag.Close(t.Context())
		var sink []event.Type
		for ev := range ch {
			sink = append(sink, ev.Type)
		}
		if len(sink) != 4 || sink[3] != event.TypeSessionClosed {
			t.Fatalf("sink %v", sink)
		}
	})
}

// TestSplitArgs: the exported splitter is the one prompt commands use, so an extension maps arguments exactly as $1 would.
func TestSplitArgs(t *testing.T) {
	for in, want := range map[string][]string{
		``:                           nil,
		`  a  b `:                    {"a", "b"},
		`'it''s' "x \"y\" \\z" w\ v`: {"its", `x "y" \z`, "w v"},
		`"" ''`:                      {"", ""},
	} {
		if got := kernel.SplitArgs(in); !slices.Equal(got, want) {
			t.Errorf("SplitArgs(%q) = %q, want %q", in, got, want)
		}
	}
}
