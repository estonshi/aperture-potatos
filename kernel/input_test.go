package kernel_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider/providertest"
)

func TestInterruptAndDropKeepsReminders(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{toolReply(call{"toolu_1", "echo", `{}`})}, kernel.WithTool(&echoTool{name: "echo", delay: time.Hour}))
		rc, _ := h.sess.Send(kernel.Text("go"))
		synctest.Wait()
		human, _ := h.sess.Send(kernel.Text("then this"), kernel.WithMode(kernel.ModeQueue))
		_, err := h.sess.Send(kernel.Input{Text: "context", Origin: message.Origin{Kind: message.OriginReminder, Source: "app"}}, kernel.NoWake())
		if err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if !h.sess.InterruptAndDrop("user gave up") {
			t.Fatal("no turn")
		}
		if res, _ := rc.Wait(t.Context()); res.Outcome != kernel.OutcomeInterrupted {
			t.Fatalf("turn %+v", res)
		}
		if res, _ := human.Wait(t.Context()); res.Outcome != kernel.OutcomeDropped {
			t.Fatalf("queued human input %+v", res)
		}
		synctest.Wait()
		if q := h.sess.Snapshot().QueuedInputs; q != 2 {
			t.Fatalf("queued %d, want the reminder and the interruption note", q)
		}
		h.close()
	})
}

func TestQueueLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{toolReply(call{"toolu_1", "echo", `{}`}), textReply("a"), textReply("b"), textReply("c")},
			kernel.WithTool(&echoTool{name: "echo", delay: time.Second}), kernel.WithLimits(kernel.Limits{MaxQueuedInputs: 2}))
		h.sess.Send(kernel.Text("go"))
		synctest.Wait()
		for i := range 2 {
			if _, err := h.sess.Send(kernel.Text("q"), kernel.WithMode(kernel.ModeQueue)); err != nil {
				t.Fatalf("send %d: %v", i, err)
			}
		}
		if _, err := h.sess.Send(kernel.Text("q"), kernel.WithMode(kernel.ModeQueue)); !errors.Is(err, kernel.ErrQueueFull) {
			t.Fatalf("third human input: %v", err)
		}
		for i := range 5 {
			if _, err := h.sess.Send(kernel.Input{Text: "r", Origin: message.Origin{Kind: message.OriginReminder, Source: "app"}}, kernel.NoWake()); err != nil {
				t.Fatalf("reminder %d: %v", i, err)
			}
		}
		h.close()
	})
}

func TestSendValidation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{textReply("literal")}, kernel.WithCommand(command("plan", kernel.WaitIdle, "planned")))
		if _, err := h.sess.Send(kernel.Input{Text: "x", Origin: message.Origin{Kind: message.OriginTask}}); !errors.Is(err, kernel.ErrInvalidOrigin) {
			t.Fatalf("task origin: %v", err)
		}
		if _, err := h.sess.Send(kernel.Input{}); !errors.Is(err, kernel.ErrEmptyInput) {
			t.Fatalf("empty: %v", err)
		}
		if _, err := h.sess.Command("nope", ""); !errors.Is(err, kernel.ErrUnknownCommand) {
			t.Fatalf("unknown command: %v", err)
		}
		rc, _ := h.sess.Send(kernel.Text("//plan to the letter"))
		if res, _ := rc.Wait(t.Context()); res.Outcome != kernel.OutcomeCompleted {
			t.Fatalf("escaped slash %+v", res)
		}
		h.close()
		if got := h.lines()[1].Text(); got != "/plan to the letter" {
			t.Fatalf("delivered %q", got)
		}
	})
}

