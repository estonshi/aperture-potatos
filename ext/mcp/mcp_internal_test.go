package mcp

import (
	"errors"
	"strings"
	"testing"

	"github.com/estonshi/aperture-potatos/kernel"
)

// TestServerNames: the host's key may carry capitals, underscores and dashes, but nothing else and no more than 64 characters.
func TestServerNames(t *testing.T) {
	for _, name := range []string{"a", "my_server", "GitHub", "claude_ai_Gmail", "_lead", "-lead", strings.Repeat("s", 64)} {
		if err := validate([]Server{{Name: name, URL: "http://x"}}); err != nil {
			t.Errorf("%q rejected: %v", name, err)
		}
	}
	for _, name := range []string{"", "bad name", "bad.name", "srv/1", "sérv", strings.Repeat("s", 65)} {
		if err := validate([]Server{{Name: name, URL: "http://x"}}); !errors.Is(err, kernel.ErrInvalidName) {
			t.Errorf("%q accepted: %v", name, err)
		}
	}
}
