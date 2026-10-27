package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/estonshi/aperture-potatos/conformance/compare"
	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/internal/determ"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/anthropic"
	"github.com/estonshi/aperture-potatos/provider/openaichat"
	"github.com/estonshi/aperture-potatos/provider/openairesponses"
	"github.com/estonshi/aperture-potatos/provider/providertest"
	"github.com/estonshi/aperture-potatos/store"
)

// LoopScript is one layer-2 case; conformance/README.md documents every field.
type LoopScript struct {
	SpecVersion string            `json:"spec_version"`
	Description string            `json:"description,omitempty"`
	Agent       LoopAgent         `json:"agent"`
	Session     LoopSession       `json:"session"`
	Providers   []LoopProvider    `json:"providers"`
	Clock       LoopClock         `json:"clock"`
	IDs         string            `json:"ids"`
	Resume      *LoopResume       `json:"resume,omitempty"`
	ExpectCalls bool              `json:"expect_calls,omitempty"` // compare the case session's calls file with expected.calls.jsonl
	Actions     []json.RawMessage `json:"actions"`
}

// LoopResume seeds the store with a session file and resumes that session instead of creating one.
type LoopResume struct {
	SessionID string `json:"session_id"`
	File      string `json:"file"`             // relative to the case
	Calls     string `json:"calls,omitempty"`  // the session's calls file, relative to the case
	Labels    string `json:"labels,omitempty"` // the session's label record, relative to the case
}

// LoopCosts is a scripted cost source: a response id is priced from the round named, and a round can fail instead.
type LoopCosts struct {
	Prices map[string]LoopPrice    `json:"prices"`
	Errors map[string]LoopCostFail `json:"errors"` // by round, from 1
}

type LoopPrice struct {
	Amount    float64 `json:"amount"`
	Currency  string  `json:"currency"`
	FromRound int     `json:"from_round"`
}

type LoopCostFail struct {
	Status  int    `json:"status"`
	Message string `json:"message"`
}

type LoopAgent struct {
	Nickname                  string                     `json:"nickname"`
	Description               string                     `json:"description"`
	Instructions              string                     `json:"instructions"`
	InputMode                 string                     `json:"input_mode"`
	AutoContinueTruncatedText bool                       `json:"auto_continue_truncated_text"`
	StorePolicy               string                     `json:"store_policy"`
	Limits                    LoopLimits                 `json:"limits"`
	Retry                     LoopRetry                  `json:"retry"`
	Tools                     []LoopTool                 `json:"tools"`
	HostedTools               []message.HostedTool       `json:"hosted_tools"`
	Commands                  []LoopCommand              `json:"commands"`
	Capabilities              []LoopCaps                 `json:"capabilities"`
	Hooks                     []LoopHook                 `json:"hooks"`
	ApprovalRules             json.RawMessage            `json:"approval_rules"` // a rule file, read with ParseApprovalRules
	Sections                  []LoopSection              `json:"sections"`
	ContextProviders          []LoopContext              `json:"context_providers"`
	RequestHeaders            map[string]string          `json:"request_headers"` // header name -> the RequestInfo field it carries: root_session_id, session_id, turn_id, bucket, api or model
	CaptureHeaders            []string                   `json:"capture_headers"`
	CostSource                *LoopCosts                 `json:"cost_source"`
	ContextPolicy             *LoopPolicy                `json:"context_policy"`
	Compactor                 *LoopCompactor             `json:"compactor"`
	TaskKinds                 []LoopTaskKind             `json:"task_kinds"`
	Extensions                map[string]json.RawMessage `json:"extensions"` // name -> options, built by the extension test that drives the case
}

// loopApprovalRules reads a rule file whose rules may carry a match behavior, which becomes the runner's native predicate.
func loopApprovalRules(data json.RawMessage) ([]kernel.ApprovalRule, error) {
	var raws []map[string]json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil {
		return nil, fmt.Errorf("approval rules: %w", err)
	}
	matches := make([]json.RawMessage, len(raws))
	for i, raw := range raws {
		if m, ok := raw["match"]; ok {
			matches[i] = m
			delete(raw, "match")
		}
	}
	stripped, err := json.Marshal(raws)
	if err != nil {
		return nil, err
	}
	rules, err := kernel.ParseApprovalRules(stripped)
	if err != nil {
		return nil, err
	}
	for i := range rules {
		if matches[i] == nil {
			continue
		}
		p, err := scriptPredicate(matches[i])
		if err != nil {
			return nil, fmt.Errorf("approval rule %q: %w", rules[i].ID, err)
		}
		rules[i].Match = p
	}
	return rules, nil
}

// scriptPredicate turns a case's match behavior into a predicate: always, never, contains (the normalized input holds value) or panic.
func scriptPredicate(raw json.RawMessage) (func(kernel.ToolCallView) bool, error) {
	var b struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	}
	if len(raw) > 0 && raw[0] == '"' {
		if err := json.Unmarshal(raw, &b.Type); err != nil {
			return nil, err
		}
	} else if err := json.Unmarshal(raw, &b); err != nil {
		return nil, err
	}
	switch b.Type {
	case "always":
		return func(kernel.ToolCallView) bool { return true }, nil
	case "never":
		return func(kernel.ToolCallView) bool { return false }, nil
	case "contains":
		if b.Value == "" {
			return nil, errors.New(`match behavior "contains" needs a value`)
		}
		return func(v kernel.ToolCallView) bool { return bytes.Contains(v.Input, []byte(b.Value)) }, nil
	case "panic":
		return func(kernel.ToolCallView) bool { panic("scripted predicate") }, nil
	}
	return nil, fmt.Errorf("unknown match behavior %q", b.Type)
}

// LoopPolicy is ContextPolicy in JSON.
type LoopPolicy struct {
	MaxContextTokens       int     `json:"max_context_tokens"`
	AutoCompactRatio       float64 `json:"auto_compact_ratio"`
	TargetRatio            float64 `json:"target_ratio"`
	MinSummaryTokens       int     `json:"min_summary_tokens"`
	MaxSummaryTokens       int     `json:"max_summary_tokens"`
	PinTokens              int     `json:"pin_tokens"`
	PinTotalTokens         int     `json:"pin_total_tokens"`
	MaxAutoCompactsPerTurn int     `json:"max_auto_compacts_per_turn"`
	MaxConsecutiveFailures int     `json:"max_consecutive_failures"`
	Disabled               bool    `json:"disabled"`
}

// LoopCompactor is a scripted Compactor: it asks the model for a summary with instructions, or uses a fixed one, and keeps what keep names.
type LoopCompactor struct {
	Instructions string `json:"instructions"` // summarize with the model; the per-call instructions follow
	Summary      string `json:"summary"`      // a fixed summary instead
	Keep         string `json:"keep"`         // none, current_turn or after_cut
	Strategy     string `json:"strategy"`
	Fail         string `json:"fail"` // nothing_to_compact, or any other text as an error
}

// LoopHook is a scripted hook registered through the JSON form of its point.
type LoopHook struct {
	Point     string         `json:"point"`
	Name      string         `json:"name"`
	Match     string         `json:"match"` // the declarative matcher form
	Priority  int            `json:"priority"`
	TimeoutMS *int64         `json:"timeout_ms"`
	Fail      string         `json:"fail"` // open | closed; absent keeps the point's
	Async     bool           `json:"async"`
	WakeLimit *LoopWakeLimit `json:"wake_limit"`
	Calls     []LoopHookCall `json:"calls"` // one per call in order, the last one repeating; none answers {}
	Cases     []LoopHookCase `json:"cases"` // checked first: the first whose paths all match the input answers
}

// LoopHookCase answers a call whose input has every value in when, keyed by dot path, so concurrent calls get their own answers.
type LoopHookCase struct {
	When map[string]any `json:"when"`
	Call LoopHookCall   `json:"call"`
}

type LoopWakeLimit struct {
	N     int   `json:"n"`
	PerMS int64 `json:"per_ms"`
}

// LoopHookCall is what one call of a scripted hook does: wait, then answer output, fail with error, or panic.
type LoopHookCall struct {
	Output    json.RawMessage `json:"output"`
	Error     string          `json:"error"`
	Panic     string          `json:"panic"`
	SleepMS   int64           `json:"sleep_ms"`
	IgnoreCtx bool            `json:"ignore_ctx"`
	Hold      bool            `json:"hold"` // waits until the call's context ends
}

// LoopSection is a prompt section that renders fixed text.
type LoopSection struct {
	Name            string `json:"name"`
	Slot            string `json:"slot"` // capabilities | catalog | environment
	Order           int    `json:"order"`
	Text            string `json:"text"`
	AnnounceChanges bool   `json:"announce_changes"`
}

// LoopContext is a context provider that answers its texts in order, the last one repeating.
type LoopContext struct {
	Name  string   `json:"name"`
	When  string   `json:"when"` // turn_start | every_step
	Texts []string `json:"texts"`
}

// LoopCaps is one capability declaration of the agent.
type LoopCaps struct {
	API       provider.API          `json:"api"`
	ModelGlob string                `json:"model_glob"`
	Caps      provider.Capabilities `json:"caps"`
}

type LoopSession struct {
	InputMode     string            `json:"input_mode"`
	Instructions  *string           `json:"instructions"`
	Labels        map[string]string `json:"labels"`
	Tools         []LoopTool        `json:"tools"` // registered on the session before its first epoch, so remove_tool can take them away
	Hooks         []LoopHook        `json:"hooks"`
	ApprovalRules json.RawMessage   `json:"approval_rules"`
}

type LoopLimits struct {
	MaxSteps                int   `json:"max_steps"`
	ToolTimeoutMS           int64 `json:"tool_timeout_ms"`
	ToolCancelGraceMS       int64 `json:"tool_cancel_grace_ms"`
	MaxConsecutiveAutoTurns int   `json:"max_consecutive_auto_turns"`
	MaxQueuedInputs         int   `json:"max_queued_inputs"`
	MaxTruncationContinues  int   `json:"max_truncation_continues"`
	ReadAhead               int   `json:"read_ahead"`
	MaxParallelTools        int   `json:"max_parallel_tools"`
	HookGraceMS             int64 `json:"hook_grace_ms"`
	MaxStopContinuations    int   `json:"max_stop_continuations"`
	MaxAsyncHooks           int   `json:"max_async_hooks"`
	MaxRunningTasks         int   `json:"max_running_tasks"`
}

