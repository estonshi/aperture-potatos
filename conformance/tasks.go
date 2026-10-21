package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
)

// LoopTaskKind is a scripted task kind: what each attempt does at once, and a start tool the model can call.
type LoopTaskKind struct {
	Name      string         `json:"name"`
	Caps      LoopTaskCaps   `json:"caps"`
	Auto      []TaskStep     `json:"auto"` // run at the start of every attempt, before any step an action hands it
	StartTool *LoopStartTool `json:"start_tool"`
	Failing   string         `json:"new_error"` // New fails with this text
}

type LoopTaskCaps struct {
	Instructions bool `json:"instructions"`
	Continue     bool `json:"continue"`
	Restart      bool `json:"restart"`
}

// LoopStartTool is the kind's TaskTool; the task's title is the input's title field when it has one.
type LoopStartTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	TimeoutMS   int64           `json:"timeout_ms"`
}

// TaskStep is one thing a scripted task does; exactly one field is set.
type TaskStep struct {
	Report      *kernel.TaskProgress `json:"report"`
	Ask         *string              `json:"ask"`   // AskInput; an empty question withdraws the one pending
	Save        json.RawMessage      `json:"save"`  // SaveState
	Read        bool                 `json:"read"`  // takes what the inbox holds now into the notes
	AwaitAnswer bool                 `json:"await"` // reads the inbox into the notes until the answer arrives
	Succeed     *string              `json:"succeed"`
	Fail        string               `json:"fail"`
	IgnoreCtx   bool                 `json:"ignore_ctx"` // from now on a stop does not end it
	WaitQuiet   bool                 `json:"wait_quiet"` // waits on its own session, noting whether that was refused as re-entrant
}

// taskScripts hands each scripted task the steps actions give it, by task id, across attempts.
type taskScripts struct {
	mu    sync.Mutex
	steps map[string]chan TaskStep
}

func (ts *taskScripts) ch(id string) chan TaskStep {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.steps == nil {
		ts.steps = map[string]chan TaskStep{}
	}
	c, ok := ts.steps[id]
	if !ok {
		c = make(chan TaskStep, 16)
		ts.steps[id] = c
	}
	return c
}

func (lk LoopTaskKind) kind(ts *taskScripts) kernel.TaskKind {
	return kernel.TaskKind{Name: lk.Name, Caps: kernel.TaskCaps{Instructions: lk.Caps.Instructions, Continue: lk.Caps.Continue, Restart: lk.Caps.Restart},
		New: func(spec kernel.TaskSpec) (kernel.Task, error) {
			if lk.Failing != "" {
				return nil, errors.New(lk.Failing)
			}
			t := &scriptTask{auto: lk.Auto, scripts: ts}
			if c := spec.Continue; c != nil {
				t.notes = append(t.notes, fmt.Sprintf("continued with %q from %s", c.Instruction.Text, c.State))
			}
			return t, nil
		}}
}

func (lk LoopTaskKind) tool() (kernel.Tool, error) {
	st := lk.StartTool
	spec := kernel.ToolSpec{Name: st.Name, Description: st.Description, InputSchema: st.InputSchema, Concurrency: kernel.Parallel, Timeout: ms(st.TimeoutMS)}
	return kernel.TaskTool(lk.Name, spec, func(input json.RawMessage) (kernel.TaskSpec, error) {
		var in struct {
			Title string `json:"title"`
		}
		_ = json.Unmarshal(input, &in)
		if in.Title == "" {
			in.Title = lk.Name
		}
		return kernel.TaskSpec{Title: in.Title, Input: input}, nil
	})
}

type scriptTask struct {
	auto      []TaskStep
	scripts   *taskScripts
	notes     []string
	ignoreCtx bool
}

func (t *scriptTask) Run(ctx context.Context, rt kernel.TaskRuntime) (kernel.TaskResult, error) {
	steps := t.scripts.ch(rt.ID())
	for i := 0; ; i++ {
		var s TaskStep
		if i < len(t.auto) {
			s = t.auto[i]
		} else {
			done := ctx.Done()
			if t.ignoreCtx {
				done = nil
			}
			select {
			case s = <-steps:
			case <-done:
				return kernel.TaskResult{}, ctx.Err()
			}
		}
		if res, err, end := t.step(ctx, rt, s); end {
			return res, err
		}
	}
}

