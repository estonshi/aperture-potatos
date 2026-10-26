package kernel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/internal/hookrun"
	"github.com/estonshi/aperture-potatos/message"
)

// ApprovalRequest is one call waiting for a reviewer; the embedded HookEnv describes the session that issued it.
type ApprovalRequest struct {
	HookEnv
	ApprovalID    string          `json:"approval_id"`
	Call          ToolCallView    `json:"call"`
	Rules         []MatchedRule   `json:"rules,omitempty"`
	HookReasons   []string        `json:"hook_reasons,omitempty"`
	BatchID       string          `json:"batch_id"`
	BatchSize     int             `json:"batch_size"`
	Round         int             `json:"round"`
	ApprovedInput json.RawMessage `json:"approved_input,omitempty"` // what the earlier reviewers approved, when a later one rewrote it
}

// MatchedRule is a rule that matched the call under review, so a reviewer can show why it was asked and which layer the rule came from.
type MatchedRule struct {
	ID     string     `json:"id"`
	Action RuleAction `json:"action"`
	Reason string     `json:"reason,omitempty"`
	Scope  string     `json:"scope,omitempty"` // "agent" or "session", the scope the rule was registered in
}

// Verdict is a reviewer's answer; an empty or unknown verdict denies.
type Verdict string

const (
	VerdictApprove     Verdict = "approve"       // run the call, with UpdatedInput if the reviewer rewrote it
	VerdictDeny        Verdict = "deny"          // the model gets a denied result carrying Reason
	VerdictDenyAndStop Verdict = "deny_and_stop" // deny, then end the turn once this batch has finished
)

// ApprovalDecision carries no Common: a reviewer only decides, and ending the turn is what deny_and_stop is for.
type ApprovalDecision struct {
	Verdict      Verdict         `json:"verdict"`
	UpdatedInput json.RawMessage `json:"updated_input,omitempty"`
	Reason       string          `json:"reason,omitempty"`
}

func (d ApprovalDecision) hookDecision() string { return string(d.Verdict) }

// Approval has no Merge or Chain because review walks the reviewers itself: a rewrite after an approval restarts the chain.
var Approval = func() *HookPoint[ApprovalRequest, ApprovalDecision] {
	p := NewHookPoint(HookPointDef[ApprovalRequest, ApprovalDecision]{
		Name:     "approval",
		Key:      func(in ApprovalRequest) string { return in.Call.Name },
		Fail:     FailClosed,
		SyncOnly: true,
		Closed: func(err error) ApprovalDecision {
			return ApprovalDecision{Verdict: VerdictDeny, Reason: failReason("reviewer", err)}
		},
	})
	p.meta.closedOnly = true // a reviewer that fails must never approve by default
	return p
}()

// WithApprover is WithApproverNamed("approver", fn): On(Approval) at agent scope, where a later Priority decides.
func WithApprover(fn func(ctx context.Context, req ApprovalRequest) (ApprovalDecision, error)) Option {
	return WithApproverNamed("approver", fn)
}

// WithApproverNamed is On(Approval) at agent scope under the given name, so one session can run several reviewers; Priority orders them and the same name in one scope is ErrDuplicate.
func WithApproverNamed(name string, fn func(ctx context.Context, req ApprovalRequest) (ApprovalDecision, error), opts ...HookOption) Option {
	return WithHook(Approval, name, HookFunc[ApprovalRequest, ApprovalDecision](fn), opts...)
}

const maxReviewRounds = 2

var errStoppedByReviewer = errors.New("a reviewer stopped the turn")

// askCall is a call the gate sent to review, with the input the rules saw.
type askCall struct {
	index       int
	call        message.ToolCallBlock
	rules       []compiledRule // the gate's snapshot, so a rewrite is judged by the policy the call was gated under
	matched     []compiledRule
	hookReasons []string
}

// reviewed is a review's outcome: an approved input to run, or the result the call gets instead.
type reviewed struct {
	approved bool
	input    json.RawMessage
	result   message.ToolResultBlock
}

