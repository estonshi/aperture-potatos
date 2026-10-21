package kernel

import (
	"encoding/json"
	"maps"
	"slices"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
)

// HookPoints lists the kernel points; spec/schema/hook.schema.json defines the JSON of each one.
func HookPoints() []AnyHookPoint {
	return []AnyHookPoint{SessionStart, SessionEnd, UserPromptSubmit, CommandInvoke, PreModelRequest, PostModelResponse,
		PreToolUse, Approval, PostToolUse, PostToolBatch, Stop, ProviderSwitch, TurnEnd, PreCompact, PostCompact, TaskStart, TaskNotify}
}

// failReason is the model-facing reason of a fail-closed hook: its kind only, because an error message is not written for the model.
func failReason(what string, err error) string {
	kind := "error"
	if he, ok := err.(*HookError); ok {
		kind = he.Kind
	}
	return what + " failed (" + kind + ")"
}

// ---- session_start ----

// SessionStartInput names why a new epoch begins and the model it begins on.
type SessionStartInput struct {
	HookEnv
	Source string             `json:"source"` // new, resume, clear, compact, rewind or fork
	Model  provider.ModelInfo `json:"model"`
}

// SessionStartOutput can only add context, through Common.
type SessionStartOutput struct {
	Common
}

// SessionStart fires when a session is created or resumed and whenever clear, compaction, rewind or a fork opens a new epoch, keyed by Source.
var SessionStart = NewHookPoint(HookPointDef[SessionStartInput, SessionStartOutput]{
	Name:    "session_start",
	Key:     func(in SessionStartInput) string { return in.Source },
	Timeout: 30 * time.Second,
	Fail:    FailOpen,
})

// ---- session_end ----

// SessionEndInput carries the close reason and the session's final usage.
type SessionEndInput struct {
	HookEnv
	Reason string      `json:"reason"`
	Usage  UsageReport `json:"usage"`
}

// SessionEndOutput exists for symmetry; nothing a session_end hook returns can reach the model any more.
type SessionEndOutput struct {
	Common
}

// SessionEnd fires once as the session closes, keyed by the reason, with a short timeout because Close waits for it.
var SessionEnd = NewHookPoint(HookPointDef[SessionEndInput, SessionEndOutput]{
	Name:    "session_end",
	Key:     func(in SessionEndInput) string { return in.Reason },
	Timeout: 5 * time.Second,
	Fail:    FailOpen,
})

// ---- user_prompt_submit ----

