package catalog_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SvetlovA/agrouter/pkg/catalog"
	"github.com/SvetlovA/agrouter/pkg/config"
	"github.com/SvetlovA/agrouter/pkg/config/defaults"
)

// loadConfig merges the embedded defaults with an optional local config.
func loadConfig(t *testing.T, local string) *config.Config {
	t.Helper()
	src := config.Sources{Embedded: defaults.Config}
	if local != "" {
		src.LocalPath = filepath.Join(t.TempDir(), "config")
		require.NoError(t, os.WriteFile(src.LocalPath, []byte(local), 0o600))
	}
	cfg, err := config.Load(src)
	require.NoError(t, err)
	return cfg
}

func ids(opts []catalog.Option) []string {
	out := make([]string, 0, len(opts))
	for _, o := range opts {
		out = append(out, o.ID)
	}
	return out
}

func TestEmbeddedCatalog(t *testing.T) {
	c, err := catalog.Build(loadConfig(t, ""))
	require.NoError(t, err)

	require.Len(t, c.Options, 33)
	assert.Len(t, catalog.ByCLI(c.Options, "claude"), 16)
	assert.Len(t, catalog.ByCLI(c.Options, "codex"), 17)
	assert.Equal(t, []string{"claude", "codex"}, catalog.CLIs(c.Options))

	assert.Equal(t, []string{
		"claude-fable-5-1@low", "claude-fable-5-1@medium", "claude-fable-5-1@high", "claude-fable-5-1@xhigh", "claude-fable-5-1@max",
		"claude-opus-5-5@low", "claude-opus-5-5@medium", "claude-opus-5-5@high", "claude-opus-5-5@xhigh", "claude-opus-5-5@max",
		"claude-sonnet-5@low", "claude-sonnet-5@medium", "claude-sonnet-5@high", "claude-sonnet-5@xhigh", "claude-sonnet-5@max",
		"claude-haiku-4-5",
		"gpt-6-astra@low", "gpt-6-astra@medium", "gpt-6-astra@high", "gpt-6-astra@xhigh", "gpt-6-astra@max", "gpt-6-astra@ultra",
		"gpt-6-sol@low", "gpt-6-sol@medium", "gpt-6-sol@high", "gpt-6-sol@xhigh", "gpt-6-sol@max", "gpt-6-sol@ultra",
		"gpt-6-luna@low", "gpt-6-luna@medium", "gpt-6-luna@high", "gpt-6-luna@xhigh", "gpt-6-luna@max",
	}, ids(c.Options), "catalog order")

	assert.Equal(t, catalog.Option{ID: "claude-opus-5-5@high", CLI: "claude", Section: "claude-opus-5-5",
		Name: "claude-opus-5-5", Effort: "high"}, c.Options[7])
	assert.Len(t, c.Models(), 7)
}

func TestModelWithoutEfforts(t *testing.T) {
	c, err := catalog.Build(loadConfig(t, ""))
	require.NoError(t, err)

	haiku := catalog.ByModel(c.Options, "claude-haiku-4-5")
	require.Len(t, haiku, 1)
	assert.Equal(t, catalog.Option{ID: "claude-haiku-4-5", CLI: "claude", Section: "claude-haiku-4-5",
		Name: "claude-haiku-4-5"}, haiku[0])
	assert.Empty(t, catalog.ByEffort(haiku, "high"), "a model without efforts has no effort options")
}

func TestStableOrdering(t *testing.T) {
	first, err := catalog.Build(loadConfig(t, ""))
	require.NoError(t, err)
	for range 20 {
		again, err := catalog.Build(loadConfig(t, ""))
		require.NoError(t, err)
		assert.Equal(t, first.Options, again.Options)
	}
}

func TestDisabledSections(t *testing.T) {
	tests := []struct {
		name  string
		local string
		want  int
		clis  []string
	}{
		{name: "one model", local: "[model.gpt-6-astra]\nenabled = false\n", want: 27, clis: []string{"claude", "codex"}},
		{name: "a whole cli", local: "[cli.codex]\nenabled = false\n", want: 16, clis: []string{"claude"}},
		{name: "model without efforts", local: "[model.claude-haiku-4-5]\nenabled = false\n", want: 32,
			clis: []string{"claude", "codex"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := catalog.Build(loadConfig(t, tt.local))
			require.NoError(t, err)
			assert.Len(t, c.Options, tt.want)
			assert.Equal(t, tt.clis, catalog.CLIs(c.Options))
		})
	}
}

