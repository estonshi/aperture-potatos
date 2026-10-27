package kernel

import (
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/estonshi/aperture-potatos/internal/tokens"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
)

// ContextPolicy decides when a session compacts and what survives; zero fields take spec/data/limits.json.
type ContextPolicy struct {
	MaxContextTokens       int // the window when set, overriding the model's reported one, so a host can correct a wrong value; 0 follows the model
	AutoCompactRatio       float64
	TargetRatio            float64
	MinSummaryTokens       int
	MaxSummaryTokens       int
	PinTokens              int
	PinTotalTokens         int
	MaxAutoCompactsPerTurn int
	MaxConsecutiveFailures int
	Disabled               bool
	StopTasksOnClear       bool
}

// WithContextPolicy sets when sessions compact and how large summaries and pins may be; zero fields keep the defaults.
func WithContextPolicy(p ContextPolicy) Option { return func(c *config) { c.context = p } }

// SessionContextWindow sets this session's window, overriding the model's reported one, which a host corrects
// when the provider does not report it or reports it wrongly; 0 follows the model. The value is written to the
// session file with the session's first line, and SetContextWindow changes it later.
func SessionContextWindow(tokens int) SessionOption {
	return func(c *sessionConfig) { c.contextWindow = &tokens }
}

// WithTokenCounter counts a request exactly when the estimate is close to the compaction threshold; a provider that counts tokens itself is asked without it.
func WithTokenCounter(tc provider.TokenCounter) Option {
	return func(c *config) { c.tokenCounter = tc }
}

func (p ContextPolicy) resolved() ContextPolicy {
	d := defaults().Context
	pickF := func(v, def float64) float64 {
		if v == 0 {
			return def
		}
		return v
	}
	p.AutoCompactRatio = pickF(p.AutoCompactRatio, d.AutoCompactRatio)
	p.TargetRatio = pickF(p.TargetRatio, d.TargetRatio)
	p.MinSummaryTokens = pickInt(p.MinSummaryTokens, d.MinSummaryTokens)
	p.MaxSummaryTokens = pickInt(p.MaxSummaryTokens, d.MaxSummaryTokens)
	p.PinTokens = pickInt(p.PinTokens, d.PinTokens)
	p.PinTotalTokens = pickInt(p.PinTotalTokens, d.PinTotalTokens)
	p.MaxAutoCompactsPerTurn = pickInt(p.MaxAutoCompactsPerTurn, d.MaxAutoCompactsPerTurn)
	p.MaxConsecutiveFailures = pickInt(p.MaxConsecutiveFailures, d.MaxConsecutiveFailures)
	return p
}

// window is the context budget of the current model under the policy (runtime.md §12.1).
type window struct {
	total     int64 // the window itself, before the output reserve
	usable    int64 // window minus the output reserve
	threshold int64 // automatic compaction starts above it
	target    int64 // the summary's token budget
}

// windowTokens is the window a session sizes its context against: the policy's own when set, else the model's.
func (p ContextPolicy) windowTokens(caps provider.Capabilities) int64 {
	if p.MaxContextTokens > 0 {
		return int64(p.MaxContextTokens)
	}
	if caps.ContextWindow > 0 {
		return int64(caps.ContextWindow)
	}
	return provider.DefaultContextWindow
}

func (p ContextPolicy) window(caps provider.Capabilities, maxOutput int) window {
	w := p.windowTokens(caps)
	out := int64(maxOutput)
	if out <= 0 {
		out = provider.DefaultMaxOutputTokens
	}
	usable := w - out
	if usable <= 0 {
		return window{total: w}
	}
	th := int64(math.Floor(p.AutoCompactRatio * float64(usable)))
	target := min(max(int64(p.TargetRatio*float64(usable)), int64(p.MinSummaryTokens)), int64(p.MaxSummaryTokens))
	if target > th/2 {
		target = th / 2 // a small window yields to the floor, so a compacted context still has room
	}
	return window{total: w, usable: usable, threshold: th, target: target}
}

func (st *state) setWindow() {
	w := st.policy.window(st.model.caps, st.model.ref.Params.MaxOutputTokens)
	st.context.WindowTokens = w.total
	st.context.ThresholdTokens = w.threshold
}

