package event

import (
	"encoding/json"
	"time"

	"github.com/estonshi/aperture-potatos/message"
)

// Each kernel payload mirrors its $defs entry in spec/schema/event.schema.json; the drift test keeps the two in step.
const (
	TypeSessionStarted           Type = "session.started"
	TypeSessionStateChanged      Type = "session.state_changed"
	TypeSessionClosed            Type = "session.closed"
	TypeSystemRendered           Type = "system.rendered"
	TypeInputAccepted            Type = "input.accepted"
	TypeInputInjected            Type = "input.injected"
	TypeInputDropped             Type = "input.dropped"
	TypeTurnStarted              Type = "turn.started"
	TypeTurnInterruptRequested   Type = "turn.interrupt_requested"
	TypeTurnCompleted            Type = "turn.completed"
	TypeStepStarted              Type = "step.started"
	TypeStepCompleted            Type = "step.completed"
	TypeModelRetry               Type = "model.retry"
	TypeModelNotice              Type = "model.notice"
	TypeMessageStarted           Type = "message.started"
	TypeMessageDelta             Type = "message.delta"
	TypeMessageBlock             Type = "message.block"
	TypeMessageCompleted         Type = "message.completed"
	TypeMessageDiscarded         Type = "message.discarded"
	TypeReasoningStarted         Type = "reasoning.started"
	TypeReasoningDelta           Type = "reasoning.delta"
	TypeReasoningCompleted       Type = "reasoning.completed"
	TypeToolCallStarted          Type = "tool_call.started"
	TypeToolCallDelta            Type = "tool_call.delta"
	TypeToolCallCompleted        Type = "tool_call.completed"
	TypeApprovalRequested        Type = "approval.requested"
	TypeApprovalResolved         Type = "approval.resolved"
	TypeToolStarted              Type = "tool.started"
	TypeToolProgress             Type = "tool.progress"
	TypeToolCompleted            Type = "tool.completed"
	TypeToolAbandoned            Type = "tool.abandoned"
	TypeTaskStarted              Type = "task.started"
	TypeTaskProgress             Type = "task.progress"
	TypeTaskStatusChanged        Type = "task.status_changed"
	TypeTaskInstruction          Type = "task.instruction"
	TypeTaskFinished             Type = "task.finished"
	TypeTasksSnapshot            Type = "tasks.snapshot"
	TypeHookStarted              Type = "hook.started"
	TypeHookCompleted            Type = "hook.completed"
	TypeHookFailed               Type = "hook.failed"
	TypeUsageUpdated             Type = "usage.updated"
	TypeContextInjected          Type = "context.injected"
	TypeContextCompactionStarted Type = "context.compaction_started"
	TypeContextCompacted         Type = "context.compacted"
	TypeContextCompactionFailed  Type = "context.compaction_failed"
	TypeContextCleared           Type = "context.cleared"
	TypeContextRewound           Type = "context.rewound"
	TypeSideChatDropped          Type = "side_chat.dropped"
	TypeCommandStarted           Type = "command.started"
	TypeCommandOutput            Type = "command.output"
	TypeCommandCompleted         Type = "command.completed"
	TypeProviderSwitched         Type = "provider.switched"
	TypeProviderSwitchRejected   Type = "provider.switch_rejected"
	TypeProviderRaw              Type = "provider.raw"
	TypeRegistryChanged          Type = "registry.changed"
	TypeNotice                   Type = "notice"
	TypeError                    Type = "error"
)

// SessionStarted is the payload of session.started.
type SessionStarted struct {
	SpecVersion  string          `json:"spec_version"`
	Source       string          `json:"source"`
	Agent        string          `json:"agent"`
	Model        ModelInfo       `json:"model"`
	Tools        []string        `json:"tools,omitempty"`
	Commands     []string        `json:"commands,omitempty"`
	Extensions   []string        `json:"extensions,omitempty"`
	Capabilities json.RawMessage `json:"capabilities,omitempty"`
	SystemHash   string          `json:"system_hash"`
}

// SessionStateChanged is the payload of session.state_changed.
type SessionStateChanged struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// SessionClosed is the payload of session.closed.
type SessionClosed struct {
	Reason        string         `json:"reason"`
	LeakedWorkers []LeakedWorker `json:"leaked_workers,omitempty"`
}

