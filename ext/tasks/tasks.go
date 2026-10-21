// Package tasks gives the model task_status, task_send, task_stop and task_restart over the session's task manager, a usage section, and the list of unfinished tasks after compaction.
package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/spec"
)

// Name is the extension's name, which std.Without takes.
const Name = "tasks"

const (
	maxWait     = 10 * time.Minute
	inspectWait = 10 * time.Second
	stopPoll    = 50 * time.Millisecond
	maxListed   = 50
)

var toolNames = []string{"task_status", "task_send", "task_stop", "task_restart"}

// New registers the four tools, the capabilities section and the post_compact list at agent scope.
func New() kernel.Extension {
	return kernel.ExtensionFunc(Name, func(r kernel.Registrar) error {
		var defs []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		}
		if err := json.Unmarshal([]byte(spec.MustText("ext/tasks/tools.json")), &defs); err != nil {
			return err
		}
		conc := map[string]kernel.Concurrency{"task_status": kernel.Parallel}
		for _, d := range defs {
			ts := kernel.ToolSpec{Name: d.Name, Description: d.Description, InputSchema: d.InputSchema, Concurrency: kernel.Serial}
			if c, ok := conc[d.Name]; ok {
				ts.Concurrency = c
			}
			var t kernel.Tool
			switch d.Name {
			case "task_status":
				t = kernel.NewTool(ts, status)
			case "task_send":
				t = kernel.NewTool(ts, send)
			case "task_stop":
				t = kernel.NewTool(ts, stop)
			case "task_restart":
				t = kernel.NewTool(ts, restart)
			default:
				return fmt.Errorf("ext/tasks: unknown tool %q in tools.json", d.Name)
			}
			if _, err := r.Tool(t); err != nil {
				return err
			}
		}
		guidance := strings.TrimSpace(spec.MustText("ext/tasks/guidance.md"))
		if _, err := r.PromptSection(kernel.PromptSection{Name: Name, Slot: kernel.SlotCapabilities, Render: func(_ context.Context, rc kernel.RenderContext) (string, error) {
			if slices.ContainsFunc(rc.Tools, func(t kernel.ToolSpec) bool { return slices.Contains(toolNames, t.Name) }) {
				return guidance, nil
			}
			return "", nil // the section would describe tools the model does not have
		}}); err != nil {
			return err
		}
		_, err := kernel.On(r, kernel.PostCompact, Name, unfinished)
		return err
	})
}

var text = kernel.SpecTexts("ext/tasks/results.md")

// failure turns a task API error into the model's text; start failures keep the runtime's own words, so every path says the same.
func failure(tc *kernel.ToolContext, id string, err error) kernel.ToolResult {
	for _, m := range []struct {
		err error
		key string
	}{
		{kernel.ErrTaskNotFound, "not_found"},
		{kernel.ErrTaskNoInstructions, "no_instructions"},
		{kernel.ErrTaskNotContinuable, "not_continuable"},
		{kernel.ErrTaskNotRestartable, "not_restartable"},
		{kernel.ErrTaskBusy, "busy"},
		{kernel.ErrQueueFull, "queue_full"},
	} {
		if errors.Is(err, m.err) {
			return kernel.ErrorResult("%s", text(m.key, "task_id", id))
		}
	}
	return tc.Session().TaskErrorResult(err)
}

type statusInput struct {
	TaskID        string `json:"task_id,omitempty"`
	IncludeResult bool   `json:"include_result,omitempty"`
	WaitMS        int64  `json:"wait_ms,omitempty"`
}

