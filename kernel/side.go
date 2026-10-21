package kernel

import (
	"context"
	"slices"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
)

// SideRequest describes one model call that never enters history (compaction summaries, caller side tasks).
type SideRequest struct {
	IncludeHistory  bool              // prepend the stable prefix history[:stableLen], never a tool call without its result, and a side chat's main part before it
	Messages        []message.Message // appended after the prefix
	NoTools         bool              // tools and tool_choice stay identical to the main loop and returned calls are dropped; the caller's own prompt asks for no calls
	Preamble        string            // reminder placed before the first appended message
	MaxOutputTokens int
	Bucket          string    // ModelCall.Bucket of its requests, e.g. "compaction"
	Hop             event.Hop // attribution of the streamed events
	Emit            bool      // stream message.* and reasoning.* events under Hop
}

// SideCall sends one request with this session's system, tools and model, retrying as the main loop does, and books every attempt; the turn it belongs to comes from ctx.
func (s *Session) SideCall(ctx context.Context, sr SideRequest) (message.Message, message.Usage, error) {
	var hist []message.Message
	var system []provider.SystemBlock
	var ms modelState
	var taught learned
	var replay provider.ReplayMode
	var view *mainView
	turnID := ""
	if sc := scopeOf(ctx); sc != nil {
		turnID = sc.turnID
	}
	if err := s.exec(func(st *state) {
		if sr.IncludeHistory && len(st.history) > 0 {
			hist = st.history[1:st.stableLen:st.stableLen]
		}
		system, ms = st.system, st.model
		replay = st.replayMode()
		if l := st.learned[ms.info]; l != nil {
			taught = *l
		}
		if st.turn != nil && st.turn.id == turnID {
			view = st.turn.view // the running turn's main part, so the call shares its prefix
		}
	}); err != nil {
		return message.Message{}, message.Usage{}, err
	}
	if s.parent != nil && sr.IncludeHistory && view == nil {
		v, err := s.parent.view()
		if err != nil {
			return message.Message{}, message.Usage{}, err
		}
		view = v
	}
	if !sr.IncludeHistory {
		view = nil // appended messages alone carry no main part
	}
	defs, _ := s.freezeTools()
	hosted := s.freezeHosted(defs)
	if ms.caps.ThinkingPrefixBound {
		_ = s.exec(func(st *state) { defs, hosted = st.history[0].Tools, st.history[0].HostedTools })
	}
	bound := ms.caps.ThinkingPrefixBound || taught.prefixBound
	cacheKey := s.id
	if view != nil {
		hist = view.messages(hist, turnID, bound, view.verbatim(ms.info, system, defs, hosted))
		cacheKey = s.parent.id
		if view.fallback && bound {
			replay = provider.ReplayFallback
		}
	}
	msgs := slices.Concat(hist, withPreamble(sr.Messages, sr.Preamble))
	req := &provider.Request{Model: ms.info.Model, System: system, Messages: msgs, Tools: defs, HostedTools: hosted, Params: ms.ref.Params, Caps: ms.caps, CacheKey: cacheKey, CacheTail: len(hist), Replay: replay}
	if view != nil {
		req.SharedPrefix = view.shared
	}
	if sr.MaxOutputTokens > 0 {
		req.Params.MaxOutputTokens = sr.MaxOutputTokens
	}
	if taught.maxOutput > 0 {
		req.Caps.MaxOutputTokens = taught.maxOutput
	}
	if taught.maxTokensField != "" {
		req.Caps.MaxTokensField = taught.maxTokensField
	}
	req.Params.Reasoning.Hide = req.Params.Reasoning.Hide || taught.noSummary
	req.FirstEventTimeout, req.IdleTimeout = s.agent.cfg.retry.FirstEventTimeout, s.agent.cfg.retry.IdleTimeout
	bucket := sr.Bucket
	if bucket == "" {
		bucket = "side"
	}
	cfg := s.agent.cfg
	started := time.Now()
	tried, recoveries := map[string]bool{}, 0
	for attempt := 1; ; attempt++ {
		m, u, err := s.sideAttempt(ctx, req, ms, bucket, turnID, sr, view)
		if err == nil {
			if sr.NoTools {
				m.Content = slices.DeleteFunc(slices.Clone(m.Content), func(b message.Block) bool { _, ok := b.(*message.ToolCallBlock); return ok })
			}
			return m, u, nil
		}
		if ctx.Err() != nil {
			return message.Message{}, message.Usage{}, context.Cause(ctx)
		}
		perr := provider.AsError(err)
		if notices, ok := s.recoverRequest(req, perr, tried, ms.info); ok {
			recoveries++ // a changed request retries at once and spends none of the transient retry budget, as in the main loop
			for _, n := range notices {
				s.notice(n, turnID, 0, attempt)
			}
			continue
		}
		d, ok := cfg.retry.Next(attempt-recoveries, perr, time.Since(started), s.jitter)
		if !ok {
			return message.Message{}, message.Usage{}, perr
		}
		if err := sleepCtx(ctx, d); err != nil {
			return message.Message{}, message.Usage{}, context.Cause(ctx)
		}
	}
}