// SystemRendered is the payload of system.rendered.
type SystemRendered struct {
	Epoch    int       `json:"epoch"`
	Sections []Section `json:"sections,omitempty"`
	Hash     string    `json:"hash"`
}

// InputAccepted is the payload of input.accepted.
type InputAccepted struct {
	InputID  string         `json:"input_id"`
	Mode     string         `json:"mode"`
	Origin   message.Origin `json:"origin"`
	Text     string         `json:"text"`
	QueueLen int            `json:"queue_len"`
}

// InputInjected is the payload of input.injected.
type InputInjected struct {
	InputIDs []string `json:"input_ids,omitempty"`
	TurnID   string   `json:"turn_id"`
	Step     int      `json:"step"`
}

// InputDropped is the payload of input.dropped.
type InputDropped struct {
	InputID string `json:"input_id"`
	Reason  string `json:"reason"`
}

// TurnStarted is the payload of turn.started.
type TurnStarted struct {
	TurnID   string   `json:"turn_id"`
	Trigger  string   `json:"trigger"`
	InputIDs []string `json:"input_ids,omitempty"`
}

// TurnInterruptRequested is the payload of turn.interrupt_requested.
type TurnInterruptRequested struct {
	TurnID string `json:"turn_id"`
	Reason string `json:"reason"`
}

// TurnCompleted is the payload of turn.completed.
type TurnCompleted struct {
	TurnID     string        `json:"turn_id"`
	Outcome    string        `json:"outcome"`
	StopReason string        `json:"stop_reason,omitempty"`
	Steps      int           `json:"steps"`
	Usage      message.Usage `json:"usage"`
	Error      *ErrorInfo    `json:"error,omitempty"`
	DurationMs int64         `json:"duration_ms"`
}

// StepStarted is the payload of step.started.
type StepStarted struct {
	MessageID string `json:"message_id"`
	API       string `json:"api"`
	Model     string `json:"model"`
	Attempt   int    `json:"attempt"`
}

// StepCompleted is the payload of step.completed.
type StepCompleted struct {
	MessageID     string        `json:"message_id"`
	StopReason    string        `json:"stop_reason"`
	RawStopReason string        `json:"raw_stop_reason,omitempty"`
	Usage         message.Usage `json:"usage"`
	TTFTMs        int64         `json:"ttft_ms"`
	DurationMs    int64         `json:"duration_ms"`
}

// ModelRetry is the payload of model.retry.
type ModelRetry struct {
	Attempt int       `json:"attempt"`
	Error   ErrorInfo `json:"error"`
	DelayMs int64     `json:"delay_ms"`
}

// ModelNotice is the payload of model.notice.
type ModelNotice struct {
	Kind   string          `json:"kind"`
	Detail json.RawMessage `json:"detail,omitempty"`
}

// MessageStarted is the payload of message.started.
type MessageStarted struct {
	MessageID string       `json:"message_id"`
	Role      message.Role `json:"role"`
}

// MessageDelta is the payload of message.delta (bulk).
type MessageDelta struct {
	MessageID  string `json:"message_id"`
	BlockIndex int    `json:"block_index"`
	Channel    string `json:"channel"`
	Text       string `json:"text"`
}

// MessageBlock is the payload of message.block.
type MessageBlock struct {
	MessageID  string      `json:"message_id"`
	BlockIndex int         `json:"block_index"`
	BlockType  string      `json:"block_type"`
	Opaque     *OpaqueInfo `json:"opaque,omitempty"`

	HostedCall   *message.HostedCallBlock   `json:"hosted_call,omitempty"`   // Native left out
	HostedResult *message.HostedResultBlock `json:"hosted_result,omitempty"` // Native left out
}

// MessageCompleted is the payload of message.completed.
type MessageCompleted struct {
	Message message.Message `json:"message"`
	Partial bool            `json:"partial"`
}

// MessageDiscarded is the payload of message.discarded.
type MessageDiscarded struct {
	MessageID   string          `json:"message_id"`
	Reason      string          `json:"reason"`
	StopDetails json.RawMessage `json:"stop_details,omitempty"`
}

// ReasoningStarted is the payload of reasoning.started.
type ReasoningStarted struct {
	MessageID  string                `json:"message_id"`
	BlockIndex int                   `json:"block_index"`
	Kind       message.ReasoningKind `json:"kind"`
}

// ReasoningDelta is the payload of reasoning.delta (bulk).
type ReasoningDelta struct {
	MessageID  string                `json:"message_id"`
	BlockIndex int                   `json:"block_index"`
	Kind       message.ReasoningKind `json:"kind"`
	Text       string                `json:"text"`
}

