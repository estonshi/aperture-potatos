// Package compare is the one implementation of the conformance comparison rules (conformance/README.md): it compares the actual.* files a binding's runner wrote for a case with the case's expected.* files, and accepts them as the new expected files (conformance/runner-protocol.md).
package compare

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Status is how a case came out.
type Status int

const (
	Passed  Status = iota
	Failed         // an output differs, or the runner wrote actual.fail
	Skipped        // the runner wrote actual.skip
	NotRun         // the runner wrote nothing for the case
)

func (s Status) String() string {
	return [...]string{"ok", "FAIL", "skip", "not run"}[s]
}

// Result is a case's status, with the first difference, the runner's failure or its reason for skipping.
type Result struct {
	Status Status
	Detail string
}

// Check compares the outputs in out with the case at path, a case directory or an errors/<api>/<case>.json file.
func Check(path, out string) Result {
	if reason, ok := marker(out, "actual.skip"); ok {
		return Result{Skipped, reason}
	}
	if reason, ok := marker(out, "actual.fail"); ok {
		return Result{Failed, "runner: " + reason}
	}
	if _, err := os.Stat(out); err != nil {
		return Result{Status: NotRun}
	}
	c, err := load(path)
	if err != nil {
		return Result{Failed, err.Error()}
	}
	files, err := c.files(out)
	if err != nil {
		return Result{Failed, err.Error()}
	}
	var errs []error
	for _, f := range files {
		if err := c.diff(f); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return Result{Failed, err.Error()}
	}
	return Result{Status: Passed}
}

// Update makes the case's expected files those in out, rewriting only the ones that differ and removing those out has no counterpart for, and returns the files it changed.
func Update(path, out string) ([]string, error) {
	if reason, ok := marker(out, "actual.fail"); ok {
		return nil, fmt.Errorf("runner: %s", reason)
	}
	if _, ok := marker(out, "actual.skip"); ok {
		return nil, nil
	}
	c, err := load(path)
	if err != nil {
		return nil, err
	}
	files, err := c.files(out)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		if err := c.valid(f); err != nil {
			return nil, err
		}
	}
	var changed []string
	for _, f := range files {
		if c.diff(f) == nil {
			continue
		}
		if err := c.accept(f); err != nil {
			return changed, err
		}
		changed = append(changed, f.expectedPath)
	}
	if !c.errorCase {
		pruneEmpty(c.path)
	}
	return changed, nil
}

func marker(out, name string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(out, name))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

// kase is what decides which rules apply to a case's files.
type kase struct {
	path        string // the case directory, or an error case's file
	errorCase   bool   // errors/<api>/<case>.json, whose expected value is the file's expected field
	loop        bool   // a case with a script.json
	interleaved bool   // its clock is interleaved
	anthropic   bool   // a render case of anthropic.messages
}

func load(path string) (kase, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return kase{}, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return kase{}, err
	}
	c := kase{path: abs}
	if !fi.IsDir() {
		c.errorCase = true
		return c, nil
	}
	c.anthropic = filepath.Base(filepath.Dir(abs)) == "anthropic.messages" && filepath.Base(filepath.Dir(filepath.Dir(abs))) == "render"
	b, err := os.ReadFile(filepath.Join(abs, "script.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return kase{}, err
	}
	var sc struct {
		Clock struct {
			Interleaved bool `json:"interleaved"`
		} `json:"clock"`
	}
	if err := json.Unmarshal(b, &sc); err != nil {
		return kase{}, fmt.Errorf("script.json: %w", err)
	}
	c.loop, c.interleaved = true, sc.Clock.Interleaved
	return c, nil
}

// file is one output: rest is its name without the expected. or actual. prefix, and a nil side is absent.
type file struct {
	rest         string
	expectedPath string
	actualPath   string
	expected     []byte
	actual       []byte
}

func (c kase) files(out string) ([]file, error) {
	if c.errorCase {
		f := file{rest: "json", expectedPath: c.path, actualPath: filepath.Join(out, "actual.json")}
		b, err := os.ReadFile(c.path)
		if err != nil {
			return nil, err
		}
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(b, &doc); err != nil {
			return nil, fmt.Errorf("%s: %w", c.path, err)
		}
		f.expected = doc["expected"]
		if f.actual, err = readIfAny(f.actualPath); err != nil {
			return nil, err
		}
		return []file{f}, nil
	}
	exp, err := prefixed(c.path, "expected.")
	if err != nil {
		return nil, err
	}
	act, err := prefixed(out, "actual.")
	if err != nil {
		return nil, err
	}
	delete(act, "skip")
	delete(act, "fail")
	var rests []string
	for r := range exp {
		rests = append(rests, r)
	}
	for r := range act {
		if _, ok := exp[r]; !ok {
			rests = append(rests, r)
		}
	}
	slices.Sort(rests)
	files := make([]file, len(rests))
	for i, r := range rests {
		files[i] = file{rest: r, expectedPath: filepath.Join(c.path, "expected."+filepath.FromSlash(r)), actualPath: filepath.Join(out, "actual."+filepath.FromSlash(r)), expected: exp[r], actual: act[r]}
	}
	return files, nil
}

