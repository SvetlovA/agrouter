package defaults_test

import (
	"bufio"
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SvetlovA/agrouter/pkg/config"
	"github.com/SvetlovA/agrouter/pkg/config/defaults"
)

func loadEmbedded(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(config.Sources{Embedded: defaults.Config})
	require.NoError(t, err)
	return cfg
}

func TestEmbeddedLoadsAndValidates(t *testing.T) {
	cfg := loadEmbedded(t)

	a := cfg.Agrouter
	assert.Empty(t, a.APIKey, "api_key ships as an empty placeholder")
	assert.Equal(t, config.LayerEmbedded, a.APIKeySource)
	assert.Equal(t, "jev-latest", a.JevModel)
	assert.Equal(t, 10*time.Second, a.Timeout)
	assert.Equal(t, 64, a.MaxChunks)
	assert.Equal(t, 4, a.ChunkParallel)
	assert.InDelta(t, 0.05, a.RelevanceFloor, 1e-9)
	assert.True(t, strings.HasPrefix(a.Question, "Which coding-agent CLI, model and reasoning effort"))
	assert.True(t, strings.HasPrefix(a.ChunkQuestion, "Which coding-agent CLI, model and reasoning effort"))
	assert.True(t, strings.HasPrefix(a.Relevance, "Does the text in `chunk.text`"))

	names := make([]string, 0, len(cfg.CLIs))
	for _, cli := range cfg.CLIs {
		names = append(names, cli.Name)
	}
	assert.Equal(t, []string{"claude", "codex"}, names)

	sections := make([]string, 0, len(cfg.Models))
	for _, m := range cfg.Models {
		sections = append(sections, m.Section)
		assert.Equal(t, m.Section, m.Name, "section names are the full model ids")
		assert.NotEmpty(t, m.Description, m.Section)
		assert.NotEmpty(t, m.Source, m.Section)
	}
	assert.Equal(t, []string{
		"claude-fable-5-1", "claude-opus-5-5", "claude-sonnet-5", "claude-haiku-4-5",
		"gpt-6-astra", "gpt-6-sol", "gpt-6-luna",
	}, sections, "catalog order")

	assert.Len(t, cfg.Efforts, 11, "5 Claude + 6 Codex effort descriptions")
	for id, eff := range cfg.Efforts {
		assert.NotEmpty(t, eff.Description, id)
		assert.NotEmpty(t, eff.Source, id)
	}
}

func TestEmbeddedModels(t *testing.T) {
	cfg := loadEmbedded(t)
	byName := map[string]config.Model{}
	for _, m := range cfg.Models {
		byName[m.Section] = m
	}

	claudeEfforts := []string{"low", "medium", "high", "xhigh", "max"}
	codexEfforts := []string{"low", "medium", "high", "xhigh", "max", "ultra"}
	tests := []struct {
		section string
		cli     string
		aliases []string
		efforts []string
	}{
		{section: "claude-fable-5-1", cli: "claude", efforts: claudeEfforts},
		{section: "claude-opus-5-5", cli: "claude", aliases: []string{"opus"}, efforts: claudeEfforts},
		{section: "claude-sonnet-5", cli: "claude", efforts: claudeEfforts},
		{section: "claude-haiku-4-5", cli: "claude"},
		{section: "gpt-6-astra", cli: "codex", efforts: codexEfforts},
		{section: "gpt-6-sol", cli: "codex", efforts: codexEfforts},
		{section: "gpt-6-luna", cli: "codex", efforts: claudeEfforts},
	}
	options := 0
	for _, tt := range tests {
		t.Run(tt.section, func(t *testing.T) {
			m, ok := byName[tt.section]
			require.True(t, ok)
			assert.Equal(t, tt.cli, m.CLI)
			assert.Equal(t, tt.aliases, m.Aliases)
			assert.Equal(t, tt.efforts, m.Efforts)
			for _, e := range m.Efforts {
				assert.Contains(t, cfg.Efforts, m.CLI+"."+e, "every effort has a description")
			}
		})
		options += max(len(tt.efforts), 1)
	}
	assert.Equal(t, 33, options, "16 Claude + 17 Codex options")
	assert.Empty(t, byName["claude-haiku-4-5"].Efforts, "Haiku 4.5 does not support effort")
}

