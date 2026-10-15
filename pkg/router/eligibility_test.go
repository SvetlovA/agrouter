package router

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SvetlovA/agrouter/pkg/args"
	"github.com/SvetlovA/agrouter/pkg/catalog"
	"github.com/SvetlovA/agrouter/pkg/config"
	"github.com/SvetlovA/agrouter/pkg/config/defaults"
)

func embedded(t *testing.T) (*config.Config, *catalog.Catalog) {
	t.Helper()
	cfg, err := config.Load(config.Sources{Embedded: defaults.Config})
	require.NoError(t, err)
	cat, err := catalog.Build(cfg)
	require.NoError(t, err)
	return cfg, cat
}

func ids(opts []catalog.Option) []string {
	out := make([]string, 0, len(opts))
	for _, o := range opts {
		out = append(out, o.ID)
	}
	return out
}

func idsOf(cat *catalog.Catalog, keep func(catalog.Option) bool) []string {
	var out []string
	for _, o := range cat.Options {
		if keep(o) {
			out = append(out, o.ID)
		}
	}
	return out
}

func TestEligibleIgnoresNativeArguments(t *testing.T) {
	_, cat := embedded(t)
	for _, raw := range [][]string{nil, {"--output-format", "json"}, {"--sandbox", "read-only"}, {"--", "--unknown"}} {
		e := Eligible(cat, &args.Request{Mode: args.ModeExec, Raw: raw})
		assert.Equal(t, ids(cat.Options), ids(e.Options), "%v: native tokens never filter or prefer CLIs", raw)
		assert.Empty(t, e.Dropped)
		assert.Empty(t, e.Warnings)
	}
}

func TestEligibleDropReasons(t *testing.T) {
	_, cat := embedded(t)

	e := Eligible(cat, &args.Request{CLI: "codex"})
	assert.Equal(t, []Drop{{CLI: "claude", Reason: "--cli codex"}}, e.Dropped)

	e = Eligible(cat, &args.Request{Model: "opus"})
	assert.Equal(t, []Drop{{CLI: "codex", Reason: "--model opus"}}, e.Dropped)

	e = Eligible(cat, &args.Request{Effort: "ultra"})
	assert.Equal(t, []Drop{{CLI: "claude", Reason: "--effort ultra"}}, e.Dropped)
}

func TestEligibleCLI(t *testing.T) {
	_, cat := embedded(t)

	e := Eligible(cat, &args.Request{CLI: "claude"})
	assert.True(t, e.Pinned)
	assert.Equal(t, []string{"claude"}, e.CLIs())

	for _, name := range []string{"gemini", "Claude"} {
		e = Eligible(cat, &args.Request{CLI: name})
		assert.False(t, e.Pinned, name)
		assert.Equal(t, ids(cat.Options), ids(e.Options), name)
		assert.Equal(t, []string{"agrouter: warning: skipped --cli " + name +
			": not an enabled CLI; routing across every CLI"}, e.Warnings)
	}
}

func TestEligibleDisabledCLI(t *testing.T) {
	local := []byte("[cli.codex]\nenabled = false\n")
	path := filepath.Join(t.TempDir(), "config")
	require.NoError(t, os.WriteFile(path, local, 0o600))
	cfg, err := config.Load(config.Sources{Embedded: defaults.Config, LocalPath: path})
	require.NoError(t, err)
	cat, err := catalog.Build(cfg)
	require.NoError(t, err)

	e := Eligible(cat, &args.Request{CLI: "codex"})
	assert.False(t, e.Pinned)
	assert.Equal(t, []string{"claude"}, e.CLIs(), "the only CLI left")
	assert.Equal(t, []string{"agrouter: warning: skipped --cli codex: not an enabled CLI; routing across every CLI"}, e.Warnings)
}

func TestEligibleModel(t *testing.T) {
	_, cat := embedded(t)
	opus := idsOf(cat, func(o catalog.Option) bool { return o.Section == "claude-opus-5-5" })

	for _, value := range []string{"claude-opus-5-5", "opus"} {
		t.Run("catalog "+value, func(t *testing.T) {
			e := Eligible(cat, &args.Request{Model: value})
			assert.Equal(t, opus, ids(e.Options))
			assert.False(t, e.ModelPassthrough)
			for _, o := range e.Options {
				assert.Equal(t, "claude-opus-5-5", o.Name, "an alias resolves to the model's name")
			}
		})
	}

	t.Run("outside the catalog, no --cli: one option per CLI", func(t *testing.T) {
		e := Eligible(cat, &args.Request{Model: "gpt-9"})
		assert.True(t, e.ModelPassthrough)
		assert.Equal(t, []catalog.Option{
			{ID: "claude", CLI: "claude", Name: "gpt-9"},
			{ID: "codex", CLI: "codex", Name: "gpt-9"},
		}, e.Options)
	})

	t.Run("outside the catalog with --cli", func(t *testing.T) {
		e := Eligible(cat, &args.Request{CLI: "codex", Model: "gpt-9"})
		assert.Equal(t, []catalog.Option{{ID: "codex", CLI: "codex", Name: "gpt-9"}}, e.Options)
	})

	t.Run("a catalog model of a CLI --cli dropped is passed through by name", func(t *testing.T) {
		e := Eligible(cat, &args.Request{CLI: "claude", Model: "gpt-6.1-sol"})
		assert.True(t, e.ModelPassthrough)
		assert.Equal(t, []catalog.Option{{ID: "claude", CLI: "claude", Name: "gpt-6.1-sol"}}, e.Options)
	})
}

