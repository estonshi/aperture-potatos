package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/internal/prompt"
	"github.com/estonshi/aperture-potatos/internal/toolexec"
	"github.com/estonshi/aperture-potatos/internal/workers"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
)

// turn is the driver's turn-local data; session state is reached only through exec.
type turn struct {
	s           *Session
	plan        *turnPlan
	model       modelState
	started     time.Time
	step        int
	attempt     int
	msgID       string
	defs        []message.ToolDef
	hosted      []message.HostedTool
	tools       map[string]frozenTool
	meta        map[string]string
	call        *modelCall
	pauseTail   bool
	nudged      bool
	truncations int
	afterTools  bool // the history ends with a tool line, maybe followed by the safe point's user line
	committed   bool // the turn's input reached history, so an interruption leaves a note
	partial     bool // an interrupted reply was committed partially
	usage       message.Usage
	lastStop    provider.StopReason
	finalText   string
	receipts    []*Receipt
	unfinished  []string
	sink        *hookSink // what this turn's hooks hand back for the next delivery point
	stops       int       // stop-hook continuations so far
	compactions int       // automatic compactions so far
	overflowAt  int       // the step an overflow compaction already ran for
	view        *mainView // a side chat's main part, taken when the turn starts
}

// turnOutcome is how a turn ended, carried from the driver to the actor.
type turnOutcome struct {
	Kind      Outcome
	stop      provider.StopReason
	err       error
	steps     int
	usage     message.Usage
	finalText string
	started   time.Time
	receipts  []*Receipt
	note      *queueItem
	clock     Clock
}

func (o turnOutcome) completed(turnID string) *event.TurnCompleted {
	ev := &event.TurnCompleted{TurnID: turnID, Outcome: string(o.Kind), StopReason: string(o.stop), Steps: o.steps, Usage: o.usage, DurationMs: o.clock.Now().Sub(o.started).Milliseconds()}
	if o.Kind == OutcomeError && o.err != nil {
		info := errInfo(o.err)
		ev.Error = &info
	}
	return ev
}

func (o turnOutcome) result(turnID string) TurnResult {
	return TurnResult{TurnID: turnID, Outcome: o.Kind, StopReason: o.stop, Steps: o.steps, Usage: o.usage, FinalText: o.finalText, Err: o.err}
}

func errInfo(err error) event.ErrorInfo {
	var pe *provider.Error
	if errors.As(err, &pe) {
		return event.ErrorInfo{Class: string(pe.Class), Status: pe.Status, Code: pe.Code, Message: pe.Message, RequestID: pe.RequestID}
	}
	return event.ErrorInfo{Class: "internal", Message: err.Error()}
}

func (t *turn) publish(p event.Payload) { t.s.publishAt(p, t.plan.id, t.step, t.attempt) }

// runTurn runs on the turn-driver goroutine; it keeps only turn-local data and reaches session state via exec.
func (s *Session) runTurn(ctx context.Context, plan *turnPlan) (out turnOutcome) {
	t := &turn{s: s, plan: plan, started: s.agent.cfg.clock.Now()}
	defer func() {
		if r := recover(); r != nil {
			if s.agent.cfg.panicOnInternal {
				panic(r)
			}
			s.agent.cfg.logger.Error("turn panic", "turn", plan.id, "panic", r, "stack", string(debug.Stack()))
			out = turnOutcome{Kind: OutcomeError, err: fmt.Errorf("turn panic: %v", r)}
		}
		out = t.finish(out) // I1 repairTail and persist; none of it honors the turn's cancellation
	}()

	t.model = plan.model
	t.sink = &hookSink{}
	if plan.switchTo != nil {
		t.applySwitch(ctx, *plan.switchTo)
	}
	t.freeze()
	if ctx.Err() != nil {
		return t.endBeforeCommit(ctx, plan.items)
	}
	note, err := t.takeView()
	if err != nil {
		_ = s.exec(func(*state) { s.giveBack(plan.items, true) })
		if errors.Is(err, ErrSessionClosed) {
			s.orphaned()
		}
		return t.fail(err)
	}
	items := t.submit(ctx, plan.items)
	if ctx.Err() != nil {
		return t.endBeforeCommit(ctx, items)
	}
	var wakes bool
	_ = s.exec(func(st *state) {
		wakes = slices.ContainsFunc(items, func(it *queueItem) bool { return st.canWake(it, s.maxAuto()) })
	})
	if !wakes { // every input that could start this turn was blocked; the passengers wait for the next one
		_ = s.exec(func(*state) { s.giveBack(items, false) })
		return t.end(OutcomeHookStopped)
	}
	pc := ProvideContext{Session: s, TurnID: plan.id, Inputs: inputsOf(items)}
	pre := slices.Concat(s.provide(ctx, AtTurnStart, pc), s.provide(ctx, AtEveryStep, pc))
	if note != "" {
		pre = append([]*message.ReminderBlock{{Source: "side_chat", Text: note}}, pre...)
	}
	if ctx.Err() != nil {
		return t.endBeforeCommit(ctx, items)
	}
	msg, info := delivery(items, pre, t.sink.take())
	t.meta = info.meta
	if err := t.commitDelivery(ctx, msg, &info, items, false); err != nil {
		return t.fail(err)
	}
	t.committed = true
	if t.sink.halted() {
		return t.end(OutcomeHookStopped)
	}

	for {
		if s.limits.MaxSteps >= 0 && t.step >= s.limits.MaxSteps {
			return t.end(OutcomeMaxSteps)
		}
		t.step++
		t.autoCompact(ctx)
		req, synthetic := t.buildRequest(ctx)
		if t.sink.halted() && ctx.Err() == nil {
			return t.end(OutcomeHookStopped)
		}
		var res *stepResult
		var err error
		if synthetic != nil {
			res = t.syntheticStep(synthetic)
		} else {
			res, err = t.callModel(ctx, req) // retries inside; never runs tools
		}
		if err != nil && res == nil && t.overflowCompact(ctx, err) {
			t.step-- // the same step again, on the compacted history
			continue
		}
		if err != nil {
			if res != nil {
				t.commitPartial(res) // interrupted mid-stream: closed blocks only
			}
			return t.endErr(ctx, err)
		}
		if synthetic == nil && t.postModelResponse(ctx, res) {
			t.discard(res, "hook_rejected") // never committed, so the history still ends with a user-side line
			t.lastStop = res.stop.Reason
			return t.end(OutcomeHookStopped)
		}
		if res.stop.Reason == provider.StopRefusal || res.stop.Reason == provider.StopContentFilter {
			t.discard(res, string(res.stop.Reason)) // partial output, thinking and tool calls never reach history
			t.lastStop = res.stop.Reason
			return t.end(OutcomeRefused)
		}
		if res.message.Empty() {
			t.discard(res, "empty") // an empty assistant message is rejected anywhere but last (RULE-MSG-001)
		} else if err := t.commitAssistant(ctx, res); err != nil {
			return t.fail(err)
		}
		t.lastStop = res.stop.Reason
		if res.stop.ReasoningExhausted {
			t.publish(&event.ModelNotice{Kind: "reasoning_exhausted"}) // the output was all reasoning; raise MaxOutputTokens or lower the effort
		}
		if t.sink.halted() {
			return t.end(OutcomeHookStopped) // finish pairs this reply's calls with not_executed
		}
		calls := res.message.ToolCalls()
		switch classify(res, calls) {
		case nextPause:
			t.pauseTail = true // resend with the assistant content last and no user message (I9's only exception)
			continue
		case nextEnd:
			if res.message.Empty() && t.afterTools && !t.nudged {
				t.nudged = true
				t.publish(&event.ModelNotice{Kind: "empty_reply_continue"})
				if err := t.continueWith(ctx, continuation(prompt.Text("empty_reply_continue"))); err != nil {
					return t.fail(err)
				}
				continue
			}
			cont := t.stopHooks(ctx, res)
			if t.sink.halted() {
				return t.end(OutcomeHookStopped)
			}
			if cont == nil {
				return t.end(OutcomeCompleted)
			}
			if err := t.continueWith(ctx, cont); err != nil {
				return t.fail(err)
			}
			continue
		case nextTruncatedText:
			if !s.agent.cfg.autoContinue || !t.takeTruncation() {
				return t.end(OutcomeMaxTokens)
			}
			if err := t.continueWith(ctx, continuation(prompt.Text("truncated_text_continue"))); err != nil { // I9: never end on assistant
				return t.fail(err)
			}
			continue
		case nextTruncatedCalls:
			paired := toolLine(calls, prompt.Text("truncated_call_not_executed"))
			t.noteUnfinished(paired)
			if !t.takeTruncation() {
				if err := t.commitLines(ctx, paired, false); err != nil {
					return t.fail(err)
				}
				return t.end(OutcomeMaxTokens)
			}
			if err := t.commitLines(ctx, paired, ctx.Err() == nil && t.withinSteps()); err != nil {
				return t.fail(err)
			}
			continue
		case nextTools:
		}

		results, stop := t.runTools(ctx, calls)
		batch := fire(t.hookCtx(ctx, ""), s, PostToolBatch, PostToolBatchInput{HookEnv: t.env(), Calls: hookCalls(calls), Results: results}, nil)
		stop = stop || batch.out.StopTurn || t.sink.halted()
		line := &message.Message{Role: message.RoleTool, Origin: message.Origin{Kind: message.OriginTool}}
		for i := range results {
			line.Content = append(line.Content, &results[i])
		}
		t.noteUnfinished(line)
		due := ctx.Err() == nil && t.withinSteps() && !stop // queue items wait for the next turn unless another model call follows
		if err := t.commitLines(ctx, line, due); err != nil {
			return t.fail(err)
		}
		if ctx.Err() != nil {
			return t.stopped(ctx)
		}
		if stop {
			return t.end(OutcomeHookStopped)
		}
	}
}