// contextPolicy is the session's policy as it stands; a caller off the actor reads it through the actor, and a
// session already closed answers with the agent's.
func (s *Session) contextPolicy() ContextPolicy {
	p := s.agent.cfg.context
	_ = s.exec(func(st *state) { p = st.policy })
	return p
}

// SetContextWindow sets the window this session sizes its context against, overriding the model's reported one;
// 0 follows the model again. The change applies from the session's next model call, so a running turn picks it
// up before its next step, and the compaction threshold follows the new window at once. The value is written to
// the session file with the next line the session writes, so a resume restores it (runtime.md §12.1).
func (s *Session) SetContextWindow(tokens int) error {
	if tokens < 0 {
		return fmt.Errorf("%w: %d", ErrInvalidWindow, tokens)
	}
	var err error
	if e := s.exec(func(st *state) {
		out := st.model.ref.Params.MaxOutputTokens
		if out <= 0 {
			out = provider.DefaultMaxOutputTokens
		}
		if tokens > 0 && tokens <= out {
			err = fmt.Errorf("%w: a %d-token window leaves no room for %d output tokens", ErrCapability, tokens, out)
			return
		}
		st.policy.MaxContextTokens = tokens
		st.windowSet, st.windowDirty = true, true
		st.setWindow()
		s.publishSnapshot()
	}); e != nil {
		return e
	}
	return err
}

// base is what the system and the frozen tools of this epoch cost, which the tail estimate adds when there is no anchor.
func (st *state) base() int64 {
	var n int64
	for _, b := range st.system {
		n += tokens.Text(b.Text)
	}
	if len(st.history) > 0 {
		n += tokens.Tools(upFront(st.history[0].Tools, st.history[0].HostedTools))
	}
	return n
}

// upFront leaves out the deferred definitions when a tool search goes with them, since the model sees only what a search loads.
func upFront(defs []message.ToolDef, hosted []message.HostedTool) []message.ToolDef {
	if !slices.ContainsFunc(hosted, func(h message.HostedTool) bool { return h.ToolSearch != nil }) {
		return defs
	}
	return slices.DeleteFunc(slices.Clone(defs), func(d message.ToolDef) bool { return d.Deferred })
}

// estimate is the size of the current context as the next request would send it (runtime.md §12.1).
func (st *state) estimate() int64 {
	if len(st.history) == 0 {
		return 0
	}
	n, _ := st.meter.Next(st.history[1:], st.base())
	return n + st.mainTokens
}

// anchor calibrates the estimate on a committed step's report, which covered everything up to the reply.
func (st *state) anchor(reported int64) {
	reported -= st.mainTokens // a side chat calibrates its own part; the main session's estimate stands for the rest
	if len(st.history) == 0 || reported <= 0 {
		return
	}
	st.meter.Anchor(reported, len(st.history)-1, st.base()+tokens.Lines(st.history[1:]))
}

// replayMode is ReplayFallback for an epoch whose kept lines follow changed content on a prefix-bound model (boundary replay_fallback), or whose reasoning the API already refused once.
func (st *state) replayMode() provider.ReplayMode {
	if st.fallbackEpoch != 0 && st.fallbackEpoch == st.epoch || len(st.history) > 0 && st.history[0].Boundary != nil && st.history[0].Boundary.ReplayFallback {
		return provider.ReplayFallback
	}
	return provider.ReplayFull
}

// ContextReport is how full the current model's window is and what fills it; Basis says whether Used was reported by the API, counted or estimated.
type ContextReport struct {
	Model     provider.ModelInfo `json:"model"`
	Epoch     int                `json:"epoch"`
	Window    int64              `json:"window_tokens"`
	Usable    int64              `json:"usable_tokens"`
	Threshold int64              `json:"threshold_tokens"`
	Used      int64              `json:"used_tokens"`
	Basis     string             `json:"basis"` // reported | estimated | counted
	Parts     []ContextPart      `json:"parts"`
}

// ContextPart is one source's share of the context, such as a prompt section, a tool definition or the tool results.
type ContextPart struct {
	Kind   string `json:"kind"`           // system, tool, summary, pin, epoch_head, user, assistant, reasoning, tool_call, tool_result, reminder, parent (a side chat's main part)
	Name   string `json:"name,omitempty"` // section, tool or pin key
	Tokens int64  `json:"tokens"`
	Count  int    `json:"count,omitempty"`
}

