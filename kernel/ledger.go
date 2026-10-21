package kernel

import (
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
)

// UsageReport holds running totals over every model call, kept apart from per-turn data so it never shrinks.
type UsageReport struct {
	Total    message.Usage            `json:"total"`
	Buckets  map[string]message.Usage `json:"buckets"`  // main, compaction, side, subagent, peer ...
	ByModel  map[string]message.Usage `json:"by_model"` // "anthropic.messages/claude-opus-5-5"
	LastStep message.Usage            `json:"last_step"`
	Turns    int                      `json:"turns"`
	Steps    int                      `json:"steps"`
	Retries  int                      `json:"retries"`
	Discards int                      `json:"discards"`
	Abandons int                      `json:"abandoned_tools"`
}

func newUsageReport() UsageReport {
	return UsageReport{Buckets: map[string]message.Usage{}, ByModel: map[string]message.Usage{}}
}

// tally adds one call to the running totals; a main-loop call of session self also counts as a step or a discard.
func (r *UsageReport) tally(mc ModelCall, self string) {
	u := mc.Usage
	r.Total = addUsage(r.Total, u)
	r.Buckets[mc.Bucket] = addUsage(r.Buckets[mc.Bucket], u)
	key := mc.API + "/" + mc.Model
	r.ByModel[key] = addUsage(r.ByModel[key], u)
	if mc.Bucket != "main" || mc.SessionID != self {
		return
	}
	switch mc.Outcome {
	case callCommitted:
		r.LastStep = u
		r.Steps++
	case callDiscarded:
		r.Discards++
	}
}

func (r UsageReport) clone() UsageReport {
	r.Buckets, r.ByModel = maps.Clone(r.Buckets), maps.Clone(r.ByModel)
	return r
}

// ContextUsage is how full the window is, as Snapshot reports it; automatic compaction starts once UsedTokens reaches ThresholdTokens.
type ContextUsage struct {
	UsedTokens      int64  `json:"used_tokens"`
	WindowTokens    int64  `json:"window_tokens"`
	ThresholdTokens int64  `json:"threshold_tokens"`
	Basis           string `json:"basis"` // reported | estimated | counted
}

func addUsage(a, b message.Usage) message.Usage {
	return message.Usage{
		InputTokens:      a.InputTokens + b.InputTokens,
		CacheReadTokens:  a.CacheReadTokens + b.CacheReadTokens,
		CacheWriteTokens: a.CacheWriteTokens + b.CacheWriteTokens,
		OutputTokens:     a.OutputTokens + b.OutputTokens,
		ReasoningTokens:  a.ReasoningTokens + b.ReasoningTokens,
		HostedCalls:      addCounts(a.HostedCalls, b.HostedCalls),
		Estimated:        a.Estimated || b.Estimated,
	}
}

// addCounts sums per-tool counts into a new map, since a total must never alias the usage it started from.
func addCounts(a, b map[string]int64) map[string]int64 {
	if len(a)+len(b) == 0 {
		return nil
	}
	out := maps.Clone(a)
	if out == nil {
		out = map[string]int64{}
	}
	for k, n := range b {
		out[k] += n
	}
	return out
}

// Call outcomes: the request's response became history, began but was dropped, or never began.
const (
	callCommitted = "committed"
	callDiscarded = "discarded"
	callFailed    = "failed"
)