// env is the HookEnv of the points this turn fires.
func (t *turn) env() HookEnv { return t.s.env(t.plan.id, t.step) }

// hookCtx attributes the hooks fired under it to this turn, step and call, and hands their context to this turn's next delivery point.
func (t *turn) hookCtx(ctx context.Context, callID string) context.Context {
	return withHookScope(ctx, &hookScope{turnID: t.plan.id, step: t.step, attempt: t.attempt, callID: callID, sink: t.sink})
}

// submit fires user_prompt_submit once per origin kind over the items about to be delivered, applies rewrites and drops what was blocked.
func (t *turn) submit(ctx context.Context, items []*queueItem) []*queueItem {
	s := t.s
	if len(items) == 0 || len(s.hookEntries(UserPromptSubmit.meta)) == 0 {
		return items
	}
	var kinds []message.OriginKind
	parts := map[string]*itemPart{}
	for _, it := range items {
		for _, p := range it.parts {
			parts[p.id] = p
			if !slices.Contains(kinds, p.origin.Kind) {
				kinds = append(kinds, p.origin.Kind)
			}
		}
	}
	blocked := map[string]bool{}
	for _, k := range kinds {
		var ins []PromptInput
		for _, it := range items {
			for _, p := range it.parts {
				if p.origin.Kind == k {
					ins = append(ins, PromptInput{ID: p.id, Text: p.text, Parts: p.content, Origin: p.origin, Metadata: p.metadata})
				}
			}
		}
		f := fire(t.hookCtx(ctx, ""), s, UserPromptSubmit, UserPromptSubmitInput{HookEnv: t.env(), Inputs: ins}, nil)
		if ctx.Err() != nil {
			return items
		}
		for _, in := range f.in.Inputs {
			if p := parts[in.ID]; p != nil {
				p.text, p.content, p.metadata = in.Text, in.Parts, in.Metadata
			}
		}
		for _, in := range ins {
			if f.out.Block || slices.Contains(f.out.BlockIDs, in.ID) {
				blocked[in.ID] = true
			}
		}
	}
	if len(blocked) == 0 {
		return items
	}
	var kept []*queueItem
	_ = s.exec(func(*state) {
		for _, it := range items {
			var left []*itemPart
			for _, p := range it.parts {
				if blocked[p.id] {
					s.dropPart(p, "blocked")
				} else {
					left = append(left, p)
				}
			}
			if len(left) == 0 {
				s.release(it)
				continue
			}
			it.parts = left
			kept = append(kept, it)
		}
	})
	return kept
}

// inputsOf is what AtTurnStart providers see of a turn's inputs.
func inputsOf(items []*queueItem) []Input {
	var out []Input
	for _, it := range items {
		for _, p := range it.parts {
			out = append(out, Input{Text: p.text, Parts: p.content, Origin: p.origin, Metadata: p.metadata})
		}
	}
	return out
}

// postModelResponse lets hooks reject a finished reply before it is committed; a rewrite is not offered, because it would break the reply's signatures.
func (t *turn) postModelResponse(ctx context.Context, res *stepResult) bool {
	f := fire(t.hookCtx(ctx, ""), t.s, PostModelResponse, PostModelResponseInput{HookEnv: t.env(), Message: publicMessage(*res.message), Stop: res.stop, Usage: res.usage}, nil)
	return f.out.Reject
}

// stopHooks asks the stop hooks whether the turn goes on; each hook that asks gives one reminder of the continuation, within MaxStopContinuations per turn.
func (t *turn) stopHooks(ctx context.Context, res *stepResult) *message.Message {
	s := t.s
	capped := !under(t.stops, s.limits.MaxStopContinuations)
	decide := func(o StopOutput) string {
		switch {
		case o.Continue && capped:
			return "continue_ignored_cap"
		case o.Continue:
			return "continue"
		}
		return ""
	}
	f := fire(t.hookCtx(ctx, ""), s, Stop, StopInput{HookEnv: t.env(), LastAssistantText: res.message.Text(), StopHookActive: t.stops > 0, Continuations: t.stops}, decide)
	if !f.out.Continue || capped || ctx.Err() != nil {
		return nil
	}
	t.stops++
	m := &message.Message{Role: message.RoleUser, Origin: message.Origin{Kind: message.OriginContinuation}}
	for _, o := range f.outs {
		if o.out.Continue {
			text := o.out.Reason
			if text == "" {
				text = prompt.Text("stop_continue")
			}
			m.Content = append(m.Content, &message.ReminderBlock{Source: "hook:" + o.name, Text: text})
		}
	}
	return m
}

// syntheticStep answers a step with the message a pre_model_request hook supplied; no request is sent, so nothing is booked.
func (t *turn) syntheticStep(m *message.Message) *stepResult {
	msg := *m
	t.attempt, t.call = 1, nil
	t.msgID = t.s.agent.cfg.ids.NewID("msg")
	msg.ID, msg.Role, msg.Origin, msg.Producer = t.msgID, message.RoleAssistant, message.Origin{Kind: message.OriginModel}, &message.Producer{API: "synthetic"}
	stop := provider.StopEndTurn
	if len(msg.ToolCalls()) > 0 {
		stop = provider.StopToolUse
	}
	t.publish(&event.StepStarted{MessageID: t.msgID, API: "synthetic", Model: t.model.info.Model, Attempt: 1})
	return &stepResult{message: &msg, stop: provider.Stop{Reason: stop}}
}

// hookCalls is the calls as hooks see them: without provider wire data.
func hookCalls(calls []message.ToolCallBlock) []message.ToolCallBlock {
	out := make([]message.ToolCallBlock, len(calls))
	for i, c := range calls {
		c.Native = nil
		out[i] = c
	}
	return out
}

type next int

const (
	nextEnd next = iota
	nextTools
	nextPause
	nextTruncatedText
	nextTruncatedCalls
)

// classify maps a committed step to what the turn does next; a call cut off by the output cap is never run.
func classify(res *stepResult, calls []message.ToolCallBlock) next {
	truncated := res.stop.Reason == provider.StopMaxTokens || res.stop.Reason == provider.StopContextWindow || res.stop.ReasoningExhausted
	for _, c := range calls {
		if c.InputError == message.InputIncomplete {
			truncated = true
		}
	}
	switch {
	case res.stop.Reason == provider.StopPause:
		return nextPause
	case truncated && len(calls) > 0:
		return nextTruncatedCalls
	case truncated:
		return nextTruncatedText
	case len(calls) > 0:
		return nextTools
	}
	return nextEnd
}

