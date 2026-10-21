package kernel_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
)

// chatty scripts a reply streamed as n small deltas, enough to fill a small subscriber queue.
func chatty(n int) []providertest.Step {
	u := usage
	text := strings.Repeat("x", n)
	m := &message.Message{Role: message.RoleAssistant, Origin: message.Origin{Kind: message.OriginModel}, Content: message.Content{&message.TextBlock{Text: text}}, Producer: producer(), Usage: &u, StopReason: "end_turn"}
	steps := []providertest.Step{
		{Event: provider.StreamEvent{Kind: provider.SEStart}},
		{Event: provider.StreamEvent{Kind: provider.SEBlockStart, Index: 0, BlockType: message.TypeText}},
	}
	for range n {
		steps = append(steps, providertest.Step{Event: provider.StreamEvent{Kind: provider.SETextDelta, Index: 0, Channel: "text", Text: "x"}})
	}
	return append(steps,
		providertest.Step{Event: provider.StreamEvent{Kind: provider.SEBlockStop, Index: 0, Block: &message.TextBlock{Text: text}}},
		providertest.Step{Event: provider.StreamEvent{Kind: provider.SEEnd, Message: m, Stop: &provider.Stop{Reason: provider.StopEndTurn}, Usage: &u}},
	)
}

// TestSlowConsumerStallsOnlyTheStream covers I5: a blocked subscriber holds back the stream, never the public methods.
func TestSlowConsumerStallsOnlyTheStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{chatty(40), textReply("second")})
		stuck := make(chan event.Event) // never read
		sub, err := h.sess.Subscribe(stuck, kernel.WithBuffer(4), kernel.WithStallTimeout(10*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		rc, _ := h.sess.Send(kernel.Text("talk"))
		synctest.Wait()
		if sn := h.sess.Snapshot(); sn.Phase != "running" {
			t.Fatalf("phase %s while the stream should be held back", sn.Phase)
		}
		start := time.Now()
		more, err := h.sess.Send(kernel.Text("more"), kernel.WithMode(kernel.ModeQueue))
		if err != nil || time.Since(start) != 0 {
			t.Fatalf("Send blocked or failed: %v", err)
		}
		res, _ := rc.Wait(t.Context())
		more.Wait(t.Context())
		if res.Outcome != kernel.OutcomeCompleted {
			t.Fatalf("result %+v", res)
		}
		<-sub.Done()
		if !errors.Is(sub.Err(), kernel.ErrSubscriberStalled) {
			t.Fatalf("err %v", sub.Err())
		}
		h.close()
		var detached bool
		for _, ev := range h.rec.all() {
			if d, ok := ev.Data.(*event.Error); ok && d.Code == "subscriber_detached" {
				detached = true
			}
		}
		if !detached || h.rec.count(event.TypeMessageDelta) != 41 {
			t.Fatalf("detached %v, deltas %d; the lossless recorder must miss nothing", detached, h.rec.count(event.TypeMessageDelta))
		}
	})
}

func TestDropDeltasLosesOnlyBulk(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{chatty(40)})
		ch := make(chan event.Event)
		sub, _ := h.sess.Subscribe(ch, kernel.WithDropDeltas(), kernel.WithBuffer(4), kernel.WithCloseOnDone())
		rc, _ := h.sess.Send(kernel.Text("talk"))
		synctest.Wait()
		var got []event.Event
		done := make(chan struct{})
		go func() {
			for ev := range ch {
				got = append(got, ev)
			}
			close(done)
		}()
		rc.Wait(t.Context())
		h.close()
		<-done
		if sub.Dropped() == 0 {
			t.Fatal("nothing dropped")
		}
		var lastSeq uint64
		var gaps, completed int
		for _, ev := range got {
			if ev.Seq != lastSeq+1 {
				gaps++
			}
			lastSeq = ev.Seq
			if ev.Type == event.TypeMessageCompleted {
				completed++
			}
		}
		if gaps == 0 || completed != 2 || got[len(got)-1].Type != event.TypeSessionClosed { // the system line was committed before this subscription
			t.Fatalf("gaps %d, completed %d, last %s", gaps, completed, got[len(got)-1].Type)
		}
	})
}

// TestCloseOnDoneAfterUnsubscribe covers I8: the runtime closes a caller's channel only when asked to.
func TestCloseOnDoneAfterUnsubscribe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{textReply("pong")})
		ch := make(chan event.Event, 1)
		sub, _ := h.sess.Subscribe(ch, kernel.WithCloseOnDone())
		sub.Unsubscribe()
		<-sub.Done()
		for range ch {
		}
		if sub.Err() != nil {
			t.Fatalf("err after Unsubscribe: %v", sub.Err())
		}
		h.close()
	})
}

