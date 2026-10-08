package router

import (
	"os"
	"path/filepath"
	"strings"
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

// flag builds a mapped argument the way cmd/agrouter records it.
func flag(name, value string) args.Arg {
	if value == "" {
		return args.Arg{Spelling: "--" + name, Key: name}
	}
	return args.Arg{Spelling: "--" + name + " " + value, Key: name + "." + value}
}

func cfgArg(kv string) args.Arg {
	key, _, _ := strings.Cut(kv, "=")
	return args.Arg{Spelling: "-c " + kv, Key: config.ConfigKeyPrefix + key, Value: kv}
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

// ralphexClaude is ralphex's Claude-mode argv (claude_command = agrouter exec --cli=claude).
func ralphexClaude() *args.Request {
	return &args.Request{Mode: args.ModeExec, CLI: "claude", Model: "opus", ModelSource: args.SourceFlag,
		Effort: "high", EffortSource: args.SourceFlag, Args: []args.Arg{
			{Spelling: "--dangerously-skip-permissions", Key: "permission-mode.bypassPermissions"},
			flag("output-format", "stream-json"), flag("verbose", ""),
		}}
}

// ralphexCodex is ralphex's --codex argv (codex_command = agrouter), no --cli.
func ralphexCodex() *args.Request {
	return &args.Request{Mode: args.ModeExec, Args: []args.Arg{
		cfgArg("features.multi_agent=true"),
		cfgArg(`agents.reviewer.description="Reviews code"`),
		{Spelling: "--dangerously-bypass-approvals-and-sandbox", Key: "permission-mode.bypassPermissions"},
		flag("sandbox", "danger-full-access"),
		cfgArg("stream_idle_timeout_ms=3600000"),
		cfgArg(`project_doc_fallback_filenames=["CLAUDE.md"]`),
	}}
}

func TestEligibleMappedArgumentPreference(t *testing.T) {
	cfg, cat := embedded(t)
	claude := idsOf(cat, func(o catalog.Option) bool { return o.CLI == "claude" })
	codex := idsOf(cat, func(o catalog.Option) bool { return o.CLI == "codex" })

	tests := []struct {
		name    string
		req     *args.Request
		want    []string
		dropped []string
		pinned  bool
	}{
		{name: "no arguments", req: &args.Request{}, want: ids(cat.Options)},
		{name: "output-format json", req: &args.Request{Args: []args.Arg{flag("output-format", "json")}},
			want: claude, dropped: []string{"codex"}},
		{name: "verbose, [] counts as skipped", req: &args.Request{Args: []args.Arg{flag("verbose", "")}},
			want: claude, dropped: []string{"codex"}},
		{name: "permission-mode manual", req: &args.Request{Args: []args.Arg{flag("permission-mode", "manual")}},
			want: claude, dropped: []string{"codex"}},
		{name: "permission-mode dontAsk", req: &args.Request{Args: []args.Arg{flag("permission-mode", "dontAsk")}},
			want: claude, dropped: []string{"codex"}},
		{name: "permission-mode manual pinned to codex",
			req:  &args.Request{CLI: "codex", Args: []args.Arg{flag("permission-mode", "manual")}},
			want: codex, dropped: []string{"claude"}, pinned: true},
		{name: "sandbox", req: &args.Request{Args: []args.Arg{flag("sandbox", "read-only")}},
			want: codex, dropped: []string{"claude"}},
		{name: "-c key", req: &args.Request{Args: []args.Arg{cfgArg("features.multi_agent=true")}},
			want: codex, dropped: []string{"claude"}},
		{name: "output-format json pinned to codex",
			req:  &args.Request{CLI: "codex", Args: []args.Arg{flag("output-format", "json")}},
			want: codex, dropped: []string{"claude"}, pinned: true},
		{name: "tie: stream-json", req: &args.Request{Args: []args.Arg{flag("output-format", "stream-json")}},
			want: ids(cat.Options)},
		{name: "tie: plan and bypass",
			req: &args.Request{Args: []args.Arg{flag("permission-mode", "plan"),
				{Spelling: "--dangerously-skip-permissions", Key: "permission-mode.bypassPermissions"}}},
			want: ids(cat.Options)},
		{name: "tie at one skip each: json and sandbox",
			req:  &args.Request{Args: []args.Arg{flag("output-format", "json"), flag("sandbox", "read-only")}},
			want: ids(cat.Options)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := Eligible(cfg, cat, tc.req)
			assert.Equal(t, tc.want, ids(e.Options))
			assert.Equal(t, tc.pinned, e.Pinned)
			var dropped []string
			for _, d := range e.Dropped {
				dropped = append(dropped, d.CLI)
				assert.NotEmpty(t, d.Reason)
			}
			assert.Equal(t, tc.dropped, dropped)
			assert.Empty(t, e.Skipped)
		})
	}
}

func TestEligibleDropReasons(t *testing.T) {
	cfg, cat := embedded(t)

	e := Eligible(cfg, cat, &args.Request{Args: []args.Arg{flag("output-format", "json")}})
	assert.Equal(t, []Drop{{CLI: "codex", Reason: "would skip 1 mapped argument(s), another CLI skips 0"}}, e.Dropped)

	e = Eligible(cfg, cat, &args.Request{CLI: "codex"})
	assert.Equal(t, []Drop{{CLI: "claude", Reason: "--cli codex"}}, e.Dropped)

	e = Eligible(cfg, cat, &args.Request{Model: "opus"})
	assert.Equal(t, []Drop{{CLI: "codex", Reason: "--model opus"}}, e.Dropped)

	e = Eligible(cfg, cat, &args.Request{Effort: "ultra"})
	assert.Equal(t, []Drop{{CLI: "claude", Reason: "--effort ultra"}}, e.Dropped)
}

func TestEligibleCLI(t *testing.T) {
	cfg, cat := embedded(t)

	e := Eligible(cfg, cat, &args.Request{CLI: "claude"})
	assert.True(t, e.Pinned)
	assert.Equal(t, []string{"claude"}, e.CLIs())

	for _, name := range []string{"gemini", "Claude"} {
		e = Eligible(cfg, cat, &args.Request{CLI: name})
		assert.False(t, e.Pinned, name)
		assert.Equal(t, ids(cat.Options), ids(e.Options), name)
		assert.Equal(t, []args.Skip{{Spelling: "--cli " + name, Warning: "agrouter: warning: skipped --cli " + name +
			": not an enabled CLI; routing across every CLI"}}, e.Skipped)
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

	e := Eligible(cfg, cat, &args.Request{CLI: "codex", Args: []args.Arg{flag("sandbox", "read-only")}})
	assert.False(t, e.Pinned)
	assert.Equal(t, []string{"claude"}, e.CLIs(), "the only CLI left, even though it skips --sandbox")
	assert.Equal(t, []args.Skip{{Spelling: "--cli codex",
		Warning: "agrouter: warning: skipped --cli codex: not an enabled CLI; routing across every CLI"}}, e.Skipped)
}

func TestEligibleModel(t *testing.T) {
	cfg, cat := embedded(t)
	opus := idsOf(cat, func(o catalog.Option) bool { return o.Section == "claude-opus-5-5" })

	for _, value := range []string{"claude-opus-5-5", "opus"} {
		t.Run("catalog "+value, func(t *testing.T) {
			e := Eligible(cfg, cat, &args.Request{Model: value, ModelSource: args.SourceFlag})
			assert.Equal(t, opus, ids(e.Options))
			assert.False(t, e.ModelPassthrough)
			for _, o := range e.Options {
				assert.Equal(t, "claude-opus-5-5", o.Name, "an alias resolves to the model's name")
			}
		})
	}

	t.Run("outside the catalog, no --cli: one option per CLI", func(t *testing.T) {
		e := Eligible(cfg, cat, &args.Request{Model: "gpt-9"})
		assert.True(t, e.ModelPassthrough)
		assert.Equal(t, []catalog.Option{
			{ID: "claude", CLI: "claude", Name: "gpt-9"},
			{ID: "codex", CLI: "codex", Name: "gpt-9"},
		}, e.Options)
	})

	t.Run("outside the catalog with --cli", func(t *testing.T) {
		e := Eligible(cfg, cat, &args.Request{CLI: "codex", Model: "gpt-9"})
		assert.Equal(t, []catalog.Option{{ID: "codex", CLI: "codex", Name: "gpt-9"}}, e.Options)
	})

	t.Run("outside the catalog, the preference leaves one CLI (ralphex -c model=)", func(t *testing.T) {
		req := ralphexCodex()
		req.Model, req.ModelSource = "gpt-5.6-sol", args.SourceConfig
		e := Eligible(cfg, cat, req)
		assert.Equal(t, []catalog.Option{{ID: "codex", CLI: "codex", Name: "gpt-5.6-sol"}}, e.Options)
	})

	t.Run("a catalog model of a CLI --cli dropped is passed through by name", func(t *testing.T) {
		e := Eligible(cfg, cat, &args.Request{CLI: "claude", Model: "gpt-6.1-sol"})
		assert.True(t, e.ModelPassthrough)
		assert.Equal(t, []catalog.Option{{ID: "claude", CLI: "claude", Name: "gpt-6.1-sol"}}, e.Options)
	})
}

func TestEligibleEffort(t *testing.T) {
	cfg, cat := embedded(t)

	t.Run("alone", func(t *testing.T) {
		e := Eligible(cfg, cat, &args.Request{Effort: "high"})
		assert.Equal(t, idsOf(cat, func(o catalog.Option) bool { return o.Effort == "high" }), ids(e.Options))
		assert.False(t, e.EffortPassthrough)
		assert.Empty(t, e.Dropped)
	})

	t.Run("only one CLI has it", func(t *testing.T) {
		e := Eligible(cfg, cat, &args.Request{Effort: "ultra"})
		assert.Equal(t, idsOf(cat, func(o catalog.Option) bool { return o.Effort == "ultra" }), ids(e.Options))
	})

	t.Run("nothing left has it: models kept without the effort", func(t *testing.T) {
		e := Eligible(cfg, cat, &args.Request{CLI: "claude", Effort: "ultra"})
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
		fixCfg, fixCat := requestFixture(t)
		e := Eligible(fixCfg, fixCat, &args.Request{Model: "fast-model", Effort: "high"})
		assert.True(t, e.EffortPassthrough)
		require.Len(t, e.Options, 1)
		assert.Equal(t, "fast", e.Options[0].ID)
		assert.Equal(t, "high", e.EffortFor(e.Options[0]))
	})

	t.Run("model and effort leave one option", func(t *testing.T) {
		e := Eligible(cfg, cat, &args.Request{Model: "opus", Effort: "high"})
		assert.Equal(t, []string{"claude-opus-5-5@high"}, ids(e.Options))
		assert.Equal(t, "high", e.EffortFor(e.Options[0]))
	})

	t.Run("model outside the catalog passes it through", func(t *testing.T) {
		e := Eligible(cfg, cat, &args.Request{CLI: "codex", Model: "gpt-9", Effort: "high"})
		assert.True(t, e.ModelPassthrough)
		assert.True(t, e.EffortPassthrough)
		require.Len(t, e.Options, 1)
		assert.Equal(t, "high", e.EffortFor(e.Options[0]))
	})

	t.Run("no effort given, model without efforts emits none", func(t *testing.T) {
		fixCfg, fixCat := requestFixture(t)
		e := Eligible(fixCfg, fixCat, &args.Request{Model: "fast-model"})
		require.Len(t, e.Options, 1)
		assert.Empty(t, e.EffortFor(e.Options[0]))
	})
}

func TestEligibleRalphexArgvHasNoSkips(t *testing.T) {
	cfg, cat := embedded(t)

	for name, req := range map[string]*args.Request{"claude": ralphexClaude(), "codex": ralphexCodex()} {
		t.Run(name, func(t *testing.T) {
			e := Eligible(cfg, cat, req)
			assert.Equal(t, []string{name}, e.CLIs())
			cli, ok := cfg.CLIByName(name)
			require.True(t, ok)
			o := e.Options[0]
			res := args.Build(cli, req, args.Choice{Model: o.Name, Effort: e.EffortFor(o), Pinned: e.Pinned})
			assert.Empty(t, res.Skipped)
		})
	}
}

func TestEligibleNeverEmpty(t *testing.T) {
	cfg, cat := embedded(t)
	argSets := [][]args.Arg{nil, {flag("output-format", "json")}, {flag("sandbox", "read-only")},
		{flag("output-format", "json"), flag("sandbox", "read-only")}, {flag("unknown", "")}}
	for _, cli := range []string{"", "claude", "codex", "gemini"} {
		for _, model := range []string{"", "opus", "gpt-6-luna", "gpt-9"} {
			for _, effort := range []string{"", "high", "ultra", "bogus"} {
				for _, a := range argSets {
					req := &args.Request{CLI: cli, Model: model, Effort: effort, Args: a}
					e := Eligible(cfg, cat, req)
					assert.NotEmpty(t, e.Options, "cli=%q model=%q effort=%q args=%v", cli, model, effort, a)
				}
			}
		}
	}
}