// reviewScope attributes a review's events and hooks: to a turn's batch, or to a restart that happens outside any turn.
type reviewScope struct {
	env     HookEnv
	publish func(event.Payload)
	hooks   func(callID string) *hookScope
}

func (t *turn) reviewScope() reviewScope {
	return reviewScope{env: t.env(), publish: t.publish, hooks: func(callID string) *hookScope {
		return &hookScope{turnID: t.plan.id, step: t.step, attempt: t.attempt, callID: callID, sink: t.sink}
	}}
}

func (t *turn) review(ctx context.Context, asks []askCall) ([]reviewed, bool) {
	return t.s.review(ctx, t.reviewScope(), asks)
}

// review asks the reviewers about every ask of one batch at once and returns once all have answered, so none of the batch runs before the whole batch is settled; true means a reviewer stopped the turn.
func (s *Session) review(ctx context.Context, rs reviewScope, asks []askCall) ([]reviewed, bool) {
	batch := s.agent.cfg.ids.NewID("batch")
	reqs := make([]ApprovalRequest, len(asks))
	for i, a := range asks {
		reqs[i] = ApprovalRequest{HookEnv: rs.env, ApprovalID: s.agent.cfg.ids.NewID("apr"), Call: ToolCallView{CallID: a.call.ID, Name: a.call.Name, Input: a.call.Input},
			Rules: matchedRules(a.matched), HookReasons: a.hookReasons, BatchID: batch, BatchSize: len(asks), Round: 1}
		rs.publish(&event.ApprovalRequested{ApprovalID: reqs[i].ApprovalID, CallID: a.call.ID, Name: a.call.Name, Input: a.call.Input,
			Rules: ruleRefs(reqs[i].Rules), BatchID: batch, BatchSize: len(asks)})
	}
	bctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	var stopped atomic.Bool
	out := make([]reviewed, len(asks))
	var wg sync.WaitGroup
	for i, a := range asks {
		wg.Add(1)
		s.wk.Go("approval "+a.call.ID, func() {
			defer wg.Done()
			out[i] = s.reviewOne(bctx, rs, reqs[i], a.call, a.rules, func() { stopped.Store(true); stop(errStoppedByReviewer) })
		})
	}
	wg.Wait() // canceling ctx ends every review within HookGrace, so an interrupt never waits on a person
	return out, stopped.Load()
}

