// Package httpx is the HTTP plumbing every adapter shares: sending through the Doer, response headers, stream timeouts and error classification.
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/spec"
)

// maxErrorBody bounds how much of an error response is read, since only its JSON head matters.
const maxErrorBody = 1 << 20

// Post sends body through doer and returns the response of a 2xx, or a classified error for anything else.
func Post(ctx context.Context, doer provider.Doer, api provider.API, u string, header http.Header, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, &provider.Error{Class: provider.ClassInvalidRequest, API: api, Message: err.Error()}
	}
	req.Header = header
	return do(ctx, doer, api, req)
}

// Get is Post for read-only endpoints such as a models listing.
func Get(ctx context.Context, doer provider.Doer, api provider.API, u string, header http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, &provider.Error{Class: provider.ClassInvalidRequest, API: api, Message: err.Error()}
	}
	req.Header = header
	return do(ctx, doer, api, req)
}

func do(ctx context.Context, doer provider.Doer, api provider.API, req *http.Request) (*http.Response, error) {
	resp, err := doer.Do(req)
	if err != nil {
		return nil, TransportError(ctx, api, err)
	}
	if resp.StatusCode/100 == 2 {
		return resp, nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	return nil, Classify(api, resp.StatusCode, resp.Header, b, false)
}

// TransportError classifies a failure below HTTP: the caller's cancellation, a stream timeout, or a transient network error.
func TransportError(ctx context.Context, api provider.API, err error) *provider.Error {
	var te *timeoutError
	switch {
	case ctx.Err() != nil:
		return &provider.Error{Class: provider.ClassCanceled, API: api, Message: context.Cause(ctx).Error()}
	case errors.As(err, &te):
		return &provider.Error{Class: provider.ClassTransient, API: api, Code: te.code, Message: te.Error()}
	}
	return &provider.Error{Class: provider.ClassTransient, API: api, Code: "connection_error", Message: err.Error()}
}

// Incomplete is the error for a stream that ended without its terminal event; it is never treated as success.
func Incomplete(api provider.API, requestID string) *provider.Error {
	return &provider.Error{Class: provider.ClassTransient, API: api, Code: "stream_incomplete", Message: "the stream ended before its terminal event", RequestID: requestID, MidStream: true}
}

// RequestID reads the API's request id header.
func RequestID(h http.Header) string {
	if id := h.Get("request-id"); id != "" {
		return id
	}
	return h.Get("x-request-id")
}

// Capture returns the named response headers with lowercased keys, or nil when none is present.
func Capture(h http.Header, names []string) map[string]string {
	var out map[string]string
	for _, n := range names {
		if v := h.Get(n); v != "" {
			if out == nil {
				out = map[string]string{}
			}
			out[strings.ToLower(n)] = v
		}
	}
	return out
}

// RetryAfter prefers retry-after-ms, then retry-after in seconds or as an HTTP date.
func RetryAfter(h http.Header, now time.Time) time.Duration {
	if ms, err := strconv.ParseFloat(h.Get("retry-after-ms"), 64); err == nil && ms > 0 {
		return time.Duration(ms * float64(time.Millisecond))
	}
	v := h.Get("retry-after")
	if s, err := strconv.ParseFloat(v, 64); err == nil && s > 0 {
		return time.Duration(s * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}

// Endpoint normalizes a base URL to the host that producers record, so replay decisions compare like with like.
func Endpoint(base string) string {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return strings.ToLower(base)
	}
	host := strings.ToLower(u.Host)
	if (u.Scheme == "https" && strings.HasSuffix(host, ":443")) || (u.Scheme == "http" && strings.HasSuffix(host, ":80")) {
		host = host[:strings.LastIndexByte(host, ':')]
	}
	return host
}

var limits = sync.OnceValue(func() (l struct {
	Retry struct {
		FirstEventTimeoutMs int64 `json:"first_event_timeout_ms"`
		IdleTimeoutMs       int64 `json:"idle_timeout_ms"`
	} `json:"retry"`
}) {
	if err := json.Unmarshal([]byte(spec.MustText("data/limits.json")), &l); err != nil {
		panic(fmt.Sprintf("httpx: limits.json: %v", err))
	}
	return l
})

// Timeouts returns the request's stream timeouts, filling zeros from spec/data/limits.json; a negative one stays negative and Watch does not arm it.
func Timeouts(req *provider.Request) (first, idle time.Duration) {
	first, idle = req.FirstEventTimeout, req.IdleTimeout
	if first == 0 {
		first = time.Duration(limits().Retry.FirstEventTimeoutMs) * time.Millisecond
	}
	if idle == 0 {
		idle = time.Duration(limits().Retry.IdleTimeoutMs) * time.Millisecond
	}
	return first, idle
}