type LoopRetry struct {
	MaxAttempts int   `json:"max_attempts"`
	BaseDelayMS int64 `json:"base_delay_ms"`
	MaxDelayMS  int64 `json:"max_delay_ms"`
}

type LoopClock struct {
	Start       time.Time `json:"start"`
	StepMS      int64     `json:"step_ms"`
	Interleaved bool      `json:"interleaved"` // tasks or async hooks read the clock while the loop does, so timestamps are not compared
}

type LoopTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	Concurrency string          `json:"concurrency"`
	TimeoutMS   int64           `json:"timeout_ms"`
	Behavior    ToolBehavior    `json:"behavior"`
	Strict      bool            `json:"strict"`     // rejects input keys its schema does not name exactly, as NewTool decodes
	Background  bool            `json:"background"` // ToolSpec.Background: the model may run it as a tool_exec task
}

// ToolBehavior is what a scripted tool does: echo returns its input, fixed returns text, error returns an error, panic panics.
type ToolBehavior struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	IsError   bool            `json:"is_error"`
	SleepMS   int64           `json:"sleep_ms"`
	IgnoreCtx bool            `json:"ignore_ctx"`
	Progress  []string        `json:"progress"`
	Pin       *message.Pin    `json:"pin"`        // the result also declares this pin
	Task      string          `json:"task"`       // the task a task_stop or task_restart behavior acts on
	TaskInput json.RawMessage `json:"task_input"` // a task_restart's new input
}

// LoopCommand is a scripted command; its prompt replaces $ARGUMENTS with the command's arguments, and a template makes it a prompt command.
type LoopCommand struct {
	Name        string       `json:"name"`
	Aliases     []string     `json:"aliases"`
	Description string       `json:"description"`
	Busy        string       `json:"busy"`
	Text        string       `json:"text"`
	Prompt      string       `json:"prompt"`
	SleepMS     int64        `json:"sleep_ms"`
	Error       string       `json:"error"`
	Template    string       `json:"template"`  // PromptCommand's template
	ArgNames    []string     `json:"arg_names"` // PromptCommand's WithArgs
	Pin         *message.Pin `json:"pin"`       // the result also declares this pin
}

type LoopProvider struct {
	Name         string                 `json:"name"`
	API          string                 `json:"api"`
	Model        string                 `json:"model"`
	Params       provider.Params        `json:"params"`
	Responses    []LoopResponse         `json:"responses"`
	Resolved     *provider.Capabilities `json:"resolved"`      // what the scripted resolver answers for this model; absent means all unknown
	ResolveError string                 `json:"resolve_error"` // makes the scripted resolver fail for this model
}

// LoopResponse answers one model request: an SSE body through the real adapter, or neutral stream events through a scripted provider.
type LoopResponse struct {
	SSE             string            `json:"sse,omitempty"`
	Status          int               `json:"status,omitempty"`
	Headers         map[string]string `json:"headers,omitempty"`
	Events          string            `json:"events,omitempty"`
	HoldAfterEvents int               `json:"hold_after_events,omitempty"`
	HoldMS          int64             `json:"hold_ms,omitempty"` // 0 holds until the request is canceled
}

// UnmarshalJSON accepts a bare path as shorthand for {"sse": path}.
func (r *LoopResponse) UnmarshalJSON(b []byte) error {
	var path string
	if json.Unmarshal(b, &path) == nil {
		*r = LoopResponse{SSE: path}
		return nil
	}
	type plain LoopResponse
	return json.Unmarshal(b, (*plain)(r))
}

type loopAction struct {
	Send           *sendAction         `json:"send"`
	Command        *commandAction      `json:"command"`
	Interrupt      *interruptAction    `json:"interrupt"`
	Withdraw       string              `json:"withdraw"`       // the as name of an input to take back
	InterruptWith  string              `json:"interrupt_with"` // the as name of a queued input to interrupt the turn with
	SwitchModel    *switchAction       `json:"switch_model"`
	AddTool        *LoopTool           `json:"add_tool"`
	RemoveTool     string              `json:"remove_tool"`
	AddHosted      *message.HostedTool `json:"add_hosted_tool"`
	RemoveHosted   string              `json:"remove_hosted_tool"`
	AddSection     *LoopSection        `json:"add_section"`
	RemoveSection  string              `json:"remove_section"`
	Ledger         *ledgerAction       `json:"ledger"`
	SetLabels      map[string]string   `json:"set_labels"`    // merged into the session's labels, an empty value removing its key
	Labels         bool                `json:"labels"`        // records the session's labels
	FindSessions   *map[string]string  `json:"find_sessions"` // records the top-level sessions whose labels hold every pair
	Resume         string              `json:"resume"`        // resume this session id again, which fails while it is open
	Compact        *compactAction      `json:"compact"`
	Clear          bool                `json:"clear"`
	Unpin          string              `json:"unpin"`
	Rewind         string              `json:"rewind"` // input or turn id
	Fork           *forkAction         `json:"fork"`
	SideChat       *forkAction         `json:"side_chat"`
	DropSideChat   string              `json:"drop_side_chat"`
	Task           *taskAction         `json:"task"`
	BackgroundCall string              `json:"background_call"` // call id the host moves to the background
	WaitQuiet      bool                `json:"wait_quiet"`
	On             string              `json:"on"` // run the action on the session a fork or side_chat action named, instead of the case's session
	Wait           json.RawMessage     `json:"wait"`
	SleepMS        int64               `json:"sleep_ms"`
	Close          bool                `json:"close"`
	As             string              `json:"as"`
	Error          string              `json:"error"` // the action must fail with an error containing this text
	Async          bool                `json:"async"` // runs on its own goroutine, so a later action can meet it half done
}

// forkAction forks the session, or opens its side chat Name, and names the result As for later actions and expected files.
type forkAction struct {
	Name  string    `json:"name"`
	As    string    `json:"as"`
	At    string    `json:"at"`
	Tools *[]string `json:"tools"` // absent allows every tool
}

// peer is a session a fork or side_chat action created, with its own recorder and expected files.
type peer struct {
	name   string
	sess   *kernel.Session
	rec    *recorder
	seeded int // lines its file held before it opened, as after a dropped side chat
	closed bool
}

type switchAction struct {
	Provider string `json:"provider"`
	Model    string `json:"model"` // overrides the provider's model
}

type sendAction struct {
	Text             string            `json:"text"`
	Mode             string            `json:"mode"`
	Origin           *message.Origin   `json:"origin"`
	NoWake           bool              `json:"no_wake"`
	NoCommandParsing bool              `json:"no_command_parsing"`
	Metadata         map[string]string `json:"metadata"`
}

type commandAction struct {
	Name string `json:"name"`
	Args string `json:"args"`
}

type interruptAction struct {
	Reason string `json:"reason"`
	Drop   bool   `json:"drop"`
}

type compactAction struct {
	Instructions string `json:"instructions"`
	From         string `json:"from"`  // turn id
	UpTo         string `json:"up_to"` // turn id
}

// ledgerAction reads a turn's calls, or prices them within timeout_ms of virtual time.
type ledgerAction struct {
	Turn      string `json:"turn"`
	Cost      bool   `json:"cost"`
	Usage     bool   `json:"usage"` // the snapshot's usage totals instead of a turn
	TimeoutMS int64  `json:"timeout_ms"`
}

type waitAction struct {
	Event   string         `json:"event"`
	Count   int            `json:"count"`
	Where   map[string]any `json:"where"`
	Receipt string         `json:"receipt"`
}

// LoopResult is everything a case produced, compared against its expected files.
type LoopResult struct {
	Events   []json.RawMessage
	Store    []json.RawMessage
	Requests []json.RawMessage // wire bodies for SSE responses, neutral requests for event responses
	Headers  []json.RawMessage // per wire request, the headers named by agent.request_headers
	Hooks    []json.RawMessage // the input of every scripted hook call, when the case has hooks
	Ledger   []json.RawMessage // what each ledger action returned
	Labels   []json.RawMessage // what each labels and find_sessions action read
	Wire     bool
	Seeded   int // lines the case's session file already held, which no event of this run announces
	Peers    []PeerResult
	Calls    []json.RawMessage // the case session's calls file, when the case compares it
	Tasks    []json.RawMessage // what each task action read

	sent []json.RawMessage // every model request in neutral form, for the invariants; never compared
}

// PeerResult is what a forked session or side chat of a case produced, compared against expected.<name>.events.jsonl and expected.<name>.store.jsonl.
type PeerResult struct {
	Name   string
	Events []json.RawMessage
	Store  []json.RawMessage
}

// RunLoop runs a case inside a synctest bubble, so every timer is virtual and every action happens at a quiescent point.
// ExtensionBuilder turns an extension name and its options in a case into an instance; the extension test that drives the case supplies it.
type ExtensionBuilder func(name string, options json.RawMessage) (kernel.Extension, error)

// ModelExtensionBuilder is ExtensionBuilder with the case's providers, for an extension whose options resolve a
// model name to one of them, such as ext/subagent. The map is keyed by the provider name in the case's script.
type ModelExtensionBuilder func(name string, options json.RawMessage, models map[string]provider.ModelRef) (kernel.Extension, error)

// ErrExtensionMissing from a builder skips the case, which names an extension that was deleted from this module.
var ErrExtensionMissing = errors.New("conformance: extension not in this module")

func RunLoop(t *testing.T, dir string) LoopResult {
	t.Helper()
	return RunLoopWith(t, dir, nil)
}

// RunLoopWith runs a case that names extensions, building them with exts.
func RunLoopWith(t *testing.T, dir string, exts ExtensionBuilder) LoopResult {
	t.Helper()
	var models ModelExtensionBuilder
	if exts != nil {
		models = func(name string, options json.RawMessage, _ map[string]provider.ModelRef) (kernel.Extension, error) {
			return exts(name, options)
		}
	}
	return runLoopWith(t, dir, models)
}

// RunLoopWithModels is RunLoopWith for an extension that resolves model names to the case's providers.
func RunLoopWithModels(t *testing.T, dir string, exts ModelExtensionBuilder) LoopResult {
	t.Helper()
	return runLoopWith(t, dir, exts)
}

