package skills_test

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/estonshi/aperture-potatos/conformance"
	"github.com/estonshi/aperture-potatos/ext/skills"
	"github.com/estonshi/aperture-potatos/kernel"
)

var update = flag.Bool("update", false, "rewrite expected files from the Go reference implementation")

type caseHostSkill struct {
	Description    string `json:"description"`
	Body           string `json:"body"`
	UserInvocable  *bool  `json:"user_invocable"`
	ModelInvocable *bool  `json:"model_invocable"`
}

type caseRewrite struct {
	Replace []struct {
		Old string `json:"old"`
		New string `json:"new"`
	} `json:"replace"`
	Error string `json:"error"`
}

// caseOptions holds the skills of a case inline, path to file content, so a case is one script.
type caseOptions struct {
	Files   map[string]string        `json:"files"`
	Host    map[string]caseHostSkill `json:"host"`
	Rewrite *caseRewrite             `json:"rewrite"`
}

// hostSource lists exactly the skills a case's host gives it, without the file-system rules.
type hostSource struct {
	list []skills.Frontmatter
	body map[string]string
}

func (h hostSource) List(context.Context) ([]skills.Frontmatter, error) {
	return slices.Clone(h.list), nil
}

func (h hostSource) Load(_ context.Context, name string) (*skills.Skill, error) {
	for _, fm := range h.list {
		if fm.Name == name {
			return &skills.Skill{Frontmatter: fm, Body: h.body[name]}, nil
		}
	}
	return nil, skills.ErrNotFound
}

func (h hostSource) Open(context.Context, string, string) (io.ReadCloser, error) {
	return nil, skills.ErrNotFound
}

func build(name string, raw json.RawMessage) (kernel.Extension, error) {
	if name != skills.Name {
		return nil, fmt.Errorf("unknown extension %q", name)
	}
	var o caseOptions
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &o); err != nil {
			return nil, err
		}
	}
	fsys := fstest.MapFS{}
	for p, content := range o.Files {
		fsys[p] = &fstest.MapFile{Data: []byte(content)}
	}
	sources := []skills.Source{skills.FS(fsys)}
	if len(o.Host) > 0 {
		names := make([]string, 0, len(o.Host))
		for name := range o.Host {
			names = append(names, name)
		}
		slices.Sort(names)
		h := hostSource{body: map[string]string{}}
		for _, name := range names {
			sk := o.Host[name]
			user, model := true, true
			if sk.UserInvocable != nil {
				user = *sk.UserInvocable
			}
			if sk.ModelInvocable != nil {
				model = *sk.ModelInvocable
			}
			h.list = append(h.list, skills.Frontmatter{Name: name, Description: sk.Description, UserInvocable: user, ModelInvocable: model, Location: name + "/SKILL.md"})
			h.body[name] = sk.Body
		}
		sources = append(sources, h)
	}
	opts := skills.Options{Sources: sources}
	if o.Rewrite != nil {
		rw := *o.Rewrite
		opts.Rewrite = func(_ context.Context, in skills.RewriteInput) (string, error) {
			if rw.Error != "" {
				return "", errors.New(rw.Error)
			}
			body := in.Body
			for _, r := range rw.Replace {
				body = strings.ReplaceAll(body, r.Old, r.New)
			}
			return body, nil
		}
	}
	return skills.New(opts), nil
}

// TestConformance runs conformance/loop/ext/skills, the cases this extension owns and deletes with itself.
func TestConformance(t *testing.T) {
	root := filepath.Join("..", "..", "conformance", "loop", "ext", "skills")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Skip("no cases")
	}
	for _, e := range entries {
		dir := filepath.Join(root, e.Name())
		t.Run(e.Name(), func(t *testing.T) {
			res := conformance.RunLoopWithPoints(t, dir, build, skills.Activate)
			if t.Failed() {
				return
			}
			if err := res.Expected(dir, *update); err != nil {
				t.Fatal(err)
			}
		})
	}
}
