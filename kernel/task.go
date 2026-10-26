package kernel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/internal/prompt"
	"github.com/estonshi/aperture-potatos/message"
)

// TaskStatus is where a task is in its life; a task moves between running and waiting_input any number of times before it ends.
type TaskStatus string

const (
	TaskPending      TaskStatus = "pending"       // created, Run not yet called
	TaskRunning      TaskStatus = "running"       // Run is in progress
	TaskWaitingInput TaskStatus = "waiting_input" // AskInput was called; the next instruction is the answer
	TaskSucceeded    TaskStatus = "succeeded"     // Run returned a result
	TaskFailed       TaskStatus = "failed"        // Run returned an error
	TaskCanceled     TaskStatus = "canceled"      // asked to stop before Run returned, whatever Run returned then
	TaskLost         TaskStatus = "lost"          // unfinished when the process restarted, so its outcome is unknown
)

// Terminal includes lost, because nothing will ever report on that run again.
func (s TaskStatus) Terminal() bool {
	switch s {
	case TaskSucceeded, TaskFailed, TaskCanceled, TaskLost:
		return true
	}
	return false
}

// Task only fixes how background work behaves; whether it runs a query, an A2A exchange or a workflow is up to the implementation.
type Task interface {
	Run(ctx context.Context, rt TaskRuntime) (TaskResult, error)
}

// TaskRuntime is a task's only channel to its session; it is safe for concurrent use, and its writes become no-ops once the attempt has ended.
type TaskRuntime interface {
	ID() string
	Spec() TaskSpec
	Attempt() int
	TurnID() string
	Report(p TaskProgress)
	Inbox() <-chan TaskInstruction
	AskInput(question string)
	SaveState(state json.RawMessage)
	Holding(ins []TaskInstruction)
	Emit(p event.Payload)
	Relay(ctx context.Context, ev event.Event) error
	Session() *Session
}

