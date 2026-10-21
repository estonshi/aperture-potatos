package provider

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ErrorClass decides how the kernel reacts to a failed call: retry, recover once, or give up.
type ErrorClass string

const (
	ClassTransient       ErrorClass = "transient"        // 5xx, overload, dropped connections and streams that end early; retried
	ClassRateLimited     ErrorClass = "rate_limited"     // retried after at least RetryAfter
	ClassQuota           ErrorClass = "quota"            // billing or a spend limit ran out, which waiting does not fix
	ClassAuth            ErrorClass = "auth"             // 401 or 403: the credentials were rejected or access was refused
	ClassInvalidRequest  ErrorClass = "invalid_request"  // not retried, except under a Rule such as max_output_too_large that the kernel fixes with one changed retry
	ClassContextOverflow ErrorClass = "context_overflow" // the kernel compacts and retries once
	ClassReasoningReplay ErrorClass = "reasoning_replay" // the kernel resends once with ReplayFallback
	ClassNotFound        ErrorClass = "not_found"        // usually a model name this endpoint does not serve
	ClassCapability      ErrorClass = "capability"       // rejected locally by capability gating
	ClassCanceled        ErrorClass = "canceled"         // the caller's ctx ended the call
)

// Error is a classified model call failure; use AsError to get one from any error a call returns.
type Error struct {
	Class      ErrorClass      `json:"class"`
	API        API             `json:"api,omitempty"`
	Status     int             `json:"status,omitempty"`
	Type       string          `json:"type,omitempty"`
	Code       string          `json:"code,omitempty"`
	Message    string          `json:"message"`
	RetryAfter time.Duration   `json:"-"`
	RequestID  string          `json:"request_id,omitempty"`
	MidStream  bool            `json:"mid_stream,omitempty"`
	Raw        json.RawMessage `json:"raw,omitempty"`
	Rule       string          `json:"rule,omitempty"`     // the errors.json sub-rule that matched, so the driver recovers without parsing messages
	Limit      int             `json:"limit,omitempty"`    // max_output_too_large: the model's output cap parsed from the message
	NoRetry    bool            `json:"no_retry,omitempty"` // the server said not to retry (x-should-retry: false)
}

// Error names the API, the class and the message.
func (e *Error) Error() string { return string(e.API) + " " + string(e.Class) + ": " + e.Message }

// Retryable reports whether a later identical request may succeed, unless the server said not to retry.
func (e *Error) Retryable() bool {
	return !e.NoRetry && (e.Class == ClassTransient || e.Class == ClassRateLimited)
}

// AsError returns err as a classified error; anything an adapter did not classify is a cancellation or a transient failure.
func AsError(err error) *Error {
	var e *Error
	switch {
	case errors.As(err, &e):
		return e
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return &Error{Class: ClassCanceled, Message: err.Error()}
	}
	return &Error{Class: ClassTransient, Message: err.Error()}
}

// MarshalJSON writes RetryAfter as retry_after_ms, because wire durations are integer milliseconds.
func (e Error) MarshalJSON() ([]byte, error) {
	type fields Error
	return json.Marshal(struct {
		fields
		RetryAfterMs int64 `json:"retry_after_ms,omitempty"`
	}{fields(e), e.RetryAfter.Milliseconds()})
}

// RetryPolicy bounds retries of retryable errors; MaxAttempts counts the first attempt, and the kernel fills every zero field from spec/data/limits.json.
type RetryPolicy struct {
	MaxAttempts       int
	BaseDelay         time.Duration
	MaxDelay          time.Duration
	MaxElapsed        time.Duration
	FirstEventTimeout time.Duration // from sending the request to the first byte; raise it for very long uncached prompts or models that send nothing until they answer
	IdleTimeout       time.Duration // between frames, comments included; a negative timeout is none
}

// Next returns the delay before the next attempt; ok=false means give up.
func (p RetryPolicy) Next(attempt int, err *Error, elapsed time.Duration, jitter func(time.Duration) time.Duration) (time.Duration, bool) {
	if !err.Retryable() || attempt >= p.MaxAttempts {
		return 0, false
	}
	d := p.BaseDelay << (attempt - 1)
	if d > p.MaxDelay || d <= 0 {
		d = p.MaxDelay
	}
	d = jitter(d) // full jitter in [0, d]
	if err.RetryAfter > d {
		d = err.RetryAfter + jitter(p.BaseDelay)
	}
	if elapsed+d > p.MaxElapsed {
		return 0, false
	}
	return d, true
}
