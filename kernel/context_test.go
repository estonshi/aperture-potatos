package kernel_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
	"github.com/estonshi/aperture-potatos/store"
)

func TestContextReportAddsUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{toolReply(call{"c1", "echo", `{"a":1}`}), textReply("done")}, kernel.WithTool(&echoTool{name: "echo"}))
		ask(t, h.sess, "echo")
		rep := h.sess.Context()
		kinds := map[string]bool{}
		var sum int64
		for _, p := range rep.Parts {
			kinds[p.Kind] = true
			sum += p.Tokens
		}
		for _, k := range []string{"system", "tool", "user", "assistant", "tool_call", "tool_result"} {
			if !kinds[k] {
				t.Fatalf("no %s part: %+v", k, rep.Parts)
			}
		}
		if rep.Used <= 0 || sum > rep.Used || rep.Used-sum > int64(len(rep.Parts)) || rep.Epoch != 1 || rep.Threshold <= 0 || rep.Usable <= rep.Threshold {
			t.Fatalf("report: %+v (parts add up to %d)", rep, sum)
		}
		h.close()
	})
}

// TestContextWindowOverride: MaxContextTokens names the window itself, so a host corrects a model that reports it
// wrongly; zero follows the model again.
func TestContextWindowOverride(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, nil, kernel.WithCapabilities(provider.APIAnthropicMessages, "m",
			provider.Capabilities{ContextWindow: 200_000, MaxOutputTokens: 8_000}))
		check := func(window, usable, threshold int64) {
			t.Helper()
			if rep := h.sess.Context(); rep.Window != window || rep.Usable != usable || rep.Threshold != threshold {
				t.Fatalf("window %d usable %d threshold %d, want %d/%d/%d", rep.Window, rep.Usable, rep.Threshold, window, usable, threshold)
			}
			if sn := h.sess.Snapshot().Context; sn.WindowTokens != window || sn.ThresholdTokens != threshold {
				t.Fatalf("snapshot: %+v", sn)
			}
		}
		check(200_000, 192_000, 172_800)
		if err := h.sess.SetContextWindow(1_000_000); err != nil {
			t.Fatal(err)
		}
		check(1_000_000, 992_000, 892_800)
		if err := h.sess.SetContextWindow(8_000); !errors.Is(err, kernel.ErrCapability) {
			t.Fatalf("a window with no room for the output: %v", err)
		}
		check(1_000_000, 992_000, 892_800) // a refused change leaves the window as it was
		if err := h.sess.SetContextWindow(-1); !errors.Is(err, kernel.ErrInvalidWindow) {
			t.Fatalf("negative window: %v", err)
		}
		if err := h.sess.SetContextWindow(0); err != nil {
			t.Fatal(err)
		}
		check(200_000, 192_000, 172_800)
		h.close()
	})
}

// TestSessionContextWindow: a session names its window at creation, and a fork keeps the source's.
func TestSessionContextWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, nil, kernel.WithCapabilities(provider.APIAnthropicMessages, "m",
			provider.Capabilities{ContextWindow: 200_000, MaxOutputTokens: 8_000}))
		second, err := h.ag.NewSession(t.Context(), kernel.SessionContextWindow(300_000))
		if err != nil {
			t.Fatal(err)
		}
		if rep := second.Context(); rep.Window != 300_000 || rep.Usable != 292_000 {
			t.Fatalf("session option: %+v", rep)
		}
		if err := h.sess.SetContextWindow(500_000); err != nil {
			t.Fatal(err)
		}
		fork, err := h.sess.Fork(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if rep := fork.Context(); rep.Window != 500_000 {
			t.Fatalf("a fork keeps the source's window: %+v", rep)
		}
		for _, s := range []*kernel.Session{second, fork} {
			if err := s.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := h.ag.NewSession(t.Context(), kernel.SessionContextWindow(-1)); !errors.Is(err, kernel.ErrInvalidWindow) {
			t.Fatalf("negative window option: %v", err)
		}
		h.close()
	})
}

// TestContextWindowChangeAppliesToNextCall: a window widened while a tool runs keeps the same turn from compacting
// at its next model call, without waiting for the turn to end (runtime.md §12.1).
func TestContextWindowChangeAppliesToNextCall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := func(t *testing.T, widen bool) int {
			comp := &countingCompactor{}
			gate := &gateTool{started: make(chan struct{}), release: make(chan struct{})}
			h := newHarness(t, [][]providertest.Step{
				toolReplyReporting(message.Usage{InputTokens: 200_000, OutputTokens: 10}, call{"c1", "gate", `{}`}),
				textReply("done"),
			},
				kernel.WithTool(gate),
				kernel.Use(kernel.ExtensionFunc("comp", func(r kernel.Registrar) error { _, err := r.Compactor(comp); return err })),
			)
			rc, err := h.sess.Send(kernel.Text("go"))
			if err != nil {
				t.Fatal(err)
			}
			<-gate.started // the turn is between its first and second model calls
			if widen {
				if err := h.sess.SetContextWindow(1_000_000); err != nil {
					t.Fatal(err)
				}
			}
			close(gate.release)
			res, err := rc.Wait(t.Context())
			if err != nil || res.Err != nil || res.FinalText != "done" {
				t.Fatalf("turn: %v %v %q", err, res.Err, res.FinalText)
			}
			h.close()
			return comp.calls
		}
		if n := run(t, false); n != 1 {
			t.Fatalf("without the change the next model call compacts: %d", n)
		}
		if n := run(t, true); n != 0 {
			t.Fatalf("a widened window keeps the running turn from compacting: %d", n)
		}
	})
}