func runLoopWith(t *testing.T, dir string, exts ModelExtensionBuilder) LoopResult {
	t.Helper()
	var res LoopResult
	var skipped error
	synctest.Test(t, func(t *testing.T) {
		r, err := runLoop(t, dir, exts)
		if errors.Is(err, ErrExtensionMissing) || errors.Is(err, errUnsupported) {
			skipped = err
			return
		}
		if err != nil {
			if err := record(dir, "actual.fail", err.Error()); err != nil {
				t.Error(err)
			}
			t.Fatal(err)
		}
		res = r
	})
	if skipped != nil {
		if err := record(dir, "actual.skip", skipped.Error()); err != nil {
			t.Error(err)
		}
		t.Skip(skipped) // outside the bubble, so the caller's test is the one skipped
	}
	return res
}

type loopRun struct {
	t        *testing.T
	dir      string
	script   LoopScript
	rec      *recorder
	sess     *kernel.Session
	ag       *kernel.Agent
	runs     *runCounter
	named    map[string]*kernel.Receipt
	pending  []*kernel.Receipt // command receipts, which must settle before the session counts as idle
	closed   bool
	models   map[string]provider.ModelRef
	log      *requestLog
	sent     *requestLog                     // neutral requests, whatever the response kind
	tools    map[string]*kernel.Registration // session-scope tools, by name
	sections map[string]*kernel.Registration // session-scope sections, by name
	hookLog  *requestLog
	ledger   []json.RawMessage
	labels   []json.RawMessage
	exts     ModelExtensionBuilder
	peers    []*peer
	store    store.SessionStore
	scripts  *taskScripts
	taskLog  []json.RawMessage
	async    sync.WaitGroup
	asyncErr chan error
}

func runLoop(t *testing.T, dir string, exts ModelExtensionBuilder) (LoopResult, error) {
	var sc LoopScript
	if err := readJSON(filepath.Join(dir, "script.json"), &sc); err != nil {
		return LoopResult{}, err
	}
	if !supports(sc.SpecVersion) {
		return LoopResult{}, fmt.Errorf("%w: the case is for spec %q", errUnsupported, sc.SpecVersion)
	}
	if len(sc.Providers) == 0 {
		return LoopResult{}, errors.New("a case needs a provider")
	}
	run := &loopRun{t: t, dir: dir, script: sc, exts: exts, rec: newRecorder(), runs: &runCounter{n: map[string]int{}}, named: map[string]*kernel.Receipt{},
		models: map[string]provider.ModelRef{}, log: &requestLog{}, sent: &requestLog{}, tools: map[string]*kernel.Registration{}, sections: map[string]*kernel.Registration{}, hookLog: &requestLog{}, scripts: &taskScripts{}}
	var wire bool
	for i, lp := range sc.Providers {
		prov, w, err := run.provider(lp)
		if err != nil {
			return LoopResult{}, err
		}
		if i > 0 && w != wire {
			return LoopResult{}, errors.New("every provider of a case answers with SSE, or every one with events")
		}
		wire = w
		run.models[lp.Name] = provider.ModelRef{Provider: prov, Model: lp.Model, Params: lp.Params}
	}
	st := store.NewMemory()
	seeded := 0
	if sc.Resume != nil {
		lines, err := readLines(filepath.Join(dir, sc.Resume.File))
		if err != nil {
			return LoopResult{}, err
		}
		if err := st.Append(context.Background(), sc.Resume.SessionID, lines); err != nil {
			return LoopResult{}, err
		}
		seeded = len(lines)
		if sc.Resume.Calls != "" {
			raw, err := os.ReadFile(filepath.Join(dir, sc.Resume.Calls))
			if err != nil {
				return LoopResult{}, err
			}
			var recs []json.RawMessage
			for _, l := range bytes.Split(raw, []byte("\n")) {
				if len(bytes.TrimSpace(l)) > 0 {
					recs = append(recs, json.RawMessage(l))
				}
			}
			if err := st.(store.CallStore).AppendCalls(context.Background(), sc.Resume.SessionID, recs); err != nil {
				return LoopResult{}, err
			}
		}
		if sc.Resume.Labels != "" {
			raw, err := os.ReadFile(filepath.Join(dir, sc.Resume.Labels))
			if err != nil {
				return LoopResult{}, err
			}
			var rec store.LabelRecord
			if err := json.Unmarshal(raw, &rec); err != nil {
				return LoopResult{}, err
			}
			if err := st.(store.LabelStore).SaveLabels(context.Background(), rec); err != nil {
				return LoopResult{}, err
			}
		}
	}
	opts, err := run.agentOptions(run.models[sc.Providers[0].Name], st)
	if err != nil {
		return LoopResult{}, err
	}
	ag, err := kernel.New(opts...)
	if err != nil {
		return LoopResult{}, err
	}
	run.ag = ag
	run.store = st
	sopts := []kernel.SessionOption{kernel.WithSessionFunc(run.rec.handle)}
	if m := sc.Session.InputMode; m != "" {
		sopts = append(sopts, kernel.SessionInputMode(kernel.InputMode(m)))
	}
	if sc.Session.Instructions != nil {
		sopts = append(sopts, kernel.SessionInstructions(*sc.Session.Instructions))
	}
	if sc.Session.Labels != nil {
		sopts = append(sopts, kernel.WithLabels(sc.Session.Labels))
	}
	if len(sc.Session.ApprovalRules) > 0 {
		rules, err := loopApprovalRules(sc.Session.ApprovalRules)
		if err != nil {
			return LoopResult{}, err
		}
		sopts = append(sopts, kernel.SessionApprovalRules(rules...))
	}
	if len(sc.Session.Tools) > 0 || len(sc.Session.Hooks) > 0 {
		sopts = append(sopts, kernel.SessionUse(kernel.ExtensionFunc("script", func(reg kernel.Registrar) error {
			for _, lt := range sc.Session.Tools {
				h, err := reg.Tool(&scriptTool{def: lt, runs: run.runs})
				if err != nil {
					return err
				}
				run.tools[lt.Name] = h
			}
			for _, lh := range sc.Session.Hooks {
				if err := run.addHook(reg, lh); err != nil {
					return err
				}
			}
			return nil
		})))
	}
	var sess *kernel.Session
	if sc.Resume != nil {
		sess, err = ag.Resume(t.Context(), sc.Resume.SessionID, sopts...)
	} else {
		sess, err = ag.NewSession(t.Context(), sopts...)
	}
	if err != nil {
		return LoopResult{}, err
	}
	run.sess = sess
	run.asyncErr = make(chan error, len(sc.Actions))
	for i, raw := range sc.Actions {
		synctest.Wait()
		var probe struct {
			Async bool `json:"async"`
		}
		_ = json.Unmarshal(raw, &probe)
		if probe.Async {
			run.async.Go(func() {
				if err := run.do(raw); err != nil {
					run.asyncErr <- fmt.Errorf("action %d %s: %w", i+1, raw, err)
				}
			})
			continue
		}
		if err := run.do(raw); err != nil {
			return LoopResult{}, fmt.Errorf("action %d %s: %w", i+1, raw, err)
		}
	}
	synctest.Wait()
	run.async.Wait()
	close(run.asyncErr)
	for err := range run.asyncErr {
		return LoopResult{}, err
	}
	if !run.closed {
		if err := sess.Close(t.Context()); err != nil {
			return LoopResult{}, err
		}
	}
	if err := ag.Close(t.Context()); err != nil {
		return LoopResult{}, err
	}
	synctest.Wait()
	res := LoopResult{Wire: wire, Seeded: seeded, Ledger: run.ledger, Labels: run.labels, Tasks: run.taskLog}
	for _, ev := range run.rec.all() {
		b, err := json.Marshal(ev)
		if err != nil {
			return LoopResult{}, err
		}
		res.Events = append(res.Events, b)
	}
	lines, err := st.Load(context.Background(), sess.Info().SessionID)
	if err != nil {
		return LoopResult{}, err
	}
	for _, m := range lines {
		b, err := json.Marshal(m)
		if err != nil {
			return LoopResult{}, err
		}
		res.Store = append(res.Store, b)
	}
	if sc.ExpectCalls {
		if res.Calls, err = st.(store.CallStore).LoadCalls(context.Background(), sess.Info().SessionID); err != nil {
			return LoopResult{}, err
		}
	}
	res.Requests = run.log.all()
	res.sent = run.sent.all()
	res.Headers = run.log.allHeaders()
	res.Hooks = run.hookLog.all()
	peerLines := make([][]message.Message, len(run.peers))
	for i, p := range run.peers {
		if peerLines[i], err = st.Load(context.Background(), p.sess.Info().SessionID); err != nil {
			return LoopResult{}, err
		}
		pr := PeerResult{Name: p.name}
		for _, ev := range p.rec.all() {
			b, err := json.Marshal(ev)
			if err != nil {
				return LoopResult{}, err
			}
			pr.Events = append(pr.Events, b)
		}
		for _, m := range peerLines[i] {
			b, err := json.Marshal(m)
			if err != nil {
				return LoopResult{}, err
			}
			pr.Store = append(pr.Store, b)
		}
		res.Peers = append(res.Peers, pr)
	}
	runs := splitRuns(run.runs.snapshot(), append([][]message.Message{lines}, peerLines...))
	if err := CheckInvariants(res, lines, runs[0]); err != nil {
		return LoopResult{}, err
	}
	for i, p := range run.peers {
		mine := peerLines[i]
		for _, q := range run.peers[i+1:] { // a side chat reopened after a drop: the later lines and the dropped mark are not this peer's
			if q.sess.Info().SessionID == p.sess.Info().SessionID {
				end := q.seeded
				if end > 0 && mine[end-1].Boundary != nil && mine[end-1].Boundary.Reason == "dropped" {
					end--
				}
				mine = mine[:end]
				break
			}
		}
		if n := len(mine); n > 0 && mine[n-1].Boundary != nil && mine[n-1].Boundary.Reason == "dropped" {
			mine = mine[:n-1] // written after the side chat closed, so no event of its own reports it
		}
		pres := LoopResult{Events: res.Peers[i].Events, Seeded: p.seeded}
		if err := CheckInvariants(pres, mine, runs[i+1]); err != nil {
			return LoopResult{}, fmt.Errorf("session %s: %w", p.name, err)
		}
	}
	return res, nil
}

