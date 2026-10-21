package kernel

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/spec"
)

// Limits bound a session's work; zero takes the default from spec/data/limits.json, a negative value lifts the limit, and for MaxTruncationContinues and ReadAhead turns the feature off.
type Limits struct {
	MaxSteps                int
	ToolTimeout             time.Duration
	ToolCancelGrace         time.Duration
	MaxConsecutiveAutoTurns int
	MaxQueuedInputs         int
	MaxTruncationContinues  int
	ReadAhead               int
	MaxParallelTools        int // calls of one parallel group running at once
	HookGrace               time.Duration
	MaxStopContinuations    int
	MaxAsyncHooks           int
	InitTimeout             time.Duration
	CloseTimeout            time.Duration
	MaxRunningTasks         int // unfinished tasks of a session, foreground and background together
	MaxToolResultChars      int // text of one tool result the model sees; past it the head and the tail are kept
}

type specDefaults struct {
	Provider struct {
		ContextWindow int `json:"context_window_tokens"`
		MaxOutput     int `json:"max_output_tokens"`
	} `json:"provider"`
	Retry struct {
		MaxAttempts int   `json:"max_attempts"`
		BaseDelay   int64 `json:"base_delay_ms"`
		MaxDelay    int64 `json:"max_delay_ms"`
		MaxElapsed  int64 `json:"max_elapsed_ms"`
		FirstEvent  int64 `json:"first_event_timeout_ms"`
		IdleTimeout int64 `json:"idle_timeout_ms"`
	} `json:"retry"`
	Limits struct {
		MaxSteps                int   `json:"max_steps"`
		ToolTimeout             int64 `json:"tool_timeout_ms"`
		ToolCancelGrace         int64 `json:"tool_cancel_grace_ms"`
		MaxConsecutiveAutoTurns int   `json:"max_consecutive_auto_turns"`
		MaxQueuedInputs         int   `json:"max_queued_inputs"`
		MaxTruncationContinues  int   `json:"max_truncation_continues"`
		ReadAhead               int   `json:"read_ahead"`
		MaxParallelTools        int   `json:"max_parallel_tools"`
		HookGrace               int64 `json:"hook_grace_ms"`
		MaxStopContinuations    int   `json:"max_stop_continuations"`
		MaxAsyncHooks           int   `json:"max_async_hooks"`
		InitTimeout             int64 `json:"init_timeout_ms"`
		CloseTimeout            int64 `json:"close_timeout_ms"`
		MaxRunningTasks         int   `json:"max_running_tasks"`
		MaxToolResultChars      int   `json:"max_tool_result_chars"`
	} `json:"limits"`
	Tasks struct {
		InboxSize    int `json:"inbox_size"`
		KeepFinished int `json:"keep_finished"`
		SummaryChars int `json:"notification_summary_chars"`
		ForeignChars int `json:"foreign_text_chars"`
	} `json:"tasks"`
	Subscription struct {
		HighWater    int   `json:"high_water"`
		HardLimit    int   `json:"hard_limit"`
		StallTimeout int64 `json:"stall_timeout_ms"`
	} `json:"subscription"`
	Context struct {
		AutoCompactRatio       float64 `json:"auto_compact_ratio"`
		TargetRatio            float64 `json:"target_ratio"`
		MinSummaryTokens       int     `json:"min_summary_tokens"`
		MaxSummaryTokens       int     `json:"max_summary_tokens"`
		PinTokens              int     `json:"pin_tokens"`
		PinTotalTokens         int     `json:"pin_total_tokens"`
		MaxAutoCompactsPerTurn int     `json:"max_auto_compacts_per_turn"`
		MaxConsecutiveFailures int     `json:"max_consecutive_failures"`
		CountBand              float64 `json:"count_band"`
	} `json:"context"`
	Hooks struct {
		AsyncTimeout int64 `json:"async_timeout_ms"`
		WakeLimit    int   `json:"wake_limit"`
		WakeWindow   int64 `json:"wake_window_ms"`
		MaxDepth     int   `json:"max_depth"`
		EndBudget    int64 `json:"session_end_budget_ms"`
	} `json:"hooks"`
	Input struct {
		ReminderMaxChars int `json:"reminder_max_chars"`
		MergedMaxBlocks  int `json:"merged_max_blocks"`
		MergedMaxBytes   int `json:"merged_max_bytes"`
	} `json:"input"`
}

var defaults = sync.OnceValue(func() specDefaults {
	var d specDefaults
	if err := json.Unmarshal([]byte(spec.MustText("data/limits.json")), &d); err != nil {
		panic(err)
	}
	return d
})

func ms(n int64) time.Duration { return time.Duration(n) * time.Millisecond }

func pickInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func pickDuration(v time.Duration, def int64) time.Duration {
	if v == 0 {
		return ms(def)
	}
	return v
}

// resolved fills every zero field; afterwards a negative count or duration means no limit.
func (l Limits) resolved() Limits {
	d := defaults().Limits
	out := Limits{
		MaxSteps:                pickInt(l.MaxSteps, d.MaxSteps),
		ToolTimeout:             pickDuration(l.ToolTimeout, d.ToolTimeout),
		ToolCancelGrace:         pickDuration(l.ToolCancelGrace, d.ToolCancelGrace),
		MaxConsecutiveAutoTurns: pickInt(l.MaxConsecutiveAutoTurns, d.MaxConsecutiveAutoTurns),
		MaxQueuedInputs:         pickInt(l.MaxQueuedInputs, d.MaxQueuedInputs),
		MaxTruncationContinues:  max(pickInt(l.MaxTruncationContinues, d.MaxTruncationContinues), 0),
		ReadAhead:               max(pickInt(l.ReadAhead, d.ReadAhead), 0),
		MaxParallelTools:        pickInt(l.MaxParallelTools, d.MaxParallelTools),
		HookGrace:               pickDuration(l.HookGrace, d.HookGrace),
		MaxStopContinuations:    pickInt(l.MaxStopContinuations, d.MaxStopContinuations),
		MaxAsyncHooks:           pickInt(l.MaxAsyncHooks, d.MaxAsyncHooks),
		InitTimeout:             pickDuration(l.InitTimeout, d.InitTimeout),
		CloseTimeout:            pickDuration(l.CloseTimeout, d.CloseTimeout),
		MaxRunningTasks:         pickInt(l.MaxRunningTasks, d.MaxRunningTasks),
		MaxToolResultChars:      pickInt(l.MaxToolResultChars, d.MaxToolResultChars),
	}
	return out
}

// under reports whether n is still within limit, where a negative limit has no bound.
func under(n, limit int) bool { return limit < 0 || n < limit }

func resolvedRetry(p provider.RetryPolicy) provider.RetryPolicy {
	d := defaults().Retry
	return provider.RetryPolicy{
		MaxAttempts:       pickInt(p.MaxAttempts, d.MaxAttempts),
		BaseDelay:         pickDuration(p.BaseDelay, d.BaseDelay),
		MaxDelay:          pickDuration(p.MaxDelay, d.MaxDelay),
		MaxElapsed:        pickDuration(p.MaxElapsed, d.MaxElapsed),
		FirstEventTimeout: pickDuration(p.FirstEventTimeout, d.FirstEvent),
		IdleTimeout:       pickDuration(p.IdleTimeout, d.IdleTimeout),
	}
}

// bounded applies d as a deadline unless d is negative, which lifts it.
func bounded(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d < 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, d)
}
