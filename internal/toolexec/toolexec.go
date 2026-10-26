// Package toolexec runs one batch of tool calls: every call gets exactly one result, and a tool that ignores cancellation is abandoned after a grace period instead of blocking the turn.
package toolexec

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/estonshi/aperture-potatos/internal/prompt"
	"github.com/estonshi/aperture-potatos/message"
)

type Runner interface {
	RunOne(ctx context.Context, call message.ToolCallBlock) message.ToolResultBlock // the tool itself; recovers panics
	Completed(call message.ToolCallBlock, res message.ToolResultBlock)              // emits tool.completed
	Parallel(name string) bool                                                      // the tool may run alongside adjacent parallel calls
	Go(name string, fn func())                                                      // tracked goroutine start
}

type Options struct {
	MaxParallel int                                            // calls of one group running at once; < 1 means no bound
	Grace       time.Duration                                  // after cancellation or a timeout; < 0 waits without a deadline
	Timeout     func(call message.ToolCallBlock) time.Duration // per call; < 0 means none
	OnAbandon   func(call message.ToolCallBlock)               // emits tool.abandoned and counts the straggler
	Movable     func(call message.ToolCallBlock) bool          // the call runs on a context a Move can detach from the turn
	Texts       prompt.Source                                  // the host's overrides of the fixed texts; nil for the built-ins
}

// Run returns exactly len(calls) results in call order on every path, interruption included; groups run one after another, so the order of side effects the model asked for holds.
func Run(ctx context.Context, r Runner, calls []message.ToolCallBlock, o Options) []message.ToolResultBlock {
	results := make([]message.ToolResultBlock, len(calls))
	for _, g := range Partition(calls, r.Parallel) {
		if ctx.Err() != nil {
			for _, i := range g {
				results[i] = NotStarted(calls[i], o.Texts)
			}
			continue
		}
		runGroup(ctx, r, calls, g, o, results)
	}
	return results
}

// Partition keeps call order: consecutive parallel calls share a group, and every other call runs alone.
func Partition(calls []message.ToolCallBlock, parallel func(string) bool) [][]int {
	var groups [][]int
	for i, c := range calls {
		n := len(groups)
		if n > 0 && parallel(c.Name) && parallel(calls[groups[n-1][0]].Name) {
			groups[n-1] = append(groups[n-1], i)
			continue
		}
		groups = append(groups, []int{i})
	}
	return groups
}

// slot is one call's hand-off between its worker and the collector; whoever settles it first finishes the call.
type slot struct {
	res     chan message.ToolResultBlock // buffered, so a worker never blocks on send
	settled atomic.Bool
	expired chan struct{} // closed a grace period after the call's context ended, by its timeout or the turn's cancellation
	expire  sync.Once
	release func() // gives the call's parallel slot back once, whether the worker or the collector finishes it
}

func runGroup(ctx context.Context, r Runner, calls []message.ToolCallBlock, g []int, o Options, results []message.ToolResultBlock) {
	width := o.MaxParallel
	if width < 1 {
		width = len(g)
	}
	sem := make(chan struct{}, width)
	slots := make([]*slot, len(g))
	for k := range g {
		slots[k] = &slot{res: make(chan message.ToolResultBlock, 1), expired: make(chan struct{})}
		slots[k].release = sync.OnceFunc(func() { <-sem })
	}
	// The dispatcher starts calls in call order, so a slow call ahead in the queue is never overtaken while the collector waits on it.
	r.Go("tools "+calls[g[0]].ID, func() {
		for k, i := range g {
			sl, call := slots[k], calls[i]
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				for j := k; j < len(g); j++ {
					if slots[j].settled.CompareAndSwap(false, true) {
						slots[j].res <- NotStarted(calls[g[j]], o.Texts)
					}
				}
				return
			}
			r.Go("tool "+call.ID, func() { work(ctx, r, call, sl, o) })
		}
	})
	for k, i := range g {
		sl := slots[k]
		select {
		case results[i] = <-sl.res:
			continue
		case <-sl.expired:
		}
		if !sl.settled.CompareAndSwap(false, true) {
			results[i] = <-sl.res // the worker settled first and its send is imminent
			continue
		}
		sl.release()
		if o.OnAbandon != nil {
			o.OnAbandon(calls[i])
		}
		if ctx.Err() == nil {
			results[i] = TimedOut(calls[i], o.Timeout(calls[i]), o.Texts)
		} else {
			results[i] = Abandoned(calls[i], o.Texts)
		}
	}
}

func work(ctx context.Context, r Runner, call message.ToolCallBlock, sl *slot, o Options) {
	defer sl.release()
	var cctx context.Context
	var cancel context.CancelFunc
	if o.Movable != nil && o.Movable(call) {
		m := newMove(ctx, r, call, sl, o.Timeout(call))
		cctx, cancel = m.ctx, m.close
	} else {
		if limit := o.Timeout(call); limit >= 0 {
			cctx, cancel = context.WithTimeout(ctx, limit)
		} else {
			cctx, cancel = context.WithCancel(ctx)
		}
	}
	defer cancel()
	stop := context.AfterFunc(cctx, func() {
		if o.Grace >= 0 {
			time.AfterFunc(o.Grace, func() { sl.expire.Do(func() { close(sl.expired) }) })
		}
	})
	res := r.RunOne(cctx, call)
	stop()
	if sl.settled.CompareAndSwap(false, true) { // losing means the collector abandoned this call; drop the late result
		r.Completed(call, res)
		sl.res <- res
	}
}

func result(call message.ToolCallBlock, outcome message.Outcome, text string) message.ToolResultBlock {
	return message.ToolResultBlock{CallID: call.ID, Name: call.Name, IsError: true, Outcome: outcome, Content: message.Content{&message.TextBlock{Text: text}}}
}

// NotStarted is the result of a call the batch never reached because the turn was interrupted; texts may be nil for the built-in text.
func NotStarted(call message.ToolCallBlock, texts prompt.Source) message.ToolResultBlock {
	return result(call, message.OutcomeInterrupted, prompt.TextFrom(texts, "tool_not_started"))
}

// Abandoned is the result of a call that was still running when the grace period after an interrupt ran out.
func Abandoned(call message.ToolCallBlock, texts prompt.Source) message.ToolResultBlock {
	return result(call, message.OutcomeInterrupted, prompt.TextFrom(texts, "tool_abandoned"))
}

// Interrupted is the result of a call that returned an error after the turn was interrupted.
func Interrupted(call message.ToolCallBlock, texts prompt.Source) message.ToolResultBlock {
	return result(call, message.OutcomeInterrupted, prompt.TextFrom(texts, "tool_interrupted"))
}

// TimedOut is the result of a call that outlived its timeout.
func TimedOut(call message.ToolCallBlock, limit time.Duration, texts prompt.Source) message.ToolResultBlock {
	secs := strconv.FormatFloat(limit.Seconds(), 'f', -1, 64)
	return result(call, message.OutcomeError, prompt.TextfFrom(texts, "tool_timed_out", map[string]string{"seconds": secs}))
}
