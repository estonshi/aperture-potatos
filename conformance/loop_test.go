package conformance

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoop runs every kernel case under loop/; extension cases live in loop/ext/ and are driven by their extensions.
func TestLoop(t *testing.T) {
	entries, err := os.ReadDir("loop")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		dir := filepath.Join("loop", e.Name())
		if _, err := os.Stat(filepath.Join(dir, "script.json")); err != nil {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			res := RunLoop(t, dir)
			if t.Failed() {
				return
			}
			if err := res.Expected(dir, *update); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSupports(t *testing.T) {
	for version, want := range map[string]bool{"0.1": true, "0.2": false, "1.0": false, "0.1.0": false, "": false} {
		if got := supports(version); got != want {
			t.Errorf("supports(%q) = %v, want %v", version, got, want)
		}
	}
}