// ReasoningCompleted is the payload of reasoning.completed.
type ReasoningCompleted struct {
	MessageID  string                `json:"message_id"`
	BlockIndex int                   `json:"block_index"`
	Kind       message.ReasoningKind `json:"kind"`
	Text       string                `json:"text"`
	Replayable bool                  `json:"replayable"`
}

// ToolCallStarted is the payload of tool_call.started.
type ToolCallStarted struct {
	MessageID  string `json:"message_id"`
	BlockIndex int    `json:"block_index"`
	CallID     string `json:"call_id"`
	Name       string `json:"name"`
}

// ToolCallDelta is the payload of tool_call.delta (bulk).
type ToolCallDelta struct {
	MessageID string `json:"message_id"`
	CallID    string `json:"call_id"`
	ArgsDelta string `json:"args_delta"`
}

// ToolCallCompleted is the payload of tool_call.completed.
type ToolCallCompleted struct {
	MessageID  string          `json:"message_id"`
	CallID     string          `json:"call_id"`
	Name       string          `json:"name"`
	Input      json.RawMessage `json:"input,omitempty"`
	InputError string          `json:"input_error,omitempty"`
}

// ApprovalRequested is the payload of approval.requested.
type ApprovalRequested struct {
	ApprovalID string          `json:"approval_id"`
	CallID     string          `json:"call_id"`
	Name       string          `json:"name"`
	Input      json.RawMessage `json:"input,omitempty"`
	Rules      []RuleRef       `json:"rules,omitempty"`
	BatchID    string          `json:"batch_id"`
	BatchSize  int             `json:"batch_size"`
}

// ApprovalResolved is the payload of approval.resolved.
type ApprovalResolved struct {
	ApprovalID   string `json:"approval_id"`
	CallID       string `json:"call_id"`
	Verdict      string `json:"verdict"`
	Reason       string `json:"reason"`
	By           string `json:"by"`
	InputUpdated bool   `json:"input_updated"`
	DurationMs   int64  `json:"duration_ms"`
}

// ToolStarted is the payload of tool.started.
type ToolStarted struct {
	CallID  string          `json:"call_id"`
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input,omitempty"`
	Group   int             `json:"group"`
	Movable bool            `json:"movable,omitempty"` // Session.BackgroundCall accepts this call while it runs
}