func (t *turn) withinSteps() bool {
	return t.s.limits.MaxSteps < 0 || t.step < t.s.limits.MaxSteps
}

func (t *turn) takeTruncation() bool {
	if t.truncations >= t.s.limits.MaxTruncationContinues {
		return false
	}
	t.truncations++
	return true
}

func continuation(text string) *message.Message {
	return &message.Message{Role: message.RoleUser, Origin: message.Origin{Kind: message.OriginContinuation}, Content: message.Content{&message.ReminderBlock{Source: "continue", Text: text}}}
}

func toolLine(calls []message.ToolCallBlock, text string) *message.Message {
	m := &message.Message{Role: message.RoleTool, Origin: message.Origin{Kind: message.OriginTool}}
	for _, c := range calls {
		m.Content = append(m.Content, notExecuted(c, text))
	}
	return m
}

// noteUnfinished remembers the calls of a tool line that did not complete, for the interruption note.
func (t *turn) noteUnfinished(line *message.Message) {
	for _, b := range line.Content {
		if r, ok := b.(*message.ToolResultBlock); ok && (r.Outcome == message.OutcomeInterrupted || r.Outcome == message.OutcomeNotExecuted) {
			t.unfinished = append(t.unfinished, r.Name+" ("+r.CallID+")")
		}
	}
}

func (t *turn) end(kind Outcome) turnOutcome { return turnOutcome{Kind: kind, stop: t.lastStop} }

func (t *turn) fail(err error) turnOutcome {
	return turnOutcome{Kind: OutcomeError, stop: t.lastStop, err: err}
}

// stopped ends a turn whose context was canceled: Close cancels it, everything else is an interruption.
func (t *turn) stopped(ctx context.Context) turnOutcome {
	cause := context.Cause(ctx)
	if errors.Is(cause, ErrSessionClosing) {
		return turnOutcome{Kind: OutcomeCanceled, stop: t.lastStop, err: cause}
	}
	return turnOutcome{Kind: OutcomeInterrupted, stop: t.lastStop, err: cause}
}

func (t *turn) endErr(ctx context.Context, err error) turnOutcome {
	if ctx.Err() != nil {
		return t.stopped(ctx)
	}
	return t.fail(err)
}

// endBeforeCommit ends a turn before its input reached history, so every item it took is still undelivered and may go back.
func (t *turn) endBeforeCommit(ctx context.Context, items []*queueItem) turnOutcome {
	_ = t.s.exec(func(*state) { t.s.giveBack(items, true) })
	return t.stopped(ctx)
}

// commit runs fn on the actor, where lines are appended and their events published in state order, and persists afterwards, because the store is caller code.
func (t *turn) commit(ctx context.Context, fn func(st *state)) error {
	var unsynced []message.Message
	if err := t.s.exec(func(st *state) {
		fn(st)
		unsynced = st.unsynced()
	}); err != nil {
		return err
	}
	return t.s.persist(ctx, unsynced)
}

// commitDelivery commits the user line of a delivery point; at a safe point it follows a tool line or continuation in the same commit.
func (t *turn) commitDelivery(ctx context.Context, m *message.Message, info *deliveryInfo, items []*queueItem, safePoint bool) error {
	err := t.commit(ctx, func(st *state) { t.appendDelivery(st, m, info, items, safePoint) })
	t.afterTools = false
	return err
}

func (t *turn) appendDelivery(st *state, m *message.Message, info *deliveryInfo, items []*queueItem, safePoint bool) {
	m.Tasks = t.s.taskRefs(st, info.tasks)
	t.s.appendLine(st, m, t.plan.id)
	t.s.publishCommitted(st, m, &lineEvents{step: t.step, delivery: info, safePoint: safePoint})
	t.s.delivered(items)
	for _, it := range items {
		for _, p := range it.parts {
			t.receipts = append(t.receipts, p.receipt)
		}
	}
}

// commitLines commits a tool line or a continuation and, when another model call follows, the safe point's delivery in the same batch; hook context is delivered either way.
func (t *turn) commitLines(ctx context.Context, line *message.Message, due bool) error {
	hold := false // a hosted call still open needs the tool results alone, so this safe point delivers nothing
	if line.Role == message.RoleTool {
		_ = t.s.exec(func(st *state) { hold = awaitsHostedResult(st.history) })
		due = due && !hold
	}
	var items []*queueItem
	if due {
		_ = t.s.exec(func(st *state) { items = st.takeAtSafePoint(t.s.maxAuto()) })
		items = t.submit(ctx, items)
	}
	if ctx.Err() != nil && len(items) > 0 {
		_ = t.s.exec(func(*state) { t.s.giveBack(items, false) }) // not delivered yet, so they stay queued for the next turn
		items = nil
	}
	var pre []*message.ReminderBlock
	if due && ctx.Err() == nil {
		pre = t.s.provide(ctx, AtEveryStep, ProvideContext{Session: t.s, TurnID: t.plan.id, Step: t.step})
	}
	var post []*message.ReminderBlock
	if !hold {
		post = t.sink.take()
	}
	var user *message.Message
	var info deliveryInfo
	if len(items) > 0 || len(pre) > 0 || len(post) > 0 {
		user, info = delivery(items, pre, post)
	}
	err := t.commit(ctx, func(st *state) {
		if line.Role == message.RoleTool {
			line.Tasks = t.s.callRefs(st, line)
		}
		t.s.appendLine(st, line, t.plan.id)
		t.s.publishCommitted(st, line, &lineEvents{step: t.step})
		if user != nil {
			t.appendDelivery(st, user, &info, items, len(items) > 0)
		}
	})
	t.afterTools = line.Role == message.RoleTool
	return err
}

// continueWith commits a continuation that no tool batch produced; that point is a safe point too, so next_loop items still reach the next model call.
func (t *turn) continueWith(ctx context.Context, m *message.Message) error {
	err := t.commitLines(ctx, m, ctx.Err() == nil && t.withinSteps())
	t.afterTools = false
	return err
}

func (t *turn) commitAssistant(ctx context.Context, res *stepResult) error {
	m := res.message
	err := t.commit(ctx, func(st *state) {
		t.s.appendLine(st, m, t.plan.id)
		t.s.publishCommitted(st, m, &lineEvents{step: t.step, attempt: t.attempt})
		t.s.publishAt(&event.StepCompleted{MessageID: m.ID, StopReason: string(res.stop.Reason), RawStopReason: res.stop.Raw, Usage: res.usage, TTFTMs: res.ttft.Milliseconds(), DurationMs: res.duration.Milliseconds()}, t.plan.id, t.step, t.attempt)
		if t.call != nil { // a synthetic step sent no request
			t.call.messageID = m.ID
			t.s.settle(st, t.call, callCommitted, t.plan.id, t.step, t.attempt)
		}
	})
	if t.call != nil {
		t.usage = addUsage(t.usage, t.call.usage)
	}
	t.afterTools = false
	if text := m.Text(); text != "" {
		t.finalText = text
	}
	return err
}

// commitPartial keeps what an interrupted attempt already closed; an attempt that closed nothing is discarded instead (RULE-MSG-001).
func (t *turn) commitPartial(res *stepResult) {
	p := res.partial
	if p == nil || p.Empty() {
		t.publish(&event.MessageDiscarded{MessageID: t.msgID, Reason: "empty"})
		t.settleOpen()
		return
	}
	p.ID = t.msgID
	_ = t.commit(t.s.ctx, func(st *state) { // the turn's context is canceled by now, and these lines must still land
		t.s.appendLine(st, p, t.plan.id)
		t.s.publishCommitted(st, p, &lineEvents{step: t.step, attempt: t.attempt})
		t.s.publishAt(&event.StepCompleted{MessageID: p.ID, Usage: t.call.usage, TTFTMs: res.ttft.Milliseconds(), DurationMs: res.duration.Milliseconds()}, t.plan.id, t.step, t.attempt)
		t.call.messageID = p.ID
		t.s.settle(st, t.call, callCommitted, t.plan.id, t.step, t.attempt)
	})
	t.usage = addUsage(t.usage, t.call.usage)
	t.partial = true
	if text := p.Text(); text != "" {
		t.finalText = text
	}
}

