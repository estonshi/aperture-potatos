// Package hookrun runs one hook call on its own goroutine, so a hook that ignores its context is abandoned instead of stalling the caller.
package hookrun

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/estonshi/aperture-potatos/internal/workers"
)

// Kind classifies why a call produced no output.
type Kind string

const (
	KindTimeout  Kind = "timeout"
	KindCanceled Kind = "canceled"
	KindPanic    Kind = "panic"
	KindError    Kind = "error"
)

// Failure is a call that produced no output.
type Failure struct {
	Kind Kind
	Err  error
}

func (f *Failure) Error() string { return f.Err.Error() }

// Call runs fn with timeout (0 adds none) on a goroutine named name; once the context ends, fn has grace to return before it is abandoned.
func Call[T any](ctx context.Context, wk *workers.Set, name string, timeout, grace time.Duration, fn func(context.Context) (T, error)) (T, *Failure) {
	var zero T
	hctx, cancel := ctx, context.CancelFunc(func() {})
	if timeout > 0 {
		hctx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	type result struct {
		v        T
		err      error
		panicked any
	}
	ch := make(chan result, 1) // buffered, so an abandoned goroutine can still finish
	wk.Go(name, func() {
		r := result{}
		defer func() {
			if p := recover(); p != nil {
				r = result{panicked: p}
			}
			ch <- r
		}()
		r.v, r.err = fn(hctx)
	})
	var r result
	select {
	case r = <-ch:
	case <-hctx.Done():
		t := time.NewTimer(grace)
		defer t.Stop()
		select {
		case r = <-ch:
		case <-t.C:
			return zero, ended(ctx, hctx, errors.New("did not return within the grace period"))
		}
	}
	switch {
	case r.panicked != nil:
		return zero, &Failure{Kind: KindPanic, Err: fmt.Errorf("panic: %v", r.panicked)}
	case hctx.Err() != nil:
		return zero, ended(ctx, hctx, r.err) // an answer that arrives after the deadline counts as none
	case r.err != nil:
		return zero, &Failure{Kind: KindError, Err: r.err}
	}
	return r.v, nil
}

// ended reports a call whose context ended: canceled when the caller's did, else timed out.
func ended(parent, hctx context.Context, err error) *Failure {
	if err == nil {
		err = hctx.Err()
	}
	if parent.Err() != nil {
		return &Failure{Kind: KindCanceled, Err: err}
	}
	return &Failure{Kind: KindTimeout, Err: err}
}

// Pool bounds the async hooks running at once; a negative size lifts the bound.
type Pool struct{ sem chan struct{} }

func NewPool(size int) *Pool {
	if size < 0 {
		return &Pool{}
	}
	return &Pool{sem: make(chan struct{}, size)}
}

// TryAcquire takes a slot without waiting; false means the pool is full.
func (p *Pool) TryAcquire() bool {
	if p.sem == nil {
		return true
	}
	select {
	case p.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

func (p *Pool) Release() {
	if p.sem != nil {
		<-p.sem
	}
}
