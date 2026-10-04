package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SvetlovA/agrouter/pkg/config"
	"github.com/SvetlovA/agrouter/pkg/config/defaults"
)

// TestGuard_NoCatalogNamesInProduction keeps catalog names in config. CLI names are checked in
// identifiers, literals, and comments; model names, aliases, and effort levels in string literals.
// Test files, testdata, mocks, vendor, and embedded defaults are exempt.
func TestGuard_NoCatalogNamesInProduction(t *testing.T) {
	cfg, err := config.Load(config.Sources{Embedded: defaults.Config})
	require.NoError(t, err)
	names := make([]string, 0, len(cfg.CLIs))
	for _, cli := range cfg.CLIs {
		names = append(names, regexp.QuoteMeta(cli.Name))
	}
	require.NotEmpty(t, names)
	re := regexp.MustCompile(`(?i)` + strings.Join(names, "|"))
	values := map[string]bool{}
	for _, model := range cfg.Models {
		values[model.Section], values[model.Name] = true, true
		for _, alias := range model.Aliases {
			values[alias] = true
		}
		for _, effort := range model.Efforts {
			values[effort] = true
		}
	}
	for _, effort := range cfg.Efforts {
		values[effort.Level] = true
	}

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
		for _, hit := range catalogNames(t, path, re, values) {
			t.Errorf("%s: hardcodes a catalog name: %s", filepath.ToSlash(path), hit)
		}
		return nil
	})
	require.NoError(t, err)
	assert.Positive(t, checked)
}

// catalogNames checks CLI names anywhere in source and catalog values in string literals.
func catalogNames(t *testing.T, path string, re *regexp.Regexp, values map[string]bool) []string {
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
			if n.Tag != nil {
				add(n.Tag.Pos(), n.Tag.Value)
			}
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
		case *ast.Ident:
			add(n.Pos(), n.Name)
		case *ast.BasicLit:
			add(n.Pos(), n.Value)
			if n.Kind == token.STRING && !re.MatchString(n.Value) {
				value, err := strconv.Unquote(n.Value)
				if err == nil && values[value] {
					hits = append(hits, fset.Position(n.Pos()).String()+": "+n.Value)
				}
			}
		}
		return true
	})
	return hits
}

func TestGuard_DetectsCLINames(t *testing.T) {
	dir := t.TempDir()
	src := `package x

// runs acme
const helpText = "acme help"

type opts struct {
	Mode string ` + "`description:\"acme mode\"`" + `
}

var acmeCommand = "acme"
`
	path := filepath.Join(dir, "x.go")
	require.NoError(t, os.WriteFile(path, []byte(src), 0o600))
	hits := catalogNames(t, path, regexp.MustCompile(`(?i)acme`), nil)
	require.Len(t, hits, 5, hits)
	assert.Contains(t, hits[0], "runs acme")
	assert.Contains(t, hits[1], "acme help")
	assert.Contains(t, hits[2], "acme mode")
	assert.Contains(t, hits[3], "acmeCommand")
	assert.Contains(t, hits[4], `"acme"`)
}

func TestGuard_DetectsModelAndEffortNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.go")
	require.NoError(t, os.WriteFile(path, []byte(`package x
var model = "test-model"
var alias = "test-alias"
var effort = "test-effort"
var label = "effort"
`), 0o600))
	hits := catalogNames(t, path, regexp.MustCompile(`(?i)acme`), map[string]bool{
		"test-model": true, "test-alias": true, "test-effort": true,
	})
	require.Len(t, hits, 3, hits)
	assert.Contains(t, hits[0], `"test-model"`)
	assert.Contains(t, hits[1], `"test-alias"`)
	assert.Contains(t, hits[2], `"test-effort"`)
}