// TaskProgress is what Report publishes and task_status reads; Percent is nil when the task cannot tell.
type TaskProgress struct {
	Text    string          `json:"text,omitempty"`
	Percent *float64        `json:"percent,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// TaskInstruction is an instruction, or an answer to AskInput, sent to a running task through its inbox.
type TaskInstruction struct {
	ID     string          `json:"id"`
	Text   string          `json:"text"`
	Data   json.RawMessage `json:"data,omitempty"`
	Mode   InputMode       `json:"mode"`
	Answer bool            `json:"answer,omitempty"` // set by the kernel on the instruction accepted while the attempt waited for input, so a kind can tell an answer from an instruction sent before the question
}

// TaskResult is what a task returns; Summary is what the model is notified with, cut at 8000 characters.
type TaskResult struct {
	Summary string          `json:"summary"`
	Content message.Content `json:"content,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// TaskCaps is what a kind promises the manager, which accepts or refuses Send, Restart and Inspect by it.
type TaskCaps struct {
	Instructions bool
	Continue     bool
	Restart      bool
	Inspect      bool
}

// TaskInspector lets task_status pull live state from a slow or remote backend instead of the last report; it runs concurrently with Run.
type TaskInspector interface {
	Inspect(ctx context.Context) (TaskProgress, error)
}

// TaskSpec describes a task to start; Input is what the kind's New decodes, and Continue is set only by the kernel.
type TaskSpec struct {
	Kind       string            `json:"kind"`
	Title      string            `json:"title"`
	Input      json.RawMessage   `json:"input"`
	Foreground bool              `json:"foreground,omitempty"`
	NotifyMode InputMode         `json:"notify_mode,omitempty"`
	Continue   *TaskContinuation `json:"-"`
}

// TaskContinuation hands a continuing attempt the state the last one saved and the instruction that continued it.
type TaskContinuation struct {
	State       json.RawMessage
	Instruction TaskInstruction
}

// TaskKind registers a kind of background work; Caps declares which of Send, Restart, continuation and Inspect the manager allows.
type TaskKind struct {
	Name        string
	Description string
	Caps        TaskCaps
	New         func(spec TaskSpec) (Task, error) // a fresh instance per start, restart and continuation, so no attempt inherits another's memory
}

// TaskInfo is a task's state as the manager last recorded it, the same view the task tools give the model.
type TaskInfo struct {
	ID                      string            `json:"task_id"`
	Kind                    string            `json:"kind"`
	Title                   string            `json:"title"`
	Status                  TaskStatus        `json:"status"`
	Attempt                 int               `json:"attempt"`
	Foreground              bool              `json:"foreground,omitempty"`
	CallID                  string            `json:"call_id,omitempty"`
	TurnID                  string            `json:"turn_id,omitempty"` // turn of the call that started this attempt; empty for host starts
	StartedAt               time.Time         `json:"started_at"`
	EndedAt                 time.Time         `json:"ended_at,omitzero"`
	Progress                TaskProgress      `json:"progress"`
	Question                string            `json:"question,omitempty"`
	Result                  *TaskResult       `json:"result,omitempty"`
	Error                   string            `json:"error,omitempty"`
	Abandoned               bool              `json:"abandoned,omitempty"` // it did not return within the grace period after a stop, so what it did is unknown
	PendingInstructions     int               `json:"pending_instructions,omitempty"`
	UnprocessedInstructions []TaskInstruction `json:"unprocessed_instructions,omitempty"`
	State                   json.RawMessage   `json:"state,omitempty"`
}

// ToolExecKind is reserved for the kernel's own kind, so every tool that opts into ToolSpec.Background runs under one kind with one review path.
const ToolExecKind = "tool_exec"

var (
	ErrTaskNotFound       = errors.New("task not found")                                                                 // no task with this id in this session
	ErrTaskNotContinuable = errors.New("task has finished and its kind does not support continuation; use task_restart") // from Send on an ended task
	ErrTaskNoInstructions = errors.New("task kind does not accept instructions")                                         // from Send on a running task whose kind lacks Caps.Instructions
	ErrTaskNotRestartable = errors.New("task kind does not support restart")                                             // from Restart on a kind that lacks Caps.Restart
	ErrTaskLimit          = errors.New("too many running tasks")                                                         // Limits.MaxRunningTasks unfinished tasks already
	ErrTaskBusy           = errors.New("task is being restarted or continued; retry")                                    // another restart or continuation of the task is still under way
)

// TaskDeniedError is a start the task_start hook refused, or a restart the approval gate refused; Text is written for the model.
type TaskDeniedError struct {
	Text string
}

// Error includes the text written for the model.
func (e *TaskDeniedError) Error() string { return "task refused: " + e.Text }

// WithTaskKind registers a kind at agent scope.
func WithTaskKind(k TaskKind) Option {
	return func(c *config) {
		c.app = append(c.app, func(r Registrar) error { _, err := r.TaskKind(k); return err })
	}
}

// ---- task_start and task_notify ----

// TaskStartInput is fired on the goroutine that calls Start, Restart or a continuing Send, so a refusal leaves nothing behind.
type TaskStartInput struct {
	HookEnv
	Spec         TaskSpec         `json:"spec"`
	CallID       string           `json:"call_id,omitempty"`
	Attempt      int              `json:"attempt"`
	Continuation bool             `json:"continuation"`
	Instruction  *TaskInstruction `json:"instruction,omitempty"`
}

// TaskStartOutput denies the start, which the caller gets as a *TaskDeniedError, or rewrites the spec.
type TaskStartOutput struct {
	Common
	Deny        bool      `json:"deny,omitempty"`
	Reason      string    `json:"reason,omitempty"`
	UpdatedSpec *TaskSpec `json:"updated_spec,omitempty"` // Kind cannot change, because the kind decides which implementation runs
}

func (o TaskStartOutput) hookDecision() string {
	if o.Deny {
		return "deny"
	}
	return ""
}

// TaskStart fires before every start, restart and continuation, keyed by kind, and a failing hook denies it.
var TaskStart = NewHookPoint(HookPointDef[TaskStartInput, TaskStartOutput]{
	Name: "task_start",
	Key:  func(in TaskStartInput) string { return in.Spec.Kind },
	Chain: func(in TaskStartInput, out TaskStartOutput) TaskStartInput {
		if out.UpdatedSpec != nil {
			in.Spec = *out.UpdatedSpec
		}
		return in
	},
	Merge: func(acc, out TaskStartOutput) (TaskStartOutput, bool) {
		if out.Deny {
			return out, true
		}
		if out.UpdatedSpec != nil {
			acc.UpdatedSpec = out.UpdatedSpec
		}
		return acc, false
	},
	Timeout: 10 * time.Second,
	Fail:    FailClosed,
	Closed: func(err error) TaskStartOutput {
		return TaskStartOutput{Deny: true, Reason: failReason("task_start hook", err)}
	},
})

// TaskNotifyInput is a task notification about to be queued for the model, with the task's state.
type TaskNotifyInput struct {
	HookEnv
	Info TaskInfo `json:"info"`
	Text string   `json:"text"`
}

// TaskNotifyOutput rewrites the notification text, or keeps it from starting a turn on an idle session.
type TaskNotifyOutput struct {
	Common
	UpdatedText string `json:"updated_text,omitempty"`
	NoWake      bool   `json:"no_wake,omitempty"`
}

// TaskNotify fires before each task notification is queued, keyed by kind.
var TaskNotify = NewHookPoint(HookPointDef[TaskNotifyInput, TaskNotifyOutput]{
	Name: "task_notify",
	Key:  func(in TaskNotifyInput) string { return in.Info.Kind },
	Chain: func(in TaskNotifyInput, out TaskNotifyOutput) TaskNotifyInput {
		if out.UpdatedText != "" {
			in.Text = out.UpdatedText
		}
		return in
	},
	Merge: func(acc, out TaskNotifyOutput) (TaskNotifyOutput, bool) {
		acc.NoWake = acc.NoWake || out.NoWake
		if out.UpdatedText != "" {
			acc.UpdatedText = out.UpdatedText
		}
		return acc, false
	},
	Timeout: 10 * time.Second,
	Fail:    FailOpen,
})

// ---- TaskTool ----

// backgroundProp is the switch every start tool and Background tool gets, byte for byte as spec/runtime.md fixes it.
const backgroundProp = `{"type":"boolean","default":false,"description":"Run in the background: return a task_id now and get a notification when it finishes."}`

// TaskTool makes a kind's start tool whose background switch lets the model choose between waiting in the call and getting a task_id; it fails when the schema is not an object or already has a background property.
func TaskTool(kind string, spec ToolSpec, toSpec func(input json.RawMessage) (TaskSpec, error)) (Tool, error) {
	schema, err := withBackground(spec.InputSchema, spec.Strict)
	if err != nil {
		return nil, fmt.Errorf("TaskTool %s: %w", spec.Name, err)
	}
	spec.InputSchema = schema
	spec.taskKind = kind
	return &taskTool{spec: spec, toSpec: toSpec}, nil
}

type taskTool struct {
	spec   ToolSpec
	toSpec func(json.RawMessage) (TaskSpec, error)
}

func (t *taskTool) Spec() ToolSpec { return t.spec }

// Call starts the task with the call's context, so the manager records it against this call and can tell a stop by the model from a stop by the host.
func (t *taskTool) Call(ctx context.Context, tc *ToolContext, input json.RawMessage) (ToolResult, error) {
	bg, rest, err := takeBackground(input)
	if err != nil {
		return ToolResult{}, err
	}
	s := tc.Session()
	spec, err := t.toSpec(rest)
	if err != nil {
		return s.TaskErrorResult(err), nil
	}
	spec.Kind, spec.Foreground, spec.Continue = t.spec.taskKind, !bg, nil
	info, w, err := s.tasks.start(ctx, spec, true)
	if err != nil {
		return s.TaskErrorResult(err), nil
	}
	if bg {
		return startedResult(s.agent.cfg.texts, info, false), nil
	}
	return s.tasks.foreground(ctx, info, w)
}

// startedResult is the text of a task that runs on without the call: started in the background, or moved there by a timeout.
func startedResult(texts prompt.Source, info TaskInfo, moved bool) ToolResult {
	vars := map[string]string{"task_id": info.ID}
	if moved {
		vars["moved"] = "yes"
	}
	res := TextResult(prompt.FilefFrom(texts, "task_started", vars))
	res.Structured = taskStructured(info)
	return res
}

func taskStructured(info TaskInfo) map[string]any {
	return map[string]any{"task_id": info.ID, "status": info.Status, "attempt": info.Attempt}
}

// finishedResult is a foreground call's result: the task's content, marked as an error when it did not succeed.
func finishedResult(texts prompt.Source, info TaskInfo) ToolResult {
	res := ToolResult{IsError: info.Status != TaskSucceeded, Structured: taskStructured(info)}
	if r := info.Result; r != nil {
		res.Content = r.Content
		if len(res.Content) == 0 && r.Summary != "" {
			res.Content = message.Content{&message.TextBlock{Text: r.Summary}}
		}
	}
	if len(res.Content) == 0 {
		text := info.Error
		if text == "" {
			text = string(info.Status)
		}
		res.Content = message.Content{&message.TextBlock{Text: prompt.TextfFrom(texts, "task_ended", map[string]string{"task_id": info.ID, "status": string(info.Status), "error": text})}}
	}
	return res
}

// TaskErrorResult is what a tool returns when a start, restart or continuation it asked for failed: the kernel's text for a refusal, the task limit or a failure, never the Go error.
func (s *Session) TaskErrorResult(err error) ToolResult {
	var de *TaskDeniedError
	switch {
	case errors.As(err, &de):
		return ErrorResult("%s", de.Text)
	case errors.Is(err, ErrTaskLimit):
		return ErrorResult("%s", s.textf("task_limit", map[string]string{"limit": strconv.Itoa(s.limits.MaxRunningTasks)}))
	}
	return ErrorResult("%s", s.textf("task_start_failed", map[string]string{"error": err.Error()}))
}

// withBackground appends the background property last, and to required when strict, keeping the caller's key order.
func withBackground(schema json.RawMessage, strict bool) (json.RawMessage, error) {
	if len(bytes.TrimSpace(schema)) == 0 {
		schema = emptySchema
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, schema); err != nil {
		return nil, err
	}
	keys, values, err := objectEntries(compact.Bytes())
	if err != nil {
		return nil, fmt.Errorf("the input schema is not a JSON object: %w", err)
	}
	props, req := -1, -1
	for i, k := range keys {
		switch k {
		case "properties":
			props = i
		case "required":
			req = i
		}
	}
	if props < 0 {
		keys, values = append(keys, "properties"), append(values, json.RawMessage(`{}`))
		props = len(keys) - 1
	}
	pk, pv, err := objectEntries(values[props])
	if err != nil {
		return nil, fmt.Errorf("properties is not an object: %w", err)
	}
	for _, k := range pk {
		if k == "background" {
			return nil, errors.New("the input schema already has a background property")
		}
	}
	values[props] = writeObject(append(pk, "background"), append(pv, json.RawMessage(backgroundProp)))
	if strict {
		var names []string
		if req >= 0 {
			if err := json.Unmarshal(values[req], &names); err != nil {
				return nil, fmt.Errorf("required is not a list of names: %w", err)
			}
		} else {
			keys, values = append(keys, "required"), append(values, nil)
			req = len(keys) - 1
		}
		b, _ := json.Marshal(append(names, "background"))
		values[req] = b
	}
	return writeObject(keys, values), nil
}

func writeObject(keys []string, values []json.RawMessage) json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		b.Write(values[i])
	}
	b.WriteByte('}')
	return b.Bytes()
}

// takeBackground reads and removes the background switch, so neither toSpec nor the tool ever sees it.
func takeBackground(input json.RawMessage) (bool, json.RawMessage, error) {
	keys, values, err := objectEntries(input)
	if err != nil {
		return false, nil, &InputError{Detail: "the arguments are not valid JSON"}
	}
	bg := false
	var ks []string
	var vs []json.RawMessage
	for i, k := range keys {
		if k != "background" {
			ks, vs = append(ks, k), append(vs, values[i])
			continue
		}
		if err := json.Unmarshal(values[i], &bg); err != nil {
			return false, nil, &InputError{Detail: "background must be a boolean"}
		}
	}
	return bg, writeObject(ks, vs), nil
}
