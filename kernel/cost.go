package kernel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

// Cost is an amount of money as the billing backend reported it.
type Cost struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"` // ISO 4217, e.g. USD
}

// CallCost is the price of one model call, found by the response id the provider returned.
type CallCost struct {
	ResponseID string `json:"response_id"`
	Cost       Cost   `json:"cost"`
}

// TurnCost is what a turn's calls cost; Total sums only Priced, so check Missing before treating it as complete.
type TurnCost struct {
	TurnCalls
	Total   Cost       `json:"total"`
	Priced  []CallCost `json:"priced"`
	Missing []string   `json:"missing,omitempty"` // response ids the source had not logged when ctx ended
}

// CostSource prices model calls from billing records kept elsewhere, such as a gateway's spend logs.
type CostSource interface {
	// Price returns the costs it found keyed by call index; calls it cannot find yet are left out and retried.
	Price(ctx context.Context, calls []ModelCall) (map[int]Cost, error)
}

// CostError is how a CostSource reports an HTTP status from its backend, so TurnCost can retry 429 and 5xx and give up on the rest.
type CostError struct {
	Status  int
	Message string
}

// Error includes the HTTP status.
func (e *CostError) Error() string {
	return fmt.Sprintf("cost source: HTTP %d: %s", e.Status, e.Message)
}

// WithCostSource lets TurnCost price calls, which it otherwise refuses with ErrNoCostSource.
func WithCostSource(src CostSource) Option { return func(c *config) { c.costSource = src } }

// Backoff between pricing rounds: spend logs land asynchronously after each request.
const (
	costFirstRetry = time.Second
	costMaxRetry   = 5 * time.Second
)

// TurnCost prices the turn's calls with the configured CostSource and waits, bounded by ctx, on the source and its backoff for spend logs that land late.
func (s *Session) TurnCost(ctx context.Context, turnID string) (TurnCost, error) {
	src := s.agent.cfg.costSource
	if src == nil {
		return TurnCost{}, ErrNoCostSource
	}
	tc, err := s.TurnCalls(turnID)
	if err != nil {
		return TurnCost{}, err
	}
	out := TurnCost{TurnCalls: tc, Priced: []CallCost{}}
	var todo []int // indexes into tc.Calls still to price
	for i, c := range tc.Calls {
		if c.ResponseID != "" { // a request that failed before its response has nothing to look up
			todo = append(todo, i)
		}
	}
	found := map[int]Cost{}
	pctx := withMarker(ctx, marker{chain: s.chain, kind: "cost"}) // a source that waits on this session chain fails fast
	delay := costFirstRetry
	var fatal error
	for len(todo) > 0 {
		batch := make([]ModelCall, len(todo))
		for k, i := range todo {
			batch[k] = tc.Calls[i]
		}
		got, err := src.Price(pctx, batch)
		if err != nil && !transientCost(err) {
			fatal = err
			break
		}
		var left []int
		for k, i := range todo {
			if c, ok := got[k]; ok && err == nil {
				found[i] = c
			} else {
				left = append(left, i)
			}
		}
		todo = left
		if len(todo) == 0 || sleepCtx(ctx, delay) != nil {
			break
		}
		delay = min(2*delay, costMaxRetry)
	}
	for i, c := range tc.Calls {
		cost, ok := found[i]
		switch {
		case ok:
			out.Priced = append(out.Priced, CallCost{ResponseID: c.ResponseID, Cost: cost})
			if out.Total.Currency == "" {
				out.Total.Currency = cost.Currency
			}
			out.Total.Amount += cost.Amount
		case c.ResponseID != "":
			out.Missing = append(out.Missing, c.ResponseID)
		}
	}
	return out, fatal
}

// transientCost says whether a pricing error is worth retrying: a network error, or a 429 or 5xx from the backend.
func transientCost(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	var ce *CostError
	return errors.As(err, &ce) && (ce.Status == 429 || ce.Status >= 500)
}
