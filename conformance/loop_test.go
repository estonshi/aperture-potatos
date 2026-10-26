package conformance

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/estonshi/aperture-potatos/spec"
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

// TestSupports: before 1.0 a binding runs its own MAJOR.MINOR and nothing else, and a three-part version is never a spec version.
func TestSupports(t *testing.T) {
	v := strings.TrimSpace(spec.MustText("VERSION"))
	major, minor, ok := strings.Cut(v, ".")
	if !ok {
		t.Fatalf("spec/VERSION is %q, want MAJOR.MINOR", v)
	}
	n, err := strconv.Atoi(minor)
	if err != nil {
		t.Fatalf("spec/VERSION is %q, want a minor number", v)
	}
	if !supports(v) {
		t.Errorf("supports(%q) = false for the binding's own version", v)
	}
	if supports(major + "." + strconv.Itoa(n+1)) {
		t.Errorf("supports(%q) = true, but a newer minor is not implemented before 1.0", major+"."+strconv.Itoa(n+1))
	}
	if major == "0" && n > 0 && supports(major+"."+strconv.Itoa(n-1)) {
		t.Errorf("supports(%q) = true, but an older minor is not implemented before 1.0", major+"."+strconv.Itoa(n-1))
	}
	for _, bad := range []string{v + ".0", "", "0"} {
		if supports(bad) {
			t.Errorf("supports(%q) = true, want false", bad)
		}
	}
}