func TestNoEnabledOptions(t *testing.T) {
	_, err := catalog.Build(loadConfig(t, "[cli.claude]\nenabled = false\n[cli.codex]\nenabled = false\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no enabled options")

	_, err = catalog.Build(&config.Config{})
	require.Error(t, err)
}

func TestOptionLimit(t *testing.T) {
	build := func(n int) (*catalog.Catalog, error) {
		cfg := &config.Config{}
		for i := range n {
			cfg.Models = append(cfg.Models, config.Model{Section: fmt.Sprintf("m%d", i), CLI: "x", Name: fmt.Sprintf("m%d", i)})
		}
		return catalog.Build(cfg)
	}

	c, err := build(catalog.MaxOptions)
	require.NoError(t, err)
	assert.Len(t, c.Options, 255)

	_, err = build(catalog.MaxOptions + 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "256 options, more than Jev's limit of 255")

	// the limit counts options, not models: 52 models × 5 efforts = 260
	cfg := &config.Config{}
	for i := range 52 {
		cfg.Models = append(cfg.Models, config.Model{Section: fmt.Sprintf("m%d", i), CLI: "x", Name: fmt.Sprintf("m%d", i),
			Efforts: []string{"a", "b", "c", "d", "e"}})
	}
	_, err = catalog.Build(cfg)
	require.ErrorContains(t, err, "260 options")
}

func TestLookupModel(t *testing.T) {
	c, err := catalog.Build(loadConfig(t, ""))
	require.NoError(t, err)

	tests := []struct {
		value     string
		wantName  string
		inCatalog bool
	}{
		{value: "claude-opus-5-5", wantName: "claude-opus-5-5", inCatalog: true},
		{value: "opus", wantName: "claude-opus-5-5", inCatalog: true},
		{value: "gpt-6-luna", wantName: "gpt-6-luna", inCatalog: true},
		{value: "claude-haiku-4-5", wantName: "claude-haiku-4-5", inCatalog: true},
		{value: "gpt-5.6-sol", wantName: "gpt-5.6-sol", inCatalog: false},
		{value: "OPUS", wantName: "OPUS", inCatalog: false},
		{value: "", wantName: "", inCatalog: false},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			m, ok := c.LookupModel(tt.value)
			assert.Equal(t, tt.inCatalog, ok)
			if ok {
				assert.Equal(t, tt.wantName, m.Name)
			} else {
				assert.Equal(t, config.Model{}, m)
			}
			name, inCatalog := c.ResolveModel(tt.value)
			assert.Equal(t, tt.wantName, name)
			assert.Equal(t, tt.inCatalog, inCatalog)
		})
	}
}

func TestLookupSkipsDisabledModel(t *testing.T) {
	c, err := catalog.Build(loadConfig(t, "[model.claude-opus-5-5]\nenabled = false\n"))
	require.NoError(t, err)
	_, ok := c.LookupModel("opus")
	assert.False(t, ok, "a disabled model's alias is not in the catalog")
	name, inCatalog := c.ResolveModel("opus")
	assert.Equal(t, "opus", name)
	assert.False(t, inCatalog)
}

func TestFilters(t *testing.T) {
	c, err := catalog.Build(loadConfig(t, ""))
	require.NoError(t, err)

	assert.Equal(t, []string{"gpt-6-astra@ultra", "gpt-6-sol@ultra"}, ids(catalog.ByEffort(c.Options, "ultra")))
	assert.Len(t, catalog.ByEffort(c.Options, "high"), 6)
	assert.Len(t, catalog.ByEffort(catalog.ByCLI(c.Options, "claude"), "high"), 3)
	assert.Equal(t, []string{"gpt-6-luna@low", "gpt-6-luna@medium", "gpt-6-luna@high", "gpt-6-luna@xhigh", "gpt-6-luna@max"},
		ids(catalog.ByModel(c.Options, "gpt-6-luna")))

	assert.Empty(t, catalog.ByCLI(c.Options, "gemini"))
	assert.Empty(t, catalog.ByModel(c.Options, "nope"))
	assert.Empty(t, catalog.ByEffort(c.Options, "turbo"))
	assert.Empty(t, catalog.CLIs(nil))

	assert.True(t, c.HasEffort("ultra"))
	assert.True(t, c.HasEffort("low"))
	assert.False(t, c.HasEffort("turbo"))
	assert.False(t, c.HasEffort(""), "the bare haiku option has no effort to match")
}
