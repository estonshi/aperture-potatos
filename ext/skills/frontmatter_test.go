package skills

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseFrontmatter(t *testing.T) {
	cases := []struct {
		name, raw string
		want      Frontmatter
	}{
		{"plain", "name: a\ndescription: Does a.", Frontmatter{Name: "a", Description: "Does a.", UserInvocable: true, ModelInvocable: true}},
		{"colon in a plain value", "name: a\ndescription: Use when: the user asks # a comment", Frontmatter{Name: "a", Description: "Use when: the user asks", UserInvocable: true, ModelInvocable: true}},
		{"quoted", "name: 'a'\ndescription: \"Say \\\"hi\\\": now\"", Frontmatter{Name: "a", Description: `Say "hi": now`, UserInvocable: true, ModelInvocable: true}},
		{"single quote escape", "name: a\ndescription: 'it''s # not a comment'", Frontmatter{Name: "a", Description: "it's # not a comment", UserInvocable: true, ModelInvocable: true}},
		{"literal block", "name: a\ndescription: |\n  one\n    two\n\n  three\n", Frontmatter{Name: "a", Description: "one\n  two\n\nthree", UserInvocable: true, ModelInvocable: true}},
		{"folded block", "name: a\ndescription: >-\n  one\n  two\n\n  three\n", Frontmatter{Name: "a", Description: "one two\nthree", UserInvocable: true, ModelInvocable: true}},
		{"continued plain value", "name: a\ndescription: one\n  two", Frontmatter{Name: "a", Description: "one two", UserInvocable: true, ModelInvocable: true}},
		{"metadata and tools", "name: a\ndescription: d\nmetadata:\n  author: me\n  \"version\": \"1.0\"\nallowed-tools: Bash(git:*) Read, Write",
			Frontmatter{Name: "a", Description: "d", Metadata: map[string]string{"author": "me", "version": "1.0"}, AllowedTools: []string{"Bash(git:*)", "Read", "Write"}, UserInvocable: true, ModelInvocable: true}},
		{"tool list", "name: a\ndescription: d\nallowed-tools:\n  - Read\n  - Bash(ls, cat)", Frontmatter{Name: "a", Description: "d", AllowedTools: []string{"Read", "Bash(ls, cat)"}, UserInvocable: true, ModelInvocable: true}},
		{"vendor fields", "name: a\ndescription: d\nuser-invocable: false\ndisable-model-invocation: True\nwhen_to_use: later\narguments:\n  - file",
			Frontmatter{Name: "a", Description: "d", Extra: map[string]any{"when_to_use": "later", "arguments": []any{"file"}}}},
		{"comments and blanks", "# top\n\nname: a\n\ndescription: d\n", Frontmatter{Name: "a", Description: "d", UserInvocable: true, ModelInvocable: true}},
	}
	for _, c := range cases {
		got, err := parseFrontmatter([]byte(c.raw))
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %+v %v, want %+v", c.name, got, err, c.want)
		}
	}
	for _, bad := range []string{"name a", "  name: a", "name: a\n\tdescription: d", "name: \"a", "user-invocable: maybe", "metadata:\n  - x", "name: 'a'\n  more"} {
		if _, err := parseFrontmatter([]byte(bad)); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestSplit(t *testing.T) {
	raw, body, err := split([]byte("\ufeff---\r\nname: a\r\n---\r\n\r\nBody\r\n"))
	if err != nil || string(raw) != "name: a\r\n" || body != "Body" {
		t.Fatalf("%q %q %v", raw, body, err)
	}
	for _, bad := range []string{"name: a", "---\nname: a\n", ""} {
		if _, _, err := split([]byte(bad)); err == nil {
			t.Errorf("%q split", bad)
		}
	}
}

func TestQuoteColons(t *testing.T) {
	got := string(quoteColons([]byte("name: a\ndescription: Use when: asked\nmetadata:\n  k: v: w\nlicense: \"x: y\"")))
	if want := "name: a\ndescription: \"Use when: asked\"\nmetadata:\n  k: v: w\nlicense: \"x: y\""; got != want {
		t.Fatalf("%q", got)
	}
}

// FuzzFrontmatter: no input makes the parser or the splitter panic.
func FuzzFrontmatter(f *testing.F) {
	for _, s := range []string{"---\nname: a\ndescription: |\n  x\n---\n", "---\nmetadata:\n  a: 'b''c'\n---", "---\nallowed-tools: A(b, c) d\n---", "---\nx: \"\\u00zz\"\n---"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, doc string) {
		raw, _, err := split([]byte(doc))
		if err != nil {
			return
		}
		fm, err := parseFrontmatter(raw)
		if err == nil && strings.ContainsRune(fm.Name, '\n') && !strings.Contains(string(raw), "name:") {
			t.Fatalf("a name from nowhere: %q", fm.Name)
		}
		_, _ = parseFrontmatter(quoteColons(raw))
	})
}