// splitRuns gives each session the tool runs its own lines committed, copies aside; a run no session committed stays with the first, whose check reports it.
func splitRuns(runs map[string]int, sessions [][]message.Message) []map[string]int {
	out := make([]map[string]int, len(sessions))
	for i := range out {
		out[i] = map[string]int{}
	}
	for id, n := range runs {
		owner := 0
		for i, lines := range sessions {
			if slices.ContainsFunc(lines, func(m message.Message) bool {
				return m.CopyOf == "" && slices.ContainsFunc(m.ToolCalls(), func(c message.ToolCallBlock) bool { return c.ID == id })
			}) {
				owner = i
			}
		}
		out[owner][id] = n
	}
	return out
}

func (r *loopRun) path(p string) string { return filepath.Join(r.dir, p) }

// requestLog keeps every model request of a case in the order they were sent, across providers.
type requestLog struct {
	mu      sync.Mutex
	bodies  []json.RawMessage
	headers []json.RawMessage // per request, the headers the case asks to compare; nil when it names none
}

func (l *requestLog) add(b []byte) {
	l.mu.Lock()
	l.bodies = append(l.bodies, append(json.RawMessage(nil), b...))
	l.mu.Unlock()
}

func (l *requestLog) addHeaders(b []byte) {
	l.mu.Lock()
	l.headers = append(l.headers, b)
	l.mu.Unlock()
}

func (l *requestLog) allHeaders() []json.RawMessage {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]json.RawMessage(nil), l.headers...)
}

func (l *requestLog) all() []json.RawMessage {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]json.RawMessage(nil), l.bodies...)
}

// loggingDoer records a wire body before the fixture answers it.
type loggingDoer struct {
	next    provider.Doer
	log     *requestLog
	headers []string // request headers the case compares
}

func (d loggingDoer) Do(req *http.Request) (*http.Response, error) {
	if len(d.headers) > 0 {
		got := map[string]string{}
		for _, h := range d.headers {
			got[strings.ToLower(h)] = req.Header.Get(h)
		}
		b, _ := json.Marshal(got)
		d.log.addHeaders(b)
	}
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		d.log.add(b)
		req.Body = io.NopCloser(bytes.NewReader(b))
	}
	return d.next.Do(req)
}

// loggingProvider records the neutral request a provider receives: in log for a scripted provider, whose cases compare it, and in sent for every provider.
type loggingProvider struct {
	provider.Provider
	log  *requestLog
	sent *requestLog
}

func (p loggingProvider) Stream(ctx context.Context, req *provider.Request) iter.Seq2[provider.StreamEvent, error] {
	b, _ := json.Marshal(req)
	if p.log != nil {
		p.log.add(b)
	}
	p.sent.add(b)
	return p.Provider.Stream(ctx, req)
}

// countingProvider keeps an adapter's token counting visible through the logging wrapper, since the kernel finds it by type assertion.
type countingProvider struct {
	loggingProvider
	counter provider.TokenCounter
}

func (p countingProvider) CountTokens(ctx context.Context, req *provider.Request) (int64, error) {
	return p.counter.CountTokens(ctx, req)
}

// provider builds one model of the case: the real adapter over FixtureTransport for SSE responses, a ScriptProvider for event responses.
func (r *loopRun) provider(p LoopProvider) (provider.Provider, bool, error) {
	if len(p.Responses) == 0 {
		return nil, false, errors.New("the provider has no responses")
	}
	if p.Responses[0].SSE != "" {
		var responses []providertest.Response
		for _, lr := range p.Responses {
			if lr.SSE == "" {
				return nil, false, errors.New("mixing SSE and event responses in one provider")
			}
			body, err := os.ReadFile(r.path(lr.SSE))
			if err != nil {
				return nil, false, err
			}
			h := http.Header{"Content-Type": {"text/event-stream"}}
			for k, v := range lr.Headers {
				h.Set(k, v)
			}
			responses = append(responses, providertest.Response{Status: lr.Status, Header: h, Body: body, HoldAfter: lr.HoldAfterEvents, HoldFor: time.Duration(lr.HoldMS) * time.Millisecond})
		}
		f, ok := loopFactories[provider.API(p.API)]
		if !ok {
			return nil, false, fmt.Errorf("no adapter for %s", p.API)
		}
		real := f(loggingDoer{next: providertest.NewFixtureTransport(responses...), log: r.log, headers: slices.Sorted(maps.Keys(r.script.Agent.RequestHeaders))})
		if c, ok := real.(provider.TokenCounter); ok {
			return countingProvider{loggingProvider{Provider: real, sent: r.sent}, c}, true, nil
		}
		return loggingProvider{Provider: real, sent: r.sent}, true, nil
	}
	var scripts [][]providertest.Step
	for _, lr := range p.Responses {
		if lr.Events == "" {
			return nil, false, errors.New("mixing SSE and event responses in one provider")
		}
		steps, err := readSteps(r.path(lr.Events))
		if err != nil {
			return nil, false, err
		}
		if n := lr.HoldAfterEvents; n > 0 && n <= len(steps) {
			hold := providertest.Step{Hold: -1}
			if lr.HoldMS > 0 {
				hold.Hold = time.Duration(lr.HoldMS) * time.Millisecond
			}
			steps = append(steps[:n:n], append([]providertest.Step{hold}, steps[n:]...)...)
		}
		scripts = append(scripts, steps)
	}
	return loggingProvider{Provider: providertest.NewScriptProvider(provider.API(p.API), scripts...), log: r.log, sent: r.sent}, false, nil
}

// loopFactories builds the real adapter for SSE responses, at its default base URL so the recorded endpoint is stable.
var loopFactories = map[provider.API]func(provider.Doer) provider.Provider{
	provider.APIAnthropicMessages: func(d provider.Doer) provider.Provider {
		return anthropic.New(anthropic.Config{APIKey: "test-key", Doer: d})
	},
	provider.APIOpenAIChat: func(d provider.Doer) provider.Provider {
		return openaichat.New(openaichat.Config{APIKey: "test-key", Doer: d})
	},
	provider.APIOpenAIResponses: func(d provider.Doer) provider.Provider {
		return openairesponses.New(openairesponses.Config{APIKey: "test-key", Doer: d})
	},
}

// scriptResolver answers capability lookups from the script, so no case ever spends a fixture response on a models request.
type scriptResolver struct{ providers []LoopProvider }

func (s scriptResolver) ResolveCapabilities(_ context.Context, m provider.ModelInfo) (provider.Capabilities, error) {
	for _, p := range s.providers {
		if provider.API(p.API) != m.API || p.Model != m.Model {
			continue
		}
		if p.ResolveError != "" {
			return provider.Capabilities{}, errors.New(p.ResolveError)
		}
		if p.Resolved != nil {
			return *p.Resolved, nil
		}
	}
	return provider.Capabilities{}, nil
}

// readSteps reads neutral stream events, one per line; a line {"error": …} ends the stream with that error.
func readSteps(path string) ([]providertest.Step, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var steps []providertest.Step
	for _, line := range compare.ReadJSONL(b) {
		var probe struct {
			Error *provider.Error `json:"error"`
		}
		if json.Unmarshal(line, &probe) == nil && probe.Error != nil {
			steps = append(steps, providertest.Step{Err: probe.Error})
			continue
		}
		ev, err := decodeStreamEvent(line)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		steps = append(steps, providertest.Step{Event: ev})
	}
	return steps, nil
}

func decodeStreamEvent(line []byte) (provider.StreamEvent, error) {
	var aux struct {
		provider.StreamEvent
		Block json.RawMessage `json:"block"`
	}
	if err := json.Unmarshal(line, &aux); err != nil {
		return provider.StreamEvent{}, err
	}
	ev := aux.StreamEvent
	if len(aux.Block) > 0 {
		var c message.Content
		if err := json.Unmarshal(append(append([]byte("["), aux.Block...), ']'), &c); err != nil {
			return provider.StreamEvent{}, err
		}
		ev.Block = c[0]
	}
	return ev, nil
}