// TestChildBackpressureChain: a stuck subscriber of the owner holds a child's stream back once the relay's queue is full, while both sessions stay responsive, and once it is detached the child finishes with every event relayed in order before task.finished.
func TestChildBackpressureChain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const n = 600 // more than the relay's high water of 512
		childModel := provider.ModelRef{Provider: providertest.NewScriptProvider(provider.APIAnthropicMessages, chatty(n)), Model: "m"}
		opened := make(chan *kernel.Session, 1)
		var derived *kernel.Agent
		job := funcKind("job", kernel.TaskCaps{}, func(ctx context.Context, rt kernel.TaskRuntime) (kernel.TaskResult, error) {
			child, err := derived.NewSession(ctx, kernel.ChildOf(rt))
			if err != nil {
				return kernel.TaskResult{}, err
			}
			opened <- child
			rc, err := child.Send(kernel.Text("talk"))
			if err != nil {
				return kernel.TaskResult{}, err
			}
			res, err := rc.Wait(ctx)
			return kernel.TaskResult{Summary: res.FinalText}, err
		})
		h := newHarness(t, nil, kernel.WithTaskKind(job), quiet)
		var err error
		if derived, err = h.ag.Derive(kernel.DeriveOptions{Model: &childModel}); err != nil {
			t.Fatal(err)
		}
		stuck := make(chan event.Event) // never read
		sub, err := h.sess.Subscribe(stuck, kernel.WithBuffer(4), kernel.WithStallTimeout(10*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		info := hostStart(t, h.sess, "job")
		child := <-opened
		synctest.Wait()
		start := time.Now()
		if sn := child.Snapshot(); sn.Phase != "running" {
			t.Fatalf("child %s while its stream should be held back", sn.Phase)
		}
		if got, _ := h.sess.Tasks().Get(info.ID); got.Status != kernel.TaskRunning || time.Since(start) != 0 {
			t.Fatalf("task %s; Snapshot and Get must not wait for the stream", got.Status)
		}
		got, err := h.sess.Tasks().Wait(t.Context(), info.ID)
		if err != nil || got.Status != kernel.TaskSucceeded || got.Result.Summary != strings.Repeat("x", n) {
			t.Fatalf("task %+v %v", got, err)
		}
		<-sub.Done()
		if !errors.Is(sub.Err(), kernel.ErrSubscriberStalled) {
			t.Fatalf("err %v", sub.Err())
		}
		synctest.Wait()
		before := h.rec.all() // the child's own close comes later, with the owner's
		h.close()
		var seqs []uint64
		deltas, finished := 0, -1
		for i, ev := range before {
			if d, ok := ev.Data.(*event.TaskFinished); ok && d.TaskID == info.ID {
				finished = i
			}
			if len(ev.Path) == 0 || ev.Path[0].ID != info.ID || ev.Path[0].SessionID != h.sess.Info().SessionID {
				continue
			}
			if finished >= 0 {
				t.Fatalf("%s relayed after task.finished", ev.Type)
			}
			seqs = append(seqs, ev.Path[0].Seq)
			if ev.Type == event.TypeMessageDelta {
				deltas++
			}
		}
		for i, s := range seqs {
			if s != uint64(i+1) {
				t.Fatalf("relayed child seqs %v: a gap or a reordering", seqs)
			}
		}
		if deltas != n || finished < 0 {
			t.Fatalf("%d child deltas relayed, task.finished at %d", deltas, finished)
		}
	})
}

// TestChildFlushBoundedByCloseTimeout: an owner subscriber that stalls longer than the close timeout delays task.finished by that timeout only.
func TestChildFlushBoundedByCloseTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		childModel := provider.ModelRef{Provider: providertest.NewScriptProvider(provider.APIAnthropicMessages, chatty(40)), Model: "m"}
		var derived *kernel.Agent
		job := funcKind("job", kernel.TaskCaps{}, func(ctx context.Context, rt kernel.TaskRuntime) (kernel.TaskResult, error) {
			child, err := derived.NewSession(ctx, kernel.ChildOf(rt))
			if err != nil {
				return kernel.TaskResult{}, err
			}
			rc, err := child.Send(kernel.Text("talk"))
			if err != nil {
				return kernel.TaskResult{}, err
			}
			_, err = rc.Wait(ctx)
			return kernel.TaskResult{}, err
		})
		h := newHarness(t, nil, kernel.WithTaskKind(job), kernel.WithLimits(kernel.Limits{CloseTimeout: time.Second}), quiet)
		var err error
		if derived, err = h.ag.Derive(kernel.DeriveOptions{Model: &childModel}); err != nil {
			t.Fatal(err)
		}
		stuck := make(chan event.Event) // read only once the task has finished
		if _, err := h.sess.Subscribe(stuck, kernel.WithBuffer(4), kernel.WithStallTimeout(time.Minute), kernel.WithCloseOnDone()); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		info := hostStart(t, h.sess, "job")
		got, err := h.sess.Tasks().Wait(t.Context(), info.ID)
		if err != nil || got.Status != kernel.TaskSucceeded {
			t.Fatalf("task %+v %v", got, err)
		}
		if d := time.Since(start); d != time.Second {
			t.Fatalf("task.finished after %s, want the close timeout", d)
		}
		go func() {
			for range stuck {
			}
		}()
		h.close()
	})
}
