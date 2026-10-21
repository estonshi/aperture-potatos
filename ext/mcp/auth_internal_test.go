package mcp

import (
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestCheckAuthURL(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://as.example/authorize?x=1": true,
		"http://as.example/authorize":      false,
		"javascript:alert(1)":              false,
		"data:text/html,hi":                false,
		"file:///etc/passwd":               false,
		"https:///nohost":                  false,
	} {
		if err := checkAuthURL(raw, false); (err == nil) != ok {
			t.Errorf("%s: err %v", raw, err)
		}
	}
	if checkAuthURL("http://as.example/a", true) != nil || checkAuthURL("javascript:alert(1)", true) == nil {
		t.Error("development mode lets http through and nothing else")
	}
}

func TestBlockedIP(t *testing.T) {
	for addr, blocked := range map[string]bool{
		"10.0.0.1": true, "172.16.3.4": true, "192.168.1.1": true, "127.0.0.1": true, "169.254.169.254": true,
		"::1": true, "fe80::1": true, "fc00::1": true, "::ffff:10.0.0.1": true, "100.64.0.1": true, "0.0.0.0": true,
		"93.184.216.34": false, "2606:2800:220:1::1": false,
	} {
		if got := blockedIP(netip.MustParseAddr(addr)); got != blocked {
			t.Errorf("%s: blocked %v", addr, got)
		}
	}
}

// TestGuardedClient: OAuth requests to a loopback address or over http are refused before anything is sent.
func TestGuardedClient(t *testing.T) {
	srv := httptest.NewTLSServer(nil)
	defer srv.Close()
	c := guardedClient(false)
	if _, err := c.Get(srv.URL); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("loopback over https: %v", err)
	}
	if _, err := c.Get("http://93.184.216.34/"); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("plain http: %v", err)
	}
}

// TestGuardedRedirect: a redirect off https is refused before it is followed.
func TestGuardedRedirect(t *testing.T) {
	c := guardedClient(false)
	req := httptest.NewRequest("GET", "http://169.254.169.254/latest/meta-data/", nil)
	if err := c.CheckRedirect(req, nil); err == nil {
		t.Fatal("a redirect to plain http was allowed")
	}
}