// statusView is task_status's object; progress data, result data and state stay out of the model's context.
type statusView struct {
	TaskID              string        `json:"task_id"`
	Kind                string        `json:"kind"`
	Title               string        `json:"title"`
	Status              string        `json:"status"`
	Attempt             int           `json:"attempt"`
	StartedAt           time.Time     `json:"started_at"`
	EndedAt             time.Time     `json:"ended_at,omitzero"`
	Progress            *progressView `json:"progress,omitempty"`
	Question            string        `json:"question,omitempty"`
	Error               string        `json:"error,omitempty"`
	Abandoned           bool          `json:"abandoned,omitempty"`
	PendingInstructions int           `json:"pending_instructions,omitempty"`
	Summary             string        `json:"summary,omitempty"`
}

type progressView struct {
	Text    string   `json:"text,omitempty"`
	Percent *float64 `json:"percent,omitempty"`
}

type listed struct {
	TaskID  string `json:"task_id"`
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	Status  string `json:"status"`
	Attempt int    `json:"attempt"`
}

func status(ctx context.Context, tc *kernel.ToolContext, in statusInput) (kernel.ToolResult, error) {
	tasks := tc.Session().Tasks()
	if in.TaskID == "" {
		out := struct {
			Tasks []listed `json:"tasks"`
		}{Tasks: []listed{}}
		for _, t := range tasks.List() {
			out.Tasks = append(out.Tasks, listed{TaskID: t.ID, Kind: t.Kind, Title: t.Title, Status: string(t.Status), Attempt: t.Attempt})
		}
		return jsonResult(out)
	}
	if _, ok := tasks.Get(in.TaskID); !ok {
		return failure(tc, in.TaskID, kernel.ErrTaskNotFound), nil
	}
	if in.WaitMS > 0 {
		wctx, cancel := context.WithTimeout(ctx, min(time.Duration(in.WaitMS)*time.Millisecond, maxWait))
		_, _ = tasks.Wait(wctx, in.TaskID) // a wait that runs out still reports where the task is
		cancel()
		if ctx.Err() != nil {
			return kernel.ToolResult{}, ctx.Err()
		}
	}
	ictx, cancel := context.WithTimeout(ctx, inspectWait)
	info, err := tasks.Inspect(ictx, in.TaskID) // a failed inspection still reports the last progress
	cancel()
	if errors.Is(err, kernel.ErrTaskNotFound) {
		return failure(tc, in.TaskID, err), nil
	}
	if ctx.Err() != nil {
		return kernel.ToolResult{}, ctx.Err()
	}
	v := statusView{TaskID: info.ID, Kind: info.Kind, Title: info.Title, Status: string(info.Status), Attempt: info.Attempt, StartedAt: info.StartedAt, EndedAt: info.EndedAt,
		Question: info.Question, Error: info.Error, Abandoned: info.Abandoned, PendingInstructions: info.PendingInstructions}
	if p := info.Progress; p.Text != "" || p.Percent != nil {
		v.Progress = &progressView{Text: p.Text, Percent: p.Percent}
	}
	finished := info.Status.Terminal()
	if finished && info.Result != nil {
		v.Summary = kernel.SanitizeForeign(info.Result.Summary)
	}
	res, err := jsonResult(v)
	if err != nil || !in.IncludeResult || !finished || info.Result == nil {
		return res, err
	}
	for _, b := range info.Result.Content {
		if t, ok := b.(*message.TextBlock); ok {
			b = &message.TextBlock{Text: kernel.SanitizeForeign(t.Text)}
		}
		res.Content = append(res.Content, b)
	}
	return res, nil
}

func jsonResult(v any) (kernel.ToolResult, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return kernel.ToolResult{}, err
	}
	return kernel.TextResult(string(b)), nil
}

type sendInput struct {
	TaskID  string `json:"task_id"`
	Message string `json:"message"`
	Mode    string `json:"mode,omitempty"`
}