// PromptInput is one queued input as prompt hooks see and rewrite it.
type PromptInput struct {
	ID       string            `json:"id"`
	Text     string            `json:"text"`
	Parts    message.Content   `json:"parts,omitempty"`
	Origin   message.Origin    `json:"origin"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// UserPromptSubmitInput is a batch of queued inputs about to reach the model, all of one origin kind.
type UserPromptSubmitInput struct {
	HookEnv
	Inputs []PromptInput `json:"inputs"` // one origin kind per firing
}

// UserPromptSubmitOutput drops or rewrites inputs before the model sees them.
type UserPromptSubmitOutput struct {
	Common
	Block         bool          `json:"block,omitempty"`     // drops every input of this firing
	BlockIDs      []string      `json:"block_ids,omitempty"` // drops these inputs only
	Reason        string        `json:"reason,omitempty"`
	UpdatedInputs []PromptInput `json:"updated_inputs,omitempty"` // matched by id; the origin never changes
}

func (o UserPromptSubmitOutput) hookDecision() string {
	switch {
	case o.Block || len(o.BlockIDs) > 0:
		return "block"
	case o.UpdatedInputs != nil:
		return "rewrite"
	}
	return ""
}

// UserPromptSubmit fires before queued inputs are delivered, keyed by origin kind, and a failing hook blocks them.
var UserPromptSubmit = NewHookPoint(HookPointDef[UserPromptSubmitInput, UserPromptSubmitOutput]{
	Name: "user_prompt_submit",
	Key: func(in UserPromptSubmitInput) string {
		if len(in.Inputs) == 0 {
			return ""
		}
		return string(in.Inputs[0].Origin.Kind)
	},
	Chain: func(in UserPromptSubmitInput, out UserPromptSubmitOutput) UserPromptSubmitInput {
		if out.UpdatedInputs == nil {
			return in
		}
		next := slices.Clone(in.Inputs)
		for _, u := range out.UpdatedInputs {
			for i := range next {
				if next[i].ID == u.ID {
					next[i].Text, next[i].Parts, next[i].Metadata = u.Text, u.Parts, u.Metadata
				}
			}
		}
		in.Inputs = next
		return in
	},
	Merge: func(acc, out UserPromptSubmitOutput) (UserPromptSubmitOutput, bool) {
		if out.Block && !acc.Block {
			acc.Block, acc.Reason = true, out.Reason
		}
		for _, id := range out.BlockIDs {
			if !slices.Contains(acc.BlockIDs, id) {
				acc.BlockIDs = append(acc.BlockIDs, id)
			}
		}
		if acc.Reason == "" {
			acc.Reason = out.Reason
		}
		return acc, acc.Block
	},
	Timeout: 30 * time.Second,
	Fail:    FailClosed,
	Closed: func(err error) UserPromptSubmitOutput {
		return UserPromptSubmitOutput{Block: true, Reason: failReason("prompt hook", err)}
	},
})

// ---- command_invoke ----

// CommandInvokeInput is a command about to run, with its arguments as the next hook would see them.
type CommandInvokeInput struct {
	HookEnv
	Name string `json:"name"`
	Args string `json:"args"`
	Busy bool   `json:"busy"` // it runs beside a turn or a history command
}

// CommandInvokeOutput blocks the command, which then completes with ErrBlocked, or rewrites its arguments.
type CommandInvokeOutput struct {
	Common
	Block       bool    `json:"block,omitempty"`
	Reason      string  `json:"reason,omitempty"`
	UpdatedArgs *string `json:"updated_args,omitempty"`
}

func (o CommandInvokeOutput) hookDecision() string {
	switch {
	case o.Block:
		return "block"
	case o.UpdatedArgs != nil:
		return "rewrite"
	}
	return ""
}

// CommandInvoke fires before every command runs, keyed by name, and a failing hook blocks it.
var CommandInvoke = NewHookPoint(HookPointDef[CommandInvokeInput, CommandInvokeOutput]{
	Name: "command_invoke",
	Key:  func(in CommandInvokeInput) string { return in.Name },
	Chain: func(in CommandInvokeInput, out CommandInvokeOutput) CommandInvokeInput {
		if out.UpdatedArgs != nil {
			in.Args = *out.UpdatedArgs
		}
		return in
	},
	Merge: func(acc, out CommandInvokeOutput) (CommandInvokeOutput, bool) {
		if out.Block {
			acc.Block, acc.Reason = true, out.Reason
		}
		return acc, acc.Block
	},
	Timeout: 10 * time.Second,
	Fail:    FailClosed,
	Closed: func(err error) CommandInvokeOutput {
		return CommandInvokeOutput{Block: true, Reason: failReason("command hook", err)}
	},
})

// ---- pre_model_request ----

// PreModelRequestInput is the step's request as far as hooks may change it, with Params already patched by earlier hooks.
type PreModelRequestInput struct {
	HookEnv
	Model     string          `json:"model"`
	Params    provider.Params `json:"params"`
	ToolNames []string        `json:"tool_names,omitempty"`
	EstTokens int64           `json:"est_tokens"`
}

// ParamsPatch changes this step's request only; a nil field keeps the configured value.
type ParamsPatch struct {
	MaxOutputTokens *int                 `json:"max_output_tokens,omitempty"`
	Temperature     *float64             `json:"temperature,omitempty"`
	TopP            *float64             `json:"top_p,omitempty"`
	StopSequences   []string             `json:"stop_sequences,omitempty"`
	ToolChoice      *provider.ToolChoice `json:"tool_choice,omitempty"`
	Headers         map[string]string    `json:"headers,omitempty"`
	Extra           map[string]any       `json:"extra,omitempty"`
}

// PreModelRequestOutput patches this step's parameters, sets the next turn's reasoning, or answers the step without calling the model.
type PreModelRequestOutput struct {
	Common
	ParamsPatch       *ParamsPatch              `json:"params_patch,omitempty"`
	NextTurnReasoning *provider.ReasoningParams `json:"next_turn_reasoning,omitempty"` // reasoning changes wait for the next turn, so a turn never mixes them (I10)
	Synthetic         *message.Message          `json:"synthetic,omitempty"`           // answers this step instead of the model
}

func (o PreModelRequestOutput) hookDecision() string {
	switch {
	case o.Synthetic != nil:
		return "synthetic"
	case o.ParamsPatch != nil:
		return "patch"
	}
	return ""
}

// merge folds a later patch over this one, field by field.
func (p *ParamsPatch) merge(q *ParamsPatch) *ParamsPatch {
	if p == nil {
		return q
	}
	if q == nil {
		return p
	}
	out := *p
	if q.MaxOutputTokens != nil {
		out.MaxOutputTokens = q.MaxOutputTokens
	}
	if q.Temperature != nil {
		out.Temperature = q.Temperature
	}
	if q.TopP != nil {
		out.TopP = q.TopP
	}
	if q.StopSequences != nil {
		out.StopSequences = q.StopSequences
	}
	if q.ToolChoice != nil {
		out.ToolChoice = q.ToolChoice
	}
	if q.Headers != nil {
		out.Headers = maps.Clone(out.Headers)
		if out.Headers == nil {
			out.Headers = map[string]string{}
		}
		maps.Copy(out.Headers, q.Headers)
	}
	if q.Extra != nil {
		out.Extra = maps.Clone(out.Extra)
		if out.Extra == nil {
			out.Extra = map[string]any{}
		}
		maps.Copy(out.Extra, q.Extra)
	}
	return &out
}

// apply writes the patch into params; the tool choice goes to the request instead.
func (p *ParamsPatch) apply(params provider.Params) provider.Params {
	if p == nil {
		return params
	}
	if p.MaxOutputTokens != nil {
		params.MaxOutputTokens = *p.MaxOutputTokens
	}
	if p.Temperature != nil {
		params.Temperature = p.Temperature
	}
	if p.TopP != nil {
		params.TopP = p.TopP
	}
	if p.StopSequences != nil {
		params.StopSequences = p.StopSequences
	}
	if p.Headers != nil {
		params.Headers = params.Headers.Clone()
		if params.Headers == nil {
			params.Headers = map[string][]string{}
		}
		for k, v := range p.Headers {
			params.Headers.Set(k, v)
		}
	}
	if p.Extra != nil {
		params.Extra = maps.Clone(params.Extra)
		if params.Extra == nil {
			params.Extra = map[string]any{}
		}
		maps.Copy(params.Extra, p.Extra)
	}
	return params
}

// PreModelRequest fires before every model call of the main loop, keyed by model name.
var PreModelRequest = NewHookPoint(HookPointDef[PreModelRequestInput, PreModelRequestOutput]{
	Name: "pre_model_request",
	Key:  func(in PreModelRequestInput) string { return in.Model },
	Chain: func(in PreModelRequestInput, out PreModelRequestOutput) PreModelRequestInput {
		in.Params = out.ParamsPatch.apply(in.Params)
		return in
	},
	Merge: func(acc, out PreModelRequestOutput) (PreModelRequestOutput, bool) {
		acc.ParamsPatch = acc.ParamsPatch.merge(out.ParamsPatch)
		if out.NextTurnReasoning != nil {
			acc.NextTurnReasoning = out.NextTurnReasoning
		}
		if out.Synthetic != nil && acc.Synthetic == nil {
			acc.Synthetic = out.Synthetic
		}
		return acc, acc.Synthetic != nil
	},
	Timeout: 10 * time.Second,
	Fail:    FailOpen,
})

// ---- post_model_response ----

// PostModelResponseInput is a complete reply before it is committed to the history.
type PostModelResponseInput struct {
	HookEnv
	Message message.Message `json:"message"` // the public projection, without provider wire data
	Stop    provider.Stop   `json:"stop"`
	Usage   message.Usage   `json:"usage"`
}

// PostModelResponseOutput can reject the reply, so it never enters the history.
type PostModelResponseOutput struct {
	Common
	Reject bool   `json:"reject,omitempty"` // drops the reply and ends the turn
	Reason string `json:"reason,omitempty"`
}

func (o PostModelResponseOutput) hookDecision() string {
	if o.Reject {
		return "reject"
	}
	return ""
}

// PostModelResponse fires on every complete model reply, not on a synthetic one, before it is committed, keyed by stop reason.
var PostModelResponse = NewHookPoint(HookPointDef[PostModelResponseInput, PostModelResponseOutput]{
	Name: "post_model_response",
	Key:  func(in PostModelResponseInput) string { return string(in.Stop.Reason) },
	Merge: func(acc, out PostModelResponseOutput) (PostModelResponseOutput, bool) {
		if out.Reject {
			acc.Reject, acc.Reason = true, out.Reason
		}
		return acc, acc.Reject
	},
	Timeout: 30 * time.Second,
	Fail:    FailOpen,
})

// ---- pre_tool_use ----

// PreToolUseInput is a call about to run, with its input as rewritten by earlier hooks, and the tool's spec.
type PreToolUseInput struct {
	HookEnv
	Call message.ToolCallBlock `json:"call"`
	Spec ToolSpec              `json:"spec"`
}

// PreToolUseOutput decides, rewrites or answers a call before the approval rules and reviewers see it.
type PreToolUseOutput struct {
	Common
	Decision        Decision         `json:"decision,omitempty"`
	Reason          string           `json:"reason,omitempty"`
	UpdatedInput    json.RawMessage  `json:"updated_input,omitempty"`    // replaces the whole input
	SyntheticResult *SyntheticResult `json:"synthetic_result,omitempty"` // skip the tool and use this result
}

// SyntheticResult is a result a hook supplies, either instead of running the tool or in place of its real result.
type SyntheticResult struct {
	Content message.Content `json:"content"`
	IsError bool            `json:"is_error,omitempty"`
}

func (o PreToolUseOutput) hookDecision() string {
	if o.SyntheticResult != nil {
		return "synthetic"
	}
	return string(o.Decision)
}

// PreToolUse fires before every tool call, keyed by tool name, and a failing hook denies the call.
var PreToolUse = NewHookPoint(HookPointDef[PreToolUseInput, PreToolUseOutput]{
	Name: "pre_tool_use",
	Key:  func(in PreToolUseInput) string { return in.Call.Name },
	Chain: func(in PreToolUseInput, out PreToolUseOutput) PreToolUseInput {
		if out.UpdatedInput != nil {
			if n, err := normalizeInput(out.UpdatedInput); err == nil {
				in.Call.Input = n
			} else {
				in.Call.Input = out.UpdatedInput // the gate normalizes the final input again and denies it
			}
		}
		return in
	},
	Merge: func(acc, out PreToolUseOutput) (PreToolUseOutput, bool) {
		if out.Decision.rank() > acc.Decision.rank() {
			acc.Decision, acc.Reason = out.Decision, out.Reason
		}
		if out.UpdatedInput != nil {
			acc.UpdatedInput = out.UpdatedInput
		}
		if out.SyntheticResult != nil && acc.SyntheticResult == nil {
			acc.SyntheticResult = out.SyntheticResult
		}
		return acc, acc.Decision == Deny || acc.SyntheticResult != nil
	},
	Timeout: 60 * time.Second,
	Fail:    FailClosed,
	Closed: func(err error) PreToolUseOutput {
		return PreToolUseOutput{Decision: Deny, Reason: failReason("policy hook", err)}
	},
})

// ---- post_tool_use ----

// PostToolUseInput is a finished call and its result, before the result is committed.
type PostToolUseInput struct {
	HookEnv
	Call       message.ToolCallBlock   `json:"call"`
	Result     message.ToolResultBlock `json:"result"`
	DurationMS int64                   `json:"duration_ms"`
}

// PostToolUseOutput replaces the result or ends the turn once the batch is in.
type PostToolUseOutput struct {
	Common
	UpdatedResult *SyntheticResult `json:"updated_result,omitempty"`
	StopTurn      bool             `json:"stop_turn,omitempty"` // end the turn once this batch is committed
}

func (o PostToolUseOutput) hookDecision() string {
	switch {
	case o.StopTurn:
		return "stop_turn"
	case o.UpdatedResult != nil:
		return "rewrite"
	}
	return ""
}

// PostToolUse fires on every call's result before it is committed, denied and background calls included, keyed by tool name.
var PostToolUse = NewHookPoint(HookPointDef[PostToolUseInput, PostToolUseOutput]{
	Name: "post_tool_use",
	Key:  func(in PostToolUseInput) string { return in.Call.Name },
	Chain: func(in PostToolUseInput, out PostToolUseOutput) PostToolUseInput {
		if u := out.UpdatedResult; u != nil {
			in.Result = rewriteResult(in.Result, u)
		}
		return in
	},
	Merge: func(acc, out PostToolUseOutput) (PostToolUseOutput, bool) {
		acc.StopTurn = acc.StopTurn || out.StopTurn
		if out.UpdatedResult != nil {
			acc.UpdatedResult = out.UpdatedResult
		}
		return acc, false
	},
	Timeout: 30 * time.Second,
	Fail:    FailOpen,
})

// rewriteResult replaces a result's content; a success or error result follows the new is_error, other outcomes keep theirs.
func rewriteResult(r message.ToolResultBlock, u *SyntheticResult) message.ToolResultBlock {
	r.Content = escapeContent(u.Content)
	r.IsError = u.IsError
	if r.Outcome == message.OutcomeSuccess || r.Outcome == message.OutcomeError {
		r.Outcome = message.OutcomeSuccess
		if u.IsError {
			r.Outcome = message.OutcomeError
		}
	}
	return r
}

// ---- post_tool_batch ----

// PostToolBatchInput is one step's calls and their results, in call order.
type PostToolBatchInput struct {
	HookEnv
	Calls   []message.ToolCallBlock   `json:"calls"`
	Results []message.ToolResultBlock `json:"results"`
}

// PostToolBatchOutput can end the turn instead of sending the results back to the model.
type PostToolBatchOutput struct {
	Common
	StopTurn bool `json:"stop_turn,omitempty"`
}

func (o PostToolBatchOutput) hookDecision() string {
	if o.StopTurn {
		return "stop_turn"
	}
	return ""
}

// PostToolBatch fires once a step's whole batch of calls has settled; it has no key.
var PostToolBatch = NewHookPoint(HookPointDef[PostToolBatchInput, PostToolBatchOutput]{
	Name: "post_tool_batch",
	Merge: func(acc, out PostToolBatchOutput) (PostToolBatchOutput, bool) {
		acc.StopTurn = acc.StopTurn || out.StopTurn
		return acc, false
	},
	Timeout: 30 * time.Second,
	Fail:    FailOpen,
})

// ---- stop ----

// StopInput is a turn about to end with the model's final answer.
type StopInput struct {
	HookEnv
	LastAssistantText string `json:"last_assistant_text"`
	StopHookActive    bool   `json:"stop_hook_active"` // this turn already continued because of a stop hook
	Continuations     int    `json:"continuations"`
}

// StopOutput keeps the turn going, up to Limits.MaxStopContinuations times.
type StopOutput struct {
	Common
	Continue bool   `json:"continue,omitempty"`
	Reason   string `json:"reason,omitempty"` // reaches the model as the continuation's reminder
}

func (o StopOutput) hookDecision() string {
	if o.Continue {
		return "continue"
	}
	return ""
}

// Stop fires when a reply would end the turn because it calls no tool; it has no key.
var Stop = NewHookPoint(HookPointDef[StopInput, StopOutput]{
	Name: "stop",
	Merge: func(acc, out StopOutput) (StopOutput, bool) {
		if out.Continue {
			acc.Continue = true
			switch {
			case acc.Reason == "":
				acc.Reason = out.Reason
			case out.Reason != "":
				acc.Reason += "\n\n" + out.Reason
			}
		}
		return acc, false
	},
	Timeout: 30 * time.Second,
	Fail:    FailOpen,
})

// ---- provider_switch ----

// ProviderSwitchInput is a pending model switch and how much replayed content it would drop.
type ProviderSwitchInput struct {
	HookEnv
	From             event.ModelName `json:"from"`
	To               event.ModelName `json:"to"`
	DroppedReasoning int             `json:"dropped_reasoning"`
	DroppedOpaque    int             `json:"dropped_opaque"`
}

// ProviderSwitchOutput can block the switch, and the session stays on the current model.
type ProviderSwitchOutput struct {
	Common
	Block  bool   `json:"block,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func (o ProviderSwitchOutput) hookDecision() string {
	if o.Block {
		return "block"
	}
	return ""
}

