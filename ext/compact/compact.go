// Package compact is the default compaction strategy: a model summary of the context, with an optional cheaper pass that clears old tool results first.
package compact

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/spec"
)

type Options struct {
	DropCurrentTurn bool
	SummaryPrompt   string
	Prune           *PruneOptions
}

// PruneOptions clears old tool results before summarizing, so a context full of large outputs shrinks without a model call.
type PruneOptions struct {
	KeepRecent int // 0 is the default, a negative value keeps none
	MinTokens  int
}

// Pruning defaults; spec/ext/compact/README.md documents them.
const (
	defaultKeepRecent = 5
	defaultMinTokens  = 1000
	minMessages       = 4
	maxSplitDepth     = 3
	startChars        = 200
)

// New registers the strategy at agent scope.
func New(o Options) kernel.Extension {
	return kernel.ExtensionFunc("compact", func(r kernel.Registrar) error {
		if _, err := kernel.RenderTemplate(o.SummaryPrompt, nil); err != nil {
			return fmt.Errorf("compact: SummaryPrompt: %w", err)
		}
		_, err := r.Compactor(&strategy{o: o})
		return err
	})
}

type strategy struct{ o Options }

func (s *strategy) Compact(ctx context.Context, req kernel.CompactRequest) (kernel.CompactResult, error) {
	body := req.History[req.HeadLen:]
	if req.Range.Kind == "up_to" {
		body = req.History[req.HeadLen:req.Range.Cut]
	}
	if req.Range.Kind == "from" {
		body = req.History[req.Range.Cut:]
	}
	if len(body) < minMessages || (req.Range.Kind == "" && req.Trigger != kernel.TriggerOverflow && req.Tokens <= req.Target) { // after an overflow the API, not the estimate, says it is too big
		return kernel.CompactResult{}, kernel.ErrNothingToCompact
	}
	if res, ok := s.prune(req); ok {
		return res, nil
	}
	switch req.Range.Kind {
	case "up_to":
		sum, err := s.summarize(ctx, req, req.History[:req.Range.Cut])
		return result(sum, req.History[req.Range.Cut:], err)
	case "from":
		start := firstChars(req.History[req.Range.Cut])
		instr := join(fill(spec.MustText("ext/compact/summary_from.md"), map[string]string{"target": strconv.FormatInt(req.Target, 10), "start": start}), req.Instructions)
		sum, _, err := req.Summarize(ctx, nil, instr)
		return result(sum, nil, err)
	}
	var sum string
	var err error
	hist := req.History
	if req.Trigger == kernel.TriggerOverflow {
		hist = fitCurrentTurn(req)
		sum, err = s.chunked(ctx, req, hist, 0) // the whole context no longer fits in one request
	} else {
		sum, err = s.summarize(ctx, req, nil)
	}
	var keep []message.Message
	if !s.o.DropCurrentTurn && !req.Caps.ThinkingPrefixBound && req.CurrentTurn >= req.HeadLen {
		keep = hist[req.CurrentTurn:] // cut on the turn's first user line, so no result is left without its call
	}
	return result(sum, keep, err)
}

func result(sum string, keep []message.Message, err error) (kernel.CompactResult, error) {
	if err != nil {
		return kernel.CompactResult{}, err
	}
	if strings.TrimSpace(sum) == "" {
		return kernel.CompactResult{}, errors.New("compact: the model returned an empty summary")
	}
	return kernel.CompactResult{Summary: sum, Replace: keep, Strategy: "summarize"}, nil
}

// instructions is the template, the carry-forward sentence a custom template needs when there is an earlier summary, and the call's own instructions.
func (s *strategy) instructions(req kernel.CompactRequest, fromHead bool) string {
	tmpl, custom := spec.MustText("ext/compact/summary.md"), false
	if s.o.SummaryPrompt != "" {
		tmpl, custom = s.o.SummaryPrompt, true
	}
	out := fill(tmpl, map[string]string{"target": strconv.FormatInt(req.Target, 10)})
	if custom && fromHead && req.PrevSummary != "" {
		out = join(out, strings.TrimSpace(spec.MustText("ext/compact/carry_forward.md")))
	}
	return join(out, req.Instructions)
}

// summarize runs one summary call; nil msgs is the whole context as the main loop's cached prefix.
func (s *strategy) summarize(ctx context.Context, req kernel.CompactRequest, msgs []message.Message) (string, error) {
	sum, _, err := req.Summarize(ctx, msgs, s.instructions(req, true))
	return sum, err
}

// chunked summarizes the oldest half and the rest apart and merges them, halving again a part that is still too long, at most maxSplitDepth levels deep.
func (s *strategy) chunked(ctx context.Context, req kernel.CompactRequest, msgs []message.Message, depth int) (string, error) {
	cut := splitPoint(msgs)
	if depth >= maxSplitDepth || cut <= 0 {
		sum, _, err := req.Summarize(ctx, msgs, s.instructions(req, depth == 0 || sameStart(msgs, req.History)))
		return sum, err
	}
	var parts []string
	for i, part := range [][]message.Message{msgs[:cut], msgs[cut:]} {
		sum, _, err := req.Summarize(ctx, part, s.instructions(req, i == 0 && sameStart(part, req.History)))
		if err != nil && provider.AsError(err).Class == provider.ClassContextOverflow {
			sum, err = s.chunked(ctx, req, part, depth+1)
		}
		if err != nil {
			return "", err
		}
		parts = append(parts, sum)
	}
	var in []message.Message
	for _, p := range parts {
		in = append(in, message.Message{Role: message.RoleUser, Origin: message.Origin{Kind: message.OriginReminder, Source: "compaction"}, Content: message.Content{&message.TextBlock{Text: p}}})
	}
	merge := join(join(fill(s.template(), map[string]string{"target": strconv.FormatInt(req.Target, 10)}), strings.TrimSpace(spec.MustText("ext/compact/merge.md"))), req.Instructions)
	sum, _, err := req.Summarize(ctx, in, merge)
	return sum, err
}

