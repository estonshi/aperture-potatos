//go:build live

package litellm_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/estonshi/aperture-potatos/ext/litellm"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/openaichat"
)

// paths records which spend endpoint each lookup used, so the test shows whether the session query found the call.
type paths struct {
	mu   sync.Mutex
	seen []string
}

func (p *paths) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(r)
	p.mu.Lock()
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	entry := r.URL.Path + " " + http.StatusText(status)
	if resp != nil && r.URL.Path == "/spend/logs/v2" { // only these three fields, since the rows also carry key hashes and metadata
		b, _ := io.ReadAll(resp.Body)
		resp.Body = io.NopCloser(bytes.NewReader(b))
		var page struct {
			Data []struct {
				RequestID string  `json:"request_id"`
				SessionID string  `json:"session_id"`
				Spend     float64 `json:"spend"`
			} `json:"data"`
		}
		_ = json.Unmarshal(b, &page)
		entry += fmt.Sprintf(" %+v", page.Data)
	}
	p.seen = append(p.seen, entry)
	p.mu.Unlock()
	return resp, err
}

// TestLiveTurnCost runs one turn through a LiteLLM proxy (OPENAI_BASE_URL, OPENAI_API_KEY, OPENAI_MODEL) and prices it from the proxy's spend logs (LITELLM_BASE_URL, LITELLM_API_KEY).
func TestLiveTurnCost(t *testing.T) {
	model, base := os.Getenv("OPENAI_MODEL"), os.Getenv("LITELLM_BASE_URL")
	if model == "" || base == "" || os.Getenv("OPENAI_API_KEY") == "" {
		t.Skip("set OPENAI_BASE_URL, OPENAI_API_KEY, OPENAI_MODEL, LITELLM_BASE_URL and LITELLM_API_KEY")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	rec := &paths{}
	lo := litellm.Options{BaseURL: base, APIKey: os.Getenv("LITELLM_API_KEY"), HTTPClient: &http.Client{Transport: rec}, SessionHeader: "x-litellm-session-id"}
	p := openaichat.New(openaichat.Config{APIKey: os.Getenv("OPENAI_API_KEY"), BaseURL: os.Getenv("OPENAI_BASE_URL")})
	ag, err := kernel.New(
		kernel.WithModel(provider.ModelRef{Provider: p, Model: model, Params: provider.Params{MaxOutputTokens: 256}}),
		kernel.WithCapabilityResolver(nil),
		kernel.WithRequestHeaders(litellm.RequestHeaders(lo)),
		kernel.WithCostSource(litellm.CostSource(lo)),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close(context.Background())
	sess, err := ag.NewSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := sess.Send(kernel.Text(fmt.Sprintf("Say hello in three words. (run %d)", time.Now().UnixNano()))) // a new prompt each run, so a gateway cache never answers with an older call's id
	if err != nil {
		t.Fatal(err)
	}
	res, err := rc.Wait(ctx)
	if err != nil || res.Err != nil {
		t.Fatalf("turn: %v %v", err, res.Err)
	}
	time.Sleep(5 * time.Second) // the proxy writes spend logs in batches, so the session query gets its chance first
	cctx, ccancel := context.WithTimeout(ctx, time.Minute)
	defer ccancel()
	tc, err := sess.TurnCost(cctx, res.TurnID)
	t.Logf("session %s, calls %d, priced %+v, missing %v, total %+v", sess.Info().SessionID, len(tc.Calls), tc.Priced, tc.Missing, tc.Total)
	rec.mu.Lock()
	t.Logf("lookups: %v", rec.seen)
	rec.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if len(tc.Priced) != 1 || len(tc.Missing) != 0 || tc.Total.Currency != "USD" {
		t.Fatalf("the turn's one call is priced: %+v", tc)
	}
}