// TestSessionContextWindowPersists: the window a session names at creation is on its first line, so a resume
// restores it without the host passing it again.
func TestSessionContextWindowPersists(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, nil, kernel.WithCapabilities(provider.APIAnthropicMessages, "m",
			provider.Capabilities{ContextWindow: 200_000, MaxOutputTokens: 8_000}))
		second, err := h.ag.NewSession(t.Context(), kernel.SessionContextWindow(300_000))
		if err != nil {
			t.Fatal(err)
		}
		id := second.Info().SessionID
		if w := recordedWindow(t, h.store, id); w != 300_000 {
			t.Fatalf("the first line records the window: %d", w)
		}
		h.close()
		ag := resumeAgent(t, h.store) // no capabilities: only the file can supply 300k
		resumed, err := ag.Resume(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if rep := resumed.Context(); rep.Window != 300_000 {
			t.Fatalf("resumed: %+v", rep)
		}
		_ = resumed.Close(context.Background())
		_ = ag.Close(context.Background())
	})
}

// TestContextWindowChangePersists: a change is written with the next line the session commits, and a recorded 0
// clears it, so a resume follows the model again.
func TestContextWindowChangePersists(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{textReply("one")},
			kernel.WithCapabilities(provider.APIAnthropicMessages, "m", provider.Capabilities{ContextWindow: 200_000, MaxOutputTokens: 8_000}))
		id := h.sess.Info().SessionID
		if err := h.sess.SetContextWindow(500_000); err != nil {
			t.Fatal(err)
		}
		ask(t, h.sess, "hello")
		if w := recordedWindow(t, h.store, id); w != 500_000 {
			t.Fatalf("the turn's lines record the window: %d", w)
		}
		h.close()

		ag := resumeAgent(t, h.store, textReply("again"))
		s, err := ag.Resume(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if rep := s.Context(); rep.Window != 500_000 {
			t.Fatalf("resumed: %+v", rep)
		}
		if err := s.SetContextWindow(0); err != nil {
			t.Fatal(err)
		}
		rc, err := s.Send(kernel.Text("again"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rc.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		if w := recordedWindow(t, h.store, id); w != 0 {
			t.Fatalf("a cleared window is recorded as 0: %d", w)
		}
		_ = s.Close(context.Background())
		_ = ag.Close(context.Background())

		ag2 := resumeAgent(t, h.store)
		s2, err := ag2.Resume(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if rep := s2.Context(); rep.Window != 200_000 { // the model's window, the default when none is declared
			t.Fatalf("a cleared window follows the model: %+v", rep)
		}
		_ = s2.Close(context.Background())
		_ = ag2.Close(context.Background())
	})
}

// recordedWindow is the last window a line of the session recorded, as a resume finds it.
func recordedWindow(t *testing.T, st store.SessionStore, id string) int {
	t.Helper()
	lines, err := st.Load(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if m := lines[i]; m.CopyOf == "" && m.ContextWindow != nil {
			return *m.ContextWindow
		}
	}
	t.Fatal("no line recorded the window")
	return 0
}

// gateTool holds a call until the test lets it go, so the test can change the window inside a running turn.
type gateTool struct {
	started chan struct{}
	release chan struct{}
}

func (g *gateTool) Spec() kernel.ToolSpec {
	return kernel.ToolSpec{Name: "gate", Description: "Wait for the host.", InputSchema: json.RawMessage(`{"type":"object"}`), Concurrency: kernel.Serial}
}

func (g *gateTool) Call(context.Context, *kernel.ToolContext, json.RawMessage) (kernel.ToolResult, error) {
	close(g.started)
	<-g.release
	return kernel.TextResult("open"), nil
}

// countingCompactor records the compactions the kernel runs.
type countingCompactor struct{ calls int }

func (c *countingCompactor) Compact(context.Context, kernel.CompactRequest) (kernel.CompactResult, error) {
	c.calls++
	return kernel.CompactResult{Summary: "Earlier.", Strategy: "test"}, nil
}

// toolReplyReporting is toolReply with the usage its step reports, which anchors the context estimate.
func toolReplyReporting(u message.Usage, calls ...call) []providertest.Step {
	steps := toolReply(calls...)
	last := len(steps) - 1
	steps[last].Event.Usage = &u
	steps[last].Event.Message.Usage = &u
	return steps
}