// discard drops a finished attempt's message without committing it.
func (t *turn) discard(res *stepResult, reason string) {
	t.publish(&event.MessageDiscarded{MessageID: t.msgID, Reason: reason, StopDetails: res.stop.Details})
	t.settleOpen()
}

// settleOpen books the open request as discarded when its response had begun, else as failed.
func (t *turn) settleOpen() {
	c := t.call
	if c == nil || c.settled {
		return
	}
	outcome := callFailed
	if c.began {
		outcome = callDiscarded
	}
	_ = t.s.exec(func(st *state) { t.s.settle(st, c, outcome, t.plan.id, t.step, t.attempt) })
	t.usage = addUsage(t.usage, c.usage)
}

// buildRequest builds the step's one request, which every retry reuses, and fires pre_model_request on it; a synthetic reply replaces the model call.
func (t *turn) buildRequest(ctx context.Context) (*provider.Request, *message.Message) {
	var hist []message.Message
	var system []provider.SystemBlock
	var taught learned
	var used int64
	var replay provider.ReplayMode
	_ = t.s.exec(func(st *state) {
		hist = st.history[1:len(st.history):len(st.history)] // committed lines are immutable, so sharing the backing array is safe
		replay = st.replayMode()
		system = st.system
		if l := st.learned[t.model.info]; l != nil {
			taught = *l
		}
		used = st.context.UsedTokens
		st.turn.step = t.step
		t.s.publishSnapshot()
	})
	cfg := t.s.agent.cfg
	req := &provider.Request{
		Model:             t.model.info.Model,
		System:            system,
		Tools:             t.defs,
		HostedTools:       t.hosted,
		Params:            t.model.ref.Params,
		Caps:              t.model.caps,
		CacheKey:          t.s.id,
		PauseTail:         t.pauseTail, // set once per step, so every retry of this step keeps it
		Replay:            replay,
		EmitRaw:           cfg.rawEvents,
		FirstEventTimeout: cfg.retry.FirstEventTimeout,
		IdleTimeout:       cfg.retry.IdleTimeout,
	}
	t.pauseTail = false
	if taught.maxOutput > 0 {
		req.Caps.MaxOutputTokens = taught.maxOutput
	}
	if taught.maxTokensField != "" {
		req.Caps.MaxTokensField = taught.maxTokensField
	}
	req.Caps.ThinkingPrefixBound = req.Caps.ThinkingPrefixBound || taught.prefixBound
	req.Params.Reasoning.Hide = req.Params.Reasoning.Hide || taught.noSummary
	req.Messages = t.withMain(hist, system, req.Caps.ThinkingPrefixBound)
	if v := t.view; v != nil {
		req.SharedPrefix, req.CacheKey = v.shared, t.s.parent.id // the main part is the main session's next prefix, so its cache serves both
		if v.fallback && req.Caps.ThinkingPrefixBound {
			req.Replay = provider.ReplayFallback
		}
	}
	if len(t.s.hookEntries(PreModelRequest.meta)) == 0 {
		return req, nil
	}
	names := make([]string, len(t.defs))
	for i, d := range t.defs {
		names[i] = d.Name
	}
	f := fire(t.hookCtx(ctx, ""), t.s, PreModelRequest, PreModelRequestInput{HookEnv: t.env(), Model: req.Model, Params: req.Params, ToolNames: names, EstTokens: used}, nil)
	req.Params = f.in.Params
	if p := f.out.ParamsPatch; p != nil && p.ToolChoice != nil {
		req.ToolChoice = *p.ToolChoice
	}
	if r := f.out.NextTurnReasoning; r != nil {
		next := *r
		_ = t.s.exec(func(st *state) { st.nextReasoning = &next })
	}
	return req, f.out.Synthetic
}

// stepResult is one attempt's outcome: the final message, or what was closed when it ended early.
type stepResult struct {
	message  *message.Message
	stop     provider.Stop
	usage    message.Usage
	partial  *message.Message
	ttft     time.Duration
	duration time.Duration
}

// callModel runs the attempt loop of one step; retries replay only the model call, so no tool runs twice (I2).
func (t *turn) callModel(ctx context.Context, req *provider.Request) (*stepResult, error) {
	s := t.s
	started := time.Now() // real elapsed time bounds the retries, whatever clock stamps the events
	tried := map[string]bool{}
	recoveries := 0
	for attempt := 1; ; attempt++ {
		res, err := t.streamOnce(ctx, req, attempt)
		if err == nil {
			return res, nil
		}
		if ctx.Err() != nil {
			return res, context.Cause(ctx) // res carries the partial message
		}
		perr := provider.AsError(err)
		if t.call != nil && t.call.requestID == "" {
			t.call.requestID = perr.RequestID // a request that failed before its response still has the API's request id
		}
		t.publish(&event.MessageDiscarded{MessageID: t.msgID, Reason: "retry"})
		t.settleOpen()
		var delay time.Duration
		notices, recovered := t.recover(req, perr, tried)
		if recovered {
			recoveries++ // a recovery changes the request instead of waiting, so it spends none of the transient retry budget
		} else {
			d, ok := s.agent.cfg.retry.Next(attempt-recoveries, perr, time.Since(started), s.jitter)
			if !ok {
				return nil, perr
			}
			delay = d
		}
		for _, n := range notices {
			s.notice(n, t.plan.id, t.step, attempt)
		}
		retry := &event.ModelRetry{Attempt: attempt, Error: errInfo(perr), DelayMs: delay.Milliseconds()}
		_ = s.exec(func(st *state) {
			st.usage.Retries++
			t.s.publishAt(retry, t.plan.id, t.step, attempt)
		})
		if err := sleepCtx(ctx, delay); err != nil {
			return nil, context.Cause(ctx)
		}
	}
}

// recover rewrites the request for the one retry an error allows per step, and keeps what later requests to the model need to know.
func (t *turn) recover(req *provider.Request, e *provider.Error, tried map[string]bool) ([]*event.ModelNotice, bool) {
	notices, ok := t.s.recoverRequest(req, e, tried, t.model.info)
	if ok && req.Caps.ThinkingPrefixBound {
		t.model.caps.ThinkingPrefixBound = true // the retry, and from now on the session, treat the model as prefix-bound
	}
	return notices, ok
}

