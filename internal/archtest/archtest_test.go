package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

var root = func() string {
	abs, err := filepath.Abs("../..")
	if err != nil {
		panic(err)
	}
	return abs
}()

// pkgFiles maps each package directory (module-relative) to its parsed non-test files, skipping nested modules and testdata.
func pkgFiles(t *testing.T, mode parser.Mode) map[string][]*ast.File {
	t.Helper()
	out := map[string][]*ast.File{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			rel = ""
		}
		if d.IsDir() {
			name := d.Name()
			if rel != "" && (strings.HasPrefix(name, ".") || name == "testdata" || name == "docs") {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(path, "go.mod")); rel != "" && err == nil {
				return filepath.SkipDir // tools/schemacheck is its own module
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, mode)
		if err != nil {
			return err
		}
		dir := filepath.ToSlash(filepath.Dir(rel))
		if dir == "." {
			dir = ""
		}
		out[dir] = append(out[dir], f)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestImportDirections(t *testing.T) {
	pkgs := pkgFiles(t, parser.ImportsOnly)
	if len(pkgs) == 0 {
		t.Fatal("no packages found; is the module root right?")
	}
	for dir, files := range pkgs {
		for _, f := range files {
			for _, imp := range f.Imports {
				path, _ := strconv.Unquote(imp.Path.Value)
				if err := checkImport(dir, path); err != nil {
					t.Error(err)
				}
			}
		}
	}
}

func TestCoreModuleRequiresOnlyTheMCPSDK(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	block := false
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "require (":
			block = true
			continue
		case block && line == ")":
			block = false
			continue
		case strings.HasPrefix(line, "require "):
			line = strings.TrimPrefix(line, "require ")
		case !block:
			continue
		}
		if line == "" || strings.HasSuffix(line, "// indirect") {
			continue
		}
		if mod := strings.Fields(line)[0]; !slices.Contains(mcpDeps, mod) {
			t.Errorf("the core go.mod requires %s; only the MCP SDK's modules are exempt from the standard-library-only rule", mod)
		}
	}
}

// contentWords are names of built-in extensions that must never leak into the kernel's exported API or its prompts.
var contentWords = regexp.MustCompile(`(?i)subagent|skill|peer|memory|btw|mcp`)

func TestKernelExportsNameNoExtension(t *testing.T) {
	files := pkgFiles(t, parser.SkipObjectResolution)["kernel"]
	if len(files) == 0 {
		t.Fatal("no kernel files found")
	}
	for _, f := range files {
		for _, name := range exportedNames(f) {
			if contentWords.MatchString(name) {
				t.Errorf("kernel exports %s, which names a built-in extension", name)
			}
		}
	}
}

func exportedNames(f *ast.File) []string {
	var out []string
	add := func(id *ast.Ident) {
		if id != nil && id.IsExported() {
			out = append(out, id.Name)
		}
	}
	fields := func(fl *ast.FieldList) {
		if fl == nil {
			return
		}
		for _, fld := range fl.List {
			for _, n := range fld.Names {
				add(n)
			}
		}
	}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			add(d.Name)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					add(s.Name)
					switch tt := s.Type.(type) {
					case *ast.StructType:
						fields(tt.Fields)
					case *ast.InterfaceType:
						fields(tt.Methods)
					}
				case *ast.ValueSpec:
					for _, n := range s.Names {
						add(n)
					}
				}
			}
		}
	}
	return out
}

// brandWords must never reach the model, so prompts and tool definitions read the same whatever the product is called.
var brandWords = regexp.MustCompile(`(?i)potatos|aperture`)

// notModelFacing lists spec/ext files written for implementers rather than sent to the model.
var notModelFacing = map[string]bool{"conformance.md": true, "a2a.md": true, "README.md": true}

func TestModelFacingText(t *testing.T) {
	scan := func(dir string, facing func(name string) bool, words ...*regexp.Regexp) {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if os.IsNotExist(err) {
				return filepath.SkipDir
			}
			if err != nil || d.IsDir() || !facing(d.Name()) {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, w := range words {
				if m := w.Find(b); m != nil {
					rel, _ := filepath.Rel(root, path)
					t.Errorf("%s contains %q", rel, m)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	scan("spec/prompts", func(string) bool { return true }, brandWords, contentWords)
	scan("spec/ext", func(name string) bool {
		return !notModelFacing[name] && !strings.HasSuffix(name, ".schema.json")
	}, brandWords)
}

// serverFacing lists the Go strings allowed to name the product because only a server sees them, never the model.
var serverFacing = map[string]bool{`"potatos-mcp"`: true}

// hostOnly matches the module path and environment variable names, which name the product but only the host ever reads.
var hostOnly = regexp.MustCompile(`github\.com/estonshi/aperture-potatos|^"[A-Z0-9_]+"$`)

// TestGoStrings keeps brand words out of Go string literals, since an error's text can reach the model as a tool result.
func TestGoStrings(t *testing.T) {
	for dir, files := range pkgFiles(t, 0) {
		for _, f := range files {
			ast.Inspect(f, func(n ast.Node) bool {
				if _, ok := n.(*ast.ImportSpec); ok {
					return false
				}
				if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING && !serverFacing[lit.Value] && brandWords.MatchString(hostOnly.ReplaceAllString(lit.Value, "")) {
					t.Errorf("%s: string %s names the product", dir, lit.Value)
				}
				return true
			})
		}
	}
}
