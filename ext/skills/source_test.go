package skills_test

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/estonshi/aperture-potatos/ext/skills"
)

func read(t *testing.T, src skills.Source, name, p string) (string, error) {
	t.Helper()
	rc, err := src.Open(t.Context(), name, p)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	return string(b), err
}

// TestFSLoadsExampleSkills reads skills written to the Agent Skills specification, with a vendor field and folded description, and skips what is broken or vendored.
func TestFSLoadsExampleSkills(t *testing.T) {
	var logs bytes.Buffer
	src := skills.FS(os.DirFS("testdata/skills"), skills.WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	list, err := src.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("%+v", list)
	}
	pdf, notes := list[0], list[1]
	if pdf.Name != "pdf-processing" || pdf.License != "Apache-2.0" || !reflect.DeepEqual(pdf.Metadata, map[string]string{"author": "example-org", "version": "1.0"}) ||
		!reflect.DeepEqual(pdf.AllowedTools, []string{"Bash(pdftotext:*)", "Read"}) || !pdf.UserInvocable || !pdf.ModelInvocable || pdf.Location != "pdf-processing/SKILL.md" {
		t.Fatalf("pdf %+v", pdf)
	}
	if notes.Name != "release-notes" || notes.Description != "Draft release notes from merged changes. Use when: a release is being cut." || notes.ModelInvocable || !notes.UserInvocable ||
		notes.Extra["when_to_use"] != "Before tagging a release." || notes.Extra["argument-hint"] != "[version]" || notes.Location != "team/release-notes/SKILL.md" {
		t.Fatalf("notes %+v", notes)
	}
	if !strings.Contains(logs.String(), "no description") || strings.Contains(logs.String(), "dep") {
		t.Fatalf("warnings:\n%s", logs.String())
	}
	sk, err := src.Load(t.Context(), "pdf-processing")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sk.Body, "# PDF processing\n\n1. Extract") || !reflect.DeepEqual(sk.Resources, []string{"references/REFERENCE.md", "scripts/extract.py"}) {
		t.Fatalf("%q %v", sk.Body, sk.Resources)
	}
	if got, err := read(t, src, "pdf-processing", "references/REFERENCE.md"); err != nil || !strings.HasPrefix(got, "# Form fields") {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := src.Load(t.Context(), "broken"); !errors.Is(err, skills.ErrNotFound) {
		t.Fatalf("broken: %v", err)
	}
}

func TestFSLenientNames(t *testing.T) {
	fsys := fstest.MapFS{
		"a/SKILL.md":            {Data: []byte("---\ndescription: No name.\n---\nA")},
		"b/SKILL.md":            {Data: []byte("---\nname: other\ndescription: Named apart from its directory.\n---\nB")},
		"c/SKILL.md":            {Data: []byte("---\nname: other\ndescription: A second one of that name.\n---\nC")},
		"deep/1/2/3/4/SKILL.md": {Data: []byte("---\nname: deep\ndescription: Too deep.\n---\n")},
		"plain/SKILL.md":        {Data: []byte("No frontmatter.")},
		"Bad--Name/SKILL.md":    {Data: []byte("---\ndescription: A name the rules refuse.\n---\n")},
	}
	var logs bytes.Buffer
	list, err := skills.FS(fsys, skills.WithLogger(slog.New(slog.NewTextHandler(&logs, nil)))).List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, fm := range list {
		names = append(names, fm.Name+"@"+fm.Location)
	}
	if strings.Join(names, " ") != "a@a/SKILL.md other@b/SKILL.md" {
		t.Fatalf("%v\n%s", names, logs.String())
	}
	for _, w := range []string{"using the directory", "differs from its directory", "duplicate name", "does not start with", "name does not follow"} {
		if !strings.Contains(logs.String(), w) {
			t.Errorf("no warning %q in\n%s", w, logs.String())
		}
	}
}

// TestFSStrictParserRetriedWithQuotes: a parser that refuses an unquoted ": " in a value gets the frontmatter again with such values quoted.
func TestFSStrictParserRetriedWithQuotes(t *testing.T) {
	var seen []string
	strict := func(raw []byte) (skills.Frontmatter, error) {
		seen = append(seen, string(raw))
		for _, l := range strings.Split(string(raw), "\n") {
			if _, v, _ := strings.Cut(l, ": "); strings.Contains(v, ": ") && !strings.HasPrefix(v, `"`) {
				return skills.Frontmatter{}, errors.New("mapping values are not allowed here")
			}
		}
		return skills.Frontmatter{Name: "a", Description: "ok", ModelInvocable: true}, nil
	}
	fsys := fstest.MapFS{"a/SKILL.md": {Data: []byte("---\nname: a\ndescription: Use when: asked\n---\n")}}
	list, err := skills.FS(fsys, skills.WithFrontmatterParser(strict)).List(t.Context())
	if err != nil || len(list) != 1 || len(seen) != 2 || !strings.Contains(seen[1], `"Use when: asked"`) {
		t.Fatalf("%+v %v %q", list, err, seen)
	}
}