func (r *loopRun) agentOptions(model provider.ModelRef, st store.SessionStore) ([]kernel.Option, error) {
	a := r.script.Agent
	start := r.script.Clock.Start
	if start.IsZero() {
		start = time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	}
	step := time.Duration(r.script.Clock.StepMS) * time.Millisecond
	if step == 0 {
		step = time.Millisecond
	}
	l := a.Limits
	opts := []kernel.Option{
		kernel.WithModel(model),
		kernel.WithCapabilityResolver(scriptResolver{providers: r.script.Providers}),
		kernel.WithStore(st),
		kernel.WithClock(determ.NewStepClock(start, step)),
		kernel.WithIDGenerator(determ.NewSequentialIDs()),
		kernel.WithPanicOnInternalError(true),
		kernel.WithLimits(kernel.Limits{
			MaxSteps: l.MaxSteps, ToolTimeout: ms(l.ToolTimeoutMS), ToolCancelGrace: ms(l.ToolCancelGraceMS),
			MaxConsecutiveAutoTurns: l.MaxConsecutiveAutoTurns, MaxQueuedInputs: l.MaxQueuedInputs,
			MaxTruncationContinues: l.MaxTruncationContinues, ReadAhead: l.ReadAhead, MaxParallelTools: l.MaxParallelTools,
			HookGrace: ms(l.HookGraceMS), MaxStopContinuations: l.MaxStopContinuations, MaxAsyncHooks: l.MaxAsyncHooks,
			MaxRunningTasks: l.MaxRunningTasks,
		}),
		kernel.WithRetryPolicy(provider.RetryPolicy{MaxAttempts: a.Retry.MaxAttempts, BaseDelay: ms(a.Retry.BaseDelayMS), MaxDelay: ms(a.Retry.MaxDelayMS)}),
		kernel.WithAutoContinueTruncatedText(a.AutoContinueTruncatedText),
	}
	if a.Nickname != "" {
		opts = append(opts, kernel.WithNickname(a.Nickname))
	}
	if a.Description != "" {
		opts = append(opts, kernel.WithDescription(a.Description))
	}
	if a.Instructions != "" {
		opts = append(opts, kernel.WithInstructions(a.Instructions))
	}
	if a.InputMode != "" {
		opts = append(opts, kernel.WithInputMode(kernel.InputMode(a.InputMode)))
	}
	if a.StorePolicy != "" {
		opts = append(opts, kernel.WithStorePolicy(kernel.StorePolicy(a.StorePolicy)))
	}
	for _, lt := range a.Tools {
		opts = append(opts, kernel.WithTool(&scriptTool{def: lt, runs: r.runs}))
	}
	for _, h := range a.HostedTools {
		opts = append(opts, kernel.WithHostedTool(h))
	}
	for _, lc := range a.Commands {
		opts = append(opts, kernel.WithCommand(scriptCommand(lc)))
	}
	for _, lk := range a.TaskKinds {
		opts = append(opts, kernel.WithTaskKind(lk.kind(r.scripts)))
		if lk.StartTool != nil {
			tool, err := lk.tool()
			if err != nil {
				return nil, err
			}
			opts = append(opts, kernel.WithTool(tool))
		}
	}
	for _, c := range a.Capabilities {
		opts = append(opts, kernel.WithCapabilities(c.API, c.ModelGlob, c.Caps))
	}
	if len(a.ApprovalRules) > 0 {
		rules, err := loopApprovalRules(a.ApprovalRules)
		if err != nil {
			return nil, err
		}
		opts = append(opts, kernel.WithApprovalRules(rules...))
	}
	for _, ls := range a.Sections {
		ps, err := scriptSection(ls)
		if err != nil {
			return nil, err
		}
		opts = append(opts, kernel.WithPromptSection(ps))
	}
	for _, lc := range a.ContextProviders {
		p, err := scriptProvider(lc)
		if err != nil {
			return nil, err
		}
		opts = append(opts, kernel.WithContextProvider(p))
	}
	if len(a.RequestHeaders) > 0 {
		opts = append(opts, kernel.WithRequestHeaders(func(ri kernel.RequestInfo) map[string]string {
			fields := map[string]string{"root_session_id": ri.RootSessionID, "session_id": ri.SessionID, "turn_id": ri.TurnID, "bucket": ri.Bucket, "api": ri.API, "model": ri.Model}
			out := map[string]string{}
			for h, f := range a.RequestHeaders {
				out[h] = fields[f]
			}
			return out
		}))
	}
	if len(a.CaptureHeaders) > 0 {
		opts = append(opts, kernel.WithCaptureHeaders(a.CaptureHeaders...))
	}
	if p := a.ContextPolicy; p != nil {
		opts = append(opts, kernel.WithContextPolicy(kernel.ContextPolicy{MaxContextTokens: p.MaxContextTokens, AutoCompactRatio: p.AutoCompactRatio, TargetRatio: p.TargetRatio,
			MinSummaryTokens: p.MinSummaryTokens, MaxSummaryTokens: p.MaxSummaryTokens, PinTokens: p.PinTokens, PinTotalTokens: p.PinTotalTokens,
			MaxAutoCompactsPerTurn: p.MaxAutoCompactsPerTurn, MaxConsecutiveFailures: p.MaxConsecutiveFailures, Disabled: p.Disabled}))
	}
	if c := a.Compactor; c != nil {
		opts = append(opts, kernel.Use(kernel.ExtensionFunc("script-compactor", func(reg kernel.Registrar) error {
			_, err := reg.Compactor(scriptCompactor{def: *c})
			return err
		})))
	}
	for _, name := range slices.Sorted(maps.Keys(a.Extensions)) {
		if r.exts == nil {
			return nil, fmt.Errorf("the case names extension %q, but no extension test drives it", name)
		}
		ext, err := r.exts(name, a.Extensions[name], r.models)
		if errors.Is(err, ErrExtensionMissing) {
			return nil, fmt.Errorf("%w: the case needs %q", ErrExtensionMissing, name)
		}
		if err != nil {
			return nil, err
		}
		opts = append(opts, kernel.Use(ext))
	}
	if a.CostSource != nil {
		opts = append(opts, kernel.WithCostSource(&scriptCosts{def: *a.CostSource}))
	}
	if len(a.Hooks) > 0 {
		opts = append(opts, kernel.Use(kernel.ExtensionFunc("script", func(reg kernel.Registrar) error {
			for _, lh := range a.Hooks {
				if err := r.addHook(reg, lh); err != nil {
					return err
				}
			}
			return nil
		})))
	}
	return opts, nil
}

var slots = map[string]kernel.Slot{"capabilities": kernel.SlotCapabilities, "catalog": kernel.SlotCatalog, "environment": kernel.SlotEnvironment}

func scriptSection(ls LoopSection) (kernel.PromptSection, error) {
	slot, ok := slots[ls.Slot]
	if !ok {
		return kernel.PromptSection{}, fmt.Errorf("section %s: unknown slot %q", ls.Name, ls.Slot)
	}
	text := ls.Text
	return kernel.PromptSection{Name: ls.Name, Slot: slot, Order: ls.Order, AnnounceChanges: ls.AnnounceChanges,
		Render: func(context.Context, kernel.RenderContext) (string, error) { return text, nil }}, nil
}

func scriptProvider(lc LoopContext) (kernel.ContextProvider, error) {
	when := map[string]kernel.ProvideAt{"turn_start": kernel.AtTurnStart, "every_step": kernel.AtEveryStep}[lc.When]
	if when == 0 {
		return kernel.ContextProvider{}, fmt.Errorf("context provider %s: unknown when %q", lc.Name, lc.When)
	}
	var mu sync.Mutex
	n := 0
	return kernel.ContextProvider{Name: lc.Name, When: when, Provide: func(context.Context, kernel.ProvideContext) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if len(lc.Texts) == 0 {
			return "", nil
		}
		t := lc.Texts[min(n, len(lc.Texts)-1)]
		n++
		return t, nil
	}}, nil
}

// kernelPoints finds a kernel hook point by name.
func kernelPoint(name string) (kernel.AnyHookPoint, bool) {
	for _, p := range kernel.HookPoints() {
		if p.Name() == name {
			return p, true
		}
	}
	return nil, false
}

// addHook registers a scripted hook through its point's JSON form, logging every input it receives.
func (r *loopRun) addHook(reg kernel.Registrar, lh LoopHook) error {
	point, ok := kernelPoint(lh.Point)
	if !ok {
		return fmt.Errorf("hook %s: unknown point %q", lh.Name, lh.Point)
	}
	opts := []kernel.HookOption{kernel.Priority(lh.Priority)}
	m, err := kernel.ParseMatcher(lh.Match)
	if err != nil {
		return err
	}
	if m != nil {
		opts = append(opts, kernel.Match(m))
	}
	if lh.TimeoutMS != nil {
		opts = append(opts, kernel.Timeout(ms(*lh.TimeoutMS)))
	}
	switch lh.Fail {
	case "open":
		opts = append(opts, kernel.Fail(kernel.FailOpen))
	case "closed":
		opts = append(opts, kernel.Fail(kernel.FailClosed))
	}
	if w := lh.WakeLimit; w != nil {
		opts = append(opts, kernel.WakeLimit(w.N, ms(w.PerMS)))
	}
	var mu sync.Mutex
	n := 0
	next := func(in json.RawMessage) LoopHookCall {
		mu.Lock()
		defer mu.Unlock()
		entry, _ := json.Marshal(map[string]any{"point": lh.Point, "name": lh.Name, "input": in})
		r.hookLog.add(entry)
		for _, c := range lh.Cases {
			if inputHas(in, c.When) {
				return c.Call
			}
		}
		if len(lh.Calls) == 0 {
			return LoopHookCall{}
		}
		c := lh.Calls[min(n, len(lh.Calls)-1)]
		n++
		return c
	}
	if lh.Async {
		_, err := kernel.OnJSONAsync(reg, point, lh.Name, func(ctx context.Context, in json.RawMessage) (kernel.Injection, error) {
			out, err := runHookCall(ctx, next(in))
			var inj kernel.Injection
			if err == nil && len(out) > 0 {
				err = json.Unmarshal(out, &inj)
			}
			return inj, err
		}, opts...)
		return err
	}
	_, err = kernel.OnJSON(reg, point, lh.Name, func(ctx context.Context, in json.RawMessage) (json.RawMessage, error) {
		return runHookCall(ctx, next(in))
	}, opts...)
	return err
}

// inputHas reports whether every dot path of when holds its value in the input, compared as printed.
func inputHas(in json.RawMessage, when map[string]any) bool {
	var v any
	if json.Unmarshal(in, &v) != nil {
		return false
	}
	for path, want := range when {
		cur := v
		for _, seg := range strings.Split(path, ".") {
			switch x := cur.(type) {
			case map[string]any:
				cur = x[seg]
			case []any:
				i, err := strconv.Atoi(seg)
				if err != nil || i < 0 || i >= len(x) {
					return false
				}
				cur = x[i]
			default:
				return false
			}
		}
		if fmt.Sprint(cur) != fmt.Sprint(want) {
			return false
		}
	}
	return true
}

func runHookCall(ctx context.Context, c LoopHookCall) (json.RawMessage, error) {
	wait := ctx.Done()
	if c.IgnoreCtx {
		wait = nil
	}
	if c.Hold {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if d := ms(c.SleepMS); d > 0 {
		select {
		case <-time.After(d):
		case <-wait:
			return nil, ctx.Err()
		}
	}
	switch {
	case c.Panic != "":
		panic(c.Panic)
	case c.Error != "":
		return nil, errors.New(c.Error)
	}
	return c.Output, nil
}

func ms(n int64) time.Duration { return time.Duration(n) * time.Millisecond }

// runCounter counts executions per call id, for the at-most-once check (I2).
type runCounter struct {
	mu sync.Mutex
	n  map[string]int
}

func (c *runCounter) add(id string) {
	c.mu.Lock()
	c.n[id]++
	c.mu.Unlock()
}

func (c *runCounter) snapshot() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int, len(c.n))
	for k, v := range c.n {
		out[k] = v
	}
	return out
}

type scriptTool struct {
	def  LoopTool
	runs *runCounter
}

func (s *scriptTool) Spec() kernel.ToolSpec {
	return kernel.ToolSpec{Name: s.def.Name, Description: s.def.Description, InputSchema: s.def.InputSchema, Concurrency: kernel.Concurrency(s.def.Concurrency), Timeout: ms(s.def.TimeoutMS), Background: s.def.Background}
}