func TestInputOptions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mt := &metaTool{got: make(chan map[string]string, 1)}
		h := newHarness(t, [][]providertest.Step{toolReply(call{"toolu_1", "meta", `{}`}), textReply("a"), textReply("b"), textReply("c")},
			kernel.WithTool(mt), kernel.WithInputMode(kernel.ModeQueue), kernel.WithCommand(command("plan", kernel.WaitIdle, "planned")))
		rc, _ := h.sess.Send(kernel.Text("go"), kernel.WithMetadata(map[string]string{"trace": "t-1"}))
		if md := <-mt.got; md["trace"] != "t-1" {
			t.Fatalf("metadata %v", md)
		}
		rc.Wait(t.Context())
		rc, _ = h.sess.Send(kernel.Text("please review"), kernel.WithOrigin(message.Origin{Kind: message.OriginAgent, Source: "agent:Boss"}))
		rc.Wait(t.Context())
		rc, _ = h.sess.Send(kernel.Text("/plan it"), kernel.NoCommandParsing())
		if res, _ := rc.Wait(t.Context()); res.Outcome != kernel.OutcomeCompleted {
			t.Fatalf("unparsed command text %+v", res)
		}
		h.close()
		lines := h.lines()
		agentLine := lines[5].Content[0].(*message.TextBlock).Text
		if !strings.HasPrefix(agentLine, "<agent-message from=\"agent:Boss\">\nThis message was sent by another agent") || !strings.HasSuffix(agentLine, "\nplease review\n</agent-message>") {
			t.Fatalf("agent message %q", agentLine)
		}
		if lines[5].Origin.Kind != message.OriginAgent || lines[7].Text() != "/plan it" {
			t.Fatalf("origins %+v, text %q", lines[5].Origin, lines[7].Text())
		}
		var modes []string
		var progress bool
		for _, ev := range h.rec.all() {
			switch d := ev.Data.(type) {
			case *event.InputAccepted:
				modes = append(modes, d.Mode)
			case *event.ToolProgress:
				progress = d.Text == "halfway" && string(d.Data) == `{"pct":50}`
			}
		}
		if !slices.Equal(modes, []string{"queue", "queue", "queue"}) || !progress {
			t.Fatalf("modes %v, progress %v", modes, progress)
		}
	})
}

func reminder(source, text string) kernel.Input {
	return kernel.Input{Text: text, Origin: message.Origin{Kind: message.OriginReminder, Source: source}}
}

func TestMergeCapsAndReminderTruncation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{textReply("noted")}, kernel.WithLimits(kernel.Limits{MaxQueuedInputs: 1}))
		for i := range 40 {
			if _, err := h.sess.Send(reminder("app", "note "+string(rune('a'+i%26))), kernel.NoWake()); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := h.sess.Send(reminder("big", strings.Repeat("é", 10005)), kernel.NoWake()); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if q := h.sess.Snapshot().QueuedInputs; q != 1 {
			t.Fatalf("queued %d; every reminder merges into one item at a limit of 1", q)
		}
		rc, _ := h.sess.Send(kernel.Text("go"))
		rc.Wait(t.Context())
		h.close()
		var dropped int
		for _, ev := range h.rec.all() {
			if d, ok := ev.Data.(*event.InputDropped); ok && d.Reason == "merged_overflow" {
				dropped++
			}
		}
		user := h.lines()[1]
		first := user.Content[0].(*message.ReminderBlock).Text
		var big string
		for _, b := range user.Content {
			if r, ok := b.(*message.ReminderBlock); ok && r.Source == "big" {
				big = r.Text
			}
		}
		if dropped != 9 || !strings.HasPrefix(first, "(9 earlier items omitted)\n") || len(user.Content) != 33 {
			t.Fatalf("dropped %d, blocks %d, first %q", dropped, len(user.Content), first)
		}
		if !strings.HasSuffix(big, "\n[truncated: 5 characters omitted]") || strings.Count(big, "é") != 10000 {
			t.Fatalf("truncation %q", big[len(big)-40:])
		}
	})
}

// metaTool reports the metadata of the inputs that started its turn.
type metaTool struct{ got chan map[string]string }

func (m *metaTool) Spec() kernel.ToolSpec { return kernel.ToolSpec{Name: "meta"} }

func (m *metaTool) Call(_ context.Context, tc *kernel.ToolContext, _ json.RawMessage) (kernel.ToolResult, error) {
	m.got <- tc.InputMetadata()
	tc.Progress("halfway", map[string]int{"pct": 50})
	return kernel.TextResult("ok"), nil
}

// requestHas reports whether the last model request carries text, so a test can see what reached the model.
func requestHas(h *harness, text string) bool {
	reqs := h.sp.Requests()
	b, _ := json.Marshal(reqs[len(reqs)-1].Messages)
	return strings.Contains(string(b), text)
}

func droppedAs(h *harness, id string) string {
	for _, ev := range h.rec.all() {
		if d, ok := ev.Data.(*event.InputDropped); ok && d.InputID == id {
			return d.Reason
		}
	}
	return ""
}

