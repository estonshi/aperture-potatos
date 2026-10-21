package kernel_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider/providertest"
)

// bgTool records every input it is called with, so a test can check it never saw the background switch.
type bgTool struct {
	spec  kernel.ToolSpec
	sleep time.Duration
	mu    sync.Mutex
	seen  []string
	path  int
}

func (b *bgTool) Spec() kernel.ToolSpec { return b.spec }

func (b *bgTool) Call(ctx context.Context, tc *kernel.ToolContext, input json.RawMessage) (kernel.ToolResult, error) {
	b.mu.Lock()
	b.seen = append(b.seen, string(input))
	b.path = len(tc.Path())
	b.mu.Unlock()
	tc.Progress("halfway", nil)
	if b.sleep > 0 {
		select {
		case <-time.After(b.sleep):
		case <-ctx.Done():
			return kernel.ToolResult{}, ctx.Err()
		}
	}
	return kernel.TextResult("fetched " + string(input)), nil
}

func fetchTool(timeout, sleep time.Duration) *bgTool {
	return &bgTool{sleep: sleep, spec: kernel.ToolSpec{Name: "fetch", InputSchema: json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"}}}`), Concurrency: kernel.Parallel, Background: true, Timeout: timeout}}
}

func hostExec(t *testing.T, s *kernel.Session, input string) (kernel.TaskInfo, error) {
	t.Helper()
	return s.Tasks().Start(t.Context(), kernel.TaskSpec{Kind: kernel.ToolExecKind, Title: "fetch", Input: json.RawMessage(`{"tool":"fetch","input":` + input + `}`)})
}

func TestBackgroundSchemaConflict(t *testing.T) {
	tool := &bgTool{spec: kernel.ToolSpec{Name: "fetch", InputSchema: json.RawMessage(`{"type":"object","properties":{"background":{"type":"string"}}}`), Background: true}}
	if _, err := kernel.New(kernel.WithTool(tool)); err == nil {
		t.Fatal("a Background tool whose schema already has background registered")
	}
}

// TestToolExecStartCannotChangeInput: the gate saw the input task_start was handed, so a hook that rewrites it fails closed.
func TestToolExecStartCannotChangeInput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rewrite := kernel.WithHook(kernel.TaskStart, "rewrite", func(_ context.Context, in kernel.TaskStartInput) (kernel.TaskStartOutput, error) {
			spec := in.Spec
			spec.Input = json.RawMessage(`{"tool":"fetch","input":{"url":"https://other.test"}}`)
			return kernel.TaskStartOutput{UpdatedSpec: &spec}, nil
		})
		h := newHarness(t, nil, kernel.WithTool(fetchTool(0, 0)), rewrite)
		_, err := hostExec(t, h.sess, `{"url":"https://a.test"}`)
		var de *kernel.TaskDeniedError
		if !errors.As(err, &de) || len(h.sess.Tasks().List()) != 0 {
			t.Fatalf("start: %v, %d tasks; the rewritten input must not run", err, len(h.sess.Tasks().List()))
		}
		h.close()
	})
}

// TestToolExecRestartKeepsTool: a restart's input is the tool's own; the switch is removed and the tool stays the same.
func TestToolExecRestartKeepsTool(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tool := fetchTool(0, 0)
		h := newHarness(t, nil, kernel.WithTool(tool), quiet)
		info, err := hostExec(t, h.sess, `{"url":"https://a.test"}`)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.sess.Tasks().Wait(t.Context(), info.ID); err != nil {
			t.Fatal(err)
		}
		if err := h.sess.Tasks().Restart(t.Context(), info.ID, json.RawMessage(`{"url":"https://b.test","background":true}`)); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		got, _ := h.sess.Tasks().Wait(t.Context(), info.ID)
		tool.mu.Lock()
		seen, path := tool.seen, tool.path
		tool.mu.Unlock()
		if got.Status != kernel.TaskSucceeded || got.Attempt != 2 || len(seen) != 2 || seen[1] != `{"url":"https://b.test"}` || path != 1 {
			t.Fatalf("attempt %d %s, the tool saw %q under %d hops", got.Attempt, got.Status, seen, path)
		}
		h.close()
	})
}

// TestToolExecTimeouts: a background run is not held to Limits.ToolTimeout, but the tool's own timeout still applies.
func TestToolExecTimeouts(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout time.Duration
		want    kernel.TaskStatus
	}{{"no_own_timeout", 0, kernel.TaskSucceeded}, {"own_timeout", time.Second, kernel.TaskFailed}} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t, nil, kernel.WithTool(fetchTool(tc.timeout, 5*time.Second)), kernel.WithLimits(kernel.Limits{ToolTimeout: time.Second}), quiet)
				info, err := hostExec(t, h.sess, `{"url":"https://a.test"}`)
				if err != nil {
					t.Fatal(err)
				}
				got, _ := h.sess.Tasks().Wait(t.Context(), info.ID)
				if got.Status != tc.want {
					t.Fatalf("%s: %s", got.Status, got.Error)
				}
				h.close()
			})
		})
	}
}

