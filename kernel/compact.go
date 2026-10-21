package kernel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/internal/prompt"
	"github.com/estonshi/aperture-potatos/internal/tokens"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
)

// CompactTrigger says why a compaction runs, so a strategy and the pre_compact hook can treat a forced one differently.
type CompactTrigger string

const (
	TriggerManual   CompactTrigger = "manual"   // Compact or /compact
	TriggerAuto     CompactTrigger = "auto"     // the context crossed the automatic threshold
	TriggerOverflow CompactTrigger = "overflow" // the API rejected the request as too long; a hook cannot block it
	TriggerSwitch   CompactTrigger = "switch"   // SwitchModel to a model whose window the context does not fit
)

// Compactor is a compaction strategy; return ErrNothingToCompact when the request leaves too little to summarize.
type Compactor interface {
	Compact(ctx context.Context, req CompactRequest) (CompactResult, error)
}

// CompactRequest is everything a strategy needs to compact without touching the session itself.
type CompactRequest struct {
	Trigger      CompactTrigger
	Instructions string            // per-call instructions (/compact arguments or Session.Compact) after pre_compact
	History      []message.Message // the current context without its system line but with its epoch head, so the earlier summary is summarized; Summarize with nil msgs sends exactly these lines as its cached prefix
	HeadLen      int               // leading History lines that form the epoch head, which the kernel rebuilds and Replace must not contain
	PrevSummary  string            // summary this epoch started from, "" when there is none, so a strategy can carry it forward
	CurrentTurn  int               // index into History where the running turn starts, -1 when idle
	Range        CompactRange      // which part to summarize; the zero value is all of it
	Tokens       int64             // the context's estimate; for a side chat its own lines only, since no compaction of its own shrinks the main part
	Target       int64             // summary token budget, clamped as in runtime.md §12.1, so the model is told one number and the call is capped near it
	Threshold    int64             // where automatic compaction starts, less a side chat's main part, so a strategy can tell whether a cheaper result leaves room
	Caps         provider.Capabilities
	Estimate     func(msgs []message.Message) int64
	Summarize    Summarizer // bound to the current model; same system, tools and tool_choice
}

// CompactRange limits a manual compaction to one side of a turn boundary.
type CompactRange struct {
	Kind string // "" (all), "from" or "up_to"
	Cut  int    // index into History of the first line of the chosen turn
}

// Summarizer runs one SideCall ending in instructions and drops any tool call; nil msgs sends the whole current context through IncludeHistory so the main loop's cached prefix is reused, other msgs go as appended messages with no cache claim.
type Summarizer func(ctx context.Context, msgs []message.Message, instructions string) (string, message.Usage, error)

// CompactResult is checked by the kernel before the new epoch is written, and an invalid one fails the compaction.
type CompactResult struct {
	Replace  []message.Message // lines of History kept after the new epoch head, verbatim or with tool results cleared (runtime.md §12.3); nil keeps none
	Summary  string
	Strategy string // "summarize" for ext/compact; free-form for custom compactors, recorded in the boundary
}

var (
	ErrNothingToCompact = errors.New("nothing to compact")                              // the strategy found too little to summarize, which is not counted as a failure
	ErrNoCompactor      = errors.New("no compactor registered")                         // install std.Defaults() or register a Compactor
	ErrUnknownTurn      = errors.New("turn is not in the current epoch after its head") // also from Rewind and ForkAt, which reach only the current epoch
)

// CompactReport is what one finished compaction did, by the estimates before and after.
type CompactReport struct {
	Strategy   string `json:"strategy"`
	PreTokens  int64  `json:"pre_tokens"`
	PostTokens int64  `json:"post_tokens"`
	Epoch      int    `json:"epoch"`
}

// CompactOption limits a manual compaction to part of the current epoch.
type CompactOption func(*compactConfig)

type compactConfig struct {
	kind, turnID string
}

// CompactFrom keeps the context before turnID verbatim and summarizes that turn and everything after it.
func CompactFrom(turnID string) CompactOption {
	return func(c *compactConfig) { c.kind, c.turnID = "from", turnID }
}

