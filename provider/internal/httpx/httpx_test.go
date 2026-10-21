package httpx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/estonshi/aperture-potatos/provider"
)

const A = provider.APIAnthropicMessages

func TestClassifyAnthropic(t *testing.T) {
	h := func(kv ...string) http.Header {
		out := http.Header{}
		for i := 0; i < len(kv); i += 2 {
			out.Set(kv[i], kv[i+1])
		}
		return out
	}
	body := func(typ, msg string) []byte {
		return []byte(`{"type":"error","error":{"type":"` + typ + `","message":"` + msg + `"},"request_id":"req_1"}`)
	}
	cases := []struct {
		name   string
		status int
		header http.Header
		body   []byte
		mid    bool
		class  provider.ErrorClass
		rule   string
		limit  int
	}{
		{"prompt too long", 400, nil, body("invalid_request_error", "prompt is too long: 210000 tokens > 200000 maximum"), false, provider.ClassContextOverflow, "", 0},
		{"max output", 400, nil, body("invalid_request_error", "max_tokens: 100000 > 64000, which is the maximum allowed number of output tokens for claude-haiku-4-5"), false, provider.ClassInvalidRequest, "max_output_too_large", 64000},
		{"bound signature", 400, nil, body("invalid_request_error", "messages.3.content.0: Invalid `signature` in `thinking` block. The block is bound to a different conversation"), false, provider.ClassReasoningReplay, "thinking_signature_bound", 0},
		{"bad signature", 400, nil, body("invalid_request_error", "messages.1.content.0: Invalid `signature` in `thinking` block"), false, provider.ClassReasoningReplay, "", 0},
		{"modified", 400, nil, body("invalid_request_error", "`thinking` or `redacted_thinking` blocks in the latest assistant message cannot be modified"), false, provider.ClassReasoningReplay, "", 0},
		{"other 400", 400, nil, body("invalid_request_error", "tools.0.name: bad"), false, provider.ClassInvalidRequest, "", 0},
		{"413", 413, nil, body("request_too_large", "too big"), false, provider.ClassInvalidRequest, "", 0},
		{"billing", 402, nil, body("billing_error", "no credit"), false, provider.ClassQuota, "", 0},
		{"spend cap", 429, nil, []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"cap","details":{"error_code":"enforced_spend_limit_reached"}}}`), false, provider.ClassQuota, "", 0},
		{"rate limit", 429, h("retry-after", "3"), body("rate_limit_error", "slow down"), false, provider.ClassRateLimited, "", 0},
		{"spend cap with retry-after", 429, h("retry-after", "3"), []byte(`{"error":{"type":"rate_limit_error","message":"cap","details":{"error_code":"enforced_spend_limit_reached"}}}`), false, provider.ClassRateLimited, "", 0},
		{"auth", 401, nil, body("authentication_error", "bad key"), false, provider.ClassAuth, "", 0},
		{"forbidden", 403, nil, body("permission_error", "no"), false, provider.ClassAuth, "", 0},
		{"not found", 404, nil, body("not_found_error", "model"), false, provider.ClassNotFound, "", 0},
		{"overloaded", 529, nil, body("overloaded_error", "Overloaded"), false, provider.ClassTransient, "", 0},
		{"502 html", 502, nil, []byte("<html>bad gateway</html>"), false, provider.ClassTransient, "", 0},
		{"mid overloaded", 0, nil, body("overloaded_error", "Overloaded"), true, provider.ClassTransient, "", 0},
		{"mid rate", 0, nil, body("rate_limit_error", "x"), true, provider.ClassRateLimited, "", 0},
		{"mid unknown", 0, nil, body("brand_new_error", "x"), true, provider.ClassTransient, "", 0},
		{"mid invalid", 0, nil, body("invalid_request_error", "x"), true, provider.ClassInvalidRequest, "", 0},
	}
	for _, c := range cases {
		e := Classify(A, c.status, c.header, c.body, c.mid)
		if e.Class != c.class || e.Rule != c.rule || e.Limit != c.limit {
			t.Errorf("%s: got class=%s rule=%q limit=%d", c.name, e.Class, e.Rule, e.Limit)
		}
	}
	e := Classify(A, 529, h("x-should-retry", "false", "request-id", "req_h"), body("overloaded_error", "o"), false)
	if e.Retryable() || e.RequestID != "req_h" && e.RequestID != "req_1" {
		t.Fatalf("x-should-retry false must stop retries: %+v", e)
	}
	if e := Classify(A, 429, h("retry-after-ms", "1500"), body("rate_limit_error", "x"), false); e.RetryAfter != 1500*time.Millisecond || e.RequestID != "req_1" {
		t.Fatalf("retry-after-ms: %+v", e)
	}
}

func TestEndpointAndCapture(t *testing.T) {
	for in, want := range map[string]string{"https://API.Anthropic.com": "api.anthropic.com", "https://x.io:443/v1": "x.io", "http://localhost:4000": "localhost:4000"} {
		if got := Endpoint(in); got != want {
			t.Errorf("Endpoint(%q) = %q", in, got)
		}
	}
	h := http.Header{}
	h.Set("X-LiteLLM-Call-Id", "c1")
	if got := Capture(h, []string{"x-litellm-call-id", "absent"}); len(got) != 1 || got["x-litellm-call-id"] != "c1" {
		t.Fatalf("capture: %v", got)
	}
	if Capture(h, nil) != nil {
		t.Fatal("no names, no map")
	}
}

type slowBody struct {
	chunks []string
	gaps   []time.Duration
	closed chan struct{}
}

func (b *slowBody) Read(p []byte) (int, error) {
	if len(b.chunks) == 0 {
		return 0, io.EOF
	}
	select {
	case <-time.After(b.gaps[0]):
	case <-b.closed:
		return 0, errors.New("read on closed body")
	}
	n := copy(p, b.chunks[0])
	b.chunks, b.gaps = b.chunks[1:], b.gaps[1:]
	return n, nil
}

func (b *slowBody) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

// slowDoer sends its response headers after delay, or fails once the request is canceled first.
type slowDoer struct {
	delay time.Duration
	body  io.ReadCloser
}

func (d slowDoer) Do(r *http.Request) (*http.Response, error) {
	select {
	case <-time.After(d.delay):
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: d.body}, nil
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
}

func TestStreamTimeouts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limits := &provider.Request{FirstEventTimeout: 5 * time.Second, IdleTimeout: 10 * time.Second}
		body := func(gaps ...time.Duration) *slowBody {
			b := &slowBody{gaps: gaps, closed: make(chan struct{})}
			for range gaps {
				b.chunks = append(b.chunks, "x")
			}
			return b
		}
		stream := func(ctx context.Context, headers time.Duration, b *slowBody, req *provider.Request) (string, error) {
			resp, err := PostStream(ctx, slowDoer{headers, b}, A, "http://x", http.Header{}, nil, req)
			if err != nil {
				return "", err
			}
			defer resp.Body.Close()
			got, err := io.ReadAll(resp.Body)
			return string(got), err
		}
		code := func(err error) string {
			var e *provider.Error
			if !errors.As(err, &e) {
				e = TransportError(context.Background(), A, err)
			}
			if e.Code != "" && !e.Retryable() {
				t.Fatalf("a timeout is retried: %+v", e)
			}
			return e.Code
		}
		if got, err := stream(t.Context(), 3*time.Second, body(time.Second, 9*time.Second), limits); err != nil || got != "xx" {
			t.Fatalf("within limits: %q %v", got, err)
		}
		if _, err := stream(t.Context(), 6*time.Second, body(time.Second), limits); code(err) != "first_event_timeout" {
			t.Fatalf("headers held past the timeout: %v", err)
		}
		if _, err := stream(t.Context(), 3*time.Second, body(3*time.Second), limits); code(err) != "first_event_timeout" {
			t.Fatalf("the first byte counts from sending, not from the headers: %v", err)
		}
		if _, err := stream(t.Context(), 0, body(time.Second, 11*time.Second), limits); code(err) != "idle_timeout" {
			t.Fatalf("idle timeout: %v", err)
		}
		if got, err := stream(t.Context(), time.Hour, body(time.Hour, time.Hour), &provider.Request{FirstEventTimeout: -1, IdleTimeout: -1}); err != nil || got != "xx" {
			t.Fatalf("negative timeouts are none: %q %v", got, err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(2*time.Second, cancel)
		var e *provider.Error
		if _, err := stream(ctx, 6*time.Second, body(time.Second), limits); !errors.As(err, &e) || e.Class != provider.ClassCanceled {
			t.Fatalf("the caller canceling is no timeout: %v", err)
		}
	})
}

func TestTimeoutsKeepNegative(t *testing.T) {
	first, idle := Timeouts(&provider.Request{FirstEventTimeout: -1})
	if first != -1 || idle != 120*time.Second {
		t.Fatalf("got %v %v", first, idle)
	}
}

func TestTransportErrorCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e := TransportError(ctx, A, errors.New("x")); e.Class != provider.ClassCanceled {
		t.Fatalf("got %+v", e)
	}
	if !strings.Contains(Incomplete(A, "r").Error(), "terminal event") {
		t.Fatal("incomplete message")
	}
}
