package args

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SvetlovA/agrouter/pkg/config"
	"github.com/SvetlovA/agrouter/pkg/config/defaults"
)

func embeddedCLI(t *testing.T, name string) config.CLI {
	t.Helper()
	cfg, err := config.Load(config.Sources{Embedded: defaults.Config})
	require.NoError(t, err)
	cli, ok := cfg.CLIByName(name)
	require.True(t, ok, name)
	return cli
}

// flag builds a mapped argument the way cmd/agrouter records it.
func flag(name, value string) Arg {
	if value == "" {
		return Arg{Spelling: "--" + name, Key: name}
	}
	return Arg{Spelling: "--" + name + " " + value, Key: name + "." + value}
}

func bypass(alias string) Arg {
	return Arg{Spelling: "--" + alias, Key: "permission-mode.bypassPermissions"}
}

func cfgArg(kv string) Arg {
	key := kv
	for i := range kv {
		if kv[i] == '=' {
			key = kv[:i]
			break
		}
	}
	return Arg{Spelling: "-c " + kv, Key: ConfigKeyPrefix + key, Value: kv}
}

func spellings(skipped []Skip) []string {
	out := []string{}
	for _, s := range skipped {
		out = append(out, s.Spelling)
	}
	return out
}

func TestBuildGoldenOutputFormat(t *testing.T) {
	claude, codex := embeddedCLI(t, "claude"), embeddedCLI(t, "codex")
	tests := []struct {
		format    string
		claude    []string
		codex     []string
		codexSkip []string
	}{
		{"text", []string{"claude", "-p", "--output-format", "text"}, []string{"codex", "exec"}, []string{"--output-format text"}},
		{"json", []string{"claude", "-p", "--output-format", "json"}, []string{"codex", "exec"}, []string{"--output-format json"}},
		{"stream-json", []string{"claude", "-p", "--output-format", "stream-json"}, []string{"codex", "exec", "--json"}, []string{}},
	}
	for _, tc := range tests {
		t.Run(tc.format, func(t *testing.T) {
			req := &Request{Args: []Arg{flag("output-format", tc.format)}}
			res := Build(claude, req, Choice{})
			assert.Equal(t, tc.claude, res.Argv)
			assert.Empty(t, res.Skipped)

			res = Build(codex, req, Choice{})
			assert.Equal(t, tc.codex, res.Argv)
			assert.Equal(t, tc.codexSkip, spellings(res.Skipped))
		})
	}
}