// CompactUpTo summarizes the context before turnID and keeps that turn and everything after it verbatim.
func CompactUpTo(turnID string) CompactOption {
	return func(c *compactConfig) { c.kind, c.turnID = "up_to", turnID }
}

// Compact is a waiting primitive: it runs inline when called by a history-token holder (a WaitIdle command), queues otherwise.
func (s *Session) Compact(ctx context.Context, instructions string, opts ...CompactOption) (CompactReport, error) {
	var cc compactConfig
	for _, o := range opts {
		o(&cc)
	}
	var rep CompactReport
	err := s.historyOp(ctx, "compact", func(ctx context.Context) error {
		r, err := s.compact(ctx, TriggerManual, instructions, cc, nil)
		rep = r
		return err
	})
	return rep, err
}

// compactor is the session's registered Compactor, shadowing the agent's.
func (s *Session) compactor() Compactor {
	if r := s.reg.state().compactor; r != nil {
		return r.c
	}
	if r := s.outer().compactor; r != nil {
		return r.c
	}
	return nil
}

// compaction is what one compaction reads from the actor before it runs, so the compactor never touches session state.
type compaction struct {
	trigger     CompactTrigger
	history     []message.Message // current epoch, system line first
	pre         int64
	win         window
	caps        provider.Capabilities
	prefixBound bool
	prev        string
	headLen     int
	current     int
	rng         CompactRange
	rngTurn     string
	pins        []Pin
	epoch       int
	scale       func(heuristic int64) int64 // the session's calibration, with the system and tools added
	main        int64                       // a side chat's main part, which no compaction of its own can shrink
}