func TestWithdrawQueuedInput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{toolReply(call{"toolu_1", "echo", `{}`}), textReply("done"), textReply("english")},
			kernel.WithTool(&echoTool{name: "echo", delay: time.Second}))
		rc, _ := h.sess.Send(kernel.Text("go"))
		synctest.Wait()
		later, _ := h.sess.Send(kernel.Text("ENGLISH-TOO"), kernel.WithMode(kernel.ModeQueue))
		yen, _ := h.sess.Send(kernel.Text("IN-YEN"))
		if err := h.sess.Withdraw(yen.InputID()); err != nil {
			t.Fatal(err)
		}
		if res, _ := yen.Wait(t.Context()); res.Outcome != kernel.OutcomeDropped {
			t.Fatalf("withdrawn input %+v", res)
		}
		synctest.Wait()
		if r := droppedAs(h, yen.InputID()); r != "withdrawn" {
			t.Fatalf("input.dropped reason %q", r)
		}
		if err := h.sess.Withdraw(yen.InputID()); !errors.Is(err, kernel.ErrNotQueued) {
			t.Fatalf("second withdraw: %v", err)
		}
		if err := h.sess.Withdraw("in_unknown"); !errors.Is(err, kernel.ErrNotQueued) {
			t.Fatalf("unknown id: %v", err)
		}
		if res, _ := rc.Wait(t.Context()); res.Outcome != kernel.OutcomeCompleted || requestHas(h, "IN-YEN") {
			t.Fatalf("turn %+v, or the withdrawn input reached the model", res)
		}
		if res, _ := later.Wait(t.Context()); res.FinalText != "english" {
			t.Fatalf("queued input %+v", res)
		}
		if err := h.sess.Withdraw(later.InputID()); !errors.Is(err, kernel.ErrNotQueued) {
			t.Fatalf("delivered input: %v", err)
		}
		h.close()
	})
}

func TestInterruptWithQueuedInput(t *testing.T) {
	for _, mode := range []kernel.InputMode{kernel.ModeQueue, kernel.ModeNextLoop} {
		t.Run(string(mode), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t, [][]providertest.Step{toolReply(call{"toolu_1", "echo", `{}`}), textReply("osaka")},
					kernel.WithTool(&echoTool{name: "echo", delay: time.Hour}))
				rc, _ := h.sess.Send(kernel.Text("go"))
				synctest.Wait()
				now, _ := h.sess.Send(kernel.Text("OSAKA-INSTEAD"), kernel.WithMode(mode))
				synctest.Wait()
				if err := h.sess.InterruptWith(now.InputID()); err != nil {
					t.Fatal(err)
				}
				first, _ := rc.Wait(t.Context())
				res, _ := now.Wait(t.Context())
				if first.Outcome != kernel.OutcomeInterrupted || res.Outcome != kernel.OutcomeCompleted || res.TurnID == first.TurnID || !requestHas(h, "OSAKA-INSTEAD") {
					t.Fatalf("interrupted turn %+v, input's turn %+v", first, res)
				}
				synctest.Wait()
				var reason string
				for _, ev := range h.rec.all() {
					if d, ok := ev.Data.(*event.TurnInterruptRequested); ok {
						reason = d.Reason
					}
				}
				if !strings.Contains(reason, now.InputID()) {
					t.Fatalf("interrupt reason %q does not name the input", reason)
				}
				if err := h.sess.InterruptWith(now.InputID()); !errors.Is(err, kernel.ErrNotQueued) {
					t.Fatalf("delivered input: %v", err)
				}
				h.close()
			})
		})
	}
}

func TestInterruptWithWakesHeldInput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{textReply("seen")})
		held, _ := h.sess.Send(kernel.Text("HELD"), kernel.NoWake())
		synctest.Wait()
		if n := h.rec.count(event.TypeTurnStarted); n != 0 {
			t.Fatalf("%d turns before InterruptWith", n)
		}
		if err := h.sess.InterruptWith(held.InputID()); err != nil {
			t.Fatal(err)
		}
		if res, _ := held.Wait(t.Context()); res.FinalText != "seen" {
			t.Fatalf("held input %+v", res)
		}
		h.close()
	})
}

