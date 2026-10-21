package providertest

import (
	"context"
	"errors"
	"iter"
	"sync"
	"time"

	"github.com/estonshi/aperture-potatos/provider"
)

// Step is one scripted item: an event to yield, an error that ends the stream, or a hold before the next step.
type Step struct {
	Event provider.StreamEvent
	Err   error
	Hold  time.Duration // > 0 pauses before the next step; < 0 blocks until the request is canceled
}

// ScriptProvider answers each Stream call with the next script, so kernel tests run without HTTP or an adapter.
type ScriptProvider struct {
	APIName      provider.API
	EndpointName string

	mu       sync.Mutex
	scripts  [][]Step
	requests []*provider.Request
}

// NewScriptProvider answers as api from the endpoint script.invalid, one script per Stream call.
func NewScriptProvider(api provider.API, scripts ...[]Step) *ScriptProvider {
	return &ScriptProvider{APIName: api, EndpointName: "script.invalid", scripts: scripts}
}

// Push appends scripts for later calls.
func (s *ScriptProvider) Push(scripts ...[]Step) {
	s.mu.Lock()
	s.scripts = append(s.scripts, scripts...)
	s.mu.Unlock()
}

// Requests returns every request received, in order.
func (s *ScriptProvider) Requests() []*provider.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*provider.Request(nil), s.requests...)
}

// API reports APIName.
func (s *ScriptProvider) API() provider.API { return s.APIName }

// Endpoint reports EndpointName.
func (s *ScriptProvider) Endpoint() string { return s.EndpointName }

// Stream records req and plays the next script, failing when none is left and ending with ClassCanceled once ctx is done.
func (s *ScriptProvider) Stream(ctx context.Context, req *provider.Request) iter.Seq2[provider.StreamEvent, error] {
	s.mu.Lock()
	s.requests = append(s.requests, req)
	var script []Step
	ok := len(s.scripts) > 0
	if ok {
		script, s.scripts = s.scripts[0], s.scripts[1:]
	}
	s.mu.Unlock()
	return func(yield func(provider.StreamEvent, error) bool) {
		if !ok {
			yield(provider.StreamEvent{}, errors.New("providertest: no script left for this call"))
			return
		}
		for _, st := range script {
			if err := ctx.Err(); err != nil {
				yield(provider.StreamEvent{}, &provider.Error{Class: provider.ClassCanceled, API: s.APIName, Message: err.Error()})
				return
			}
			if st.Err != nil {
				yield(provider.StreamEvent{}, st.Err)
				return
			}
			if st.Hold != 0 {
				if !holdFor(ctx, st.Hold) {
					yield(provider.StreamEvent{}, &provider.Error{Class: provider.ClassCanceled, API: s.APIName, Message: ctx.Err().Error()})
					return
				}
				continue
			}
			if !yield(st.Event, nil) {
				return
			}
		}
	}
}

// holdFor pauses for d, or until ctx ends when d is negative; false means ctx ended first.
func holdFor(ctx context.Context, d time.Duration) bool {
	if d < 0 {
		<-ctx.Done()
		return false
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