// compact runs the kernel side of runtime.md §12.3 on the history-token holder; t is the running turn for auto and overflow compaction, nil otherwise.
func (s *Session) compact(ctx context.Context, trigger CompactTrigger, instructions string, cc compactConfig, t *turn) (CompactReport, error) {
	comp := s.compactor()
	if comp == nil {
		return CompactReport{}, ErrNoCompactor
	}
	turnID := ""
	if t != nil {
		turnID = t.plan.id
	}
	var c compaction
	if err := s.exec(func(st *state) {
		meter, base := st.meter, st.base()
		c = compaction{trigger: trigger, history: st.history, pre: st.estimate(), win: st.policy.window(st.model.caps, st.model.ref.Params.MaxOutputTokens),
			caps: st.model.caps, prefixBound: st.model.caps.ThinkingPrefixBound, current: -1, pins: slices.Clone(st.pins), epoch: st.epoch, main: st.mainTokens,
			scale: func(h int64) int64 { return meter.Scale(base + h) }}
	}); err != nil {
		return CompactReport{}, err
	}
	hist := c.history[1:]
	for c.headLen < len(hist) && hist[c.headLen].Origin.Kind == message.OriginEpochHead {
		c.headLen++
	}
	c.prev = summaryOf(hist[:c.headLen])
	if turnID != "" {
		if i := slices.IndexFunc(hist, func(m message.Message) bool { return m.TurnID == turnID }); i >= c.headLen {
			c.current = i
		}
	}
	if cc.kind != "" {
		cut := turnStart(hist, c.headLen, cc.turnID)
		if cut < 0 {
			return CompactReport{}, fmt.Errorf("%w: %s", ErrUnknownTurn, cc.turnID)
		}
		c.rng, c.rngTurn = CompactRange{Kind: cc.kind, Cut: cut}, cc.turnID
	}
	hctx := withHookScope(ctx, &hookScope{turnID: turnID})
	var rngIn *message.CompactRange
	if c.rng.Kind != "" {
		rngIn = &message.CompactRange{Kind: c.rng.Kind, TurnID: c.rngTurn}
	}
	f := fire(hctx, s, PreCompact, PreCompactInput{HookEnv: s.env(turnID, stepOf(t)), Trigger: string(trigger), Instructions: instructions, PreTokens: c.pre, Range: rngIn}, nil)
	if f.out.Block && trigger != TriggerOverflow {
		return CompactReport{}, fmt.Errorf("%w: %s", ErrBlocked, f.out.Reason)
	}
	instructions = f.in.Instructions
	s.publishAt(&event.ContextCompactionStarted{Trigger: string(trigger), PreTokens: c.pre}, turnID, stepOf(t), 0)
	failed := func(code string, err error) (CompactReport, error) {
		canceled := ctx.Err() != nil
		_ = s.exec(func(st *state) {
			if trigger != TriggerManual && !canceled && code != "nothing_to_compact" {
				st.compactFailures++
			}
			s.publishAt(&event.ContextCompactionFailed{Trigger: string(trigger), Error: code, Canceled: canceled}, turnID, stepOf(t), 0)
			if st.compactFailures >= st.policy.MaxConsecutiveFailures && !st.autoCompactOff {
				st.autoCompactOff = true
				s.publishAt(&event.ContextCompactionFailed{Trigger: string(trigger), Error: "thrash"}, turnID, stepOf(t), 0)
			}
		})
		return CompactReport{}, err
	}
	req := CompactRequest{
		Trigger: trigger, Instructions: instructions, History: hist, HeadLen: c.headLen, PrevSummary: c.prev, CurrentTurn: c.current, Range: c.rng,
		Tokens: c.pre - c.main, Target: c.win.target, Threshold: max(c.win.threshold-c.main, 0), Caps: c.caps, // a side chat's compactor weighs its own lines against the room the main part leaves
		Estimate:  func(msgs []message.Message) int64 { return tokens.Lines(msgs) },
		Summarize: s.summarizer(turnID, c.win.target),
	}
	res, err := callCompactor(hctx, comp, req)
	switch {
	case errors.Is(err, ErrNothingToCompact):
		return failed("nothing_to_compact", err)
	case err != nil:
		return failed("compactor_failed", err)
	}
	if err := validCompaction(req, res); err != nil {
		return failed("invalid_result", err)
	}
	if after := c.scale(tokens.Lines(res.Replace) + tokens.Text(res.Summary)); c.win.threshold > 0 && after >= c.win.threshold*9/10 && req.Range.Kind == "" {
		return failed("invalid_result", fmt.Errorf("the result is still about %d tokens, above 90%% of the %d threshold", after, c.win.threshold))
	}
	return s.applyCompaction(ctx, hctx, c, req, res, t)
}

func stepOf(t *turn) int {
	if t == nil {
		return 0
	}
	return t.step
}

func callCompactor(ctx context.Context, c Compactor, req CompactRequest) (res CompactResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("compactor panicked: %v", r)
		}
	}()
	return c.Compact(ctx, req)
}

// summarizer binds Summarize to this session's model; the call is capped near the target so the model is told one number.
func (s *Session) summarizer(turnID string, target int64) Summarizer {
	return func(ctx context.Context, msgs []message.Message, instructions string) (string, message.Usage, error) {
		limit := min(target, int64(s.agent.cfg.context.MaxSummaryTokens)) * 5 / 4
		sr := SideRequest{IncludeHistory: msgs == nil, Messages: append(slices.Clone(msgs), message.Message{Role: message.RoleUser, Origin: message.Origin{Kind: message.OriginReminder, Source: "compaction"}, Content: message.Content{&message.TextBlock{Text: instructions}}}),
			NoTools: true, MaxOutputTokens: int(limit), Bucket: "compaction"}
		if s.parent != nil && msgs == nil {
			sr.Preamble = prompt.Text("side_chat_compact_scope") // the main part rides in front but is never this summary's to keep
		}
		if turnID != "" && scopeOf(ctx) == nil {
			ctx = withHookScope(ctx, &hookScope{turnID: turnID})
		}
		m, u, err := s.SideCall(ctx, sr)
		if err != nil || m.StopReason != string(provider.StopMaxTokens) {
			return m.Text(), u, err
		}
		retry := sr // one retry with room to finish, since the cut-off tail is the progress and next step the summary ends with
		retry.MaxOutputTokens *= 2
		retry.Messages = slices.Clone(sr.Messages)
		retry.Messages[len(retry.Messages)-1].Content = message.Content{&message.TextBlock{Text: instructions + "\n\n" + prompt.Textf("summary_too_long", map[string]string{"target": strconv.FormatInt(target, 10)})}}
		m2, u2, err := s.SideCall(ctx, retry)
		if err != nil {
			return m.Text(), u, nil // the cut-off summary still beats none
		}
		return m2.Text(), u2, nil
	}
}

