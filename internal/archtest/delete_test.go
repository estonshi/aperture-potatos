package archtest

import (
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// deletions is the self-check table of the layering: deleting a row's paths, and its line in std, leaves the whole module building and passing.
var deletions = []struct {
	name  string
	paths []string
}{
	{"ext/cmd/clearcmd", []string{"ext/cmd/clearcmd", "spec/ext/clearcmd", "conformance/loop/ext/clearcmd"}},
	{"ext/cmd/compactcmd", []string{"ext/cmd/compactcmd", "spec/ext/compactcmd", "conformance/loop/ext/compactcmd"}},
	{"ext/cmd/contextcmd", []string{"ext/cmd/contextcmd", "spec/ext/contextcmd", "conformance/loop/ext/contextcmd"}},
	{"ext/cmd/forkcmd", []string{"ext/cmd/forkcmd", "spec/ext/forkcmd", "conformance/loop/ext/forkcmd"}},
	{"ext/cmd/helpcmd", []string{"ext/cmd/helpcmd", "spec/ext/helpcmd", "conformance/loop/ext/helpcmd"}},
	{"ext/cmd/rewindcmd", []string{"ext/cmd/rewindcmd", "spec/ext/rewindcmd", "conformance/loop/ext/rewindcmd"}},
	{"ext/cmd/usagecmd", []string{"ext/cmd/usagecmd", "spec/ext/usagecmd", "conformance/loop/ext/usagecmd"}},
	{"ext/compact", []string{"ext/compact", "spec/ext/compact", "conformance/loop/ext/compact"}},
	{"ext/history", []string{"ext/history", "spec/ext/history", "conformance/loop/ext/history"}},
	{"ext/subagent", []string{"ext/subagent", "spec/ext/subagent", "conformance/loop/ext/subagent"}},
	{"ext/skills", []string{"ext/skills", "spec/ext/skills", "conformance/loop/ext/skills"}},
	{"ext/memory", []string{"ext/memory", "spec/ext/memory", "conformance/loop/ext/memory"}},
	{"ext/peer", []string{"ext/peer", "spec/ext/peer", "conformance/loop/ext/peer", "conformance/a2a"}},
	{"ext/tasks", []string{"ext/tasks", "spec/ext/tasks", "conformance/loop/ext/tasks"}},
	{"ext/litellm", []string{"ext/litellm"}},         // not an extension, so std has no line of it
	{"ext/mcp", []string{"ext/mcp", "spec/ext/mcp"}}, // not a default extension, so std has no line of it
}

// TestDeletePackages copies the module once per row and runs go build, vet and test there; it is slow, so it runs only with POTATOS_DELETE_TEST=1.
func TestDeletePackages(t *testing.T) {
	if os.Getenv("POTATOS_DELETE_TEST") != "1" {
		t.Skip("set POTATOS_DELETE_TEST=1 to delete each extension in a copy of the module and build and test what is left")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range deletions {
		t.Run(row.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := copyModule(root, dir); err != nil {
				t.Fatal(err)
			}
			for _, p := range row.paths {
				if err := os.RemoveAll(filepath.Join(dir, p)); err != nil {
					t.Fatal(err)
				}
			}
			if err := dropStdLine(filepath.Join(dir, "std", "std.go"), row.name); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"build", "./..."}, {"vet", "./..."}, {"test", "./..."}} {
				cmd := exec.Command("go", args...)
				cmd.Dir = dir
				cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "POTATOS_DELETE_TEST=") }) // the copy's own run of this test must skip, or it recurses
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("go %s after deleting %s: %v\n%s", strings.Join(args, " "), row.name, err, out)
				}
			}
		})
	}
}

func copyModule(from, to string) error {
	return filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		if d.IsDir() && (d.Name() == ".git" || rel != "." && strings.HasPrefix(d.Name(), ".")) {
			return filepath.SkipDir
		}
		dst := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
}

// dropStdLine removes the extension's import and its line in the default list, the one edit the table allows outside the deleted paths.
func dropStdLine(file, pkg string) error {
	b, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	name := path.Base(pkg)
	var keep []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, `"`+module+"/"+pkg+`"`) || strings.Contains(l, name+".") {
			continue
		}
		keep = append(keep, l)
	}
	return os.WriteFile(file, []byte(strings.Join(keep, "\n")), 0o644)
}
