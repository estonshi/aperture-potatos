package compactcmd

import "testing"

func TestTokens(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 999: "999", 1000: "1k", 1540: "1.5k", 9990: "10k", 23400: "23k", 182600: "183k"} {
		if got := tokens(n); got != want {
			t.Errorf("tokens(%d) = %q, want %q", n, got, want)
		}
	}
}
