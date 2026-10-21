package provider_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/estonshi/aperture-potatos/provider"
)

// TestDefaultClientStaysOnHost: a redirect within the host is followed, one to another host is refused before anything is sent there.
func TestDefaultClientStaysOnHost(t *testing.T) {
	hits := 0
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/same":
			http.Redirect(w, r, "/done", http.StatusTemporaryRedirect)
		case "/other":
			http.Redirect(w, r, other.URL+"/steal", http.StatusTemporaryRedirect)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	resp, err := provider.DefaultClient.Post(srv.URL+"/same", "application/json", strings.NewReader("{}"))
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("same host: %v %v", err, resp)
	}
	resp.Body.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/other", strings.NewReader(`{"history":"secret"}`))
	req.Header.Set("x-api-key", "k")
	if _, err := provider.DefaultClient.Do(req); err == nil || hits != 0 {
		t.Fatalf("another host: %v, %d hits", err, hits)
	}
}
