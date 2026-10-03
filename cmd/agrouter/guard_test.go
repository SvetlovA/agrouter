package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SvetlovA/agrouter/pkg/config"
	"github.com/SvetlovA/agrouter/pkg/config/defaults"
)

// TestGuard_NoCLINamesInProduction enforces the CLI-agnostic core: production Go sources outside the
// embedded defaults name no shipped CLI in identifiers, literals or comments. --help text (the
// flag descriptions and helpText) is exempt, and so are test files, testdata and vendor.
func TestGuard_NoCLINamesInProduction(t *testing.T) {
	cfg, err := config.Load(config.Sources{Embedded: defaults.Config})
	require.NoError(t, err)
	names := make([]string, 0, len(cfg.CLIs))
	for _, cli := range cfg.CLIs {
		names = append(names, regexp.QuoteMeta(cli.Name))
	}
	require.NotEmpty(t, names)
	re := regexp.MustCompile(`(?i)` + strings.Join(names, "|"))

	root := filepath.Join("..", "..")
	skipDirs := map[string]bool{
		filepath.Join(root, "vendor"):                    true,
		filepath.Join(root, ".git"):                      true,
		filepath.Join(root, "pkg", "config", "defaults"): true,
	}
	checked := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[path] || d.Name() == "testdata" || d.Name() == "mocks" ||
				(strings.HasPrefix(d.Name(), ".") && path != root) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		checked++
		for _, hit := range cliNames(t, path, re) {
			t.Errorf("%s: names a CLI: %s", filepath.ToSlash(path), hit)
		}
		return nil
	})
	require.NoError(t, err)
	assert.Positive(t, checked)
}

// cliNames returns every identifier, literal and comment in the file at path that matches re,
// except struct tags and the helpText constant.
func cliNames(t *testing.T, path string, re *regexp.Regexp) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	require.NoError(t, err)

	var hits []string
	add := func(pos token.Pos, s string) {
		if re.MatchString(s) {
			hits = append(hits, fset.Position(pos).String()+": "+s)
		}
	}
	for _, cg := range f.Comments {
		add(cg.Pos(), cg.Text())
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Field:
			// struct tags are go-flags' --help descriptions; the rest of the field is still checked
			for _, name := range n.Names {
				add(name.Pos(), name.Name)
			}
			ast.Inspect(n.Type, func(m ast.Node) bool {
				if id, ok := m.(*ast.Ident); ok {
					add(id.Pos(), id.Name)
				}
				return true
			})
			return false
		case *ast.ValueSpec:
			if len(n.Names) == 1 && n.Names[0].Name == "helpText" {
				return false
			}
		case *ast.Ident:
			add(n.Pos(), n.Name)
		case *ast.BasicLit:
			add(n.Pos(), n.Value)
		}
		return true
	})
	return hits
}

func TestGuard_DetectsCLINames(t *testing.T) {
	dir := t.TempDir()
	src := `package x

// runs acme
const helpText = "acme help is exempt"

type opts struct {
	Mode string ` + "`description:\"acme mode, exempt\"`" + `
}

var acmeCommand = "acme"
`
	path := filepath.Join(dir, "x.go")
	require.NoError(t, os.WriteFile(path, []byte(src), 0o600))
	hits := cliNames(t, path, regexp.MustCompile(`(?i)acme`))
	require.Len(t, hits, 3, hits)
	assert.Contains(t, hits[0], "runs acme")
	assert.Contains(t, hits[1], "acmeCommand")
	assert.Contains(t, hits[2], `"acme"`)
}