// prefixed reads every file under dir whose top-level entry starts with prefix, keyed by its slash path without the prefix; a directory such as expected.requests contributes each file in it.
func prefixed(dir, prefix string) (map[string][]byte, error) {
	out := map[string][]byte{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		top := filepath.Join(dir, e.Name())
		err := filepath.WalkDir(top, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(dir, p)
			b, err := os.ReadFile(p)
			out[strings.TrimPrefix(filepath.ToSlash(rel), prefix)] = b
			return err
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func readIfAny(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// diff compares one output by the rules for its name, an absent JSONL file counting as empty.
func (c kase) diff(f file) error {
	name := "expected." + f.rest
	if c.errorCase {
		name = "expected"
	}
	if strings.HasSuffix(f.rest, ".jsonl") {
		w, g := c.lines(f.rest, f.expected), c.lines(f.rest, f.actual)
		for i := range min(len(w), len(g)) {
			if d := differ(w[i], g[i]); d != "" {
				return fmt.Errorf("%s line %d: %s", name, i+1, d)
			}
		}
		if len(w) != len(g) {
			return fmt.Errorf("%s: %d lines, want %d", name, len(g), len(w))
		}
		return nil
	}
	switch {
	case f.expected == nil && f.actual == nil:
		return nil
	case f.expected == nil:
		return fmt.Errorf("%s: missing, the case produced %s", name, filepath.Base(f.actualPath))
	case f.actual == nil:
		return fmt.Errorf("%s: the case produced none", name)
	}
	if d := differ(c.doc(f.rest, f.expected), c.doc(f.rest, f.actual)); d != "" {
		return fmt.Errorf("%s: %s", name, d)
	}
	return c.valid(f)
}

// valid checks what an output must satisfy whatever its expected file says, so update refuses it too.
func (c kase) valid(f file) error {
	if c.anthropic && f.rest == "body.json" && f.actual != nil {
		if n := cacheControls(f.actual); n > 4 {
			return fmt.Errorf("%s: %d cache_control breakpoints, the API allows 4", filepath.Base(f.actualPath), n)
		}
	}
	return nil
}

// lines normalizes a JSONL file's lines by what its name says they hold.
func (c kase) lines(rest string, b []byte) []json.RawMessage {
	ls := ReadJSONL(b)
	events := rest == "events.jsonl" || strings.HasSuffix(rest, ".events.jsonl")
	store := rest == "store.jsonl" || strings.HasSuffix(rest, ".store.jsonl")
	switch {
	case c.loop && events:
		ls = noticeKinds(concurrent(ls))
	case c.loop && rest == "hooks.jsonl":
		ls = concurrentHooks(ls)
	case rest == "stream.jsonl":
		for i, l := range ls {
			ls[i] = unenveloped(l, true)
		}
	}
	if c.interleaved && (events || store || rest == "calls.jsonl") {
		ls = untimed(ls)
	}
	return ls
}

// doc normalizes a JSON file by what its name says it holds.
func (c kase) doc(rest string, b []byte) []byte {
	switch {
	case rest == "message.json":
		return unenveloped(b, false)
	case c.interleaved && rest == "ledger.json":
		return untimed([]json.RawMessage{b})[0]
	}
	return b
}

// cacheControls counts the cache_control fields of an Anthropic request body.
func cacheControls(b []byte) int {
	v, err := Canonical(b)
	if err != nil {
		return 0
	}
	n := 0
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, c := range x {
				if k == "cache_control" {
					n++
				}
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	walk(v)
	return n
}

// accept writes the actual side as the expected file, formatted the one way expected files are, or removes the expected file when there is no actual one.
func (c kase) accept(f file) error {
	if c.errorCase {
		b, err := os.ReadFile(c.path)
		if err != nil {
			return err
		}
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(b, &doc); err != nil {
			return err
		}
		if f.actual == nil {
			delete(doc, "expected")
		} else {
			doc["expected"] = f.actual
		}
		pretty, err := Pretty(doc)
		if err != nil {
			return err
		}
		return os.WriteFile(c.path, pretty, 0o644)
	}
	if f.actual == nil {
		return os.Remove(f.expectedPath)
	}
	var out []byte
	var err error
	if strings.HasSuffix(f.rest, ".jsonl") {
		out, err = JSONL(ReadJSONL(f.actual))
	} else {
		var v any
		if v, err = Canonical(f.actual); err == nil {
			out, err = Pretty(v)
		}
	}
	if err != nil {
		return fmt.Errorf("%s: %w", f.actualPath, err)
	}
	if err := os.MkdirAll(filepath.Dir(f.expectedPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(f.expectedPath, out, 0o644)
}

// pruneEmpty removes expected.* directories that an update left empty.
func pruneEmpty(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "expected.") {
			if left, _ := os.ReadDir(filepath.Join(dir, e.Name())); len(left) == 0 {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
}