// ModelCall is one HTTP request to a model API, retried attempts included, so gateway logs can be matched one to one.
type ModelCall struct {
	Seq         int64             `json:"seq"` // order in this session's ledger, which the calls file is keyed on
	TurnID      string            `json:"turn_id,omitempty"`
	SessionID   string            `json:"session_id"`
	RootSession string            `json:"root_session"`
	MessageID   string            `json:"message_id,omitempty"`
	Bucket      string            `json:"bucket"`
	API         string            `json:"api"`
	Endpoint    string            `json:"endpoint"`
	Model       string            `json:"model"`
	ResponseID  string            `json:"response_id,omitempty"`
	RequestID   string            `json:"request_id,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Outcome     string            `json:"outcome"` // committed | discarded | failed
	Usage       message.Usage     `json:"usage"`
	StartedAt   time.Time         `json:"started_at"`
	Step        int               `json:"step,omitempty"`       // main-loop calls only
	Attempt     int               `json:"attempt,omitempty"`    // main-loop calls only; above 1 is a retry
	ParentSeq   int64             `json:"parent_seq,omitempty"` // a side chat's call: the main session's last line its request carried, so the main part can be rebuilt from the main file
}

// TurnCalls lists the model calls booked to one turn, including those of the tasks it started, in ledger order.
type TurnCalls struct {
	TurnID  string      `json:"turn_id"`
	Calls   []ModelCall `json:"calls"`
	Pending bool        `json:"pending"` // the turn or a task attempt it started still runs, so more calls may follow
}

// RequestInfo is what WithRequestHeaders sees of a request about to be sent.
type RequestInfo struct {
	RootSessionID string // shared by a session and all its ChildOf descendants
	SessionID     string
	TurnID        string // turn the call is attributed to; empty outside a turn
	Bucket        string // main, compaction, side, subagent ...
	API           string
	Model         string
}

// WithRequestHeaders runs fn before every model request, so a gateway can group or tag them; its headers win over Params.Headers of the same name.
func WithRequestHeaders(fn func(RequestInfo) map[string]string) Option {
	return func(c *config) { c.requestHeaders = fn }
}

// WithCaptureHeaders keeps the named response headers on each ModelCall and the committed message's producer, such as a gateway's own call id.
func WithCaptureHeaders(names ...string) Option {
	return func(c *config) { c.captureHeaders = append(c.captureHeaders, names...) }
}

// keptTurns is how many turns the in-memory ledger keeps besides those whose tasks still run; older turns report ErrTurnNotFound.
const keptTurns = 100

type callLedger struct {
	turns map[string][]ModelCall
	order []string          // oldest first
	busy  func(string) bool // a task attempt started in the turn still runs, so more calls may follow
}

func (l *callLedger) open(turnID string) {
	if l.turns == nil {
		l.turns = map[string][]ModelCall{}
	}
	if _, ok := l.turns[turnID]; ok {
		return
	}
	l.turns[turnID] = []ModelCall{}
	l.order = append(l.order, turnID)
	for len(l.order) > keptTurns {
		i := slices.IndexFunc(l.order, func(id string) bool { return l.busy == nil || !l.busy(id) })
		if i < 0 {
			return
		}
		delete(l.turns, l.order[i])
		l.order = slices.Delete(l.order, i, i+1)
	}
}

func (l *callLedger) add(c ModelCall) {
	if c.TurnID == "" {
		return // counted in the totals only
	}
	l.open(c.TurnID)
	l.turns[c.TurnID] = append(l.turns[c.TurnID], c)
}

// modelCall is one HTTP request to the model, settled exactly once when its outcome is known.
type modelCall struct {
	model      provider.ModelInfo
	bucket     string
	started    time.Time
	began      bool // the response started, so a failure discards it instead of failing it
	usage      message.Usage
	responseID string
	requestID  string
	headers    map[string]string
	messageID  string // the committed message, when the response became history
	settled    bool
	main       *mainView // a side chat's main part that the request carried
}

// settle books one request into the ledger and the usage buckets, on the actor, in the closure that decides its outcome.
func (s *Session) settle(st *state, c *modelCall, outcome string, turnID string, step, attempt int) {
	if c == nil || c.settled {
		return
	}
	c.settled = true
	mc := ModelCall{TurnID: turnID, SessionID: s.id, RootSession: s.chain[0], Bucket: c.bucket, API: string(c.model.API), Endpoint: c.model.Endpoint, Model: c.model.Model, ResponseID: c.responseID, RequestID: c.requestID, Headers: c.headers, Outcome: outcome, Usage: c.usage, StartedAt: c.started, Step: step, Attempt: attempt}
	if outcome == callCommitted {
		mc.MessageID = c.messageID
	}
	if v := c.main; v != nil {
		mc.ParentSeq = v.seq
		st.mainTokens, st.mainLines = v.tokens, len(v.lines) // what the report about to calibrate the estimate covered
	}
	if mc.Bucket == "" {
		mc.Bucket = "main"
	}
	s.book(st, mc, false, step, attempt)
}

// book adds one call to the ledger and the running totals and publishes usage.updated.
func (s *Session) book(st *state, mc ModelCall, forwarded bool, step, attempt int) {
	u := mc.Usage
	st.callSeq++
	mc.Seq = st.callSeq
	st.usage.tally(mc, s.id)
	if mc.Bucket == "main" && mc.SessionID == s.id {
		if mc.Outcome == callCommitted && !u.Estimated {
			st.anchor(u.InputTokens + u.OutputTokens) // the reply is already appended, so the report covers every line
		}
		if !u.Estimated && u.InputTokens+u.OutputTokens > 0 {
			st.context.UsedTokens, st.context.Basis = u.InputTokens+u.OutputTokens, "reported"
		}
	}
	st.ledger.add(mc)
	s.queueCall(st, mc)
	if p := s.parent; p != nil && !forwarded { // a side chat's calls also count in its main session, outside any turn
		fwd := mc
		fwd.Bucket, fwd.TurnID = "side", ""
		_ = p.exec(func(pst *state) { p.book(pst, fwd, true, 0, 0) }) // waits, so the main session books it before the side chat goes on; a main actor never waits on a side chat's
	}
	if c := s.owner; c != nil { // a ChildOf session's calls count in the owner's turn that started the attempt, and on up the chain
		rt := c.rt.Load()
		fwd := mc
		fwd.Bucket, fwd.TurnID = rt.kind, rt.turnID
		c.owner.post(func(pst *state) { c.owner.book(pst, fwd, true, 0, 0) })
	}
	s.publishSnapshot()
	s.publishAt(&event.UsageUpdated{Bucket: mc.Bucket, StepUsage: u, SessionTotal: st.usage.Total, Forwarded: forwarded, Context: event.ContextInfo(st.context)}, mc.TurnID, step, attempt)
}

// TurnCalls lists the model calls attributed to a turn from memory, or from the store's calls file for a turn memory no longer keeps.
func (s *Session) TurnCalls(turnID string) (TurnCalls, error) {
	var out TurnCalls
	found := false
	if err := s.exec(func(st *state) {
		calls, ok := st.ledger.turns[turnID]
		if !ok {
			return
		}
		found = true
		out = TurnCalls{TurnID: turnID, Calls: slices.Clone(calls), Pending: st.turn != nil && st.turn.id == turnID || st.tasks.startedIn(turnID)}
	}); err != nil {
		return TurnCalls{}, err
	}
	if !found {
		return s.storedTurnCalls(turnID) // older than the turns kept in memory
	}
	return out, nil
}

// RecordCalls adds calls made on this session's behalf somewhere else, such as an in-process peer's turn, to its ledger and usage buckets.
func (s *Session) RecordCalls(bucket, turnID string, calls []ModelCall) {
	calls = slices.Clone(calls)
	s.post(func(st *state) {
		for _, c := range calls {
			c.Bucket, c.TurnID = bucket, turnID
			s.book(st, c, false, 0, 0)
		}
	})
}

// requestHeaders merges WithRequestHeaders into a copy of the request's headers, so the caller's Params are never shared.
func (s *Session) requestHeaders(p provider.Params, turnID, bucket string, model provider.ModelInfo) provider.Params {
	fn := s.agent.cfg.requestHeaders
	if fn == nil {
		return p
	}
	add := fn(RequestInfo{RootSessionID: s.chain[0], SessionID: s.id, TurnID: turnID, Bucket: bucket, API: string(model.API), Model: model.Model})
	if len(add) == 0 {
		return p
	}
	h := p.Headers.Clone()
	if h == nil {
		h = http.Header{}
	}
	for k, v := range add {
		h.Set(k, v)
	}
	p.Headers = h
	return p
}