// recoverRequest is the main loop's and SideCall's shared answer to an error that allows one changed retry: it rewrites req and teaches the session.
func (s *Session) recoverRequest(req *provider.Request, e *provider.Error, tried map[string]bool, info provider.ModelInfo) ([]*event.ModelNotice, bool) {
	key := e.Rule
	if e.Class == provider.ClassReasoningReplay {
		key = string(provider.ClassReasoningReplay)
	}
	if key == "" || tried[key] {
		return nil, false
	}
	var notices []*event.ModelNotice
	note := func(kind string, detail map[string]any) {
		d, _ := json.Marshal(detail)
		notices = append(notices, &event.ModelNotice{Kind: kind, Detail: d})
	}
	var teach func(*learned)
	fallback := false
	switch {
	case e.Class == provider.ClassReasoningReplay:
		req.Replay = provider.ReplayFallback
		fallback = true
		note("reasoning_replay_fallback", map[string]any{"message": e.Message})
		if e.Rule == "thinking_signature_bound" && !req.Caps.ThinkingPrefixBound {
			note("capability_hint", map[string]any{"field": "thinking_prefix_bound"})
			req.Caps.ThinkingPrefixBound = true
			teach = func(l *learned) { l.prefixBound = true }
		}
	case e.Rule == "max_output_too_large" && e.Limit > 0:
		req.Caps.MaxOutputTokens = e.Limit
		teach = func(l *learned) { l.maxOutput = e.Limit }
		note("capability_hint", map[string]any{"field": "max_output_tokens", "value": e.Limit})
	case e.Rule == "max_tokens_field_unsupported" && info.API == provider.APIOpenAIChat:
		field := "max_completion_tokens"
		if req.Caps.MaxTokensField == field {
			field = "max_tokens"
		}
		req.Caps.MaxTokensField = field
		teach = func(l *learned) { l.maxTokensField = field }
		note("capability_hint", map[string]any{"field": "max_tokens_field", "value": field})
	case e.Rule == "reasoning_summary_unavailable" && !req.Params.Reasoning.Hide:
		req.Params.Reasoning.Hide = true
		teach = func(l *learned) { l.noSummary = true }
		note("reasoning_summary_disabled", map[string]any{"message": e.Message})
	default:
		return nil, false
	}
	tried[key] = true
	if teach != nil || fallback {
		_ = s.exec(func(st *state) {
			if fallback {
				st.fallbackEpoch = st.epoch // the refused reasoning stays in this epoch, so every later request of it replays the fallback way
			}
			if teach == nil {
				return
			}
			l := st.learned[info]
			if l == nil {
				l = &learned{}
				st.learned[info] = l
			}
			teach(l)
			if l.prefixBound && st.model.info == info {
				st.model.caps.ThinkingPrefixBound = true // tool freezing, compaction and side calls follow it
			}
		})
	}
	return notices, true
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-tm.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// streamOnce consumes one attempt through a bounded read-ahead; bulk events wait for backpressure credit, which stalls this goroutine and never the actor.
func (t *turn) streamOnce(ctx context.Context, req *provider.Request, attempt int) (*stepResult, error) {
	s := t.s
	clock := s.agent.cfg.clock
	t.attempt = attempt
	t.msgID = s.agent.cfg.ids.NewID("msg")
	begin := clock.Now()
	t.call = &modelCall{model: t.model.info, bucket: "main", started: begin, main: t.view}
	t.publish(&event.StepStarted{MessageID: t.msgID, API: string(t.model.info.API), Model: req.Model, Attempt: attempt})
	proj := newProjector(t.msgID, t.model.info)
	proj.family = req.Caps.Family
	res := &stepResult{}
	actx, cancel := context.WithCancel(ctx)
	defer cancel() // ends the read-ahead and closes the body on every exit
	early := func(err error) (*stepResult, error) {
		res.partial = proj.partial()
		res.duration = clock.Now().Sub(begin)
		return res, err
	}
	sent := *req // each attempt gets its own copy, since a recovery rewrites the request for the next one
	sent.Params = s.requestHeaders(sent.Params, t.plan.id, "main", t.model.info)
	sent.CaptureHeaders = s.agent.cfg.captureHeaders
	for sev, err := range readAhead(actx, s.wk, "readahead "+t.msgID, t.model.ref.Provider.Stream(actx, &sent), s.limits.ReadAhead) {
		if err != nil {
			return early(err)
		}
		t.call.observe(sev)
		if proj.bulk(sev) {
			if err := s.bus.WaitWritable(ctx); err != nil {
				return early(err)
			}
		}
		payloads, content := proj.project(sev, req.EmitRaw)
		if content && res.ttft == 0 {
			res.ttft = clock.Now().Sub(begin)
		}
		for _, p := range payloads {
			if n, ok := p.(*event.ModelNotice); ok {
				s.notice(n, t.plan.id, t.step, t.attempt)
				continue
			}
			t.publish(p)
		}
		if sev.Kind == provider.SEEnd {
			if sev.Message == nil || sev.Stop == nil || sev.Usage == nil { // adapter contract breach: fail the attempt instead of panicking
				return early(&provider.Error{Class: provider.ClassTransient, Message: "incomplete terminal event"})
			}
			m := *sev.Message
			m.ID = t.msgID
			res.message, res.stop, res.usage = &m, *sev.Stop, *sev.Usage
			res.duration = clock.Now().Sub(begin)
			return res, nil
		}
	}
	if ctx.Err() != nil {
		return early(context.Cause(ctx))
	}
	return early(&provider.Error{Class: provider.ClassTransient, Message: "stream ended without a terminal event"})
}

// observe keeps the request's usage and whether its response began, so it settles correctly whichever way it ends.
func (c *modelCall) observe(sev provider.StreamEvent) {
	switch sev.Kind {
	case provider.SEStart:
		c.began = true
		c.responseID, c.requestID, c.headers = sev.ResponseID, sev.RequestID, sev.Headers
	case provider.SEUsage, provider.SEEnd:
		if sev.Usage != nil {
			c.usage = *sev.Usage
		}
	}
}

// readAhead decouples the provider stream from the consumer by n events; it ends with ctx, the stream or the consumer.
func readAhead(ctx context.Context, wk *workers.Set, name string, seq iter.Seq2[provider.StreamEvent, error], n int) iter.Seq2[provider.StreamEvent, error] {
	if n <= 0 {
		return seq
	}
	return func(yield func(provider.StreamEvent, error) bool) {
		type item struct {
			ev  provider.StreamEvent
			err error
		}
		ch := make(chan item, n)
		stop := make(chan struct{})
		wk.Go(name, func() {
			defer close(ch)
			for ev, err := range seq {
				select {
				case ch <- item{ev, err}:
				case <-stop:
					return
				case <-ctx.Done():
					return
				}
				if err != nil {
					return
				}
			}
		})
		defer close(stop)
		for it := range ch {
			if !yield(it.ev, it.err) {
				return
			}
		}
	}
}

// projector turns stream events into public events and keeps what an interrupted attempt may still commit.
type projector struct {
	msgID  string
	model  provider.ModelInfo
	family string // the request's declared family, which a partial records as the adapters record it on a whole message
	start  provider.StreamEvent
	began  bool
	blocks map[int]*projBlock
	order  []int
}

type projBlock struct {
	typ    message.BlockType
	kind   message.ReasoningKind
	text   strings.Builder
	closed message.Block
}

func newProjector(msgID string, model provider.ModelInfo) *projector {
	return &projector{msgID: msgID, model: model, blocks: map[int]*projBlock{}}
}

func (p *projector) bulk(sev provider.StreamEvent) bool {
	switch sev.Kind {
	case provider.SETextDelta, provider.SEReasoningDelta, provider.SEToolArgsDelta, provider.SERaw:
		return true
	}
	return false
}

func (p *projector) block(i int) *projBlock {
	b, ok := p.blocks[i]
	if !ok {
		b = &projBlock{}
		p.blocks[i] = b
		p.order = append(p.order, i)
	}
	return b
}

// project returns the events for sev and whether it carried content, which is what time-to-first-token measures.
func (p *projector) project(sev provider.StreamEvent, raw bool) ([]event.Payload, bool) {
	switch sev.Kind {
	case provider.SEStart:
		p.began, p.start = true, sev
		return []event.Payload{&event.MessageStarted{MessageID: p.msgID, Role: message.RoleAssistant}}, false
	case provider.SEBlockStart:
		b := p.block(sev.Index)
		b.typ, b.kind = sev.BlockType, sev.ReasoningKind
		switch sev.BlockType {
		case message.TypeReasoning:
			return []event.Payload{&event.ReasoningStarted{MessageID: p.msgID, BlockIndex: sev.Index, Kind: sev.ReasoningKind}}, true
		case message.TypeToolCall:
			return []event.Payload{&event.ToolCallStarted{MessageID: p.msgID, BlockIndex: sev.Index, CallID: sev.ToolCallID, Name: sev.ToolName}}, true
		}
		return nil, true
	case provider.SETextDelta:
		b := p.block(sev.Index)
		if b.typ == "" {
			b.typ = message.TypeText
			if sev.Channel == "refusal" {
				b.typ = message.TypeRefusal
			}
		}
		b.text.WriteString(sev.Text)
		channel := sev.Channel
		if channel == "" {
			channel = "text"
		}
		return []event.Payload{&event.MessageDelta{MessageID: p.msgID, BlockIndex: sev.Index, Channel: channel, Text: sev.Text}}, true
	case provider.SEReasoningDelta:
		b := p.block(sev.Index)
		b.text.WriteString(sev.Text)
		return []event.Payload{&event.ReasoningDelta{MessageID: p.msgID, BlockIndex: sev.Index, Kind: sev.ReasoningKind, Text: sev.Text}}, true
	case provider.SEToolArgsDelta:
		return []event.Payload{&event.ToolCallDelta{MessageID: p.msgID, CallID: sev.ToolCallID, ArgsDelta: sev.Text}}, true
	case provider.SEBlockStop:
		p.block(sev.Index).closed = sev.Block
		switch b := sev.Block.(type) {
		case *message.ReasoningBlock:
			return []event.Payload{&event.ReasoningCompleted{MessageID: p.msgID, BlockIndex: sev.Index, Kind: b.Kind, Text: b.Text, Replayable: b.Native != nil}}, true
		case *message.ToolCallBlock:
			return []event.Payload{&event.ToolCallCompleted{MessageID: p.msgID, CallID: b.ID, Name: b.Name, Input: b.Input, InputError: b.InputError}}, true
		case *message.OpaqueBlock:
			return []event.Payload{&event.MessageBlock{MessageID: p.msgID, BlockIndex: sev.Index, BlockType: string(message.TypeOpaque), Opaque: &event.OpaqueInfo{Kind: b.Kind, Text: b.Text}}}, true
		case *message.HostedCallBlock:
			return []event.Payload{&event.MessageBlock{MessageID: p.msgID, BlockIndex: sev.Index, BlockType: string(message.TypeHostedCall), HostedCall: publicBlock(b).(*message.HostedCallBlock)}}, true
		case *message.HostedResultBlock:
			return []event.Payload{&event.MessageBlock{MessageID: p.msgID, BlockIndex: sev.Index, BlockType: string(message.TypeHostedResult), HostedResult: publicBlock(b).(*message.HostedResultBlock)}}, true
		}
		return nil, true
	case provider.SENotice:
		if sev.Notice != nil {
			return []event.Payload{&event.ModelNotice{Kind: sev.Notice.Kind, Detail: sev.Notice.Detail}}, false
		}
	case provider.SERaw:
		if raw {
			return []event.Payload{&event.ProviderRaw{API: string(p.model.API), Event: sev.Text, Data: sev.Raw}}, false
		}
	}
	return nil, false
}

// partial is the message an interrupted attempt leaves: closed blocks, plus text and raw reasoning cut where the stream stopped; thinking, calls and opaque blocks count only once closed.
func (p *projector) partial() *message.Message {
	if !p.began {
		return nil
	}
	m := &message.Message{
		Role:     message.RoleAssistant,
		Origin:   message.Origin{Kind: message.OriginModel},
		Producer: &message.Producer{API: string(p.model.API), Endpoint: p.model.Endpoint, Model: p.model.Model, ResponseID: p.start.ResponseID, RequestID: p.start.RequestID, Headers: p.start.Headers},
		Partial:  true,
	}
	if p.family != "" && p.family != p.model.Model {
		m.Producer.Family = p.family
	}
	for _, i := range p.order {
		b := p.blocks[i]
		switch {
		case b.closed != nil:
			if tb, ok := b.closed.(*message.TextBlock); ok && tb.Text == "" && tb.Native == nil {
				continue
			}
			m.Content = append(m.Content, b.closed)
		case b.typ == message.TypeText && b.text.Len() > 0:
			m.Content = append(m.Content, &message.TextBlock{Text: b.text.String()})
		case b.typ == message.TypeRefusal && b.text.Len() > 0:
			m.Content = append(m.Content, &message.RefusalBlock{Text: b.text.String()})
		case b.typ == message.TypeReasoning && b.kind == message.ReasoningRaw && b.text.Len() > 0:
			m.Content = append(m.Content, &message.ReasoningBlock{Kind: message.ReasoningRaw, Text: b.text.String()})
		}
	}
	return m
}

// runTools executes one batch: the gate settles what must not run, then the rest run in call-order groups, adjacent parallel calls together; true means a hook or reviewer ends the turn after this batch.
func (t *turn) runTools(ctx context.Context, calls []message.ToolCallBlock) ([]message.ToolResultBlock, bool) {
	s := t.s
	results, runnable, index, stop := t.gate(ctx, calls)
	r := &toolRunner{s: s, tools: t.tools, turnCtx: ctx, turnID: t.plan.id, step: t.step, attempt: t.attempt, meta: t.meta, starts: map[string]time.Time{}, groups: map[string]int{}, sinks: map[string]*hookSink{}}
	gated := runnable
	runnable, index = r.divert(ctx, results, runnable, index)
	for _, g := range toolexec.Partition(runnable, r.Parallel) {
		for _, k := range g {
			r.groups[runnable[k].ID] = index[g[0]] // a group is named by the batch position of its first call
		}
	}
	ran := toolexec.Run(ctx, r, runnable, toolexec.Options{
		MaxParallel: s.limits.MaxParallelTools,
		Grace:       s.limits.ToolCancelGrace,
		Timeout:     r.timeout,
		Movable:     func(c message.ToolCallBlock) bool { return r.tools[c.Name].spec.Background },
		OnAbandon: func(c message.ToolCallBlock) {
			s.movable.Delete(c.ID) // a tool that never returns would hold its batch in the map for the session's lifetime
			s.post(func(st *state) { st.usage.Abandons++ })
			t.publish(&event.ToolAbandoned{CallID: c.ID, Name: c.Name, GraceMs: s.limits.ToolCancelGrace.Milliseconds()})
		},
	})
	for i, res := range ran {
		results[index[i]] = res
	}
	for _, c := range gated { // in call order, so parallel hooks hand their context over in a fixed order
		r.mu.Lock()
		k := r.sinks[c.ID]
		r.mu.Unlock()
		t.sink.absorb(k)
	}
	return results, stop || r.stopTurn.Load()
}

// gate settles every call of a batch before any call runs, so no verdict depends on another call's result and reviewers see the whole batch at once: normalization, pre_tool_use, the approval rules, then one concurrent review of every ask.
func (t *turn) gate(ctx context.Context, calls []message.ToolCallBlock) (results []message.ToolResultBlock, runnable []message.ToolCallBlock, index []int, stop bool) {
	s := t.s
	results = make([]message.ToolResultBlock, len(calls))
	done := make([]bool, len(calls))
	_ = s.exec(func(st *state) {
		for i, c := range calls {
			if r, ok := st.executed[c.ID]; ok { // I2 defense in depth: a call id that already ran reuses its result
				results[i], done[i] = *r, true
				s.publishAt(&event.Error{Code: "duplicate_call", Message: "call " + c.ID + " already has a result"}, t.plan.id, t.step, t.attempt)
			}
		}
	})
	ready := make([]*message.ToolCallBlock, len(calls))
	var settled []int // denied or synthetic here, so post_tool_use still sees them
	var asks []askCall
	for i, c := range calls {
		if done[i] {
			continue
		}
		if ctx.Err() != nil {
			ready[i] = &c // runs as not started, because the batch was interrupted
			continue
		}
		switch {
		case s.allowed != nil && !s.allowed[c.Name]: // ForkTools: refused before any hook, rule or review
			results[i] = kernelResult(c, message.OutcomeDenied, prompt.Textf("tool_not_allowed", map[string]string{"name": c.Name}))
			continue
		case t.tools[c.Name].tool == nil:
			results[i] = kernelResult(c, message.OutcomeUnknownTool, prompt.Textf("unknown_tool", map[string]string{"name": c.Name}))
			continue
		case c.InputError == message.InputInvalidJSON || c.InputError == message.InputNotObject:
			raw := c.RawInput
			if raw == "" {
				raw = string(c.Input)
			}
			results[i] = kernelResult(c, message.OutcomeInvalidInput, prompt.Textf("invalid_input", map[string]string{"raw": raw}))
			continue
		}
		sc := s.screen(ctx, t.hookCtx(ctx, c.ID), t.env(), c, t.tools[c.Name].spec)
		switch sc.kind {
		case screenInvalid:
			results[i] = sc.result
		case screenSettled:
			results[i] = sc.result
			settled = append(settled, i)
		case screenAsk:
			sc.ask.index = i
			asks = append(asks, sc.ask)
		default:
			ready[i] = &sc.call
		}
	}
	if len(asks) > 0 && ctx.Err() == nil {
		outs, stopped := t.review(ctx, asks)
		stop = stopped
		for k, a := range asks {
			if o := outs[k]; o.approved {
				c := a.call
				c.Input = o.input
				ready[a.index] = &c
			} else {
				results[a.index] = o.result
				if o.result.Outcome == message.OutcomeDenied {
					settled = append(settled, a.index)
				}
			}
		}
	} else {
		for _, a := range asks {
			c := a.call
			ready[a.index] = &c // interrupted before review, so they run as not started
		}
	}
	slices.Sort(settled)
	for _, i := range settled {
		res, stopTurn := postToolUse(t.hookCtx(ctx, calls[i].ID), t.s, t.env(), calls[i], results[i], 0)
		results[i], stop = res, stop || stopTurn
	}
	for i, c := range ready {
		if c != nil {
			runnable = append(runnable, *c)
			index = append(index, i)
		}
	}
	return results, runnable, index, stop
}

type screenKind int

const (
	screenRun         screenKind = iota // goes on to run
	screenInterrupted                   // the context ended on the way, so it runs as not started
	screenAsk                           // waits for a reviewer
	screenSettled                       // a hook or rule answered it, and post_tool_use still sees that
	screenInvalid                       // the input is unusable, which is no execution of the tool
)

type screened struct {
	kind   screenKind
	call   message.ToolCallBlock // with the input rules and reviewers see
	result message.ToolResultBlock
	ask    askCall
}

// screen takes one call through normalization, pre_tool_use and the approval rules, the part of the gate that needs no reviewer.
func (s *Session) screen(ctx, hctx context.Context, env HookEnv, c message.ToolCallBlock, spec ToolSpec) screened {
	in, err := normalizeInput(c.Input)
	if err != nil {
		return screened{kind: screenInvalid, result: kernelResult(c, message.OutcomeInvalidInput, prompt.Textf("invalid_input_detail", map[string]string{"detail": err.(*InputError).Detail}))}
	}
	c.Input = in
	c.Native = nil
	f := fire(hctx, s, PreToolUse, PreToolUseInput{HookEnv: env, Call: c, Spec: spec}, nil)
	if ctx.Err() != nil {
		return screened{kind: screenInterrupted, call: c}
	}
	if sr := f.out.SyntheticResult; sr != nil {
		return screened{kind: screenSettled, result: message.ToolResultBlock{CallID: c.ID, Name: c.Name, IsError: sr.IsError, Outcome: message.OutcomeSynthetic, Content: escapeContent(sr.Content)}}
	}
	if f.out.Decision == Deny {
		return screened{kind: screenSettled, result: kernelResult(c, message.OutcomeDenied, prompt.Textf("denied_by_hook", map[string]string{"reason": f.out.Reason}))}
	}
	final, err := normalizeInput(f.in.Call.Input)
	if err != nil {
		return screened{kind: screenSettled, result: kernelResult(c, message.OutcomeDenied, prompt.Textf("denied_by_hook", map[string]string{"reason": "policy hook failed (invalid input)"}))}
	}
	c.Input = final
	v := decide(s.rules, ToolCallView{CallID: c.ID, Name: c.Name, Input: final})
	if v.deny != nil {
		return screened{kind: screenSettled, result: kernelResult(c, message.OutcomeDenied, prompt.Textf("denied_by_rule", map[string]string{"id": v.deny.ID, "reason": v.deny.Reason}))}
	}
	if f.out.Decision == Ask || v.action == RuleAsk { // the strictest of hook and rules: neither side's allow lifts the other's ask
		a := askCall{call: c, matched: v.matched}
		for _, o := range f.outs {
			if o.out.Decision == Ask && o.out.Reason != "" {
				a.hookReasons = append(a.hookReasons, o.out.Reason)
			}
		}
		return screened{kind: screenAsk, ask: a}
	}
	return screened{kind: screenRun, call: c}
}

// postToolUse lets hooks see and rewrite a result before it is committed; after an interrupt the result stays as it is. It takes env instead of reading the turn, because a tool worker may outlive its step.
func postToolUse(ctx context.Context, s *Session, env HookEnv, call message.ToolCallBlock, res message.ToolResultBlock, took time.Duration) (message.ToolResultBlock, bool) {
	call.Native = nil
	f := fire(ctx, s, PostToolUse, PostToolUseInput{HookEnv: env, Call: call, Result: res, DurationMS: took.Milliseconds()}, nil)
	return f.in.Result, f.out.StopTurn
}

// escapeContent neutralizes runtime tags in text a hook supplied, as for any tool result.
func escapeContent(c message.Content) message.Content {
	out := message.Content{}
	for _, b := range c {
		if tb, ok := b.(*message.TextBlock); ok {
			b = &message.TextBlock{Text: prompt.EscapeRuntimeTags(tb.Text), Native: tb.Native}
		}
		out = append(out, b)
	}
	return out
}

func kernelResult(c message.ToolCallBlock, outcome message.Outcome, text string) message.ToolResultBlock {
	return message.ToolResultBlock{CallID: c.ID, Name: c.Name, IsError: true, Outcome: outcome, Content: message.Content{&message.TextBlock{Text: prompt.EscapeRuntimeTags(text)}}}
}

// toolRunner fixes a batch's attribution when the batch starts, because an abandoned worker may outlive the step that started it.
type toolRunner struct {
	s        *Session
	stopTurn atomic.Bool // a post_tool_use hook asked to end the turn after this batch
	tools    map[string]frozenTool
	turnCtx  context.Context
	turnID   string
	step     int
	attempt  int
	meta     map[string]string
	mu       sync.Mutex
	clockMu  sync.Mutex // pairs the clock reads of one start or completion
	starts   map[string]time.Time
	groups   map[string]int
	sinks    map[string]*hookSink // per call, merged in call order after the batch
	cut      sync.Map             // call id → text length before capText, for tool.completed
}

func (r *toolRunner) Go(name string, fn func()) { r.s.wk.Go(name, fn) }

func (r *toolRunner) Parallel(name string) bool { return r.tools[name].spec.Concurrency == Parallel }

func (r *toolRunner) publish(p event.Payload) { r.s.publishAt(p, r.turnID, r.step, r.attempt) }

func (r *toolRunner) timeout(c message.ToolCallBlock) time.Duration {
	if d := r.tools[c.Name].spec.Timeout; d != 0 {
		return d
	}
	return r.s.limits.ToolTimeout
}

// RunOne runs on the tool worker: tool.started, the call with the tool's marker in ctx, and the conversion of its return values.
func (r *toolRunner) RunOne(ctx context.Context, call message.ToolCallBlock) message.ToolResultBlock {
	ft := r.tools[call.Name]
	began := time.Now() // real time, so timing a hook's input never moves the event clock
	sink := &hookSink{}
	r.mu.Lock()
	r.sinks[call.ID] = sink
	r.mu.Unlock()
	scope := &hookScope{turnID: r.turnID, step: r.step, attempt: r.attempt, callID: call.ID, sink: sink}
	tctx := withMarker(withHookScope(ctx, scope), marker{chain: r.s.chain, kind: "tool", call: call.ID})
	tc := &ToolContext{s: r.s, ctx: tctx, callID: call.ID, turnID: r.turnID, step: r.step, attempt: r.attempt, meta: r.meta}
	tc.live.Store(true)
	input := append(json.RawMessage(nil), call.Input...)
	if ft.spec.Background {
		_, input, _ = takeBackground(input) // the tool never sees the switch, on this path or in a tool_exec run
	}
	var mc *movableCall
	if mv := toolexec.MoveOf(ctx); mv != nil {
		mc = r.s.offerMove(r, call, input, ft, tc, tctx, scope, mv, began) // offered before tool.started, so a host that reacts to it finds the call
	}
	// A task's child session is not the host's to move calls from.
	movable := r.s.owner == nil && (mc != nil || waitsForTask(ft.spec, call.Input))
	r.clockMu.Lock() // the start time and the event take adjacent clock reads, so parallel workers cannot interleave them
	r.mu.Lock()
	r.starts[call.ID] = r.s.agent.cfg.clock.Now()
	group := r.groups[call.ID]
	r.mu.Unlock()
	r.publish(&event.ToolStarted{CallID: call.ID, Name: call.Name, Input: call.Input, Group: group, Movable: movable})
	r.clockMu.Unlock()
	if mc != nil {
		close(mc.started)
	}
	res, err := callTool(tctx, ft.tool, tc, input)
	tc.live.Store(false)
	if mc != nil && mc.returned(res, err) {
		return message.ToolResultBlock{} // the batch settled the call when it moved, so this is dropped
	}
	out := r.convert(ctx, call, res, err)
	out, stop := postToolUse(withHookScope(r.turnCtx, scope), r.s, r.s.env(r.turnID, r.step), call, out, time.Since(began))
	if stop {
		r.stopTurn.Store(true)
	}
	return out
}

func callTool(ctx context.Context, tool Tool, tc *ToolContext, input json.RawMessage) (res ToolResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return tool.Call(ctx, tc, input)
}

// convert turns a tool's return values into the committed result; an error after an interrupt or a timeout is reported as that, not as the tool's own words.
func (r *toolRunner) convert(ctx context.Context, call message.ToolCallBlock, res ToolResult, err error) message.ToolResultBlock {
	var ie *InputError
	switch {
	case err == nil:
	case errors.As(err, &ie):
		return kernelResult(call, message.OutcomeInvalidInput, prompt.Textf("invalid_input_detail", map[string]string{"detail": ie.Detail}))
	case r.turnCtx.Err() != nil:
		return toolexec.Interrupted(call)
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return toolexec.TimedOut(call, r.timeout(call))
	default:
		return message.ToolResultBlock{CallID: call.ID, Name: call.Name, IsError: true, Outcome: message.OutcomeError, Content: message.Content{&message.TextBlock{Text: prompt.EscapeRuntimeTags(err.Error())}}}
	}
	out := message.ToolResultBlock{CallID: call.ID, Name: call.Name, IsError: res.IsError, Outcome: message.OutcomeSuccess, Pin: res.Pin}
	if res.IsError {
		out.Outcome = message.OutcomeError
	}
	for _, b := range res.Content {
		if tb, ok := b.(*message.TextBlock); ok {
			b = &message.TextBlock{Text: prompt.EscapeRuntimeTags(tb.Text), Native: tb.Native}
		}
		out.Content = append(out.Content, b)
	}
	if capped, total := capText(out.Content, r.s.limits.MaxToolResultChars); total > 0 {
		out.Content = capped
		r.cut.Store(call.ID, total)
	}
	if out.Content == nil {
		out.Content = message.Content{}
	}
	if res.Structured != nil {
		out.Structured, _ = json.Marshal(res.Structured)
	}
	return out
}

func (r *toolRunner) Completed(call message.ToolCallBlock, res message.ToolResultBlock) {
	r.mu.Lock()
	started := r.starts[call.ID]
	r.mu.Unlock()
	pub := publicMessage(message.Message{Content: res.Content})
	r.clockMu.Lock()
	r.publish(&event.ToolCompleted{CallID: call.ID, Name: call.Name, Outcome: res.Outcome, IsError: res.IsError, Content: pub.Content, Structured: res.Structured, DurationMs: r.s.agent.cfg.clock.Now().Sub(started).Milliseconds(), OriginalChars: r.cutChars(call.ID)})
	r.clockMu.Unlock()
}

// cutChars is the length of the call's text before capText cut it, or 0.
func (r *toolRunner) cutChars(callID string) int {
	n, _ := r.cut.LoadAndDelete(callID)
	total, _ := n.(int)
	return total
}

// capText keeps the head and the tail of a result's text past limit and says how much was left out, so one huge output can neither fill the context nor leave it impossible to compact; total is the text's length when it cut, else 0.
func capText(c message.Content, limit int) (out message.Content, total int) {
	for _, b := range c {
		if tb, ok := b.(*message.TextBlock); ok {
			total += utf8.RuneCountInString(tb.Text)
		}
	}
	if limit < 0 || total <= limit {
		return c, 0
	}
	head := limit / 2
	tailStart := total - (limit - head)
	note := &message.TextBlock{Text: prompt.Textf("tool_result_truncated", map[string]string{"omitted": strconv.Itoa(total - limit), "total": strconv.Itoa(total)})}
	pos, noted := 0, false
	for _, b := range c {
		tb, ok := b.(*message.TextBlock)
		if !ok {
			out = append(out, b)
			continue
		}
		r := []rune(tb.Text)
		start := pos
		pos += len(r)
		if h := min(max(head-start, 0), len(r)); h > 0 {
			out = append(out, &message.TextBlock{Text: string(r[:h])})
		}
		if !noted && pos > head {
			out, noted = append(out, note), true
		}
		if t := min(max(tailStart-start, 0), len(r)); t < len(r) {
			out = append(out, &message.TextBlock{Text: string(r[t:])})
		}
	}
	return out, total
}

// finish is I1's first safety net: it settles an open request, repairs the tail and persists, on the session's context because the turn's may be canceled.
func (t *turn) finish(out turnOutcome) turnOutcome {
	s := t.s
	var unsynced []message.Message
	_ = s.exec(func(st *state) {
		if c := t.call; c != nil && !c.settled {
			outcome := callFailed
			if c.began {
				outcome = callDiscarded
			}
			s.settle(st, c, outcome, t.plan.id, t.step, t.attempt)
			t.usage = addUsage(t.usage, c.usage)
		}
		if repaired := st.repairTail(out.Kind); repaired != nil {
			s.appendLine(st, repaired, t.plan.id)
			s.publishCommitted(st, repaired, &lineEvents{step: t.step})
			t.noteUnfinished(repaired)
		}
		unsynced = st.unsynced()
	})
	_ = s.persist(s.ctx, unsynced)
	out.steps, out.usage, out.finalText, out.started, out.receipts, out.clock = t.step, t.usage, t.finalText, t.started, t.receipts, s.agent.cfg.clock
	if out.Kind == OutcomeInterrupted && t.committed {
		out.note = t.interruptedNote()
	}
	var notes []*message.ReminderBlock
	if t.sink != nil {
		notes = t.sink.take() // hook context no delivery point took rides the next turn
	}
	// the session's context, because the turn's may be canceled by now, under one budget for every hook so slow ones cannot hold the turn open in turn
	end := &hookSink{}
	hctx, cancel := context.WithTimeout(withHookScope(s.ctx, &hookScope{turnID: t.plan.id, step: t.step, sink: end}), TurnEnd.def.Timeout)
	fire(hctx, s, TurnEnd, TurnEndInput{HookEnv: t.env(), Result: out.result(t.plan.id)}, nil)
	cancel()
	for _, n := range append(notes, end.take()...) {
		s.post(func(*state) {
			it := s.reminderItem(n.Source, n.Text, false)
			it.tail = true
			s.enqueue(it)
		})
	}
	return out
}

// interruptedNote is a reminder that never wakes, so the next turn learns the previous one was cut off and which calls did not complete.
func (t *turn) interruptedNote() *queueItem {
	vars := map[string]string{"calls": strings.Join(t.unfinished, ", ")}
	if t.partial {
		vars["partial"] = "yes"
	}
	id := t.s.agent.cfg.ids.NewID("in")
	origin := message.Origin{Kind: message.OriginReminder, Source: "interrupted"}
	return &queueItem{id: id, origin: origin, mode: ModeNextLoop, tail: true, parts: []*itemPart{{id: id, origin: origin, text: prompt.Textf("interrupted_note", vars), receipt: t.s.newReceipt(id)}}}
}