func (s *scriptTool) Call(ctx context.Context, tc *kernel.ToolContext, input json.RawMessage) (kernel.ToolResult, error) {
	if s.def.Strict {
		if err := strictKeys(s.def.InputSchema, input); err != nil {
			return kernel.ToolResult{}, err
		}
	}
	if len(tc.Path()) == 0 { // a tool_exec run has its own call id, which no line commits; the task records it instead
		s.runs.add(tc.CallID())
	}
	b := s.def.Behavior
	for _, p := range b.Progress {
		tc.Progress(p, nil)
	}
	if d := ms(b.SleepMS); d > 0 {
		wait := ctx.Done()
		if b.IgnoreCtx {
			wait = nil
		}
		select {
		case <-time.After(d):
		case <-wait:
			return kernel.ToolResult{}, ctx.Err()
		}
	}
	switch b.Type {
	case "echo", "":
		res := kernel.TextResult(string(input))
		res.Pin = b.Pin
		return res, nil
	case "fixed":
		res := kernel.TextResult(b.Text)
		res.IsError, res.Pin = b.IsError, b.Pin
		return res, nil
	case "error":
		return kernel.ToolResult{}, errors.New(b.Text)
	case "panic":
		panic(b.Text)
	case "task_stop", "task_restart":
		var err error
		if b.Type == "task_stop" {
			err = tc.Session().Tasks().Stop(ctx, b.Task, b.Text) // with the call's context, so the kernel knows the model asked
			// like task_stop, the result waits for the stopped attempt to end, so the line is ordered after it
			for err == nil {
				if info, _ := tc.Session().Tasks().Get(b.Task); info.Status.Terminal() {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
		} else {
			err = tc.Session().Tasks().Restart(ctx, b.Task, b.TaskInput)
		}
		var de *kernel.TaskDeniedError
		switch {
		case errors.As(err, &de):
			return kernel.ErrorResult("%s", de.Text), nil
		case err != nil:
			return kernel.ErrorResult("%s", err.Error()), nil
		}
		return kernel.TextResult(b.Type + " " + b.Task), nil
	}
	return kernel.ToolResult{}, fmt.Errorf("unknown tool behavior %q", b.Type)
}

// strictKeys rejects a top-level key the schema does not name exactly, the way NewTool's decoding does.
func strictKeys(schema, input json.RawMessage) error {
	var sc struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	_ = json.Unmarshal(schema, &sc)
	var in map[string]json.RawMessage
	if err := json.Unmarshal(input, &in); err != nil {
		return &kernel.InputError{Detail: "the arguments are not valid JSON"}
	}
	for _, k := range slices.Sorted(maps.Keys(in)) {
		if _, ok := sc.Properties[k]; !ok {
			return &kernel.InputError{Detail: fmt.Sprintf("unknown field %q", k)}
		}
	}
	return nil
}

func scriptCommand(lc LoopCommand) kernel.Command {
	if lc.Template != "" {
		var opts []kernel.PromptOption
		if len(lc.ArgNames) > 0 {
			opts = append(opts, kernel.WithArgs(lc.ArgNames...))
		}
		if lc.Busy != "" {
			opts = append(opts, kernel.PromptBusy(kernel.BusyPolicy(lc.Busy)))
		}
		c := kernel.PromptCommand(lc.Name, lc.Description, lc.Template, opts...)
		c.Aliases = lc.Aliases
		return c
	}
	return kernel.Command{
		Name: lc.Name, Aliases: lc.Aliases, Description: lc.Description, Busy: kernel.BusyPolicy(lc.Busy),
		Run: func(ctx context.Context, cc *kernel.CommandContext) (kernel.CommandResult, error) {
			if d := ms(lc.SleepMS); d > 0 {
				select {
				case <-time.After(d):
				case <-ctx.Done():
					return kernel.CommandResult{}, ctx.Err()
				}
			}
			if lc.Error != "" {
				return kernel.CommandResult{}, errors.New(lc.Error)
			}
			res := kernel.CommandResult{Text: lc.Text, Pin: lc.Pin}
			if lc.Prompt != "" {
				in := kernel.Text(strings.ReplaceAll(lc.Prompt, "$ARGUMENTS", cc.Args()))
				res.Prompt = &in
			}
			return res, nil
		},
	}
}

func (r *loopRun) do(raw json.RawMessage) error {
	var a loopAction
	if err := json.Unmarshal(raw, &a); err != nil {
		return err
	}
	if a.On != "" {
		p := r.peer(a.On)
		if p == nil {
			return fmt.Errorf("no session named %q", a.On)
		}
		sess, rec := r.sess, r.rec
		r.sess, r.rec = p.sess, p.rec
		defer func() { r.sess, r.rec = sess, rec }()
		if a.Close {
			p.closed = true
			return p.sess.Close(r.t.Context())
		}
	}
	switch {
	case a.Send != nil:
		var opts []kernel.SendOption
		if a.Send.Mode != "" {
			opts = append(opts, kernel.WithMode(kernel.InputMode(a.Send.Mode)))
		}
		if a.Send.NoWake {
			opts = append(opts, kernel.NoWake())
		}
		if a.Send.NoCommandParsing {
			opts = append(opts, kernel.NoCommandParsing())
		}
		in := kernel.Input{Text: a.Send.Text, Metadata: a.Send.Metadata}
		if a.Send.Origin != nil {
			in.Origin = *a.Send.Origin
		}
		rc, err := r.sess.Send(in, opts...)
		if err != nil {
			return err
		}
		r.remember(a.As, rc, strings.HasPrefix(rc.InputID(), "cmd_"))
	case a.Command != nil:
		rc, err := r.sess.Command(a.Command.Name, a.Command.Args)
		if err != nil {
			return err
		}
		r.remember(a.As, rc, true)
	case a.Interrupt != nil:
		if a.Interrupt.Drop {
			r.sess.InterruptAndDrop(a.Interrupt.Reason)
		} else {
			r.sess.Interrupt(a.Interrupt.Reason)
		}
	case a.Withdraw != "" || a.InterruptWith != "":
		name, act := a.Withdraw, r.sess.Withdraw
		if name == "" {
			name, act = a.InterruptWith, r.sess.InterruptWith
		}
		rc, ok := r.named[name]
		if !ok {
			return fmt.Errorf("no input named %q", name)
		}
		return r.expect(a.Error, act(rc.InputID()))
	case a.SwitchModel != nil:
		ref, ok := r.models[a.SwitchModel.Provider]
		if !ok {
			return fmt.Errorf("no provider %q", a.SwitchModel.Provider)
		}
		if a.SwitchModel.Model != "" {
			ref.Model = a.SwitchModel.Model
		}
		return r.expect(a.Error, r.sess.SwitchModel(r.t.Context(), ref))
	case a.AddTool != nil:
		h, err := r.sess.Registrar().Tool(&scriptTool{def: *a.AddTool, runs: r.runs})
		if err != nil {
			return err
		}
		r.tools[a.AddTool.Name] = h
	case a.RemoveTool != "":
		h, ok := r.tools[a.RemoveTool]
		if !ok {
			return fmt.Errorf("no session tool %q", a.RemoveTool)
		}
		h.Remove()
		delete(r.tools, a.RemoveTool)
	case a.AddHosted != nil:
		h, err := r.sess.Registrar().HostedTool(*a.AddHosted)
		if err != nil {
			return err
		}
		r.tools[a.AddHosted.Name] = h
	case a.RemoveHosted != "":
		h, ok := r.tools[a.RemoveHosted]
		if !ok {
			return fmt.Errorf("no session hosted tool %q", a.RemoveHosted)
		}
		h.Remove()
		delete(r.tools, a.RemoveHosted)
	case a.AddSection != nil:
		ps, err := scriptSection(*a.AddSection)
		if err != nil {
			return err
		}
		h, err := r.sess.Registrar().PromptSection(ps)
		if err != nil {
			return err
		}
		r.sections[a.AddSection.Name] = h
	case a.RemoveSection != "":
		h, ok := r.sections[a.RemoveSection]
		if !ok {
			return fmt.Errorf("no session section %q", a.RemoveSection)
		}
		h.Remove()
		delete(r.sections, a.RemoveSection)
	case a.SetLabels != nil:
		return r.expect(a.Error, r.sess.SetLabels(r.t.Context(), a.SetLabels))
	case a.Labels:
		return r.record(map[string]any{"session_id": r.sess.Info().SessionID, "labels": r.sess.Info().Labels})
	case a.FindSessions != nil:
		found, err := r.ag.FindSessions(r.t.Context(), *a.FindSessions)
		if err != nil {
			return err
		}
		out := []map[string]any{}
		for _, f := range found {
			out = append(out, map[string]any{"session_id": f.SessionID, "labels": f.Labels})
		}
		return r.record(map[string]any{"found": out})
	case a.Ledger != nil:
		return r.readLedger(*a.Ledger)
	case a.Compact != nil:
		var opts []kernel.CompactOption
		if a.Compact.From != "" {
			opts = append(opts, kernel.CompactFrom(a.Compact.From))
		}
		if a.Compact.UpTo != "" {
			opts = append(opts, kernel.CompactUpTo(a.Compact.UpTo))
		}
		_, err := r.sess.Compact(r.t.Context(), a.Compact.Instructions, opts...)
		return r.expect(a.Error, err)
	case a.Clear:
		return r.expect(a.Error, r.sess.Clear(r.t.Context()))
	case a.Unpin != "":
		return r.expect(a.Error, r.sess.Unpin(r.t.Context(), a.Unpin))
	case a.Rewind != "":
		_, err := r.sess.Rewind(r.t.Context(), a.Rewind)
		return r.expect(a.Error, err)
	case a.Fork != nil || a.SideChat != nil:
		return r.expect(a.Error, r.fork(a))
	case a.DropSideChat != "":
		return r.expect(a.Error, r.sess.DropSideChat(r.t.Context(), a.DropSideChat))
	case a.Task != nil:
		return r.expect(a.Error, r.task(*a.Task))
	case a.BackgroundCall != "":
		_, err := r.sess.BackgroundCall(r.t.Context(), a.BackgroundCall)
		return r.expect(a.Error, err)
	case a.WaitQuiet:
		return r.expect(a.Error, r.waitQuiet())
	case a.Resume != "" && a.As != "":
		return r.expect(a.Error, r.resumePeer(a.Resume, a.As))
	case a.Resume != "":
		s, err := r.ag.Resume(r.t.Context(), a.Resume)
		if err == nil {
			_ = s.Close(r.t.Context())
		}
		return r.expect(a.Error, err)
	case a.Wait != nil:
		return r.wait(a.Wait)
	case a.SleepMS > 0:
		time.Sleep(ms(a.SleepMS))
	case a.Close:
		r.closed = true
		return r.sess.Close(r.t.Context())
	default:
		return errors.New("unknown action")
	}
	return nil
}

func (r *loopRun) peer(name string) *peer {
	for _, p := range r.peers {
		if p.name == name {
			return p
		}
	}
	return nil
}

// fork runs a fork or side_chat action; a side chat that is already open keeps the peer that opened it.
func (r *loopRun) fork(a loopAction) error {
	fa := a.Fork
	if fa == nil {
		fa = a.SideChat
	}
	if fa.As == "" || r.peer(fa.As) != nil {
		return fmt.Errorf("a fork needs a new name in as, got %q", fa.As)
	}
	p := &peer{name: fa.As, rec: newRecorder()}
	opts := []kernel.ForkOption{kernel.ForkFunc(p.rec.handle)}
	if fa.At != "" {
		opts = append(opts, kernel.ForkAt(fa.At))
	}
	if fa.Tools != nil {
		opts = append(opts, kernel.ForkTools(*fa.Tools...))
	}
	var err error
	if a.Fork != nil {
		p.sess, err = r.sess.Fork(r.t.Context(), opts...)
	} else {
		if before, lerr := r.store.Load(r.t.Context(), r.sess.Info().SessionID+".side."+fa.Name); lerr == nil {
			p.seeded = len(before)
		}
		p.sess, err = r.sess.SideChat(r.t.Context(), fa.Name, opts...)
	}
	if err != nil {
		return err
	}
	for _, q := range r.peers {
		if q.sess == p.sess {
			return fmt.Errorf("side chat %q is already open as %q", fa.Name, q.name)
		}
	}
	r.peers = append(r.peers, p)
	return nil
}

// resumePeer resumes a stored session, such as one a command forked, and names it like a fork; its events start at the resume, so the lines already stored are left out of its invariants.
func (r *loopRun) resumePeer(id, as string) error {
	if r.peer(as) != nil {
		return fmt.Errorf("a resumed session needs a new name in as, got %q", as)
	}
	p := &peer{name: as, rec: newRecorder()}
	if before, err := r.store.Load(r.t.Context(), id); err == nil {
		p.seeded = len(before)
	}
	var err error
	if p.sess, err = r.ag.Resume(r.t.Context(), id, kernel.WithSessionFunc(p.rec.handle)); err != nil {
		return err
	}
	r.peers = append(r.peers, p)
	return nil
}

// record keeps what a labels or find_sessions action read, for expected.labels.jsonl.
func (r *loopRun) record(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	r.labels = append(r.labels, b)
	return nil
}

func (r *loopRun) readLedger(a ledgerAction) error {
	var v any
	var err error
	if a.Cost {
		ctx := r.t.Context()
		if a.TimeoutMS > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, ms(a.TimeoutMS))
			defer cancel()
		}
		v, err = r.sess.TurnCost(ctx, a.Turn)
	} else if a.Usage {
		v = r.sess.Snapshot().Usage
	} else {
		v, err = r.sess.TurnCalls(a.Turn)
	}
	if err != nil {
		v = map[string]string{"error": ledgerErrCode(err)}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	r.ledger = append(r.ledger, b)
	return nil
}

// ledgerErrCode names a ledger failure portably, since each binding words its errors its own way.
func ledgerErrCode(err error) string {
	var ce *kernel.CostError
	switch {
	case errors.Is(err, kernel.ErrTurnNotFound):
		return "turn_not_found"
	case errors.Is(err, kernel.ErrNoCostSource):
		return "no_cost_source"
	case errors.As(err, &ce):
		return "cost_http_" + strconv.Itoa(ce.Status)
	}
	return "other"
}

type scriptCompactor struct{ def LoopCompactor }

func (c scriptCompactor) Compact(ctx context.Context, req kernel.CompactRequest) (kernel.CompactResult, error) {
	switch c.def.Fail {
	case "":
	case "nothing_to_compact":
		return kernel.CompactResult{}, kernel.ErrNothingToCompact
	default:
		return kernel.CompactResult{}, errors.New(c.def.Fail)
	}
	sum := c.def.Summary
	if c.def.Instructions != "" {
		instr := c.def.Instructions
		if req.Instructions != "" {
			instr += "\n\n" + req.Instructions
		}
		var msgs []message.Message // the whole context as the cached prefix
		if req.Range.Kind == "up_to" {
			msgs = req.History[:req.Range.Cut]
		}
		text, _, err := req.Summarize(ctx, msgs, instr)
		if err != nil {
			return kernel.CompactResult{}, err
		}
		sum = text
	}
	var keep []message.Message
	switch c.def.Keep {
	case "current_turn":
		if req.CurrentTurn >= 0 {
			keep = req.History[req.CurrentTurn:]
		}
	case "after_cut":
		keep = req.History[req.Range.Cut:]
	}
	strategy := c.def.Strategy
	if strategy == "" {
		strategy = "scripted"
	}
	return kernel.CompactResult{Summary: sum, Replace: keep, Strategy: strategy}, nil
}

// scriptCosts prices calls per LoopCosts, counting rounds from 1.
type scriptCosts struct {
	def   LoopCosts
	mu    sync.Mutex
	round int
}

func (c *scriptCosts) Price(_ context.Context, calls []kernel.ModelCall) (map[int]kernel.Cost, error) {
	c.mu.Lock()
	c.round++
	round := c.round
	c.mu.Unlock()
	if f, ok := c.def.Errors[strconv.Itoa(round)]; ok {
		return nil, &kernel.CostError{Status: f.Status, Message: f.Message}
	}
	out := map[int]kernel.Cost{}
	for i, mc := range calls {
		if p, ok := c.def.Prices[mc.ResponseID]; ok && round >= max(p.FromRound, 1) {
			out[i] = kernel.Cost{Amount: p.Amount, Currency: p.Currency}
		}
	}
	return out, nil
}

// readLines reads a session file as written, without dropping anything, so a case can seed exactly those lines.
func readLines(path string) ([]message.Message, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []message.Message
	for _, l := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(l)) == 0 {
			continue
		}
		var m message.Message
		if err := json.Unmarshal(l, &m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// expect checks an action's error against the script: none when it names none, and one containing its text when it does.
func (r *loopRun) expect(want string, err error) error {
	switch {
	case want == "" && err != nil:
		return err
	case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
		return fmt.Errorf("want an error containing %q, got %v", want, err)
	}
	return nil
}

func (r *loopRun) remember(name string, rc *kernel.Receipt, command bool) {
	if name != "" {
		r.named[name] = rc
	}
	if command {
		r.pending = append(r.pending, rc)
	}
}

// wait advances virtual time until its condition holds at a quiescent point; the runner blocks on new events, so the clock moves only when nothing else can.
func (r *loopRun) wait(raw json.RawMessage) error {
	var idle string
	if json.Unmarshal(raw, &idle) == nil {
		if idle != "idle" {
			return fmt.Errorf("unknown wait %q", idle)
		}
		return r.until(r.idle)
	}
	var w waitAction
	if err := json.Unmarshal(raw, &w); err != nil {
		return err
	}
	if w.Receipt != "" {
		rc, ok := r.named[w.Receipt]
		if !ok {
			return fmt.Errorf("no receipt named %q", w.Receipt)
		}
		<-rc.Done()
		return nil
	}
	count := max(w.Count, 1)
	return r.until(func() bool { return r.rec.matching(w.Event, w.Where) >= count })
}

func (r *loopRun) until(cond func() bool) error {
	for {
		synctest.Wait()
		if cond() {
			return nil
		}
		if r.rec.closed() {
			return errors.New("the session closed while waiting")
		}
		<-r.rec.changed
	}
}

// idle means no turn or history command runs and every command the script invoked has completed.
func (r *loopRun) idle() bool {
	if r.sess.Snapshot().Phase != "idle" {
		return false
	}
	for _, rc := range r.pending {
		select {
		case <-rc.Done():
		default:
			return false
		}
	}
	return true
}

// recorder keeps every event of the session through a lossless handler that never blocks, and wakes a waiting runner on each one.
type recorder struct {
	mu      sync.Mutex
	events  []event.Event
	changed chan struct{}
}

func newRecorder() *recorder { return &recorder{changed: make(chan struct{}, 1)} }

func (r *recorder) handle(_ context.Context, ev event.Event) error {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
	select {
	case r.changed <- struct{}{}:
	default:
	}
	return nil
}

func (r *recorder) closed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events) > 0 && r.events[len(r.events)-1].Type == event.TypeSessionClosed
}