func TestOpenStaysInsideTheSkill(t *testing.T) {
	root := t.TempDir()
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(root, "a", "refs"), 0o755))
	must(os.WriteFile(filepath.Join(root, "a", "SKILL.md"), []byte("---\nname: a\ndescription: d\n---\nA"), 0o644))
	must(os.WriteFile(filepath.Join(root, "a", "refs", "ok.md"), []byte("fine"), 0o644))
	must(os.WriteFile(filepath.Join(root, "secret.txt"), []byte("secret"), 0o644))
	must(os.Symlink(filepath.Join(root, "secret.txt"), filepath.Join(root, "a", "link.txt")))
	must(os.Symlink(root, filepath.Join(root, "a", "up")))
	src := skills.FS(os.DirFS(root))
	if got, err := read(t, src, "a", "refs/ok.md"); err != nil || got != "fine" {
		t.Fatalf("%q %v", got, err)
	}
	for _, p := range []string{"../secret.txt", "/secret.txt", "refs/../../secret.txt", "%2e%2e/secret.txt", "%2E%2E/secret.txt", `refs\..\..\secret.txt`, ".", "", "link.txt", "up/secret.txt", "refs", "./refs/ok.md", "refs//ok.md"} {
		if got, err := read(t, src, "a", p); !errors.Is(err, skills.ErrInvalidPath) {
			t.Errorf("%q: %q %v", p, got, err)
		}
	}
	if _, err := read(t, src, "a", "refs/none.md"); !errors.Is(err, skills.ErrNotFound) {
		t.Errorf("missing file: %v", err)
	}
	if _, err := read(t, src, "b", "x"); !errors.Is(err, skills.ErrNotFound) {
		t.Errorf("missing skill: %v", err)
	}
}

func TestMergeFirstWins(t *testing.T) {
	one := skills.FS(fstest.MapFS{"x/SKILL.md": {Data: []byte("---\nname: x\ndescription: first\n---\nONE")}})
	two := skills.FS(fstest.MapFS{
		"x/SKILL.md": {Data: []byte("---\nname: x\ndescription: second\n---\nTWO")},
		"y/SKILL.md": {Data: []byte("---\nname: y\ndescription: only\n---\nY")},
	})
	m := skills.Merge(one, two)
	list, err := m.List(t.Context())
	if err != nil || len(list) != 2 || list[0].Description != "first" || list[1].Name != "y" {
		t.Fatalf("%+v %v", list, err)
	}
	for name, body := range map[string]string{"x": "ONE", "y": "Y"} {
		if sk, err := m.Load(t.Context(), name); err != nil || sk.Body != body {
			t.Fatalf("%s: %+v %v", name, sk, err)
		}
	}
}

// FuzzResourcePath: whatever path the model asks for, Open never hands out a file outside the skill's directory.
func FuzzResourcePath(f *testing.F) {
	fsys := fstest.MapFS{
		"skills/a/SKILL.md":   {Data: []byte("---\nname: a\ndescription: d\n---\nA")},
		"skills/a/refs/ok.md": {Data: []byte("inside")},
		"skills/b/SKILL.md":   {Data: []byte("---\nname: b\ndescription: d\n---\nB")},
		"skills/b/private.md": {Data: []byte("outside")},
		"secret.txt":          {Data: []byte("outside")},
	}
	src := skills.FS(fsys)
	for _, p := range []string{"refs/ok.md", "../b/private.md", "../../secret.txt", "%2e%2e/b/private.md", "refs/../../b/private.md", `..\b\private.md`, "/secret.txt"} {
		f.Add(p)
	}
	f.Fuzz(func(t *testing.T, p string) {
		sub, err := src.Open(t.Context(), "a", "../"+p) // the same prefix a model might try, as well as the path itself
		if err == nil {
			sub.Close()
			t.Fatalf("../%q opened", p)
		}
		rc, err := src.Open(t.Context(), "a", p)
		if err != nil {
			return
		}
		defer rc.Close()
		if b, _ := io.ReadAll(rc); string(b) == "outside" {
			t.Fatalf("%q reached a file outside the skill", p)
		}
	})
}