// withPreamble puts the preamble reminder first in the first appended message, or in a message of its own when that one is not user-side.
func withPreamble(msgs []message.Message, preamble string) []message.Message {
	if preamble == "" {
		return msgs
	}
	rem := &message.ReminderBlock{Source: "side", Text: preamble}
	out := slices.Clone(msgs)
	if len(out) > 0 && out[0].Role == message.RoleUser {
		out[0].Content = append(message.Content{rem}, out[0].Content...)
		return out
	}
	head := message.Message{Role: message.RoleUser, Origin: message.Origin{Kind: message.OriginReminder, Source: "side"}, Content: message.Content{rem}}
	return append([]message.Message{head}, out...)
}

// sideAttempt streams one request and books it; its response is complete when it returns without an error.
func (s *Session) sideAttempt(ctx context.Context, req *provider.Request, ms modelState, bucket, turnID string, sr SideRequest, view *mainView) (message.Message, message.Usage, error) {
	clock := s.agent.cfg.clock
	c := &modelCall{model: ms.info, bucket: bucket, started: clock.Now(), main: view}
	sent := *req
	sent.Params = s.requestHeaders(sent.Params, turnID, bucket, ms.info)
	sent.CaptureHeaders = s.agent.cfg.captureHeaders
	msgID := s.agent.cfg.ids.NewID("msg")
	proj := newProjector(msgID, ms.info)
	emit := func(p event.Payload) {
		if sr.Emit {
			ev := s.envelope(p, turnID, 0, 0)
			ev.Path = []event.Hop{sr.Hop}
			s.bus.Publish(ev)
		}
	}
	book := func(outcome string) {
		_ = s.exec(func(st *state) { s.settle(st, c, outcome, turnID, 0, 0) })
	}
	for sev, err := range ms.ref.Provider.Stream(ctx, &sent) {
		if err != nil {
			if pe := provider.AsError(err); c.requestID == "" {
				c.requestID = pe.RequestID
			}
			if c.began {
				book(callDiscarded)
			} else {
				book(callFailed)
			}
			return message.Message{}, c.usage, err
		}
		c.observe(sev)
		if sr.Emit {
			if proj.bulk(sev) {
				if err := s.bus.WaitWritable(ctx); err != nil {
					book(callDiscarded)
					return message.Message{}, c.usage, err
				}
			}
			payloads, _ := proj.project(sev, false)
			for _, p := range payloads {
				emit(p)
			}
		}
		if sev.Kind == provider.SEEnd {
			if sev.Message == nil || sev.Usage == nil {
				book(callDiscarded)
				return message.Message{}, c.usage, &provider.Error{Class: provider.ClassTransient, Message: "incomplete terminal event"}
			}
			m := *sev.Message
			book(callCommitted)
			return m, *sev.Usage, nil
		}
	}
	if c.began {
		book(callDiscarded)
	} else {
		book(callFailed)
	}
	if ctx.Err() != nil {
		return message.Message{}, c.usage, context.Cause(ctx)
	}
	return message.Message{}, c.usage, &provider.Error{Class: provider.ClassTransient, Message: "stream ended without a terminal event"}
}