func send(ctx context.Context, tc *kernel.ToolContext, in sendInput) (kernel.ToolResult, error) {
	mode := kernel.InputMode(in.Mode)
	switch mode {
	case "":
		mode = kernel.ModeNextLoop
	case kernel.ModeQueue, kernel.ModeNextLoop, kernel.ModeInterrupt:
	default:
		return kernel.ToolResult{}, &kernel.InputError{Detail: "mode must be queue, next_loop or interrupt"}
	}
	tasks := tc.Session().Tasks()
	before, ok := tasks.Get(in.TaskID)
	if !ok {
		return failure(tc, in.TaskID, kernel.ErrTaskNotFound), nil
	}
	if err := tasks.Send(ctx, in.TaskID, kernel.TaskInstruction{Text: in.Message, Mode: mode}); err != nil {
		return failure(tc, in.TaskID, err), nil
	}
	if !before.Status.Terminal() {
		return kernel.TextResult(text("delivered", "task_id", in.TaskID, "mode", string(mode))), nil
	}
	after, _ := tasks.Get(in.TaskID)
	return kernel.TextResult(text("continuing", "task_id", in.TaskID, "attempt", strconv.Itoa(after.Attempt))), nil
}

type stopInput struct {
	TaskID string `json:"task_id"`
	Reason string `json:"reason,omitempty"`
}

// stop waits on the attempt it stopped, not with Wait, which returns for a question and follows a restart to the next attempt.
func stop(ctx context.Context, tc *kernel.ToolContext, in stopInput) (kernel.ToolResult, error) {
	tasks := tc.Session().Tasks()
	before, ok := tasks.Get(in.TaskID)
	if !ok {
		return failure(tc, in.TaskID, kernel.ErrTaskNotFound), nil
	}
	if before.Status.Terminal() {
		return kernel.TextResult(text("already_finished", "task_id", in.TaskID, "status", string(before.Status))), nil
	}
	if err := tasks.Stop(ctx, in.TaskID, in.Reason); err != nil {
		return failure(tc, in.TaskID, err), nil
	}
	for {
		info, ok := tasks.Get(in.TaskID)
		switch {
		case !ok || info.Attempt != before.Attempt:
			return kernel.TextResult(text("stopped", "task_id", in.TaskID)), nil
		case info.Status.Terminal() && info.Abandoned:
			return kernel.TextResult(text("abandoned", "task_id", in.TaskID)), nil
		case info.Status.Terminal():
			return kernel.TextResult(text("stopped", "task_id", in.TaskID)), nil
		}
		select {
		case <-time.After(stopPoll):
		case <-ctx.Done():
			return kernel.ToolResult{}, ctx.Err()
		}
	}
}

type restartInput struct {
	TaskID string          `json:"task_id"`
	Input  json.RawMessage `json:"input,omitempty"`
}

func restart(ctx context.Context, tc *kernel.ToolContext, in restartInput) (kernel.ToolResult, error) {
	tasks := tc.Session().Tasks()
	before, ok := tasks.Get(in.TaskID)
	if !ok {
		return failure(tc, in.TaskID, kernel.ErrTaskNotFound), nil
	}
	if err := tasks.Restart(ctx, in.TaskID, in.Input); err != nil {
		return failure(tc, in.TaskID, err), nil
	}
	return kernel.TextResult(text("restarted", "task_id", in.TaskID, "attempt", strconv.Itoa(before.Attempt+1))), nil
}

// unfinished hands the new epoch the tasks still running, so a summary that lost their ids does not lose control of them.
func unfinished(_ context.Context, in kernel.PostCompactInput) (kernel.PostCompactOutput, error) {
	var lines []string
	for _, t := range in.Session.Tasks().List() {
		if !t.Status.Terminal() && len(lines) < maxListed {
			lines = append(lines, fmt.Sprintf("- %s %s \"%s\": %s", t.ID, t.Kind, t.Title, t.Status))
		}
	}
	if len(lines) == 0 {
		return kernel.PostCompactOutput{}, nil
	}
	return kernel.PostCompactOutput{Common: kernel.Common{AdditionalContext: text("unfinished") + "\n" + strings.Join(lines, "\n")}}, nil
}