func TestEmbeddedMappings(t *testing.T) {
	cfg := loadEmbedded(t)
	claude, ok := cfg.CLIByName("claude")
	require.True(t, ok)
	codex, ok := cfg.CLIByName("codex")
	require.True(t, ok)

	assert.Equal(t, "claude", claude.Command)
	assert.Equal(t, "codex", codex.Command)

	assert.Equal(t, []string{"-p", "{prompt}"}, claude.Args["print"])
	assert.Equal(t, []string{"exec", "{prompt}"}, codex.Args["print"])
	assert.Equal(t, []string{"-c", `model_reasoning_effort="{effort}"`}, codex.Args["effort"], "TOML quotes survive byte-exact")
	assert.Equal(t, []string{"--sandbox", "read-only", "-c", `approval_policy="never"`, "--skip-git-repo-check"},
		codex.Args["permission-mode.plan"])

	for _, mode := range []string{"bypassPermissions", "plan", "acceptEdits", "auto", "manual", "dontAsk"} {
		assert.Contains(t, claude.Args, "permission-mode."+mode)
	}
	for _, mode := range []string{"bypassPermissions", "plan", "acceptEdits", "auto"} {
		assert.Contains(t, codex.Args, "permission-mode."+mode)
	}
	assert.NotContains(t, codex.Args, "permission-mode.manual")
	assert.NotContains(t, codex.Args, "permission-mode.dontAsk")

	for _, f := range []string{"text", "json", "stream-json"} {
		assert.Contains(t, claude.Args, "output-format."+f)
	}
	assert.NotContains(t, codex.Args, "output-format.json")
	assert.Equal(t, []string{}, codex.Args["output-format.text"], "supported, adds nothing")
	assert.Equal(t, []string{"--json"}, codex.Args["output-format.stream-json"])
	assert.Equal(t, []string{"--verbose"}, claude.Args["verbose"])
	assert.Equal(t, []string{}, codex.Args["verbose"])

	for _, key := range []string{
		"stream_idle_timeout_ms", "project_doc", "project_doc_fallback_filenames", "features.multi_agent",
		"agents.reviewer.description", "model", "model_reasoning_effort",
	} {
		assert.Equal(t, []string{"-c", "{value}"}, codex.Args["config."+key], key)
	}
	for _, v := range []string{"read-only", "workspace-write", "danger-full-access"} {
		assert.Equal(t, []string{"--sandbox", v}, codex.Args["sandbox."+v])
	}
	assert.NotContains(t, claude.Args, "sandbox.read-only")
}

// TestNoInlineComments guards against a trailing "; ..." or "# ..." on a value line: with
// IgnoreInlineComment it would silently become part of the value.
func TestNoInlineComments(t *testing.T) {
	inline := regexp.MustCompile(`\s[;#]`)
	sc := bufio.NewScanner(bytes.NewReader(defaults.Config))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' || line[0] == '[' {
			continue
		}
		_, value, ok := strings.Cut(line, "=")
		require.True(t, ok, "line %d is neither a comment, a section nor a key: %q", n, line)
		assert.False(t, inline.MatchString(value), "line %d carries an inline comment: %q", n, line)
	}
	require.NoError(t, sc.Err())
}

// TestMatchesDesign keeps the [agrouter] and [cli.*] sections identical to the config block in
// docs/design.md, the specification.
func TestMatchesDesign(t *testing.T) {
	design, err := os.ReadFile("../../../docs/design.md")
	require.NoError(t, err)
	_, block, ok := bytes.Cut(design, []byte("### Sections\n\n```ini\n"))
	require.True(t, ok, "design has an ini block under Sections")
	block, _, ok = bytes.Cut(block, []byte("\n```"))
	require.True(t, ok)

	want := keyValues(t, block)
	got := keyValues(t, defaults.Config)
	for _, section := range []string{"agrouter", "cli.claude", "cli.claude.args", "cli.codex", "cli.codex.args"} {
		require.Contains(t, want, section)
		assert.Equal(t, want[section], got[section], "[%s] differs from docs/design.md", section)
	}
}

// keyValues splits INI text into section → key → value, trimming spaces around both.
func keyValues(t *testing.T, data []byte) map[string]map[string]string {
	t.Helper()
	out := map[string]map[string]string{}
	var cur string
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "" || line[0] == '#' || line[0] == ';':
		case line[0] == '[':
			cur = strings.Trim(line, "[]")
			out[cur] = map[string]string{}
		default:
			k, v, _ := strings.Cut(line, "=")
			out[cur][strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	require.NoError(t, sc.Err())
	return out
}
