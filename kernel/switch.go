package kernel

import (
	"context"
	"errors"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/internal/tokens"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
)

// SwitchModel takes ctx because it waits for capability resolution while the target is not cached yet; once cached it returns at once, and the switch takes effect when the next turn starts (I10).
func (s *Session) SwitchModel(ctx context.Context, ref provider.ModelRef) error {
	if ref.Provider == nil {
		return errors.New("SwitchModel needs a provider")
	}
	if !s.agent.capsCached(ref) && reentrant(ctx, s.id) {
		return ErrReentrantWait
	}
	rctx, cancel := bounded(ctx, s.limits.InitTimeout)
	defer cancel()
	defs, _ := s.freezeTools()
	ms, err := s.prepareModel(rctx, ref, len(defs))
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	return s.exec(func(st *state) {
		if st.phase != phaseClosing && st.phase != phaseClosed {
			st.pendingSwitch = &ms // a later switch replaces one that has not taken effect yet
		}
	})
}

// applySwitch makes a pending switch take effect at a turn's start: provider_switch hooks may block it, it is refused when the history cannot fit the new window, and a prefix-bound target opens an epoch with tools frozen for it.
func (t *turn) applySwitch(ctx context.Context, to modelState) {
	s := t.s
	_ = s.exec(func(st *state) {
		if l := st.learned[to.info]; l != nil && l.prefixBound {
			to.caps.ThinkingPrefixBound = true // learned earlier in this session, before switching away
		}
	})
	var defs []message.ToolDef
	var hosted []message.HostedTool
	if to.caps.ThinkingPrefixBound {
		defs, _ = s.freezeTools() // tool specs are caller code, so they are read here and never on the actor
		hosted = s.freezeHosted(defs)
	}
	var ev event.ProviderSwitched
	_ = s.exec(func(st *state) {
		ev = event.ProviderSwitched{From: st.model.name(), To: to.name(), CacheReset: true}
		target := provider.ReplayTarget{API: to.info.API, Endpoint: to.info.Endpoint, Family: to.family()}
		for _, m := range st.history[1:] {
			for _, b := range m.Content {
				if provider.Replays(b, m.Producer, target) {
					continue
				}
				switch b.(type) {
				case *message.ReasoningBlock:
					ev.DroppedReasoning++
				case *message.OpaqueBlock, *message.HostedCallBlock, *message.HostedResultBlock:
					ev.DroppedOpaque++
				}
			}
		}
	})
	f := fire(t.hookCtx(ctx, ""), s, ProviderSwitch, ProviderSwitchInput{HookEnv: t.env(), From: ev.From, To: ev.To, DroppedReasoning: ev.DroppedReasoning, DroppedOpaque: ev.DroppedOpaque}, nil)
	if f.out.Block {
		t.publish(&event.ProviderSwitchRejected{To: to.name(), Reason: "hook_blocked"})
		return
	}
	var room int64
	over := false
	_ = s.exec(func(st *state) {
		room = st.policy.window(to.caps, to.ref.Params.MaxOutputTokens).usable
		over = st.context.UsedTokens > room
	})
	if over && s.compactor() != nil {
		_, _ = s.compact(ctx, TriggerSwitch, "", compactConfig{}, t) // on the current model, which still holds the history; a failure is refused below
	}
	var unsynced []message.Message
	applied := false
	_ = s.exec(func(st *state) {
		if st.context.UsedTokens > room {
			s.publishAt(&event.ProviderSwitchRejected{To: to.name(), Reason: "capacity"}, t.plan.id, 0, 0)
			return
		}
		st.model = to
		st.setWindow()
		if defs != nil {
			s.openSwitchEpoch(st, defs, hosted, t.plan.id)
		}
		st.meter = tokens.Estimate{} // the anchor and the calibration belong to the old model's tokenizer
		st.context.UsedTokens, st.context.Basis = st.estimate(), "estimated"
		s.publishSnapshot()
		s.publishAt(&ev, t.plan.id, 0, 0)
		unsynced = st.unsynced()
		applied = true
	})
	if applied {
		t.model = to
		_ = s.persist(s.ctx, unsynced) // on the session's context, because the new epoch must land even if this turn is interrupted
	}
}

// openSwitchEpoch starts an epoch that keeps the system prompt, refreezes the tools for the new model and carries every line over as a copy (I6).
func (s *Session) openSwitchEpoch(st *state, defs []message.ToolDef, hosted []message.HostedTool, turnID string) {
	old := st.history
	st.epoch++
	st.history, st.stableLen = nil, 0
	head := &message.Message{Role: message.RoleSystem, Origin: message.Origin{Kind: message.OriginSystem}, Content: old[0].Content, Boundary: &message.Boundary{Reason: "switch"}, Tools: defs, HostedTools: hosted}
	le := &lineEvents{turnID: turnID} // the lines are published by the turn that switched, while each copy keeps the turn it came from
	s.appendLine(st, head, "")
	s.publishCommitted(st, head, le)
	for _, m := range old[1:] {
		c := m
		c.ID, c.CopyOf, c.Batch, c.BatchEnd = "", m.ID, "", false
		s.appendLine(st, &c, m.TurnID)
		s.publishCommitted(st, &c, le)
	}
}
