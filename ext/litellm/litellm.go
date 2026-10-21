// Package litellm prices model calls from a LiteLLM proxy's spend logs; it is a CostSource and a request tagger, not an Extension.
package litellm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/provider"
)

type Options struct {
	BaseURL    string // proxy root, e.g. https://litellm.example.com
	APIKey     string // a key allowed to read spend logs, usually the master key
	HTTPClient *http.Client

	SessionHeader string // e.g. x-litellm-session-id; empty skips tagging and reads spend one request at a time
}

// RequestHeaders tags every model request with its root session id, so CostSource can read a whole turn with one query.
func RequestHeaders(o Options) func(kernel.RequestInfo) map[string]string {
	return func(ri kernel.RequestInfo) map[string]string {
		if o.SessionHeader == "" {
			return nil
		}
		return map[string]string{o.SessionHeader: ri.RootSessionID}
	}
}

// CostSource reads the proxy's spend logs, which it fills asynchronously after each request.
func CostSource(o Options) kernel.CostSource { return &source{o: o} }

type source struct{ o Options }

const (
	pageSize     = 100
	lookups      = 4           // concurrent one-request lookups
	window       = time.Minute // slack on both ends of the v2 time window for clock skew between us and the proxy
	windowLayout = "2006-01-02 15:04:05"
)

func (s *source) Price(ctx context.Context, calls []kernel.ModelCall) (map[int]kernel.Cost, error) {
	out := map[int]kernel.Cost{}
	if s.o.SessionHeader != "" {
		groups := map[string][]int{}
		for i, c := range calls {
			if c.ResponseID != "" && c.RootSession != "" {
				groups[c.RootSession] = append(groups[c.RootSession], i)
			}
		}
		for _, root := range slices.Sorted(maps.Keys(groups)) {
			if err := s.bySession(ctx, root, calls, groups[root], out); err != nil {
				return out, err
			}
		}
	}
	var rest []int
	for i, c := range calls {
		if _, ok := out[i]; !ok && c.ResponseID != "" {
			rest = append(rest, i)
		}
	}
	return out, s.byRequest(ctx, calls, rest, out)
}

// bySession reads one root session's spend logs over the time its calls could have been logged in, page by page; session_id matches loosely, so only rows of these calls count.
func (s *source) bySession(ctx context.Context, root string, calls []kernel.ModelCall, idx []int, out map[int]kernel.Cost) error {
	want := map[string][]int{}
	start := calls[idx[0]].StartedAt
	for _, i := range idx {
		want[calls[i].ResponseID] = append(want[calls[i].ResponseID], i)
		if calls[i].StartedAt.Before(start) {
			start = calls[i].StartedAt
		}
	}
	q := url.Values{
		"session_id": {root},
		"start_date": {start.Add(-window).UTC().Format(windowLayout)},
		"end_date":   {time.Now().Add(window).UTC().Format(windowLayout)},
		"page_size":  {strconv.Itoa(pageSize)},
	}
	for page, pages := 1, 1; page <= pages; page++ {
		q.Set("page", strconv.Itoa(page))
		var body struct {
			Data       []spendRow `json:"data"`
			TotalPages int        `json:"total_pages"`
		}
		if err := s.get(ctx, "/spend/logs/v2", q, &body); err != nil {
			return err
		}
		for _, r := range body.Data {
			for _, i := range want[r.RequestID] {
				out[i] = r.cost()
			}
		}
		pages = max(body.TotalPages, 1)
	}
	return nil
}

// byRequest looks each call up by its response id, a few at a time; the first error wins.
func (s *source) byRequest(ctx context.Context, calls []kernel.ModelCall, idx []int, out map[int]kernel.Cost) error {
	var mu sync.Mutex
	var first error
	sem := make(chan struct{}, lookups)
	var wg sync.WaitGroup
	for _, i := range idx {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			var rows spendRows
			err := s.get(ctx, "/spend/logs", url.Values{"request_id": {calls[i].ResponseID}}, &rows)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				if first == nil {
					first = err
				}
			case len(rows) > 0: // none yet means not logged yet, which TurnCost retries
				out[i] = rows[0].cost()
			}
		})
	}
	wg.Wait()
	return first
}

func (s *source) get(ctx context.Context, path string, q url.Values, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(s.o.BaseURL, "/")+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.o.APIKey)
	req.Header.Set("Accept", "application/json")
	hc := s.o.HTTPClient
	if hc == nil {
		hc = provider.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return &kernel.CostError{Status: resp.StatusCode, Message: strings.TrimSpace(string(b[:min(len(b), 200)]))}
	}
	if err := json.Unmarshal(b, into); err != nil {
		return fmt.Errorf("litellm: %s: %w", path, err)
	}
	return nil
}

type spendRow struct {
	RequestID string  `json:"request_id"`
	Spend     float64 `json:"spend"`
}

func (r spendRow) cost() kernel.Cost { return kernel.Cost{Amount: r.Spend, Currency: "USD"} }

// spendRows is /spend/logs, which answers a list, or on some versions a single log.
type spendRows []spendRow

func (r *spendRows) UnmarshalJSON(b []byte) error {
	if t := strings.TrimSpace(string(b)); strings.HasPrefix(t, "{") {
		var one spendRow
		if err := json.Unmarshal(b, &one); err != nil {
			return err
		}
		if one.RequestID != "" || one.Spend != 0 {
			*r = spendRows{one}
		}
		return nil
	}
	return json.Unmarshal(b, (*[]spendRow)(r))
}