// summaryOf is the summary an epoch head carries, the reminder with source compaction.
func summaryOf(head []message.Message) string {
	for _, m := range head {
		for _, b := range m.Content {
			if r, ok := b.(*message.ReminderBlock); ok && r.Source == "compaction" {
				return r.Text
			}
		}
	}
	return ""
}

// turnStart is the index of a turn's first line after the epoch head, or -1.
func turnStart(hist []message.Message, headLen int, turnID string) int {
	for i := headLen; i < len(hist); i++ {
		if hist[i].TurnID == turnID {
			return i
		}
	}
	return -1
}

// validCompaction is step 3 of runtime.md §12.3: the result keeps history valid and actually shrinks it.
func validCompaction(req CompactRequest, res CompactResult) error {
	byID := map[string]message.Message{}
	for _, m := range req.History[req.HeadLen:] {
		byID[m.ID] = m
	}
	for i, m := range res.Replace {
		orig, ok := byID[m.ID]
		if !ok {
			return fmt.Errorf("Replace line %d is not a line of the context after its head", i)
		}
		if !sameOrCleared(orig, m) {
			return fmt.Errorf("Replace line %d changes more than cleared tool results", i)
		}
	}
	if len(res.Replace) > 0 && res.Replace[0].Role == message.RoleAssistant {
		return errors.New("Replace starts with an assistant line")
	}
	pending := map[string]bool{}
	for _, m := range res.Replace {
		for _, c := range m.ToolCalls() {
			pending[c.ID] = true
		}
		for _, b := range m.Content {
			if r, ok := b.(*message.ToolResultBlock); ok {
				delete(pending, r.CallID)
			}
		}
	}
	if len(pending) > 0 {
		return errors.New("Replace keeps a tool call without its result")
	}
	if req.Range.Kind == "" && res.Summary == "" && len(res.Replace) == len(req.History)-req.HeadLen && !clearsAny(res.Replace) {
		return errors.New("the result neither summarizes, drops nor clears anything")
	}
	return nil
}

func clearsAny(lines []message.Message) bool {
	for _, m := range lines {
		for _, b := range m.Content {
			if r, ok := b.(*message.ToolResultBlock); ok && r.Cleared {
				return true
			}
		}
	}
	return false
}

// sameOrCleared says whether kept is orig, or orig with some tool results replaced by the prune placeholder.
func sameOrCleared(orig, kept message.Message) bool {
	if len(orig.Content) != len(kept.Content) {
		return false
	}
	o, k := orig, kept
	o.Content, k.Content = nil, nil
	if same, _ := sameJSON(o, k); !same {
		return false
	}
	for i := range orig.Content {
		if kr, ok := kept.Content[i].(*message.ToolResultBlock); ok && kr.Cleared {
			or, ok := orig.Content[i].(*message.ToolResultBlock)
			if !ok || or.CallID != kr.CallID {
				return false
			}
			continue
		}
		if same, _ := sameJSON(orig.Content[i], kept.Content[i]); !same {
			return false
		}
	}
	return true
}