func TestWithdrawRefusesTakenInput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{toolReply(call{"toolu_1", "echo", `{}`}), {{Hold: -1}}},
			kernel.WithTool(&echoTool{name: "echo", delay: time.Second}))
		h.sess.Send(kernel.Text("go"))
		synctest.Wait()
		injected, _ := h.sess.Send(kernel.Text("ALSO-THIS"))
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if n := h.rec.count(event.TypeInputInjected); n != 1 {
			t.Fatalf("input.injected %d", n)
		}
		if err := h.sess.Withdraw(injected.InputID()); !errors.Is(err, kernel.ErrNotQueued) {
			t.Fatalf("withdraw after injection: %v", err)
		}
		if err := h.sess.InterruptWith(injected.InputID()); !errors.Is(err, kernel.ErrNotQueued) {
			t.Fatalf("interrupt with an injected input: %v", err)
		}
		h.close()
	})
}

func TestWithdrawMergedReminderPart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{toolReply(call{"toolu_1", "echo", `{}`}), textReply("done")},
			kernel.WithTool(&echoTool{name: "echo", delay: time.Second}), kernel.WithLimits(kernel.Limits{MaxQueuedInputs: 1}))
		rc, _ := h.sess.Send(kernel.Text("go"))
		synctest.Wait()
		note := func(text string) *kernel.Receipt {
			r, err := h.sess.Send(kernel.Input{Text: text, Origin: message.Origin{Kind: message.OriginReminder, Source: "app"}}, kernel.NoWake())
			if err != nil {
				t.Fatal(err)
			}
			return r
		}
		a, b := note("REMINDER-ONE"), note("REMINDER-TWO")
		synctest.Wait()
		if q := h.sess.Snapshot().QueuedInputs; q != 1 {
			t.Fatalf("queued %d, want one merged item", q)
		}
		if err := h.sess.Withdraw(a.InputID()); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if q := h.sess.Snapshot().QueuedInputs; q != 1 || droppedAs(h, a.InputID()) != "withdrawn" || droppedAs(h, b.InputID()) != "" {
			t.Fatalf("queued %d after withdrawing one part; only that part may drop", q)
		}
		if res, _ := rc.Wait(t.Context()); res.Outcome != kernel.OutcomeCompleted || !requestHas(h, "REMINDER-TWO") || requestHas(h, "REMINDER-ONE") {
			t.Fatalf("turn %+v: the other part must ride the next safe point, the withdrawn one must not", res)
		}
		h.close()
	})
}

func TestWithdrawReturnsQueueSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{toolReply(call{"toolu_1", "echo", `{}`})},
			kernel.WithTool(&echoTool{name: "echo", delay: time.Hour}), kernel.WithLimits(kernel.Limits{MaxQueuedInputs: 1}))
		h.sess.Send(kernel.Text("go"))
		synctest.Wait()
		first, _ := h.sess.Send(kernel.Text("first"), kernel.WithMode(kernel.ModeQueue))
		if _, err := h.sess.Send(kernel.Text("second"), kernel.WithMode(kernel.ModeQueue)); !errors.Is(err, kernel.ErrQueueFull) {
			t.Fatalf("second input before the withdrawal: %v", err)
		}
		if err := h.sess.Withdraw(first.InputID()); err != nil {
			t.Fatal(err)
		}
		if _, err := h.sess.Send(kernel.Text("second"), kernel.WithMode(kernel.ModeQueue)); err != nil {
			t.Fatalf("second input after the withdrawal: %v", err)
		}
		h.close()
	})
}

func TestWithdrawRefusesCommandsAndInputsBeingSubmitted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		slow := kernel.WithHook(kernel.UserPromptSubmit, "slow", func(ctx context.Context, _ kernel.UserPromptSubmitInput) (kernel.UserPromptSubmitOutput, error) {
			select {
			case <-time.After(2 * time.Second):
			case <-ctx.Done():
			}
			return kernel.UserPromptSubmitOutput{}, nil
		})
		h := newHarness(t, [][]providertest.Step{toolReply(call{"toolu_1", "echo", `{}`}), textReply("done"), textReply("planned")},
			kernel.WithTool(&echoTool{name: "echo", delay: time.Second}), kernel.WithCommand(command("plan", kernel.WaitIdle, "planned")), slow)
		rc, _ := h.sess.Send(kernel.Text("go"))
		synctest.Wait()
		if err := h.sess.Withdraw(rc.InputID()); !errors.Is(err, kernel.ErrNotQueued) {
			t.Fatalf("withdraw while user_prompt_submit runs: %v", err)
		}
		time.Sleep(3 * time.Second)
		synctest.Wait()
		plan, _ := h.sess.Send(kernel.Text("/plan"))
		synctest.Wait()
		if err := h.sess.Withdraw(plan.InputID()); !errors.Is(err, kernel.ErrNotQueued) {
			t.Fatalf("withdraw a queued command: %v", err)
		}
		if err := h.sess.InterruptWith(plan.InputID()); !errors.Is(err, kernel.ErrNotQueued) {
			t.Fatalf("interrupt with a queued command: %v", err)
		}
		h.close()
	})
}

