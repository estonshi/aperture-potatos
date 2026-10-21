package httpx

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/estonshi/aperture-potatos/provider"
)

type timeoutError struct{ code string }

func (e *timeoutError) Error() string {
	if e.code == "first_event_timeout" {
		return "no data arrived before the first-event timeout"
	}
	return "the stream stalled longer than the idle timeout"
}

// PostStream sends a streaming request under its stream timeouts; the first-event timeout runs from sending, since a gateway may hold the response headers until the first chunk arrives.
func PostStream(ctx context.Context, doer provider.Doer, api provider.API, u string, header http.Header, body []byte, req *provider.Request) (*http.Response, error) {
	first, idle := Timeouts(req)
	sent, cancel := context.WithCancel(ctx)
	w := &watch{idle: idle, cancel: cancel}
	w.mu.Lock()
	w.timer = arm(first, func() { w.fire("first_event_timeout") })
	w.mu.Unlock()
	resp, err := Post(sent, doer, api, u, header, body)
	if err != nil {
		fired := w.stop()
		if fired != nil && ctx.Err() == nil {
			return nil, TransportError(ctx, api, fired)
		}
		return nil, err
	}
	w.mu.Lock()
	w.body = resp.Body
	w.mu.Unlock()
	resp.Body = w
	return resp, nil
}

// watch is a response body that fails once no bytes arrive within the first-event timeout, then within the idle timeout between reads.
type watch struct {
	body   io.ReadCloser
	idle   time.Duration
	cancel context.CancelFunc
	mu     sync.Mutex
	timer  *time.Timer
	fired  *timeoutError
	done   bool
}

// arm starts a timer for d, or none when d is negative, which spec/data/limits.json reads as no limit.
func arm(d time.Duration, f func()) *time.Timer {
	if d < 0 {
		return nil
	}
	return time.AfterFunc(d, f)
}

func (w *watch) fire(code string) {
	w.mu.Lock()
	if w.done || w.fired != nil {
		w.mu.Unlock()
		return
	}
	w.fired = &timeoutError{code: code}
	body := w.body
	w.mu.Unlock()
	w.cancel() // before headers this aborts the request; after, it ends a read blocked on the body
	if body != nil {
		body.Close()
	}
}

// stop ends the watch and reports whether a timeout had fired.
func (w *watch) stop() *timeoutError {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.done = true
	if w.timer != nil {
		w.timer.Stop()
	}
	w.cancel()
	return w.fired
}

func (w *watch) Read(p []byte) (int, error) {
	n, err := w.body.Read(p)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fired != nil {
		return n, w.fired
	}
	if n > 0 && !w.done {
		if w.timer != nil {
			w.timer.Stop()
		}
		w.timer = arm(w.idle, func() { w.fire("idle_timeout") })
	}
	return n, err
}

func (w *watch) Close() error {
	w.stop()
	return w.body.Close()
}