func (s *strategy) template() string {
	if s.o.SummaryPrompt != "" {
		return s.o.SummaryPrompt
	}
	return spec.MustText("ext/compact/summary.md")
}

func sameStart(part, all []message.Message) bool {
	return len(part) > 0 && len(all) > 0 && part[0].ID == all[0].ID
}

// splitPoint is the user-side line nearest the middle that starts no tool result, so each part is valid on its own; 0 when there is none.
func splitPoint(msgs []message.Message) int {
	best := 0
	for i := 1; i < len(msgs); i++ {
		if msgs[i].Role != message.RoleUser || msgs[i].Origin.Kind == message.OriginTool {
			continue
		}
		if best == 0 || abs(i-len(msgs)/2) < abs(best-len(msgs)/2) {
			best = i
		}
	}
	return best
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// prune clears old tool results when that alone brings the context under half the threshold; the summary stays the previous one.
func (s *strategy) prune(req kernel.CompactRequest) (kernel.CompactResult, bool) {
	p := s.o.Prune
	if p == nil || req.Range.Kind != "" || req.Caps.ThinkingPrefixBound || (req.Trigger != kernel.TriggerAuto && req.Trigger != kernel.TriggerOverflow) {
		return kernel.CompactResult{}, false
	}
	keepRecent, minTokens := p.KeepRecent, p.MinTokens
	switch {
	case keepRecent == 0:
		keepRecent = defaultKeepRecent
	case keepRecent < 0:
		keepRecent = 0 // negative keeps none
	}
	if minTokens == 0 {
		minTokens = defaultMinTokens
	}
	body := req.History[req.HeadLen:]
	current := len(body)
	if req.CurrentTurn >= req.HeadLen {
		current = req.CurrentTurn - req.HeadLen
	}
	type pos struct{ line, block int }
	var results []pos
	for i, m := range body[:current] {
		for k, b := range m.Content {
			if _, ok := b.(*message.ToolResultBlock); ok {
				results = append(results, pos{i, k})
			}
		}
	}
	if len(results) <= keepRecent {
		return kernel.CompactResult{}, false
	}
	out := slices.Clone(body)
	cleared := 0
	for _, p := range results[:len(results)-keepRecent] {
		r := out[p.line].Content[p.block].(*message.ToolResultBlock)
		if req.Estimate([]message.Message{{Content: r.Content}}) < int64(minTokens) {
			continue
		}
		m := out[p.line]
		m.Content = slices.Clone(m.Content)
		m.Content[p.block] = kernel.ClearedResult(*r)
		out[p.line] = m
		cleared++
	}
	if cleared == 0 {
		return kernel.CompactResult{}, false
	}
	after := req.Tokens - (req.Estimate(body) - req.Estimate(out)) // what clearing removed comes off the measured size
	if after > req.Threshold/2 {
		return kernel.CompactResult{}, false // not enough: summarize the original instead
	}
	return kernel.CompactResult{Replace: out, Summary: req.PrevSummary, Strategy: "prune"}, true
}

// fitCurrentTurn clears the running turn's largest tool results until the turn takes at most half the threshold, since after an overflow a turn that alone fills the window can be neither kept nor summarized.
func fitCurrentTurn(req kernel.CompactRequest) []message.Message {
	if req.CurrentTurn < req.HeadLen || req.CurrentTurn >= len(req.History) || req.Threshold <= 0 {
		return req.History
	}
	out := slices.Clone(req.History)
	turn := out[req.CurrentTurn:]
	type pos struct {
		line, block int
		size        int64
	}
	var results []pos
	for i, m := range turn {
		for k, b := range m.Content {
			if r, ok := b.(*message.ToolResultBlock); ok && !r.Cleared {
				results = append(results, pos{i, k, req.Estimate([]message.Message{{Content: r.Content}})})
			}
		}
	}
	slices.SortStableFunc(results, func(a, b pos) int { return cmp.Compare(b.size, a.size) })
	for _, p := range results {
		if req.Estimate(turn) <= req.Threshold/2 {
			break
		}
		m := turn[p.line]
		m.Content = slices.Clone(m.Content)
		m.Content[p.block] = kernel.ClearedResult(*m.Content[p.block].(*message.ToolResultBlock))
		turn[p.line] = m
	}
	return out
}

// firstChars quotes where a from-compaction starts; committed text already has its runtime tags escaped.
func firstChars(m message.Message) string {
	t := strings.TrimSpace(m.Text())
	if utf8.RuneCountInString(t) > startChars {
		t = string([]rune(t)[:startChars])
	}
	return t
}

// fill renders a compaction template; the spec's are checked by the tests and a caller's when the extension is installed.
func fill(tmpl string, vars map[string]string) string {
	s, _ := kernel.RenderTemplate(tmpl, vars)
	return s
}

func join(a, b string) string {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "\n\n" + b
}