func TestInterruptWithWaitsForBarrier(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{toolReply(call{"toolu_1", "echo", `{}`}), textReply("after the plan")},
			kernel.WithTool(&echoTool{name: "echo", delay: time.Hour}), kernel.WithCommand(command("plan", kernel.WaitIdle, "planned")))
		rc, _ := h.sess.Send(kernel.Text("go"))
		synctest.Wait()
		h.sess.Send(kernel.Text("/plan"))
		x, _ := h.sess.Send(kernel.Text("BEHIND-THE-PLAN"), kernel.WithMode(kernel.ModeQueue))
		if err := h.sess.InterruptWith(x.InputID()); err != nil {
			t.Fatal(err)
		}
		if res, _ := rc.Wait(t.Context()); res.Outcome != kernel.OutcomeInterrupted {
			t.Fatalf("turn %+v: the barrier must not stop the interrupt", res)
		}
		if res, _ := x.Wait(t.Context()); res.FinalText != "after the plan" {
			t.Fatalf("input %+v", res)
		}
		synctest.Wait()
		var order []string
		for _, ev := range h.rec.all() {
			switch d := ev.Data.(type) {
			case *event.CommandCompleted:
				order = append(order, "command")
			case *event.TurnStarted:
				if slices.Contains(d.InputIDs, x.InputID()) {
					order = append(order, "input")
				}
			}
		}
		if strings.Join(order, ",") != "command,input" {
			t.Fatalf("order %v: the input waits for the command ahead of it", order)
		}
		h.close()
	})
}

func TestWithdrawKeepsTaskNotificationsAndUnpins(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		done := funcKind("done", kernel.TaskCaps{}, func(context.Context, kernel.TaskRuntime) (kernel.TaskResult, error) {
			return kernel.TaskResult{Summary: "TASK-SUMMARY-7F3"}, nil
		})
		pinner := kernel.NewTool(kernel.ToolSpec{Name: "pinner", InputSchema: json.RawMessage(`{"type":"object"}`), Concurrency: kernel.Serial},
			func(context.Context, *kernel.ToolContext, struct{}) (kernel.ToolResult, error) {
				return kernel.ToolResult{Content: message.Content{&message.TextBlock{Text: "pinned"}}, Pin: &kernel.Pin{Key: "dir", Content: "/work"}}, nil
			})
		h := newHarness(t, [][]providertest.Step{toolReply(call{"toolu_1", "pinner", `{}`}), textReply("ok"), textReply("noted")},
			kernel.WithTaskKind(done), kernel.WithTool(pinner), quiet)
		ask(t, h.sess, "pin it")
		if err := h.sess.Unpin(t.Context(), "dir"); err != nil {
			t.Fatal(err)
		}
		info := hostStart(t, h.sess, "done")
		if _, err := h.sess.Tasks().Wait(t.Context(), info.ID); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		var note, unpin string
		for _, ev := range h.rec.all() {
			if d, ok := ev.Data.(*event.InputAccepted); ok {
				switch {
				case d.Origin.Kind == message.OriginTask:
					note = d.InputID
				case d.Origin.Source == "pin:dir":
					unpin = d.InputID
				}
			}
		}
		for _, id := range []string{note, unpin} {
			if err := h.sess.Withdraw(id); id == "" || !errors.Is(err, kernel.ErrNotQueued) {
				t.Fatalf("withdraw %q: %v", id, err)
			}
		}
		if err := h.sess.InterruptWith(note); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if n := h.rec.count(event.TypeTurnStarted); n != 2 || !requestHas(h, "TASK-SUMMARY-7F3") {
			t.Fatalf("%d turns after InterruptWith on the notification", n)
		}
		h.close()
	})
}