// step does one step; end reports that the attempt returns with res and err.
func (t *scriptTask) step(ctx context.Context, rt kernel.TaskRuntime, s TaskStep) (kernel.TaskResult, error, bool) {
	switch {
	case s.Report != nil:
		rt.Report(*s.Report)
	case s.Ask != nil:
		rt.AskInput(*s.Ask)
	case s.Save != nil:
		rt.SaveState(s.Save)
	case s.Read:
		for len(rt.Inbox()) > 0 {
			t.note(<-rt.Inbox())
		}
	case s.AwaitAnswer:
		for {
			select {
			case in := <-rt.Inbox():
				t.note(in)
				if in.Answer {
					return kernel.TaskResult{}, nil, false
				}
			case <-ctx.Done():
				return kernel.TaskResult{}, ctx.Err(), true
			}
		}
	case s.Succeed != nil:
		sum := strings.Join(append([]string{*s.Succeed}, t.notes...), "\n")
		return kernel.TaskResult{Summary: sum, Content: message.Content{&message.TextBlock{Text: sum}}}, nil, true
	case s.Fail != "":
		return kernel.TaskResult{Summary: strings.Join(t.notes, "\n")}, errors.New(s.Fail), true
	case s.IgnoreCtx:
		t.ignoreCtx = true
	case s.WaitQuiet:
		_, err := rt.Session().WaitQuiet(ctx)
		t.notes = append(t.notes, fmt.Sprintf("wait_quiet refused: %v", errors.Is(err, kernel.ErrReentrantWait)))
	}
	return kernel.TaskResult{}, nil, false
}

func (t *scriptTask) note(in kernel.TaskInstruction) {
	n := fmt.Sprintf("%s (%s)", in.Text, in.Mode)
	if in.Answer {
		n += " [answer]"
	}
	t.notes = append(t.notes, n)
}

// taskAction drives the session's task manager as a host would, or hands a scripted task its next step.
type taskAction struct {
	Start   *kernel.TaskSpec `json:"start"`
	Send    string           `json:"send"`
	Text    string           `json:"text"`
	Mode    string           `json:"mode"`
	Stop    string           `json:"stop"`
	Reason  string           `json:"reason"`
	Restart string           `json:"restart"`
	Input   json.RawMessage  `json:"input"`
	Step    string           `json:"step"` // the task that gets Do
	Do      *TaskStep        `json:"do"`
	Get     string           `json:"get"`  // record the task's info
	Wait    string           `json:"wait"` // wait for it, then record its info
	List    bool             `json:"list"` // record every task's info
}

func (r *loopRun) task(a taskAction) (err error) {
	ctx := r.t.Context()
	tasks := r.sess.Tasks()
	switch {
	case a.Start != nil:
		_, err = tasks.Start(ctx, *a.Start)
	case a.Send != "":
		err = tasks.Send(ctx, a.Send, kernel.TaskInstruction{Text: a.Text, Mode: kernel.InputMode(a.Mode)})
	case a.Stop != "":
		err = tasks.Stop(ctx, a.Stop, a.Reason)
	case a.Restart != "":
		err = tasks.Restart(ctx, a.Restart, a.Input)
	case a.Step != "" && a.Do != nil:
		r.scripts.ch(a.Step) <- *a.Do
	case a.Get != "":
		info, ok := tasks.Get(a.Get)
		if !ok {
			return kernel.ErrTaskNotFound
		}
		return r.recordTask(info)
	case a.Wait != "":
		info, werr := tasks.Wait(ctx, a.Wait)
		if werr != nil {
			return werr
		}
		return r.recordTask(info)
	case a.List:
		return r.recordTask(tasks.List())
	default:
		return errors.New("unknown task action")
	}
	return err
}

// recordTask keeps what a task action read for expected.tasks.json, with times left out, since they only repeat the clock.
func (r *loopRun) recordTask(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var x any
	if err := json.Unmarshal(b, &x); err != nil {
		return err
	}
	dropTimes(x)
	b, err = json.Marshal(x)
	if err != nil {
		return err
	}
	r.taskLog = append(r.taskLog, b)
	return nil
}

func dropTimes(x any) {
	switch v := x.(type) {
	case map[string]any:
		delete(v, "started_at")
		delete(v, "ended_at")
		for _, c := range v {
			dropTimes(c)
		}
	case []any:
		for _, c := range v {
			dropTimes(c)
		}
	}
}

// waitQuiet is WaitQuiet bounded by virtual time, so a case that never goes quiet fails instead of hanging.
func (r *loopRun) waitQuiet() error {
	ctx, cancel := context.WithTimeout(r.t.Context(), time.Hour)
	defer cancel()
	_, err := r.sess.WaitQuiet(ctx)
	return err
}