// reviewOne walks the reviewers of one call; a rewrite after an approval restarts the walk, at most twice.
func (s *Session) reviewOne(ctx context.Context, rs reviewScope, req ApprovalRequest, call message.ToolCallBlock, rules []compiledRule, stopBatch func()) reviewed {
	started := time.Now()
	asked := req.Call.Input
	hctx := withHookScope(ctx, rs.hooks(call.ID))
	resolve := func(v Verdict, reason, by string, r reviewed) reviewed {
		rs.publish(&event.ApprovalResolved{ApprovalID: req.ApprovalID, CallID: call.ID, Verdict: string(v), Reason: reason, By: by,
			InputUpdated: !bytes.Equal(req.Call.Input, asked), DurationMs: time.Since(started).Milliseconds()})
		return r
	}
	deny := func(key string, vars map[string]string) reviewed {
		return reviewed{result: kernelResult(call, message.OutcomeDenied, s.textf(key, vars))}
	}
	canceled := func() reviewed {
		if errors.Is(context.Cause(ctx), errStoppedByReviewer) {
			return resolve(VerdictDeny, "", "canceled", reviewed{result: kernelResult(call, message.OutcomeNotExecuted, s.text("approval_stopped"))})
		}
		return resolve(VerdictDeny, "", "canceled", reviewed{result: kernelResult(call, message.OutcomeInterrupted, s.text("approval_interrupted"))})
	}
	for {
		chain := s.reviewers(call.Name)
		if len(chain) == 0 {
			return resolve(VerdictDeny, "no reviewer registered", "none", deny("denied_by_reviewer", map[string]string{"reason": "no reviewer registered"}))
		}
		approvedBy, restart := "", false
		for _, h := range chain {
			d, ok, fail := callSyncFail(hctx, s, Approval, h, req)
			if ok && d.Verdict != VerdictApprove {
				by := h.name
				if fail != nil && fail.Kind == hookrun.KindTimeout {
					by = "timeout"
				}
				verdict := VerdictDeny
				reason := d.Reason
				if d.Verdict != VerdictDeny && d.Verdict != VerdictDenyAndStop && reason == "" {
					reason = "the reviewer gave no verdict" // an empty or unknown verdict denies
				}
				if d.Verdict == VerdictDenyAndStop {
					verdict = VerdictDenyAndStop
					stopBatch()
					s.stoppedByReviewer(reason)
				}
				return resolve(verdict, reason, by, deny("denied_by_reviewer", map[string]string{"reason": reason}))
			}
			if !ok || ctx.Err() != nil {
				return canceled() // approvals given earlier in the chain no longer count
			}
			if d.UpdatedInput != nil {
				in, err := normalizeInput(d.UpdatedInput)
				if err != nil {
					reason := "reviewer failed (invalid input)"
					return resolve(VerdictDeny, reason, h.name, deny("denied_by_reviewer", map[string]string{"reason": reason}))
				}
				if !bytes.Equal(in, req.Call.Input) {
					v := decide(rules, ToolCallView{CallID: call.ID, Name: call.Name, Input: in})
					s.logRuleFailures(v.failures)
					if v.deny != nil {
						req.Call.Input = in
						return resolve(VerdictDeny, v.deny.Reason, h.name, deny("denied_by_rule", map[string]string{"id": v.deny.ID, "reason": v.deny.Reason}))
					}
					if approvedBy != "" {
						req.ApprovedInput, restart = req.Call.Input, true // the earlier approvals covered other bytes
					}
					req.Call.Input, req.Rules = in, matchedRules(v.matched)
				}
			}
			approvedBy = h.name
			if restart {
				break
			}
		}
		if !restart {
			return resolve(VerdictApprove, "", approvedBy, reviewed{approved: true, input: req.Call.Input})
		}
		if req.Round++; req.Round > maxReviewRounds {
			reason := "input was rewritten after approval more than once"
			return resolve(VerdictDeny, reason, approvedBy, deny("denied_by_reviewer", map[string]string{"reason": reason}))
		}
	}
}

// reviewers are the approval hooks of every layer that match the tool, read now, so a reviewer added since the session started applies at once.
func (s *Session) reviewers(tool string) []*hookEntry {
	var out []*hookEntry
	for _, h := range s.reviewerChain() {
		if h.matches(tool) {
			out = append(out, h)
		}
	}
	return out
}

func matchedRules(rs []compiledRule) []MatchedRule {
	var out []MatchedRule
	for _, r := range rs {
		out = append(out, MatchedRule{ID: r.ID, Action: r.Action, Reason: r.Reason, Scope: r.scope.String()})
	}
	return out
}

func ruleRefs(rs []MatchedRule) []event.RuleRef {
	var out []event.RuleRef
	for _, r := range rs {
		out = append(out, event.RuleRef{ID: r.ID, Action: string(r.Action), Reason: r.Reason, Scope: r.Scope})
	}
	return out
}

// warnNoReviewer tells people when a rule asks for review and nobody can give it, because such calls are then always denied.
func (s *Session) warnNoReviewer() {
	for _, r := range s.ruleSet() {
		if r.Action == RuleAsk && len(s.hookEntries(Approval.meta)) == 0 {
			s.publish(&event.Notice{Source: "approval", Kind: "no_reviewer", Detail: map[string]any{"rule": r.ID},
				Text: "approval rule " + r.ID + " asks for review, but no reviewer is registered, so such calls are denied"})
			return
		}
	}
}