func TestBuildGoldenPermissionMode(t *testing.T) {
	claude, codex := embeddedCLI(t, "claude"), embeddedCLI(t, "codex")
	tests := []struct {
		mode   string
		claude []string
		codex  []string // nil: skipped on codex
	}{
		{"bypassPermissions", []string{"--dangerously-skip-permissions"},
			[]string{"--dangerously-bypass-approvals-and-sandbox", "--skip-git-repo-check"}},
		{"plan", []string{"--permission-mode", "plan"},
			[]string{"--sandbox", "read-only", "-c", `approval_policy="never"`, "--skip-git-repo-check"}},
		{"acceptEdits", []string{"--permission-mode", "acceptEdits"}, []string{"--sandbox", "workspace-write"}},
		{"auto", []string{"--permission-mode", "auto"}, []string{"--approve-for-me"}},
		{"manual", []string{"--permission-mode", "manual"}, nil},
		{"dontAsk", []string{"--permission-mode", "dontAsk"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.mode, func(t *testing.T) {
			req := &Request{Args: []Arg{flag("permission-mode", tc.mode)}}
			res := Build(claude, req, Choice{})
			assert.Equal(t, append([]string{"claude", "-p"}, tc.claude...), res.Argv)
			assert.Empty(t, res.Skipped)

			res = Build(codex, req, Choice{})
			assert.Equal(t, append([]string{"codex", "exec"}, tc.codex...), res.Argv)
			if tc.codex == nil {
				require.Len(t, res.Skipped, 1)
				assert.Equal(t, "agrouter: warning: skipped --permission-mode "+tc.mode+": codex has no mapping for it",
					res.Skipped[0].Warning)
			} else {
				assert.Empty(t, res.Skipped)
			}
		})
	}
}

func TestBuildDesignExamples(t *testing.T) {
	claude, codex := embeddedCLI(t, "claude"), embeddedCLI(t, "codex")
	req := &Request{Args: []Arg{
		bypass("dangerously-skip-permissions"), flag("output-format", "stream-json"), flag("verbose", ""),
	}}

	res := Build(claude, req, Choice{Model: "claude-opus-5-5", Effort: "high"})
	assert.Equal(t, []string{"claude", "-p", "--dangerously-skip-permissions", "--output-format", "stream-json",
		"--verbose", "--model", "claude-opus-5-5", "--effort", "high"}, res.Argv)
	assert.Empty(t, res.Skipped)

	res = Build(codex, req, Choice{Model: "gpt-6-sol", Effort: "high"})
	assert.Equal(t, []string{"codex", "exec", "--dangerously-bypass-approvals-and-sandbox", "--skip-git-repo-check",
		"--json", "--model", "gpt-6-sol", "-c", `model_reasoning_effort="high"`}, res.Argv)
	require.Len(t, res.Skipped, 1)
	assert.Equal(t, "agrouter: warning: skipped --verbose: maps to nothing for codex", res.Skipped[0].Warning)
	assert.Equal(t, "--verbose", res.Skipped[0].Spelling)
}

func TestBuildCodexNoPermissionModeNoSkipGitRepoCheck(t *testing.T) {
	res := Build(embeddedCLI(t, "codex"), &Request{Prompt: Optional{Value: "x", Set: true}}, Choice{Model: "gpt-6-luna"})
	assert.Equal(t, []string{"codex", "exec", "x", "--model", "gpt-6-luna"}, res.Argv)
	assert.NotContains(t, res.Argv, "--skip-git-repo-check")
}

func TestBuildBypassAliasesEmittedOnce(t *testing.T) {
	codex := embeddedCLI(t, "codex")
	for _, req := range []*Request{
		{Args: []Arg{bypass("dangerously-skip-permissions")}},
		{Args: []Arg{flag("permission-mode", "bypassPermissions")}},
		{Args: []Arg{bypass("dangerously-skip-permissions"), bypass("dangerously-bypass-approvals-and-sandbox"),
			flag("permission-mode", "bypassPermissions")}},
	} {
		res := Build(codex, req, Choice{})
		assert.Equal(t, []string{"codex", "exec", "--dangerously-bypass-approvals-and-sandbox", "--skip-git-repo-check"},
			res.Argv)
	}
}

func TestBuildContradictionsInOrder(t *testing.T) {
	res := Build(embeddedCLI(t, "codex"), &Request{Args: []Arg{
		flag("sandbox", "read-only"), flag("permission-mode", "acceptEdits"), flag("sandbox", "read-only"),
	}}, Choice{})
	assert.Equal(t, []string{"codex", "exec", "--sandbox", "read-only", "--sandbox", "workspace-write"}, res.Argv)

	res = Build(embeddedCLI(t, "claude"), &Request{Args: []Arg{
		bypass("dangerously-skip-permissions"), flag("permission-mode", "plan"),
	}}, Choice{})
	assert.Equal(t, []string{"claude", "-p", "--dangerously-skip-permissions", "--permission-mode", "plan"}, res.Argv)
}

func TestBuildConfigArgs(t *testing.T) {
	desc := `agents.reviewer.description="Reviews code; flags \"bugs\" = issues"`
	req := &Request{Args: []Arg{
		cfgArg("stream_idle_timeout_ms=3600000"),
		cfgArg("features.multi_agent=true"),
		cfgArg("approval_policy=never"),
		cfgArg(desc),
		cfgArg(`project_doc_fallback_filenames=["CLAUDE.md"]`),
		cfgArg("stream_idle_timeout_ms=1000"),
		cfgArg("features.multi_agent=true"),
	}}
	res := Build(embeddedCLI(t, "codex"), req, Choice{})
	assert.Equal(t, []string{"codex", "exec",
		"-c", "stream_idle_timeout_ms=3600000",
		"-c", "features.multi_agent=true",
		"-c", desc,
		"-c", `project_doc_fallback_filenames=["CLAUDE.md"]`,
		"-c", "stream_idle_timeout_ms=1000",
	}, res.Argv)
	require.Len(t, res.Skipped, 1)
	assert.Equal(t, "-c approval_policy=never", res.Skipped[0].Spelling)
	assert.Equal(t, "agrouter: warning: skipped -c approval_policy=never: codex has no mapping for it",
		res.Skipped[0].Warning)

	res = Build(embeddedCLI(t, "claude"), req, Choice{})
	assert.Equal(t, []string{"claude", "-p"}, res.Argv)
	assert.Len(t, res.Skipped, 6, "each distinct key=value warned once")
}

func TestBuildPrompt(t *testing.T) {
	claude, codex := embeddedCLI(t, "claude"), embeddedCLI(t, "codex")
	prompt := "fix \"it\" in 'a b'\nthen\t$HOME & %PATH%"
	req := &Request{Prompt: Optional{Value: prompt, Set: true}, Args: []Arg{flag("verbose", "")}}

	res := Build(claude, req, Choice{Model: "claude-sonnet-5", Effort: "low"})
	assert.Equal(t, []string{"claude", "-p", prompt, "--verbose", "--model", "claude-sonnet-5", "--effort", "low"}, res.Argv)

	res = Build(claude, &Request{}, Choice{})
	assert.Equal(t, []string{"claude", "-p"}, res.Argv, "{prompt} dropped to zero tokens")
	res = Build(claude, &Request{Prompt: Optional{Set: true}}, Choice{})
	assert.Equal(t, []string{"claude", "-p"}, res.Argv, "an empty prompt is never an empty token")

	// prompt at the end through the prompt key
	codex.Args = cloneArgs(codex.Args)
	codex.Args[KeyPrint] = []string{"exec"}
	codex.Args[KeyPrompt] = []string{"{prompt}"}
	res = Build(codex, &Request{Prompt: Optional{Value: prompt, Set: true}, Args: []Arg{flag("output-format", "stream-json")},
		Raw: []string{"--search"}}, Choice{Model: "gpt-6-sol", Effort: "high", Pinned: true})
	assert.Equal(t, []string{"codex", "exec", "--json", "--model", "gpt-6-sol", "-c", `model_reasoning_effort="high"`,
		prompt, "--search"}, res.Argv)
}

func TestBuildModelAndEffort(t *testing.T) {
	claude := embeddedCLI(t, "claude")

	res := Build(claude, &Request{}, Choice{Model: "claude-haiku-4-5"})
	assert.Equal(t, []string{"claude", "-p", "--model", "claude-haiku-4-5"}, res.Argv, "no effort argument")

	res = Build(claude, &Request{}, Choice{Effort: "high"})
	assert.Equal(t, []string{"claude", "-p", "--effort", "high"}, res.Argv, "caller's effort kept, model left to the CLI")

	noEffort := config.CLI{Name: "mini", Command: "mini", Args: map[string][]string{
		"print": {"run", "{prompt}"}, "model": {"-m", "{model}"},
	}}
	res = Build(noEffort, &Request{}, Choice{Model: "m1", Effort: "high", EffortSpell: "-c model_reasoning_effort=high"})
	assert.Equal(t, []string{"mini", "run", "-m", "m1"}, res.Argv)
	require.Len(t, res.Skipped, 1)
	assert.Equal(t, "agrouter: warning: skipped -c model_reasoning_effort=high: mini has no mapping for it",
		res.Skipped[0].Warning)
	res = Build(noEffort, &Request{}, Choice{Effort: "high"})
	assert.Equal(t, "--effort high", res.Skipped[0].Spelling)
}

func TestBuildRawPassthrough(t *testing.T) {
	claude := embeddedCLI(t, "claude")
	req := &Request{Raw: []string{"--add-dir", "/tmp/x y"}, Prompt: Optional{Value: "p", Set: true}}

	res := Build(claude, req, Choice{Pinned: true})
	assert.Equal(t, []string{"claude", "-p", "p", "--add-dir", "/tmp/x y"}, res.Argv)
	assert.Empty(t, res.Skipped)

	res = Build(claude, req, Choice{})
	assert.Equal(t, []string{"claude", "-p", "p"}, res.Argv)
	require.Len(t, res.Skipped, 1)
	assert.Equal(t, "-- --add-dir /tmp/x y", res.Skipped[0].Spelling)
	assert.Equal(t, "agrouter: warning: skipped 2 raw argument(s) after --: passed through only with --cli",
		res.Skipped[0].Warning)
}

func TestBuildOwnFlagsNeverInArgv(t *testing.T) {
	req := &Request{CLI: "claude", APIKey: Optional{Value: "sk-secret", Set: true}, Model: "opus",
		ModelSource: SourceFlag, Args: []Arg{flag("verbose", "")}}
	res := Build(embeddedCLI(t, "claude"), req, Choice{Model: "claude-opus-5-5", Pinned: true})
	assert.Equal(t, []string{"claude", "-p", "--verbose", "--model", "claude-opus-5-5"}, res.Argv)
	for _, tok := range res.Argv {
		assert.NotContains(t, tok, "sk-secret")
		assert.NotContains(t, tok, "--cli")
		assert.NotContains(t, tok, "--jev-api-key")
	}
}

func TestBuildMadeUpCLI(t *testing.T) {
	const ini = `
[agrouter]
timeout         = 10s
max_chunks      = 8
chunk_parallel  = 2
relevance_floor = 0.1
question        = Which option?
chunk_question  = Which option for this chunk?
relevance       = Is this chunk relevant?

[cli.zeta]
command     = zeta-agent
description = A made-up agent.

[cli.zeta.args]
print                     = ["--headless", "--task", "{prompt}"]
model                     = ["--llm={model}"]
effort                    = ["--think", "{effort}"]
output-format.stream-json = ["--emit", "ndjson"]
permission-mode.plan      = ["--read-only"]
verbose                   = []

[model.zeta-one]
cli     = zeta
name    = z1
efforts = low, high
`
	cfg, err := config.Load(config.Sources{Embedded: []byte(ini)})
	require.NoError(t, err)
	zeta, ok := cfg.CLIByName("zeta")
	require.True(t, ok)

	req := &Request{Prompt: Optional{Value: "do it", Set: true}, Args: []Arg{
		flag("output-format", "stream-json"), flag("verbose", ""), flag("permission-mode", "plan"),
		flag("output-format", "json"),
	}}
	res := Build(zeta, req, Choice{Model: "z1", Effort: "high"})
	assert.Equal(t, []string{"zeta-agent", "--headless", "--task", "do it", "--emit", "ndjson", "--read-only",
		"--llm=z1", "--think", "high"}, res.Argv)
	assert.Equal(t, []string{
		"agrouter: warning: skipped --verbose: maps to nothing for zeta",
		"agrouter: warning: skipped --output-format json: zeta has no mapping for it",
	}, []string{res.Skipped[0].Warning, res.Skipped[1].Warning})
	assert.Equal(t, []string{"--verbose", "--output-format json"}, spellings(res.Skipped))
}

func TestRedacted(t *testing.T) {
	claude := embeddedCLI(t, "claude")
	res := Build(claude, &Request{Prompt: Optional{Value: "secret task text", Set: true},
		Args: []Arg{flag("output-format", "json")}, Raw: []string{"--add-dir", "/private"}},
		Choice{Model: "claude-opus-5-5", Effort: "high", Pinned: true})
	got := res.Redacted()
	assert.Equal(t, "claude -p <prompt> --output-format json --model claude-opus-5-5 --effort high <2 raw argument(s)>", got)
	assert.NotContains(t, got, "secret")
	assert.NotContains(t, got, "/private")

	res = Build(embeddedCLI(t, "codex"), &Request{}, Choice{Effort: "low"})
	assert.Equal(t, `codex exec -c "model_reasoning_effort=\"low\""`, res.Redacted())

	assert.Empty(t, Result{}.Redacted())
	assert.Equal(t, `"" "a b" "x\x01"`, Result{Argv: []string{"", "a b", "x\x01"}, promptAt: -1, rawAt: 3}.Redacted())
}

func cloneArgs(m map[string][]string) map[string][]string {
	out := make(map[string][]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