func TestEligibleEffort(t *testing.T) {
	cfg, cat := embedded(t)

	t.Run("alone", func(t *testing.T) {
		e := Eligible(cat, &args.Request{Effort: "high"})
		assert.Equal(t, idsOf(cat, func(o catalog.Option) bool { return o.Effort == "high" }), ids(e.Options))
		assert.False(t, e.EffortPassthrough)
		assert.Empty(t, e.Dropped)
	})

	t.Run("only one CLI has it", func(t *testing.T) {
		e := Eligible(cat, &args.Request{Effort: "ultra"})
		assert.Equal(t, idsOf(cat, func(o catalog.Option) bool { return o.Effort == "ultra" }), ids(e.Options))
	})

	t.Run("nothing left has it: models kept without the effort", func(t *testing.T) {
		e := Eligible(cat, &args.Request{CLI: "claude", Effort: "ultra"})
		assert.True(t, e.EffortPassthrough)
		var models []string
		for _, m := range cfg.Models {
			if m.CLI == "claude" {
				models = append(models, m.Section)
			}
		}
		assert.Equal(t, models, ids(e.Options))
		for _, o := range e.Options {
			assert.Empty(t, o.Effort)
			assert.Equal(t, "ultra", e.EffortFor(o))
		}
	})

	t.Run("a model without efforts passes it through", func(t *testing.T) {
		_, fixCat := requestFixture(t)
		e := Eligible(fixCat, &args.Request{Model: "fast-model", Effort: "high"})
		assert.True(t, e.EffortPassthrough)
		require.Len(t, e.Options, 1)
		assert.Equal(t, "fast", e.Options[0].ID)
		assert.Equal(t, "high", e.EffortFor(e.Options[0]))
	})

	t.Run("model and effort leave one option", func(t *testing.T) {
		e := Eligible(cat, &args.Request{Model: "opus", Effort: "high"})
		assert.Equal(t, []string{"claude-opus-5-5@high"}, ids(e.Options))
		assert.Equal(t, "high", e.EffortFor(e.Options[0]))
	})

	t.Run("model outside the catalog passes it through", func(t *testing.T) {
		e := Eligible(cat, &args.Request{CLI: "codex", Model: "gpt-9", Effort: "high"})
		assert.True(t, e.ModelPassthrough)
		assert.True(t, e.EffortPassthrough)
		require.Len(t, e.Options, 1)
		assert.Equal(t, "high", e.EffortFor(e.Options[0]))
	})

	t.Run("no effort given, model without efforts emits none", func(t *testing.T) {
		_, fixCat := requestFixture(t)
		e := Eligible(fixCat, &args.Request{Model: "fast-model"})
		require.Len(t, e.Options, 1)
		assert.Empty(t, e.EffortFor(e.Options[0]))
	})
}

func TestEligibleNeverEmpty(t *testing.T) {
	_, cat := embedded(t)
	for _, cli := range []string{"", "claude", "codex", "gemini"} {
		for _, model := range []string{"", "opus", "gpt-6-luna", "gpt-9"} {
			for _, effort := range []string{"", "high", "ultra", "bogus"} {
				e := Eligible(cat, &args.Request{CLI: cli, Model: model, Effort: effort})
				assert.NotEmpty(t, e.Options, "cli=%q model=%q effort=%q", cli, model, effort)
			}
		}
	}
}

func TestEligibilityExclude(t *testing.T) {
	_, cat := requestFixture(t)
	cases := []struct {
		name    string
		req     *args.Request
		exclude []string
		want    []string
		dropped []Drop
	}{
		{name: "nothing to exclude", exclude: nil,
			want: []string{"strong@low", "strong@high", "fast", "worker@low", "worker@high"}},
		{name: "one model of a cli", exclude: []string{"strong@low", "strong@high"},
			want: []string{"fast", "worker@low", "worker@high"}},
		{name: "every option of a cli is a drop", exclude: []string{"worker@low", "worker@high"},
			want: []string{"strong@low", "strong@high", "fast"}, dropped: []Drop{{CLI: "beta", Reason: "unavailable"}}},
		{name: "unknown ids are ignored", exclude: []string{"missing"},
			want: []string{"strong@low", "strong@high", "fast", "worker@low", "worker@high"}},
		{name: "a fixed cli is never broadened", req: &args.Request{CLI: "alpha"}, exclude: []string{"strong@low", "strong@high"},
			want: []string{"fast"}, dropped: []Drop{{CLI: "beta", Reason: "--cli alpha"}}},
		{name: "a passed-through model", req: &args.Request{CLI: "beta", Model: "other-model"}, exclude: []string{"beta"},
			want: []string{}, dropped: []Drop{{CLI: "alpha", Reason: "--cli beta"}, {CLI: "beta", Reason: "unavailable"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.req
			if req == nil {
				req = &args.Request{}
			}
			e := Eligible(cat, req)
			e.Exclude(tc.exclude)
			assert.Equal(t, tc.want, append([]string{}, ids(e.Options)...))
			assert.Equal(t, tc.dropped, e.Dropped)
		})
	}
}

func TestEligibilityExcludeKeepsOriginal(t *testing.T) {
	_, cat := requestFixture(t)
	e := Eligible(cat, &args.Request{})
	before := e.Options
	e.Exclude([]string{"fast"})
	assert.Len(t, before, 5, "the slice handed out earlier is not rewritten")
	assert.Len(t, e.Options, 4)
}