// ToolProgress is the payload of tool.progress (bulk).
type ToolProgress struct {
	CallID string          `json:"call_id"`
	Text   string          `json:"text,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
}

// ToolCompleted is the payload of tool.completed.
type ToolCompleted struct {
	CallID     string          `json:"call_id"`
	Name       string          `json:"name"`
	Outcome    message.Outcome `json:"outcome"`
	IsError    bool            `json:"is_error"`
	Content    message.Content `json:"content,omitempty"`
	Structured json.RawMessage `json:"structured,omitempty"`
	DurationMs int64           `json:"duration_ms"`
	// OriginalChars is the length of the tool's text when the model was given only its head and tail; 0 when nothing was cut.
	OriginalChars int `json:"original_chars,omitempty"`
}

// ToolAbandoned is the payload of tool.abandoned.
type ToolAbandoned struct {
	CallID  string `json:"call_id"`
	Name    string `json:"name"`
	GraceMs int64  `json:"grace_ms"`
}

// TaskStarted is the payload of task.started.
type TaskStarted struct {
	TaskID       string     `json:"task_id"`
	Kind         string     `json:"kind"`
	Title        string     `json:"title"`
	Foreground   bool       `json:"foreground"`
	Attempt      int        `json:"attempt"`
	CallID       string     `json:"call_id,omitempty"`
	Continuation bool       `json:"continuation"`
	Detail       TaskDetail `json:"detail"`
}

// TaskProgress is the payload of task.progress (bulk).
type TaskProgress struct {
	TaskID  string          `json:"task_id"`
	Text    string          `json:"text,omitempty"`
	Percent *float64        `json:"percent,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// TaskStatusChanged is the payload of task.status_changed.
type TaskStatusChanged struct {
	TaskID   string `json:"task_id"`
	From     string `json:"from"`
	To       string `json:"to"`
	Question string `json:"question,omitempty"`
}

// TaskInstruction is the payload of task.instruction.
type TaskInstruction struct {
	TaskID        string `json:"task_id"`
	InstructionID string `json:"instruction_id"`
	Mode          string `json:"mode"`
	Text          string `json:"text"`
	Accepted      bool   `json:"accepted"`
	Reason        string `json:"reason,omitempty"`
}

// TaskFinished is the payload of task.finished.
type TaskFinished struct {
	TaskID                  string        `json:"task_id"`
	Status                  string        `json:"status"`
	Attempt                 int           `json:"attempt"`
	IsError                 bool          `json:"is_error"`
	Summary                 string        `json:"summary"`
	UnprocessedInstructions []Instruction `json:"unprocessed_instructions,omitempty"`
	DurationMs              int64         `json:"duration_ms"`
}

// TasksSnapshot is the payload of tasks.snapshot.
type TasksSnapshot struct {
	Tasks []TaskSummary `json:"tasks,omitempty"`
}

// HookStarted is the payload of hook.started.
type HookStarted struct {
	HookID string `json:"hook_id"`
	Point  string `json:"point"`
	Name   string `json:"name"`
	Async  bool   `json:"async"`
	CallID string `json:"call_id,omitempty"` // the tool call a per-call point fired for
}

// HookCompleted is the payload of hook.completed.
type HookCompleted struct {
	HookID        string `json:"hook_id"`
	Point         string `json:"point"`
	Name          string `json:"name"`
	CallID        string `json:"call_id,omitempty"`
	Decision      string `json:"decision,omitempty"`
	DurationMs    int64  `json:"duration_ms"`
	SystemMessage string `json:"system_message,omitempty"`
}

// HookFailed is the payload of hook.failed.
type HookFailed struct {
	HookID        string `json:"hook_id"`
	Point         string `json:"point"`
	Name          string `json:"name"`
	CallID        string `json:"call_id,omitempty"`
	Error         string `json:"error"`
	Timeout       bool   `json:"timeout"`
	Canceled      bool   `json:"canceled"`
	PolicyApplied string `json:"policy_applied"`
}

// UsageUpdated is the payload of usage.updated.
type UsageUpdated struct {
	Bucket       string        `json:"bucket"`
	StepUsage    message.Usage `json:"step_usage"`
	SessionTotal message.Usage `json:"session_total"`
	Forwarded    bool          `json:"forwarded,omitempty"`
	Context      ContextInfo   `json:"context"`
}

// ContextInjected is the payload of context.injected.
type ContextInjected struct {
	MessageID string   `json:"message_id"`
	Sources   []string `json:"sources,omitempty"`
	Count     int      `json:"count"`
	Truncated bool     `json:"truncated"`
}

// ContextCompactionStarted is the payload of context.compaction_started.
type ContextCompactionStarted struct {
	Trigger   string `json:"trigger"`
	PreTokens int64  `json:"pre_tokens"`
}

// ContextCompacted is the payload of context.compacted.
type ContextCompacted struct {
	Trigger    string `json:"trigger"`
	Strategy   string `json:"strategy"`
	PreTokens  int64  `json:"pre_tokens"`
	PostTokens int64  `json:"post_tokens"`
	Epoch      int    `json:"epoch"`
}

// ContextCompactionFailed is the payload of context.compaction_failed.
type ContextCompactionFailed struct {
	Trigger  string `json:"trigger"`
	Error    string `json:"error"`
	Canceled bool   `json:"canceled"`
}

// ContextCleared is the payload of context.cleared.
type ContextCleared struct {
	Epoch int `json:"epoch"`
}

// ContextRewound is the payload of context.rewound.
type ContextRewound struct {
	TurnID  string `json:"turn_id"`
	Epoch   int    `json:"epoch"`
	Dropped int    `json:"dropped"`
}

// SideChatDropped is the payload of side_chat.dropped.
type SideChatDropped struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// CommandStarted is the payload of command.started.
type CommandStarted struct {
	CommandID string `json:"command_id"`
	Name      string `json:"name"`
	Args      string `json:"args"`
	Busy      bool   `json:"busy"`
}

// CommandOutput is the payload of command.output (bulk).
type CommandOutput struct {
	CommandID string `json:"command_id"`
	Text      string `json:"text"`
}

// CommandCompleted is the payload of command.completed.
type CommandCompleted struct {
	CommandID string          `json:"command_id"`
	Name      string          `json:"name"`
	Text      string          `json:"text"`
	Data      json.RawMessage `json:"data,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// ProviderSwitched is the payload of provider.switched.
type ProviderSwitched struct {
	From             ModelName `json:"from"`
	To               ModelName `json:"to"`
	DroppedReasoning int       `json:"dropped_reasoning"`
	DroppedOpaque    int       `json:"dropped_opaque"`
	CacheReset       bool      `json:"cache_reset"`
}

// ProviderSwitchRejected is the payload of provider.switch_rejected.
type ProviderSwitchRejected struct {
	To     ModelName `json:"to"`
	Reason string    `json:"reason"`
}

// ProviderRaw is the payload of provider.raw (bulk).
type ProviderRaw struct {
	API   string          `json:"api"`
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// RegistryChanged is the payload of registry.changed.
type RegistryChanged struct {
	Scope     string `json:"scope"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Op        string `json:"op"` // add, remove, or update for a tool whose definition changed
	Effective string `json:"effective"`
}

// Notice is the payload of notice.
type Notice struct {
	Source string `json:"source"`
	Text   string `json:"text"`
}

// Error is the payload of error.
type Error struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Fatal   bool            `json:"fatal"`
	Detail  json.RawMessage `json:"detail,omitempty"`
}