// TestToolExecProgressAndPostToolUse: the tool's progress becomes the task's, and post_tool_use rewrites the result while its stop_turn has no turn to end.
func TestToolExecProgressAndPostToolUse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rewrite := kernel.WithHook(kernel.PostToolUse, "redact", func(_ context.Context, in kernel.PostToolUseInput) (kernel.PostToolUseOutput, error) {
			return kernel.PostToolUseOutput{StopTurn: true, UpdatedResult: &kernel.SyntheticResult{Content: message.Content{&message.TextBlock{Text: "redacted"}}}}, nil
		})
		h := newHarness(t, nil, kernel.WithTool(fetchTool(0, 0)), rewrite, quiet)
		info, err := hostExec(t, h.sess, `{"url":"https://a.test"}`)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := h.sess.Tasks().Wait(t.Context(), info.ID)
		synctest.Wait()
		if got.Result == nil || got.Result.Summary != "redacted" {
			t.Fatalf("result %+v", got.Result)
		}
		var progress, toolProgress int
		for _, ev := range h.rec.all() {
			switch d := ev.Data.(type) {
			case *event.TaskProgress:
				if d.Text == "halfway" {
					progress++
				}
			case *event.ToolProgress:
				toolProgress++
			}
		}
		if progress != 1 || toolProgress != 0 {
			t.Fatalf("%d task.progress, %d tool.progress: the run reports through its task", progress, toolProgress)
		}
		h.close()
	})
}

// moveTool runs until release closes or its context ends, recording how each run ended; onRelease runs before it returns.
type moveTool struct {
	release   chan struct{}
	onRelease func(ctx context.Context, tc *kernel.ToolContext)
	mu        sync.Mutex
	runs      int
	ended     []error
}

func (m *moveTool) Spec() kernel.ToolSpec {
	return kernel.ToolSpec{Name: "build", InputSchema: json.RawMessage(`{"type":"object"}`), Background: true}
}

func (m *moveTool) Call(ctx context.Context, tc *kernel.ToolContext, _ json.RawMessage) (kernel.ToolResult, error) {
	m.mu.Lock()
	m.runs++
	m.mu.Unlock()
	select {
	case <-m.release:
		if m.onRelease != nil {
			m.onRelease(ctx, tc)
		}
		return kernel.TextResult("built"), nil
	case <-ctx.Done():
		m.mu.Lock()
		m.ended = append(m.ended, ctx.Err())
		m.mu.Unlock()
		return kernel.ToolResult{}, ctx.Err()
	}
}

func (m *moveTool) state() (int, []error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runs, append([]error(nil), m.ended...)
}

// moveBuild sends a turn whose build call the host moves to the background once it runs, and waits for the turn to end.
func moveBuild(t *testing.T, h *harness) kernel.TaskInfo {
	t.Helper()
	rc, err := h.sess.Send(kernel.Text("build"))
	if err != nil {
		t.Fatal(err)
	}
	synctest.Wait()
	info, err := h.sess.BackgroundCall(t.Context(), "toolu_1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rc.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	return info
}

func buildScripts() [][]providertest.Step {
	return [][]providertest.Step{toolReply(call{"toolu_1", "build", `{}`}), textReply("It runs in the background.")}
}

// TestBackgroundCallStopCancelsMovedTool: stopping the task a moved call became cancels the tool, which sees context.Canceled.
func TestBackgroundCallStopCancelsMovedTool(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tool := &moveTool{release: make(chan struct{})}
		h := newHarness(t, buildScripts(), kernel.WithTool(tool), quiet)
		info := moveBuild(t, h)
		if err := h.sess.Tasks().Stop(t.Context(), info.ID, "not needed"); err != nil {
			t.Fatal(err)
		}
		got, _ := h.sess.Tasks().Wait(t.Context(), info.ID)
		runs, ended := tool.state()
		if got.Status != kernel.TaskCanceled || runs != 1 || len(ended) != 1 || ended[0] != context.Canceled {
			t.Fatalf("%s after %d runs, the tool ended with %v", got.Status, runs, ended)
		}
		h.close()
	})
}