// ProviderSwitch fires at the start of the turn a SwitchModel takes effect, keyed by the target model, and a failing hook blocks it.
var ProviderSwitch = NewHookPoint(HookPointDef[ProviderSwitchInput, ProviderSwitchOutput]{
	Name: "provider_switch",
	Key:  func(in ProviderSwitchInput) string { return in.To.Model },
	Merge: func(acc, out ProviderSwitchOutput) (ProviderSwitchOutput, bool) {
		if out.Block {
			acc.Block, acc.Reason = true, out.Reason
		}
		return acc, acc.Block
	},
	Timeout: 10 * time.Second,
	Fail:    FailClosed,
	Closed: func(err error) ProviderSwitchOutput {
		return ProviderSwitchOutput{Block: true, Reason: failReason("switch hook", err)}
	},
})

// ---- turn_end ----

// TurnEndInput is the result the turn's receipts resolve with.
type TurnEndInput struct {
	HookEnv
	Result TurnResult `json:"result"`
}

// TurnEndOutput can add context, which waits for the next turn.
type TurnEndOutput struct {
	Common
}

// TurnEnd fires after every turn whatever its outcome, keyed by the outcome.
var TurnEnd = NewHookPoint(HookPointDef[TurnEndInput, TurnEndOutput]{
	Name:    "turn_end",
	Key:     func(in TurnEndInput) string { return string(in.Result.Outcome) },
	Timeout: 10 * time.Second,
	Fail:    FailOpen,
})

