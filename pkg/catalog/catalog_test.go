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
)

const catalogConfig = `
[agrouter]
timeout = 10s
routing_policy = Prefer cheap.
complexity_policy = Judge the codebase.

[cli.alpha]
command = alpha
[cli.alpha.args]
print = []
prompt = ["--", "{prompt}"]
model = ["--model", "{model}"]
effort = ["--effort", "{effort}"]

[cli.beta]
command = beta
[cli.beta.args]
print = []
prompt = ["--", "{prompt}"]
model = ["--model", "{model}"]
effort = ["--effort", "{effort}"]

[model.alpha-big]
cli = alpha
name = alpha-big
aliases = big
efforts = low, high

[model.alpha-small]
cli = alpha
name = alpha-small

[model.beta-one]
cli = beta
name = beta-one
efforts = low, ultra
`

// loadConfig merges a synthetic fixture with an optional local config.
func loadConfig(t *testing.T, local string) *config.Config {
	t.Helper()
	src := config.Sources{Embedded: []byte(catalogConfig)}
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

func TestModelWithoutEfforts(t *testing.T) {
	c, err := catalog.Build(loadConfig(t, ""))
	require.NoError(t, err)

	withoutEfforts := catalog.ByModel(c.Options, "alpha-small")
	require.Len(t, withoutEfforts, 1)
	assert.Equal(t, catalog.Option{ID: "alpha-small", CLI: "alpha", Section: "alpha-small",
		Name: "alpha-small"}, withoutEfforts[0])
	assert.Empty(t, catalog.ByEffort(withoutEfforts, "high"), "a model without efforts has no effort options")
}

func TestStableOrdering(t *testing.T) {
	first, err := catalog.Build(loadConfig(t, ""))
	require.NoError(t, err)
	assert.Equal(t, []string{"alpha-big@low", "alpha-big@high", "alpha-small", "beta-one@low", "beta-one@ultra"}, ids(first.Options))
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
		{name: "one model", local: "[model.beta-one]\nenabled = false\n", want: 3, clis: []string{"alpha"}},
		{name: "a whole cli", local: "[cli.beta]\nenabled = false\n", want: 3, clis: []string{"alpha"}},
		{name: "model without efforts", local: "[model.alpha-small]\nenabled = false\n", want: 4,
			clis: []string{"alpha", "beta"}},
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
	_, err := catalog.Build(loadConfig(t, "[cli.alpha]\nenabled = false\n[cli.beta]\nenabled = false\n"))
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
		{value: "alpha-big", wantName: "alpha-big", inCatalog: true},
		{value: "big", wantName: "alpha-big", inCatalog: true},
		{value: "beta-one", wantName: "beta-one", inCatalog: true},

		{value: "alpha-small", wantName: "alpha-small", inCatalog: true},
		{value: "unknown", wantName: "unknown", inCatalog: false},
		{value: "BIG", wantName: "BIG", inCatalog: false},
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
		})
	}
}

func TestLookupSkipsDisabledModel(t *testing.T) {
	c, err := catalog.Build(loadConfig(t, "[model.alpha-big]\nenabled = false\n"))
	require.NoError(t, err)
	_, ok := c.LookupModel("big")
	assert.False(t, ok, "a disabled model's alias is not in the catalog")
}

func TestFilters(t *testing.T) {
	c, err := catalog.Build(loadConfig(t, ""))
	require.NoError(t, err)

	assert.Equal(t, []string{"beta-one@ultra"}, ids(catalog.ByEffort(c.Options, "ultra")))
	assert.Len(t, catalog.ByEffort(c.Options, "high"), 1)
	assert.Len(t, catalog.ByEffort(catalog.ByCLI(c.Options, "alpha"), "high"), 1)
	assert.Equal(t, []string{"beta-one@low", "beta-one@ultra"},
		ids(catalog.ByModel(c.Options, "beta-one")))

	assert.Empty(t, catalog.ByCLI(c.Options, "gemini"))
	assert.Empty(t, catalog.ByModel(c.Options, "nope"))
	assert.Empty(t, catalog.ByEffort(c.Options, "turbo"))
	assert.Empty(t, catalog.CLIs(nil))
}