// TestBackgroundCallRestartRerunsTool: a restart replaces the adopted run, cancelling it, and runs the tool anew.
func TestBackgroundCallRestartRerunsTool(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tool := &moveTool{release: make(chan struct{})}
		h := newHarness(t, buildScripts(), kernel.WithTool(tool), quiet)
		info := moveBuild(t, h)
		if err := h.sess.Tasks().Restart(t.Context(), info.ID, nil); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		close(tool.release)
		got, _ := h.sess.Tasks().Wait(t.Context(), info.ID)
		runs, ended := tool.state()
		if got.Status != kernel.TaskSucceeded || got.Attempt != 2 || runs != 2 || len(ended) != 1 || ended[0] != context.Canceled {
			t.Fatalf("attempt %d %s after %d runs, ended %v", got.Attempt, got.Status, runs, ended)
		}
		h.close()
	})
}

// TestBackgroundCallCloseCancelsMovedTool: closing the session stops the task and its tool, leaving no worker behind.
func TestBackgroundCallCloseCancelsMovedTool(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tool := &moveTool{release: make(chan struct{})}
		h := newHarness(t, buildScripts(), kernel.WithTool(tool), quiet)
		moveBuild(t, h)
		h.close()
		if runs, ended := tool.state(); runs != 1 || len(ended) != 1 || ended[0] != context.Canceled {
			t.Fatalf("%d runs, ended %v", runs, ended)
		}
	})
}

// TestBackgroundCallTaskFromMovedToolHasNoCall: a task a moved tool starts is the task's doing, so it names no call, whose line is already committed.
func TestBackgroundCallTaskFromMovedToolHasNoCall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var sub kernel.TaskInfo
		var subErr error
		tool := &moveTool{release: make(chan struct{})}
		tool.onRelease = func(ctx context.Context, tc *kernel.ToolContext) {
			sub, subErr = tc.Session().Tasks().Start(ctx, kernel.TaskSpec{Kind: "noop", Title: "sub", Input: json.RawMessage(`{}`)})
		}
		noop := funcKind("noop", kernel.TaskCaps{}, func(context.Context, kernel.TaskRuntime) (kernel.TaskResult, error) {
			return kernel.TaskResult{Summary: "done"}, nil
		})
		h := newHarness(t, buildScripts(), kernel.WithTool(tool), kernel.WithTaskKind(noop), quiet)
		info := moveBuild(t, h)
		close(tool.release)
		h.sess.Tasks().Wait(t.Context(), info.ID)
		if subErr != nil || sub.ID == "" || sub.CallID != "" || sub.TurnID != "turn_01" {
			t.Fatalf("sub task %+v, %v", sub, subErr)
		}
		h.close()
	})
}

// TestBackgroundCallRefusesEmptyID: a host-started foreground task waits under no call, and an empty id must not move it.
func TestBackgroundCallRefusesEmptyID(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		job := funcKind("job", kernel.TaskCaps{}, func(ctx context.Context, _ kernel.TaskRuntime) (kernel.TaskResult, error) {
			select {
			case <-release:
			case <-ctx.Done():
			}
			return kernel.TaskResult{Summary: "done"}, nil
		})
		h := newHarness(t, nil, kernel.WithTaskKind(job), quiet)
		info, err := h.sess.Tasks().Start(t.Context(), kernel.TaskSpec{Kind: "job", Title: "job", Input: json.RawMessage(`{}`), Foreground: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.sess.BackgroundCall(t.Context(), ""); !errors.Is(err, kernel.ErrNotMovable) {
			t.Fatalf("empty id: %v", err)
		}
		if got, _ := h.sess.Tasks().Get(info.ID); !got.Foreground {
			t.Fatal("the host's foreground task was moved")
		}
		close(release)
		h.close()
	})
}

// TestCanceledStartFailsClosed: a task_start hook that never got to run on a canceled start refuses the task instead of letting it through.
func TestCanceledStartFailsClosed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ran := false
		policy := kernel.WithHook(kernel.TaskStart, "policy", func(context.Context, kernel.TaskStartInput) (kernel.TaskStartOutput, error) {
			ran = true
			return kernel.TaskStartOutput{}, nil
		})
		h := newHarness(t, nil, kernel.WithTool(fetchTool(0, 0)), policy)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := h.sess.Tasks().Start(ctx, kernel.TaskSpec{Kind: kernel.ToolExecKind, Title: "fetch", Input: json.RawMessage(`{"tool":"fetch","input":{"url":"https://a.test"}}`)})
		var de *kernel.TaskDeniedError
		if !errors.As(err, &de) || ran || len(h.sess.Tasks().List()) != 0 {
			t.Fatalf("start: %v, hook ran %v, %d tasks; a canceled start must not skip the policy", err, ran, len(h.sess.Tasks().List()))
		}
		h.close()
	})
}