func (r *recorder) all() []event.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]event.Event(nil), r.events...)
}

// matching counts events of a type whose data has every field in where.
func (r *recorder) matching(typ string, where map[string]any) int {
	n := 0
	for _, ev := range r.all() {
		if string(ev.Type) != typ {
			continue
		}
		if len(where) > 0 {
			b, _ := json.Marshal(ev.Data)
			var data map[string]any
			_ = json.Unmarshal(b, &data)
			ok := true
			for k, v := range where {
				if fmt.Sprint(data[k]) != fmt.Sprint(v) {
					ok = false
				}
			}
			if !ok {
				continue
			}
		}
		n++
	}
	return n
}

// CheckInvariants asserts what every case must satisfy whatever it scripts: I1, I2, I4, I6, I9, I10 and the O1 and O5 orderings; the rest of runtime.md §1 is not visible in a case's output and has its own tests.
func CheckInvariants(res LoopResult, lines []message.Message, runs map[string]int) error {
	evs := make([]event.Event, 0, len(res.Events))
	dec := event.NewDecoder()
	for _, b := range res.Events {
		ev, err := dec.Decode(b)
		if err != nil {
			return err
		}
		evs = append(evs, ev)
	}
	for i, ev := range evs {
		if ev.Seq != uint64(i+1) {
			return fmt.Errorf("O1: event %d has seq %d", i+1, ev.Seq)
		}
	}
	if len(evs) == 0 || evs[len(evs)-1].Type != event.TypeSessionClosed {
		return errors.New("O5: session.closed is not the last event")
	}
	if err := checkPairs(evs); err != nil {
		return err
	}
	if err := checkRebuild(evs, lines[res.Seeded:]); err != nil {
		return err
	}
	if err := checkEpochs(lines); err != nil {
		return err
	}
	if err := checkRequests(res.sent); err != nil {
		return err
	}
	current := store.Current(lines)
	for i, m := range current {
		calls := m.ToolCalls()
		if m.Role != message.RoleAssistant || len(calls) == 0 {
			continue
		}
		if i+1 >= len(current) || current[i+1].Role != message.RoleTool {
			return fmt.Errorf("I1: line %d has calls and no tool line after it", m.Seq)
		}
		got := map[string]int{}
		for _, b := range current[i+1].Content {
			if r, ok := b.(*message.ToolResultBlock); ok {
				got[r.CallID]++
			}
		}
		for _, c := range calls {
			if got[c.ID] != 1 {
				return fmt.Errorf("I1: call %s has %d results", c.ID, got[c.ID])
			}
		}
	}
	committed := map[string]bool{}
	for _, m := range lines {
		for _, c := range m.ToolCalls() {
			committed[c.ID] = true
		}
	}
	for id, n := range runs {
		if n > 1 || !committed[id] {
			return fmt.Errorf("I2: call %s ran %d times, committed %v", id, n, committed[id])
		}
	}
	return nil
}