func sameJSON(a, b any) (bool, error) {
	x, err := json.Marshal(a)
	if err != nil {
		return false, err
	}
	y, err := json.Marshal(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(x, y), nil
}

// ClearedResult is the prune placeholder for one tool result: the kernel's fixed text, marked cleared.
func ClearedResult(r message.ToolResultBlock) *message.ToolResultBlock {
	c := r
	c.Content = message.Content{&message.TextBlock{Text: prompt.Text("tool_result_cleared")}}
	c.Cleared, c.Pin, c.Structured = true, nil, nil
	return &c
}

// applyCompaction opens the new epoch in one batch: system line, epoch head, kept lines, and a continue reminder mid-turn (runtime.md §12.3 step 5).
func (s *Session) applyCompaction(ctx, hctx context.Context, c compaction, req CompactRequest, res CompactResult, t *turn) (CompactReport, error) {
	turnID := ""
	if t != nil {
		turnID = t.plan.id
	}
	from := req.Range.Kind == "from"
	boundary := &message.Boundary{Reason: "compaction", Trigger: string(req.Trigger), Strategy: res.Strategy, PreTokens: c.pre}
	if req.Range.Kind != "" {
		boundary.Range = &message.CompactRange{Kind: req.Range.Kind, TurnID: c.rngTurn}
	}
	if req.Range.Kind == "up_to" && c.prefixBound {
		boundary.ReplayFallback = true // kept thinking follows a changed prefix, so its signatures no longer verify
	}
	if from && c.history[0].Boundary != nil && c.history[0].Boundary.ReplayFallback {
		boundary.ReplayFallback = true // the kept prefix already followed a changed one, as rewind and fork carry it
	}
	var sys *message.Message
	var parts []prompt.Part
	if from {
		sys = &message.Message{Role: message.RoleSystem, Origin: message.Origin{Kind: message.OriginSystem}, Content: c.history[0].Content, Boundary: boundary, Tools: c.history[0].Tools, HostedTools: c.history[0].HostedTools}
	} else {
		defs, tools := s.freezeTools()
		var err error
		if parts, err = s.renderSystem(ctx, tools, c.epoch+1); err != nil {
			return CompactReport{}, err
		}
		sys = &message.Message{Role: message.RoleSystem, Origin: message.Origin{Kind: message.OriginSystem}, Content: systemContent(parts), Boundary: boundary, Tools: defs, HostedTools: s.freezeHosted(defs)}
	}
	// what the summary line carries: pins that the kept lines do not already hold
	kept := res.Replace
	if from {
		kept = req.History[:req.Range.Cut]
	}
	pins := c.pins
	if from {
		pins = slices.DeleteFunc(slices.Clone(pins), func(p Pin) bool { return pinHeld(kept, p.Key) })
	}
	var head []*message.ReminderBlock
	if !from {
		sink := &hookSink{}
		fire(withHookScope(hctx, &hookScope{turnID: turnID, sink: sink}), s, SessionStart, SessionStartInput{HookEnv: s.env(turnID, stepOf(t)), Source: "compact", Model: s.Snapshot().Model}, nil)
		head = append(head, sink.take()...)
	}
	if res.Summary != "" {
		head = append(head, &message.ReminderBlock{Source: "compaction", Text: res.Summary})
	}
	head = append(head, s.pinReminders(pins)...)
	if !from {
		head = s.sideHead(head) // a partial compaction from a turn keeps the old head, note included
	}
	post := tokens.Lines(kept) + tokens.Text(res.Summary)
	sink := &hookSink{}
	fire(withHookScope(hctx, &hookScope{turnID: turnID, sink: sink}), s, PostCompact, PostCompactInput{HookEnv: s.env(turnID, stepOf(t)), Trigger: string(req.Trigger), Summary: res.Summary, PreTokens: c.pre, PostTokens: post, Epoch: c.epoch + 1}, nil)
	head = append(head, sink.take()...)
	var headLine *message.Message
	if len(head) > 0 {
		headLine, _ = delivery(nil, nil, head)
		headLine.Origin = message.Origin{Kind: message.OriginEpochHead}
	}
	var rep CompactReport
	var unsynced []message.Message
	if err := s.exec(func(st *state) {
		st.epoch++
		st.history, st.stableLen = nil, 0
		st.executed = map[string]*message.ToolResultBlock{}
		st.meter.Reset()
		st.compactFailures = 0
		if parts != nil {
			st.system = s.systemBlocks(parts)
		}
		le := &lineEvents{turnID: turnID}
		s.appendLine(st, sys, "")
		s.publishCommitted(st, sys, le)
		copyLine := func(m message.Message) {
			cp := m
			cp.ID, cp.CopyOf, cp.Batch, cp.BatchEnd = "", m.ID, "", false
			s.appendLine(st, &cp, m.TurnID)
			s.publishCommitted(st, &cp, le)
		}
		if from {
			for _, m := range kept {
				copyLine(m)
			}
		}
		if headLine != nil {
			s.appendLine(st, headLine, "")
			s.publishCommitted(st, headLine, le)
		}
		if !from {
			for _, m := range kept {
				copyLine(m)
			}
		}
		if t != nil && c.current >= 0 && !endsWithInput(kept) { // a turn that has only its input so far needs no nudge
			cont := continuation(prompt.Text("compact_continue"))
			s.appendLine(st, cont, turnID)
			s.publishCommitted(st, cont, le)
		}
		st.pins = rebuildPins(st.history[1:])
		if parts != nil {
			s.publish(systemRendered(st.epoch, parts))
		}
		rep = CompactReport{Strategy: res.Strategy, PreTokens: c.pre, PostTokens: st.estimate(), Epoch: st.epoch}
		st.context.UsedTokens, st.context.Basis = rep.PostTokens, "estimated"
		s.publishSnapshot()
		s.publishAt(&event.ContextCompacted{Trigger: string(req.Trigger), Strategy: res.Strategy, PreTokens: rep.PreTokens, PostTokens: rep.PostTokens, Epoch: rep.Epoch}, turnID, stepOf(t), 0)
		unsynced = st.unsynced()
	}); err != nil {
		return CompactReport{}, err
	}
	if t != nil {
		t.stops = 0
	}
	return rep, s.persist(s.ctx, unsynced) // the new epoch must land even if the turn is interrupted now
}

// endsWithInput says whether the kept lines end with a delivered input, which the next request answers as it is.
func endsWithInput(kept []message.Message) bool {
	if len(kept) == 0 {
		return false
	}
	switch kept[len(kept)-1].Origin.Kind {
	case message.OriginHuman, message.OriginCommand, message.OriginAgent:
		return true
	}
	return false
}

// pinHeld says whether a line of kept already carries the pin, as a tool result or an epoch-head reminder.
func pinHeld(kept []message.Message, key string) bool {
	for _, m := range kept {
		for _, b := range m.Content {
			switch v := b.(type) {
			case *message.ToolResultBlock:
				if v.Pin != nil && v.Pin.Key == key {
					return true
				}
			case *message.ReminderBlock:
				if v.Source == "pin:"+key {
					return true
				}
			}
		}
	}
	return false
}

// pinReminders re-injects pins within the budgets, newest first; one over its own budget is cut with a note, and the oldest go when the total is spent.
func (s *Session) pinReminders(pins []Pin) []*message.ReminderBlock {
	p := s.agent.cfg.context // immutable, so a worker may read it
	var out []*message.ReminderBlock
	total := int64(p.PinTotalTokens)
	for i := len(pins) - 1; i >= 0; i-- {
		pin := pins[i]
		limit := int64(p.PinTokens)
		if pin.MaxTokens > 0 {
			limit = int64(pin.MaxTokens)
		}
		text := pin.Content
		if tokens.Text(text) > limit {
			text = cutTokens(text, limit) + "\n" + prompt.Text("pin_truncated")
		}
		n := tokens.Text(text)
		if n > total {
			break
		}
		total -= n
		out = append(out, &message.ReminderBlock{Source: "pin:" + pin.Key, Text: text})
	}
	slices.Reverse(out) // oldest first, as they were declared
	return out
}

// cutTokens keeps the longest prefix of s whose estimate fits in limit.
func cutTokens(s string, limit int64) string {
	lo, hi := 0, len(s)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if tokens.Text(s[:mid]) <= limit {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return strings.ToValidUTF8(s[:lo], "")
}

// rebuildPins reads the pin set back from lines, in order, the later pin of a key winning and an unpin retiring it (runtime.md §12.4).
func rebuildPins(lines []message.Message) []Pin {
	var pins []Pin
	put := func(p Pin) {
		pins = slices.DeleteFunc(pins, func(x Pin) bool { return x.Key == p.Key })
		pins = append(pins, p)
	}
	for _, m := range lines {
		for _, b := range m.Content {
			switch v := b.(type) {
			case *message.ToolResultBlock:
				if v.Pin != nil {
					put(*v.Pin)
				}
			case *message.ReminderBlock:
				if v.Unpin != "" {
					pins = slices.DeleteFunc(pins, func(x Pin) bool { return x.Key == v.Unpin })
				} else if key, ok := strings.CutPrefix(v.Source, "pin:"); ok && m.Origin.Kind == message.OriginEpochHead {
					put(Pin{Key: key, Content: v.Text})
				}
			}
		}
	}
	return pins
}

func (st *state) addPin(p Pin) {
	st.pins = slices.DeleteFunc(st.pins, func(x Pin) bool { return x.Key == p.Key })
	st.pins = append(st.pins, p)
}

// Pins is a snapshot of the effective pin set, oldest first, so an extension can rebuild its state on resume.
func (s *Session) Pins() []Pin {
	var out []Pin
	_ = s.exec(func(st *state) { out = slices.Clone(st.pins) })
	return out
}

// ErrUnknownPin is returned by Unpin for a key the pin set does not hold.
var ErrUnknownPin = errors.New("no pin with this key")

// Unpin retires a pin, so later epochs no longer re-inject it; its text already in the current context stays until the next epoch.
func (s *Session) Unpin(ctx context.Context, key string) error {
	var err error
	if e := s.exec(func(st *state) {
		n := len(st.pins)
		st.pins = slices.DeleteFunc(st.pins, func(x Pin) bool { return x.Key == key })
		if len(st.pins) == n {
			err = fmt.Errorf("%w: %s", ErrUnknownPin, key)
			return
		}
		it := s.reminderItem("pin:"+key, prompt.Textf("pin_removed", map[string]string{"key": key}), false)
		it.parts[0].unpin = key
		s.enqueue(it)
	}); e != nil {
		return e
	}
	return err
}

// Clear is a waiting primitive like Compact: a new epoch with a re-rendered system and no old line, the pin set emptied, and reminders queued before it dropped (runtime.md §12.6).
func (s *Session) Clear(ctx context.Context) error {
	return s.historyOp(ctx, "clear", s.clear)
}

func (s *Session) clear(ctx context.Context) error {
	var seq uint64
	if m, ok := ctx.Value(markerKey{}).(marker); ok {
		seq = m.seq
	}
	epoch := 0
	_ = s.exec(func(st *state) { epoch = st.epoch })
	defs, tools := s.freezeTools()
	hosted := s.freezeHosted(defs)
	parts, err := s.renderSystem(ctx, tools, epoch+1)
	if err != nil {
		return err
	}
	s.clearGen.Add(1) // async hooks started before this answer a context that no longer exists
	sink := &hookSink{}
	fire(withHookScope(ctx, &hookScope{sink: sink}), s, SessionStart, SessionStartInput{HookEnv: s.env("", 0), Source: "clear", Model: s.Snapshot().Model}, nil)
	var head *message.Message
	var info deliveryInfo
	if notes := s.sideHead(sink.take()); len(notes) > 0 {
		head, info = delivery(nil, nil, notes)
		head.Origin = message.Origin{Kind: message.OriginEpochHead}
	}
	var unsynced []message.Message
	if err := s.exec(func(st *state) {
		s.dropQueued("cleared", func(it *queueItem) bool {
			return it.cmd != nil || it.origin.Kind != message.OriginReminder || it.seq > seq && !it.tail
		})
		st.epoch++
		st.history, st.stableLen = nil, 0
		st.executed = map[string]*message.ToolResultBlock{}
		st.pins = nil
		st.meter.Reset()
		st.compactFailures = 0
		st.system = s.systemBlocks(parts)
		sys := &message.Message{Role: message.RoleSystem, Origin: message.Origin{Kind: message.OriginSystem}, Content: systemContent(parts), Boundary: &message.Boundary{Reason: "clear"}, Tools: defs, HostedTools: hosted}
		s.appendLine(st, sys, "")
		s.publishCommitted(st, sys, nil)
		s.publish(systemRendered(st.epoch, parts))
		if head != nil {
			s.appendLine(st, head, "")
			s.publishCommitted(st, head, &lineEvents{delivery: &info})
		}
		st.context.UsedTokens, st.context.Basis = st.estimate(), "estimated"
		s.publishSnapshot()
		s.publish(&event.ContextCleared{Epoch: st.epoch})
		if s.agent.cfg.context.StopTasksOnClear {
			s.stopAll(st, "the conversation was cleared")
		}
		unsynced = st.unsynced()
	}); err != nil {
		return err
	}
	return s.persist(s.ctx, unsynced)
}

// autoCompact compacts before a model call once the estimate reaches the threshold; near the threshold a token counter, when there is one, decides.
func (t *turn) autoCompact(ctx context.Context) {
	s := t.s
	p := s.agent.cfg.context
	if p.Disabled || t.pauseTail || !under(t.compactions, p.MaxAutoCompactsPerTurn) || s.compactor() == nil || ctx.Err() != nil { // a pause_turn resend must keep the reply last
		return
	}
	var used, th int64
	var off bool
	var req *provider.Request
	counter := s.tokenCounter(t.model)
	_ = s.exec(func(st *state) {
		used, th, off = st.estimate(), st.context.ThresholdTokens, st.autoCompactOff
		if t.view != nil && used-st.mainTokens-st.base() <= st.policy.window(st.model.caps, st.model.ref.Params.MaxOutputTokens).target {
			off = true // a side chat's own lines within the summary target leave no room to win
		}
		if counter != nil {
			req = &provider.Request{Model: t.model.info.Model, System: st.system, Messages: t.withMain(st.history[1:len(st.history):len(st.history)], st.system, t.model.caps.ThinkingPrefixBound), Tools: t.defs, HostedTools: t.hosted, Params: t.model.ref.Params, Caps: t.model.caps}
		}
	})
	if off || th <= 0 {
		return
	}
	band := int64(defaults().Context.CountBand * float64(th))
	if req != nil && used >= th-band && used <= th+band {
		if n, err := counter.CountTokens(ctx, req); err == nil && n > 0 {
			used = n
			_ = s.exec(func(st *state) { st.context.UsedTokens, st.context.Basis = n, "counted" })
		}
	}
	if used < th {
		return
	}
	t.compactions++
	if _, err := s.compact(ctx, TriggerAuto, "", compactConfig{}, t); err == nil {
		t.freeze() // a new epoch may refreeze the tools of a prefix-bound model
	}
}

// overflowCompact is the reactive path: a request the API rejected as too long compacts once per step and is sent again.
func (t *turn) overflowCompact(ctx context.Context, err error) bool {
	s := t.s
	if ctx.Err() != nil || t.overflowAt == t.step || provider.AsError(err).Class != provider.ClassContextOverflow || s.compactor() == nil {
		return false
	}
	t.overflowAt = t.step
	if _, cerr := s.compact(ctx, TriggerOverflow, "", compactConfig{}, t); cerr != nil {
		return false
	}
	t.freeze()
	return true
}

// tokenCounter is WithTokenCounter's, or the model's own when its provider counts tokens.
func (s *Session) tokenCounter(m modelState) provider.TokenCounter {
	if c := s.agent.cfg.tokenCounter; c != nil {
		return c
	}
	if c, ok := m.ref.Provider.(provider.TokenCounter); ok {
		return c
	}
	return nil
}