// Messages is the current context, the committed lines of the current epoch from its system line on; lines are shared, never to be modified.
func (s *Session) Messages() []message.Message {
	var out []message.Message
	_ = s.exec(func(st *state) { out = slices.Clone(st.history) })
	return out
}

// Context breaks the current context down by source; each part is a heuristic scaled so the parts add up to Used, and it reads through the actor, so it works while a turn runs.
func (s *Session) Context() ContextReport {
	var rep ContextReport
	_ = s.exec(func(st *state) {
		if len(st.history) == 0 {
			return // still initializing
		}
		w := st.policy.window(st.model.caps, st.model.ref.Params.MaxOutputTokens)
		rep = ContextReport{Model: st.model.info, Epoch: st.epoch, Window: w.total, Usable: w.usable, Threshold: w.threshold, Used: st.estimate(), Basis: "estimated"}
		if len(st.history) > 0 && st.meter.AnchorLen == len(st.history)-1 {
			rep.Basis = st.context.Basis // nothing was added since the last report or count
		}
		defs, hosted := st.history[0].Tools, st.history[0].HostedTools
		if !st.model.caps.ThinkingPrefixBound {
			defs, _ = s.freezeTools() // what the next turn sends
			hosted = s.freezeHosted(defs)
		}
		rep.Parts = contextParts(st.system, upFront(defs, hosted), st.history[1:])
		if st.mainLines > 0 {
			rep.Parts = append(rep.Parts, ContextPart{Kind: "parent", Tokens: st.mainTokens, Count: st.mainLines})
		}
	})
	var sum, parent int64
	for _, p := range rep.Parts {
		if p.Kind == "parent" {
			parent = p.Tokens // already the main session's calibrated estimate
			continue
		}
		sum += p.Tokens
	}
	if sum > 0 {
		for i := range rep.Parts {
			if rep.Parts[i].Kind != "parent" {
				rep.Parts[i].Tokens = rep.Parts[i].Tokens * max(rep.Used-parent, 0) / sum
			}
		}
	}
	return rep
}

// contextParts is the unscaled breakdown: system by section, tools and pins by name, the summary alone, the rest by kind.
func contextParts(system []provider.SystemBlock, defs []message.ToolDef, hist []message.Message) []ContextPart {
	var parts []ContextPart
	for _, b := range system {
		name := b.Name
		if name == "" {
			name = b.Slot
		}
		parts = append(parts, ContextPart{Kind: "system", Name: name, Tokens: tokens.Text(b.Text)})
	}
	for _, d := range defs {
		parts = append(parts, ContextPart{Kind: "tool", Name: d.Name, Tokens: tokens.Tools([]message.ToolDef{d})})
	}
	byKind := map[string]*ContextPart{}
	var order []string
	add := func(kind string, n int64) {
		p := byKind[kind]
		if p == nil {
			p = &ContextPart{Kind: kind}
			byKind[kind] = p
			order = append(order, kind)
		}
		p.Tokens += n
		p.Count++
	}
	for _, m := range hist {
		for _, b := range m.Content {
			n := tokens.Block(b)
			switch v := b.(type) {
			case *message.ReminderBlock:
				key, pinned := strings.CutPrefix(v.Source, "pin:")
				switch {
				case m.Origin.Kind == message.OriginEpochHead && v.Source == "compaction":
					parts = append(parts, ContextPart{Kind: "summary", Tokens: n})
				case m.Origin.Kind == message.OriginEpochHead && pinned:
					parts = append(parts, ContextPart{Kind: "pin", Name: key, Tokens: n})
				case m.Origin.Kind == message.OriginEpochHead:
					add("epoch_head", n)
				default:
					add("reminder", n)
				}
			case *message.ReasoningBlock:
				add("reasoning", n)
			case *message.ToolCallBlock:
				add("tool_call", n)
			case *message.ToolResultBlock:
				add("tool_result", n)
			default:
				if m.Role == message.RoleAssistant {
					add("assistant", n)
				} else {
					add("user", n)
				}
			}
		}
	}
	for _, k := range order {
		parts = append(parts, *byKind[k])
	}
	return parts
}