func checkPairs(evs []event.Event) error {
	open := map[string]int{}
	accepted := map[string]bool{}
	started := map[string]int{} // taken by a turn at its start; given back and taken again when the turn stopped before committing
	injected := map[string]int{}
	dropped := map[string]int{}
	for _, ev := range evs {
		switch d := ev.Data.(type) {
		case *event.TurnStarted:
			open["turn "+d.TurnID]++
			for _, id := range d.InputIDs {
				started[id]++
			}
		case *event.TurnCompleted:
			open["turn "+d.TurnID]--
		case *event.StepStarted:
			open["step "+d.MessageID]++
		case *event.StepCompleted:
			open["step "+d.MessageID]--
		case *event.MessageDiscarded:
			open["step "+d.MessageID]--
		case *event.ToolStarted:
			open["tool "+d.CallID]++
		case *event.ToolCompleted:
			open["tool "+d.CallID]--
		case *event.ToolAbandoned:
			open["tool "+d.CallID]--
		case *event.CommandStarted:
			open["command "+d.CommandID]++
		case *event.CommandCompleted:
			if d.Error != "blocked" { // a command_invoke hook refused it before it started
				open["command "+d.CommandID]--
			}
		case *event.HookStarted:
			open["hook "+d.HookID]++
		case *event.HookCompleted:
			open["hook "+d.HookID]--
		case *event.HookFailed:
			open["hook "+d.HookID]--
		case *event.ApprovalRequested:
			open["approval "+d.ApprovalID]++
		case *event.ApprovalResolved:
			open["approval "+d.ApprovalID]--
		case *event.TaskStarted:
			open[fmt.Sprintf("task %s#%d", d.TaskID, d.Attempt)]++
		case *event.TaskFinished:
			if d.Status != string(kernel.TaskLost) { // a resume reports a lost attempt whose start belonged to the process before
				open[fmt.Sprintf("task %s#%d", d.TaskID, d.Attempt)]--
			}
		case *event.InputAccepted:
			accepted[d.InputID] = true
		case *event.InputInjected:
			for _, id := range d.InputIDs {
				injected[id]++
			}
		case *event.InputDropped:
			dropped[d.InputID]++
		}
	}
	for k, n := range open {
		if n != 0 {
			return fmt.Errorf("O5: %s is unpaired (%d)", k, n)
		}
	}
	for id := range accepted {
		d, i := dropped[id], injected[id]
		if d > 1 || i > 1 || d == 1 && i == 1 || d == 0 && i == 0 && started[id] == 0 {
			return fmt.Errorf("O5: input %s dropped %d, injected %d, taken at turn start %d times", id, d, i, started[id])
		}
	}
	return nil
}

// checkRebuild is I4: the lines rebuilt from message.completed events alone equal the session file, native data and batch markers aside.
func checkRebuild(evs []event.Event, lines []message.Message) error {
	var rebuilt []json.RawMessage
	for _, ev := range evs {
		if d, ok := ev.Data.(*event.MessageCompleted); ok && len(ev.Path) == 0 { // a child session's lines are in its own file
			b, err := json.Marshal(d.Message)
			if err != nil {
				return err
			}
			rebuilt = append(rebuilt, b)
		}
	}
	if len(rebuilt) != len(lines) {
		return fmt.Errorf("I4: %d message.completed events for %d lines", len(rebuilt), len(lines))
	}
	for i, m := range lines {
		m.Batch, m.BatchEnd = "", false
		b, err := json.Marshal(publicLine(m))
		if err != nil {
			return err
		}
		if ok, err := compare.Same(b, rebuilt[i]); err != nil || !ok {
			return fmt.Errorf("I4: line %d differs from its event\n file  %s\n event %s", m.Seq, b, rebuilt[i])
		}
	}
	return nil
}

// publicLine applies the documented event projection to a stored line, so the two can be compared.
func publicLine(m message.Message) message.Message {
	m.Native = nil
	out := make(message.Content, len(m.Content))
	for i, b := range m.Content {
		switch v := b.(type) {
		case *message.TextBlock:
			c := *v
			c.Native = nil
			out[i] = &c
		case *message.RefusalBlock:
			c := *v
			c.Native = nil
			out[i] = &c
		case *message.ReasoningBlock:
			c := *v
			c.Native = nil
			out[i] = &c
		case *message.ToolCallBlock:
			c := *v
			c.Native = nil
			out[i] = &c
		case *message.OpaqueBlock:
			c := *v
			if v.Native != nil {
				c.Native = &message.Native{API: v.Native.API, Raw: json.RawMessage(`{}`)}
			}
			out[i] = &c
		case *message.HostedCallBlock:
			c := *v
			c.Native = nil
			out[i] = &c
		case *message.HostedResultBlock:
			c := *v
			c.Native = nil
			out[i] = &c
		default:
			out[i] = b
		}
	}
	m.Content = out
	return m
}

// Expected compares or rewrites a case's expected files.
func (res LoopResult) Expected(dir string, update bool) error {
	return Verify(dir, update, res.Write)
}

// Write writes the case's outputs into out as the actual.* files of runner-protocol.md.
func (res LoopResult) Write(out string) error {
	lines := map[string][]json.RawMessage{
		"actual.events.jsonl": res.Events,
		"actual.store.jsonl":  res.Store,
		"actual.labels.jsonl": res.Labels,
		"actual.hooks.jsonl":  res.Hooks,
		"actual.tasks.jsonl":  res.Tasks,
		"actual.calls.jsonl":  res.Calls,
	}
	for _, p := range res.Peers {
		lines["actual."+p.Name+".events.jsonl"] = p.Events
		lines["actual."+p.Name+".store.jsonl"] = p.Store
	}
	for name, ls := range lines {
		b, err := compare.JSONL(ls)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(out, name), b, 0o644); err != nil {
			return err
		}
	}
	if len(res.Ledger) > 0 {
		if err := writeJSON(out, "actual.ledger.json", res.Ledger); err != nil {
			return err
		}
	}
	if len(res.Requests) == 0 {
		return nil
	}
	reqDir := filepath.Join(out, "actual.requests")
	if err := os.MkdirAll(reqDir, 0o755); err != nil {
		return err
	}
	suffix := ".request.json"
	if res.Wire {
		suffix = ".body.json"
	}
	for i, body := range res.Requests {
		if err := os.WriteFile(filepath.Join(reqDir, fmt.Sprintf("%02d%s", i+1, suffix)), body, 0o644); err != nil {
			return err
		}
	}
	for i, h := range res.Headers {
		if err := os.WriteFile(filepath.Join(reqDir, fmt.Sprintf("%02d.headers.json", i+1)), h, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// checkEpochs asserts I6 on a session file: epochs never go back, each opens with a system line carrying a boundary and holds no other, and a copy points back to an earlier epoch.
func checkEpochs(lines []message.Message) error {
	epochOf := map[string]int{}
	for i, m := range lines {
		switch {
		case i > 0 && m.Epoch < lines[i-1].Epoch:
			return fmt.Errorf("I6: line %d goes back to epoch %d", m.Seq, m.Epoch)
		case i == 0 || m.Epoch != lines[i-1].Epoch:
			if m.Role != message.RoleSystem || m.Boundary == nil {
				return fmt.Errorf("I6: epoch %d opens with line %d, not a system line with a boundary", m.Epoch, m.Seq)
			}
		case m.Role == message.RoleSystem && (m.Boundary == nil || m.Boundary.Reason != "dropped"): // a side chat's dropped mark is the one later system line
			return fmt.Errorf("I6: line %d is a second system line in epoch %d", m.Seq, m.Epoch)
		}
		if e, ok := epochOf[m.CopyOf]; ok && e >= m.Epoch {
			return fmt.Errorf("I6: line %d copies line %s of epoch %d", m.Seq, m.CopyOf, e)
		}
		epochOf[m.ID] = m.Epoch
	}
	return nil
}

// checkRequests asserts I9 and I10 on the neutral requests: none ends with an assistant message except a pause_turn continuation, and the main loop's requests of one turn agree on model, reasoning and tools, of one epoch too when signatures bind the prefix.
func checkRequests(sent []json.RawMessage) error {
	type frozen struct {
		model     string
		reasoning string
		tools     string
	}
	turns, epochs := map[string]frozen{}, map[int]string{}
	for i, b := range sent {
		var req provider.Request
		if err := json.Unmarshal(b, &req); err != nil {
			return fmt.Errorf("request %d: %w", i+1, err)
		}
		if len(req.Messages) == 0 {
			continue
		}
		last := req.Messages[len(req.Messages)-1]
		if last.Role == message.RoleAssistant && !req.PauseTail {
			return fmt.Errorf("I9: request %d ends with an assistant message", i+1)
		}
		if last.Seq == 0 || last.TurnID == "" {
			continue // a side call ends with its own uncommitted prompt
		}
		reasoning, _ := json.Marshal(req.Params.Reasoning)
		tools, _ := json.Marshal(req.Tools)
		f := frozen{req.Model, string(reasoning), string(tools)}
		if prev, ok := turns[last.TurnID]; ok && prev != f {
			return fmt.Errorf("I10: request %d changes the model, reasoning or tools within %s", i+1, last.TurnID)
		}
		turns[last.TurnID] = f
		if !req.Caps.ThinkingPrefixBound {
			continue
		}
		if prev, ok := epochs[last.Epoch]; ok && prev != f.tools {
			return fmt.Errorf("I10: request %d changes the tools within epoch %d on a prefix-bound model", i+1, last.Epoch)
		}
		epochs[last.Epoch] = f.tools
	}
	return nil
}