// ---- pre_compact ----

// PreCompactInput is a compaction about to run, with the instructions the strategy will get.
type PreCompactInput struct {
	HookEnv
	Trigger      string                `json:"trigger"` // manual, auto, overflow or switch
	Instructions string                `json:"instructions"`
	PreTokens    int64                 `json:"pre_tokens"`
	Range        *message.CompactRange `json:"range,omitempty"`
}

// PreCompactOutput blocks the compaction or rewrites its instructions.
type PreCompactOutput struct {
	Common
	Block               bool    `json:"block,omitempty"` // ignored for overflow, which must compact to go on
	Reason              string  `json:"reason,omitempty"`
	UpdatedInstructions *string `json:"updated_instructions,omitempty"`
}

func (o PreCompactOutput) hookDecision() string {
	switch {
	case o.Block:
		return "block"
	case o.UpdatedInstructions != nil:
		return "rewrite"
	}
	return ""
}

// PreCompact fires before every compaction, keyed by trigger.
var PreCompact = NewHookPoint(HookPointDef[PreCompactInput, PreCompactOutput]{
	Name: "pre_compact",
	Key:  func(in PreCompactInput) string { return in.Trigger },
	Chain: func(in PreCompactInput, out PreCompactOutput) PreCompactInput {
		if out.UpdatedInstructions != nil {
			in.Instructions = *out.UpdatedInstructions
		}
		return in
	},
	Merge: func(acc, out PreCompactOutput) (PreCompactOutput, bool) {
		if out.Block {
			acc.Block, acc.Reason = true, out.Reason
		}
		return acc, acc.Block
	},
	Timeout: 30 * time.Second,
	Fail:    FailOpen,
})

// ---- post_compact ----

// PostCompactInput is a finished compaction and the summary that opens the new epoch.
type PostCompactInput struct {
	HookEnv
	Trigger    string `json:"trigger"`
	Summary    string `json:"summary"`
	PreTokens  int64  `json:"pre_tokens"`
	PostTokens int64  `json:"post_tokens"` // estimated before this point's own context joins the epoch head
	Epoch      int    `json:"epoch"`       // the epoch the compaction opens
}

// PostCompactOutput can add context, which joins the new epoch head and so survives with the summary.
type PostCompactOutput struct {
	Common
}

// PostCompact fires after every successful compaction, keyed by trigger.
var PostCompact = NewHookPoint(HookPointDef[PostCompactInput, PostCompactOutput]{
	Name:    "post_compact",
	Key:     func(in PostCompactInput) string { return in.Trigger },
	Timeout: 30 * time.Second,
	Fail:    FailOpen,
})
