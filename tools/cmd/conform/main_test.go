package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRun(t *testing.T) {
	cases, out := t.TempDir(), t.TempDir()
	write(t, cases, map[string]string{
		"loop/ok/expected.events.jsonl":     "{\"seq\":1}\n",
		"loop/wrong/expected.events.jsonl":  "{\"seq\":1}\n",
		"loop/skipped/expected.store.jsonl": "{}\n",
		"loop/absent/expected.store.jsonl":  "{}\n",
	})
	write(t, out, map[string]string{
		"loop/ok/actual.events.jsonl":    `{"seq": 1}`,
		"loop/wrong/actual.events.jsonl": `{"seq": 2}`,
		"loop/skipped/actual.skip":       "pending in this binding",
	})
	var stdout, stderr bytes.Buffer
	if code := run([]string{"diff", cases, out}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1\n%s%s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{"FAIL\tloop/wrong: ", "skip\tloop/skipped: pending in this binding", "not run\tloop/absent", "1 passed, 1 failed, 1 skipped, 1 not run"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, stdout.String())
		}
	}
	stdout.Reset()
	if code := run([]string{"diff", filepath.Join(cases, "loop", "ok"), filepath.Join(out, "loop", "ok")}, &stdout, &stderr); code != 0 {
		t.Fatalf("one passing case: exit %d\n%s", code, stdout.String())
	}
	stdout.Reset()
	if code := run([]string{"update", cases, out}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "1 files updated") {
		t.Fatalf("update: exit %d\n%s%s", code, stdout.String(), stderr.String())
	}
	if code := run([]string{"diff", filepath.Join(cases, "loop", "wrong"), filepath.Join(out, "loop", "wrong")}, &stdout, &stderr); code != 0 {
		t.Fatalf("after update: exit %d\n%s", code, stdout.String())
	}
	if code := run([]string{"diff", cases}, &stdout, &stderr); code != 2 {
		t.Fatalf("usage error: exit %d, want 2", code)
	}
}