// ModelInfo is the model identity of a session.
type ModelInfo struct {
	API      string `json:"api"`
	Endpoint string `json:"endpoint"`
	Model    string `json:"model"`
}

// ModelName names one end of a provider switch.
type ModelName struct {
	API   string `json:"api"`
	Model string `json:"model"`
}

// LeakedWorker is a goroutine that did not exit in time.
type LeakedWorker struct {
	Name  string `json:"name"`
	AgeMs int64  `json:"age_ms"`
}

// Section is one rendered system prompt section.
type Section struct {
	Slot  string `json:"slot"`
	Name  string `json:"name"`
	Chars int    `json:"chars"`
}

// ErrorInfo is a classified error as events report it.
type ErrorInfo struct {
	Class     string `json:"class"`
	Status    int    `json:"status,omitempty"`
	Code      string `json:"code,omitempty"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

// OpaqueInfo is the readable part of an opaque block.
type OpaqueInfo struct {
	Kind string `json:"kind"`
	Text string `json:"text,omitempty"`
}

// RuleRef is an approval rule that matched a call.
type RuleRef struct {
	ID     string `json:"id"`
	Action string `json:"action"`
	Reason string `json:"reason,omitempty"`
}

// TaskDetail is the spec and state an attempt starts with.
type TaskDetail struct {
	Spec  json.RawMessage `json:"spec,omitempty"`
	State json.RawMessage `json:"state,omitempty"`
}

// Instruction is a task instruction as events report it.
type Instruction struct {
	ID     string          `json:"id"`
	Text   string          `json:"text"`
	Data   json.RawMessage `json:"data,omitempty"`
	Mode   string          `json:"mode"`
	Answer bool            `json:"answer,omitempty"`
}

// TaskSummary is one task in a tasks.snapshot.
type TaskSummary struct {
	TaskID    string    `json:"task_id"`
	Kind      string    `json:"kind"`
	Title     string    `json:"title"`
	Status    string    `json:"status"`
	Attempt   int       `json:"attempt"`
	StartedAt time.Time `json:"started_at"`
}

// ContextInfo is the context window use after a model call.
type ContextInfo struct {
	UsedTokens      int64  `json:"used_tokens"`
	WindowTokens    int64  `json:"window_tokens"`
	ThresholdTokens int64  `json:"threshold_tokens"`
	Basis           string `json:"basis"`
}

func (*SessionStarted) EventType() Type           { return TypeSessionStarted }
func (*SessionStateChanged) EventType() Type      { return TypeSessionStateChanged }
func (*SessionClosed) EventType() Type            { return TypeSessionClosed }
func (*SystemRendered) EventType() Type           { return TypeSystemRendered }
func (*InputAccepted) EventType() Type            { return TypeInputAccepted }
func (*InputInjected) EventType() Type            { return TypeInputInjected }
func (*InputDropped) EventType() Type             { return TypeInputDropped }
func (*TurnStarted) EventType() Type              { return TypeTurnStarted }
func (*TurnInterruptRequested) EventType() Type   { return TypeTurnInterruptRequested }
func (*TurnCompleted) EventType() Type            { return TypeTurnCompleted }
func (*StepStarted) EventType() Type              { return TypeStepStarted }
func (*StepCompleted) EventType() Type            { return TypeStepCompleted }
func (*ModelRetry) EventType() Type               { return TypeModelRetry }
func (*ModelNotice) EventType() Type              { return TypeModelNotice }
func (*MessageStarted) EventType() Type           { return TypeMessageStarted }
func (*MessageDelta) EventType() Type             { return TypeMessageDelta }
func (*MessageBlock) EventType() Type             { return TypeMessageBlock }
func (*MessageCompleted) EventType() Type         { return TypeMessageCompleted }
func (*MessageDiscarded) EventType() Type         { return TypeMessageDiscarded }
func (*ReasoningStarted) EventType() Type         { return TypeReasoningStarted }
func (*ReasoningDelta) EventType() Type           { return TypeReasoningDelta }
func (*ReasoningCompleted) EventType() Type       { return TypeReasoningCompleted }
func (*ToolCallStarted) EventType() Type          { return TypeToolCallStarted }
func (*ToolCallDelta) EventType() Type            { return TypeToolCallDelta }
func (*ToolCallCompleted) EventType() Type        { return TypeToolCallCompleted }
func (*ApprovalRequested) EventType() Type        { return TypeApprovalRequested }
func (*ApprovalResolved) EventType() Type         { return TypeApprovalResolved }
func (*ToolStarted) EventType() Type              { return TypeToolStarted }
func (*ToolProgress) EventType() Type             { return TypeToolProgress }
func (*ToolCompleted) EventType() Type            { return TypeToolCompleted }
func (*ToolAbandoned) EventType() Type            { return TypeToolAbandoned }
func (*TaskStarted) EventType() Type              { return TypeTaskStarted }
func (*TaskProgress) EventType() Type             { return TypeTaskProgress }
func (*TaskStatusChanged) EventType() Type        { return TypeTaskStatusChanged }
func (*TaskInstruction) EventType() Type          { return TypeTaskInstruction }
func (*TaskFinished) EventType() Type             { return TypeTaskFinished }
func (*TasksSnapshot) EventType() Type            { return TypeTasksSnapshot }
func (*HookStarted) EventType() Type              { return TypeHookStarted }
func (*HookCompleted) EventType() Type            { return TypeHookCompleted }
func (*HookFailed) EventType() Type               { return TypeHookFailed }
func (*UsageUpdated) EventType() Type             { return TypeUsageUpdated }
func (*ContextInjected) EventType() Type          { return TypeContextInjected }
func (*ContextCompactionStarted) EventType() Type { return TypeContextCompactionStarted }
func (*ContextCompacted) EventType() Type         { return TypeContextCompacted }
func (*ContextCompactionFailed) EventType() Type  { return TypeContextCompactionFailed }
func (*ContextCleared) EventType() Type           { return TypeContextCleared }
func (*ContextRewound) EventType() Type           { return TypeContextRewound }
func (*SideChatDropped) EventType() Type          { return TypeSideChatDropped }
func (*CommandStarted) EventType() Type           { return TypeCommandStarted }
func (*CommandOutput) EventType() Type            { return TypeCommandOutput }
func (*CommandCompleted) EventType() Type         { return TypeCommandCompleted }
func (*ProviderSwitched) EventType() Type         { return TypeProviderSwitched }
func (*ProviderSwitchRejected) EventType() Type   { return TypeProviderSwitchRejected }
func (*ProviderRaw) EventType() Type              { return TypeProviderRaw }
func (*RegistryChanged) EventType() Type          { return TypeRegistryChanged }
func (*Notice) EventType() Type                   { return TypeNotice }
func (*Error) EventType() Type                    { return TypeError }

func (*MessageDelta) Bulk() bool   { return true }
func (*ReasoningDelta) Bulk() bool { return true }
func (*ToolCallDelta) Bulk() bool  { return true }
func (*ToolProgress) Bulk() bool   { return true }
func (*TaskProgress) Bulk() bool   { return true }
func (*CommandOutput) Bulk() bool  { return true }
func (*ProviderRaw) Bulk() bool    { return true }

// kernelPayloads lists every kernel payload so a Decoder knows them without registration.
var kernelPayloads = []Constructor{
	{TypeSessionStarted, func() Payload { return new(SessionStarted) }},
	{TypeSessionStateChanged, func() Payload { return new(SessionStateChanged) }},
	{TypeSessionClosed, func() Payload { return new(SessionClosed) }},
	{TypeSystemRendered, func() Payload { return new(SystemRendered) }},
	{TypeInputAccepted, func() Payload { return new(InputAccepted) }},
	{TypeInputInjected, func() Payload { return new(InputInjected) }},
	{TypeInputDropped, func() Payload { return new(InputDropped) }},
	{TypeTurnStarted, func() Payload { return new(TurnStarted) }},
	{TypeTurnInterruptRequested, func() Payload { return new(TurnInterruptRequested) }},
	{TypeTurnCompleted, func() Payload { return new(TurnCompleted) }},
	{TypeStepStarted, func() Payload { return new(StepStarted) }},
	{TypeStepCompleted, func() Payload { return new(StepCompleted) }},
	{TypeModelRetry, func() Payload { return new(ModelRetry) }},
	{TypeModelNotice, func() Payload { return new(ModelNotice) }},
	{TypeMessageStarted, func() Payload { return new(MessageStarted) }},
	{TypeMessageDelta, func() Payload { return new(MessageDelta) }},
	{TypeMessageBlock, func() Payload { return new(MessageBlock) }},
	{TypeMessageCompleted, func() Payload { return new(MessageCompleted) }},
	{TypeMessageDiscarded, func() Payload { return new(MessageDiscarded) }},
	{TypeReasoningStarted, func() Payload { return new(ReasoningStarted) }},
	{TypeReasoningDelta, func() Payload { return new(ReasoningDelta) }},
	{TypeReasoningCompleted, func() Payload { return new(ReasoningCompleted) }},
	{TypeToolCallStarted, func() Payload { return new(ToolCallStarted) }},
	{TypeToolCallDelta, func() Payload { return new(ToolCallDelta) }},
	{TypeToolCallCompleted, func() Payload { return new(ToolCallCompleted) }},
	{TypeApprovalRequested, func() Payload { return new(ApprovalRequested) }},
	{TypeApprovalResolved, func() Payload { return new(ApprovalResolved) }},
	{TypeToolStarted, func() Payload { return new(ToolStarted) }},
	{TypeToolProgress, func() Payload { return new(ToolProgress) }},
	{TypeToolCompleted, func() Payload { return new(ToolCompleted) }},
	{TypeToolAbandoned, func() Payload { return new(ToolAbandoned) }},
	{TypeTaskStarted, func() Payload { return new(TaskStarted) }},
	{TypeTaskProgress, func() Payload { return new(TaskProgress) }},
	{TypeTaskStatusChanged, func() Payload { return new(TaskStatusChanged) }},
	{TypeTaskInstruction, func() Payload { return new(TaskInstruction) }},
	{TypeTaskFinished, func() Payload { return new(TaskFinished) }},
	{TypeTasksSnapshot, func() Payload { return new(TasksSnapshot) }},
	{TypeHookStarted, func() Payload { return new(HookStarted) }},
	{TypeHookCompleted, func() Payload { return new(HookCompleted) }},
	{TypeHookFailed, func() Payload { return new(HookFailed) }},
	{TypeUsageUpdated, func() Payload { return new(UsageUpdated) }},
	{TypeContextInjected, func() Payload { return new(ContextInjected) }},
	{TypeContextCompactionStarted, func() Payload { return new(ContextCompactionStarted) }},
	{TypeContextCompacted, func() Payload { return new(ContextCompacted) }},
	{TypeContextCompactionFailed, func() Payload { return new(ContextCompactionFailed) }},
	{TypeContextCleared, func() Payload { return new(ContextCleared) }},
	{TypeContextRewound, func() Payload { return new(ContextRewound) }},
	{TypeSideChatDropped, func() Payload { return new(SideChatDropped) }},
	{TypeCommandStarted, func() Payload { return new(CommandStarted) }},
	{TypeCommandOutput, func() Payload { return new(CommandOutput) }},
	{TypeCommandCompleted, func() Payload { return new(CommandCompleted) }},
	{TypeProviderSwitched, func() Payload { return new(ProviderSwitched) }},
	{TypeProviderSwitchRejected, func() Payload { return new(ProviderSwitchRejected) }},
	{TypeProviderRaw, func() Payload { return new(ProviderRaw) }},
	{TypeRegistryChanged, func() Payload { return new(RegistryChanged) }},
	{TypeNotice, func() Payload { return new(Notice) }},
	{TypeError, func() Payload { return new(Error) }},
}
