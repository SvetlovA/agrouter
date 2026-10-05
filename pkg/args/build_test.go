package args

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SvetlovA/agrouter/pkg/config"
)

// fixtureCLI supplies mappings for behavior tests independently of embedded defaults.
func fixtureCLI(t *testing.T, name string) config.CLI {
	t.Helper()
	cli := config.CLI{Name: name, Command: name, Args: map[string][]string{
		"model": {"--model", "{model}"},
	}}
	switch name {
	case "alpha":
		cli.Args["print"] = []string{"-p"}
		cli.Args["prompt"] = []string{"--", "{prompt}"}
		cli.Args["effort"] = []string{"--effort", "{effort}"}
		cli.Args["verbose"] = []string{"--verbose"}
		cli.Args["permission-mode.bypassPermissions"] = []string{"--dangerously-skip-permissions"}
		for _, mode := range []string{"plan", "acceptEdits", "auto", "manual", "dontAsk"} {
			cli.Args["permission-mode."+mode] = []string{"--permission-mode", mode}
		}
		for _, format := range []string{"text", "json", "stream-json"} {
			cli.Args["output-format."+format] = []string{"--output-format", format}
		}
	case "beta":
		cli.Args["print"] = []string{"exec"}
		cli.Args["prompt"] = []string{"--", "{prompt}"}
		cli.Args["effort"] = []string{"-c", `model_reasoning_effort="{effort}"`}
		cli.Args["verbose"] = []string{}
		cli.Args["output-format.text"] = []string{}
		cli.Args["output-format.stream-json"] = []string{"--json"}
		cli.Args["permission-mode.bypassPermissions"] = []string{"--dangerously-bypass-approvals-and-sandbox", "--skip-git-repo-check"}
		cli.Args["permission-mode.plan"] = []string{"--sandbox", "read-only", "-c", `approval_policy="never"`, "--skip-git-repo-check"}
		cli.Args["permission-mode.acceptEdits"] = []string{"--sandbox", "workspace-write"}
		cli.Args["permission-mode.auto"] = []string{"--approve-for-me"}
		for _, sandbox := range []string{"read-only", "workspace-write", "danger-full-access"} {
			cli.Args["sandbox."+sandbox] = []string{"--sandbox", sandbox}
		}
		for _, key := range []string{"stream_idle_timeout_ms", "project_doc", "project_doc_fallback_filenames",
			"features.multi_agent", "agents.reviewer.description", "model", "model_reasoning_effort"} {
			cli.Args[config.ConfigKeyPrefix+key] = []string{"-c", "{value}"}
		}
	default:
		t.Fatalf("unknown CLI fixture %q", name)
	}
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
	return Arg{Spelling: "-c " + kv, Key: config.ConfigKeyPrefix + key, Value: kv}
}

func spellings(skipped []Skip) []string {
	out := make([]string, 0, len(skipped))
	for _, s := range skipped {
		out = append(out, s.Spelling)
	}
	return out
}

func TestBuildOutputFormat(t *testing.T) {
	alpha, beta := fixtureCLI(t, "alpha"), fixtureCLI(t, "beta")
	tests := []struct {
		format   string
		alpha    []string
		beta     []string
		betaSkip []string
	}{
		{"text", []string{"alpha", "-p", "--output-format", "text"}, []string{"beta", "exec"}, []string{"--output-format text"}},
		{"json", []string{"alpha", "-p", "--output-format", "json"}, []string{"beta", "exec"}, []string{"--output-format json"}},
		{"stream-json", []string{"alpha", "-p", "--output-format", "stream-json"}, []string{"beta", "exec", "--json"}, []string{}},
	}
	for _, tc := range tests {
		t.Run(tc.format, func(t *testing.T) {
			req := &Request{Args: []Arg{flag("output-format", tc.format)}}
			res := Build(alpha, req, Choice{})
			assert.Equal(t, tc.alpha, res.Argv)
			assert.Empty(t, res.Skipped)

			res = Build(beta, req, Choice{})
			assert.Equal(t, tc.beta, res.Argv)
			assert.Equal(t, tc.betaSkip, spellings(res.Skipped))
		})
	}
}

func TestBuildPermissionMode(t *testing.T) {
	alpha, beta := fixtureCLI(t, "alpha"), fixtureCLI(t, "beta")
	tests := []struct {
		mode  string
		alpha []string
		beta  []string // nil: skipped on beta
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
			res := Build(alpha, req, Choice{})
			assert.Equal(t, append([]string{"alpha", "-p"}, tc.alpha...), res.Argv)
			assert.Empty(t, res.Skipped)

			res = Build(beta, req, Choice{})
			assert.Equal(t, append([]string{"beta", "exec"}, tc.beta...), res.Argv)
			if tc.beta == nil {
				require.Len(t, res.Skipped, 1)
				assert.Equal(t, "agrouter: warning: skipped --permission-mode "+tc.mode+": beta has no mapping for it",
					res.Skipped[0].Warning)
			} else {
				assert.Empty(t, res.Skipped)
			}
		})
	}
}

func TestBuildExamples(t *testing.T) {
	alpha, beta := fixtureCLI(t, "alpha"), fixtureCLI(t, "beta")
	req := &Request{Args: []Arg{
		bypass("dangerously-skip-permissions"), flag("output-format", "stream-json"), flag("verbose", ""),
	}}

	res := Build(alpha, req, Choice{Model: "large-model", Effort: "high"})
	assert.Equal(t, []string{"alpha", "-p", "--dangerously-skip-permissions", "--output-format", "stream-json",
		"--verbose", "--model", "large-model", "--effort", "high"}, res.Argv)
	assert.Empty(t, res.Skipped)

	res = Build(beta, req, Choice{Model: "worker-model", Effort: "high"})
	assert.Equal(t, []string{"beta", "exec", "--dangerously-bypass-approvals-and-sandbox", "--skip-git-repo-check",
		"--json", "--model", "worker-model", "-c", `model_reasoning_effort="high"`}, res.Argv)
	require.Len(t, res.Skipped, 1)
	assert.Equal(t, "agrouter: warning: skipped --verbose: maps to nothing for beta", res.Skipped[0].Warning)
	assert.Equal(t, "--verbose", res.Skipped[0].Spelling)
}

func TestBuildCodexNoPermissionModeNoSkipGitRepoCheck(t *testing.T) {
	res := Build(fixtureCLI(t, "beta"), &Request{Prompt: Optional{Value: "x", Set: true}}, Choice{Model: "small-model"})
	assert.Equal(t, []string{"beta", "exec", "--model", "small-model", "--", "x"}, res.Argv)
	assert.NotContains(t, res.Argv, "--skip-git-repo-check")
}

func TestBuildBypassAliasesEmittedOnce(t *testing.T) {
	beta := fixtureCLI(t, "beta")
	for _, req := range []*Request{
		{Args: []Arg{bypass("dangerously-skip-permissions")}},
		{Args: []Arg{flag("permission-mode", "bypassPermissions")}},
		{Args: []Arg{bypass("dangerously-skip-permissions"), bypass("dangerously-bypass-approvals-and-sandbox"),
			flag("permission-mode", "bypassPermissions")}},
	} {
		res := Build(beta, req, Choice{})
		assert.Equal(t, []string{"beta", "exec", "--dangerously-bypass-approvals-and-sandbox", "--skip-git-repo-check"},
			res.Argv)
	}
}

func TestBuildContradictionsInOrder(t *testing.T) {
	res := Build(fixtureCLI(t, "beta"), &Request{Args: []Arg{
		flag("sandbox", "read-only"), flag("permission-mode", "acceptEdits"), flag("sandbox", "read-only"),
	}}, Choice{})
	assert.Equal(t, []string{"beta", "exec", "--sandbox", "read-only", "--sandbox", "workspace-write"}, res.Argv)

	res = Build(fixtureCLI(t, "alpha"), &Request{Args: []Arg{
		bypass("dangerously-skip-permissions"), flag("permission-mode", "plan"),
	}}, Choice{})
	assert.Equal(t, []string{"alpha", "-p", "--dangerously-skip-permissions", "--permission-mode", "plan"}, res.Argv)
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
	res := Build(fixtureCLI(t, "beta"), req, Choice{})
	assert.Equal(t, []string{"beta", "exec",
		"-c", "stream_idle_timeout_ms=3600000",
		"-c", "features.multi_agent=true",
		"-c", desc,
		"-c", `project_doc_fallback_filenames=["CLAUDE.md"]`,
		"-c", "stream_idle_timeout_ms=1000",
	}, res.Argv)
	require.Len(t, res.Skipped, 1)
	assert.Equal(t, "-c approval_policy=never", res.Skipped[0].Spelling)
	assert.Equal(t, "agrouter: warning: skipped -c approval_policy=never: beta has no mapping for it",
		res.Skipped[0].Warning)

	res = Build(fixtureCLI(t, "alpha"), req, Choice{})
	assert.Equal(t, []string{"alpha", "-p"}, res.Argv)
	assert.Len(t, res.Skipped, 6, "each distinct key=value warned once")
}

func TestBuildPrompt(t *testing.T) {
	alpha, beta := fixtureCLI(t, "alpha"), fixtureCLI(t, "beta")
	prompt := "fix \"it\" in 'a b'\nthen\t$HOME & %PATH%"
	req := &Request{Prompt: Optional{Value: prompt, Set: true}, Args: []Arg{flag("verbose", "")}}

	res := Build(alpha, req, Choice{Model: "everyday-model", Effort: "low"})
	assert.Equal(t, []string{"alpha", "-p", "--verbose", "--model", "everyday-model", "--effort", "low", "--", prompt}, res.Argv)

	res = Build(alpha, &Request{}, Choice{})
	assert.Equal(t, []string{"alpha", "-p"}, res.Argv, "no prompt: the prompt template is dropped, \"--\" included")
	res = Build(alpha, &Request{Prompt: Optional{Set: true}}, Choice{})
	assert.Equal(t, []string{"alpha", "-p"}, res.Argv, "an empty prompt is never an empty token")
	res = Build(alpha, &Request{PromptFlag: Optional{Set: true}, Prompt: Optional{Set: true}, PromptFile: Optional{Value: "f", Set: true}},
		Choice{})
	assert.Equal(t, []string{"alpha", "-p"}, res.Argv, "empty sources and the file path never reach argv")

	// raw passthrough before the prompt, so it stays options
	res = Build(beta, &Request{Prompt: Optional{Value: prompt, Set: true}, Args: []Arg{flag("output-format", "stream-json")},
		Raw: []string{"--search"}}, Choice{Model: "worker-model", Effort: "high", Pinned: true})
	assert.Equal(t, []string{"beta", "exec", "--json", "--model", "worker-model", "-c", `model_reasoning_effort="high"`,
		"--search", "--", prompt}, res.Argv)
}

// TestBuildPromptStaysLiteral pins that prompt text shaped like a flag reaches the child as the last
// argument, right after the template's "--", from every prompt source and with raw passthrough.
func TestBuildPromptStaysLiteral(t *testing.T) {
	alpha := fixtureCLI(t, "alpha")
	for _, text := range []string{"--help", "--dangerously-skip-permissions", "-", "--", "-p\nsecond line"} {
		for name, req := range map[string]Request{
			"-p":            {PromptFlag: Optional{Value: text, Set: true}},
			"positional":    {Prompt: Optional{Value: text, Set: true}},
			"--prompt-file": {PromptFile: Optional{Value: "task.md", Set: true}, FileText: text},
		} {
			t.Run(name+" "+text, func(t *testing.T) {
				req.Raw = []string{"--add-dir", "x"}
				res := Build(alpha, &req, Choice{Model: "m", Effort: "high", Pinned: true})
				assert.Equal(t, []string{"alpha", "-p", "--model", "m", "--effort", "high", "--add-dir", "x", "--", text},
					res.Argv)
			})
		}
	}
}

func TestBuildPromptSources(t *testing.T) {
	alpha := fixtureCLI(t, "alpha")
	tests := []struct {
		name string
		req  Request
		want string
	}{
		{"-p only", Request{PromptFlag: Optional{Value: "flag", Set: true}}, "flag"},
		{"file only", Request{PromptFile: Optional{Value: "task.md", Set: true}, FileText: "file\r\n"}, "file\r\n"},
		{"all three in order", Request{PromptFlag: Optional{Value: "flag", Set: true}, Prompt: Optional{Value: "pos", Set: true},
			PromptFile: Optional{Value: "task.md", Set: true}, FileText: "file"}, "flag\n\npos\n\nfile"},
		{"empty -p skipped", Request{PromptFlag: Optional{Set: true}, FileText: "file"}, "file"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := Build(alpha, &tc.req, Choice{})
			assert.Equal(t, []string{"alpha", "-p", "--", tc.want}, res.Argv, "one {prompt} token, without the file path")
			assert.Equal(t, tc.want, tc.req.ArgvPrompt())
			assert.Equal(t, "alpha -p -- <prompt>", res.Redacted())
		})
	}
}

func TestBuildModelAndEffort(t *testing.T) {
	alpha := fixtureCLI(t, "alpha")

	res := Build(alpha, &Request{}, Choice{Model: "alpha-haiku-4-5"})
	assert.Equal(t, []string{"alpha", "-p", "--model", "alpha-haiku-4-5"}, res.Argv, "no effort argument")

	res = Build(alpha, &Request{}, Choice{Effort: "high"})
	assert.Equal(t, []string{"alpha", "-p", "--effort", "high"}, res.Argv, "caller's effort kept, model left to the CLI")

	noEffort := config.CLI{Name: "mini", Command: "mini", Args: map[string][]string{
		"print": {"run"}, "prompt": {"--", "{prompt}"}, "model": {"-m", "{model}"},
	}}
	req := &Request{Effort: "high", EffortSource: SourceConfig, EffortSpell: "-c model_reasoning_effort=high"}
	res = Build(noEffort, req, Choice{Model: "m1", Effort: "high"})
	assert.Equal(t, []string{"mini", "run", "-m", "m1"}, res.Argv)
	require.Len(t, res.Skipped, 1)
	assert.Equal(t, "agrouter: warning: skipped -c model_reasoning_effort=high: mini has no mapping for it",
		res.Skipped[0].Warning)
	res = Build(noEffort, &Request{}, Choice{Effort: "high"})
	assert.Equal(t, "--effort high", res.Skipped[0].Spelling)
}

func TestBuildRawPassthrough(t *testing.T) {
	alpha := fixtureCLI(t, "alpha")
	req := &Request{Raw: []string{"--add-dir", "/tmp/x y"}, Prompt: Optional{Value: "p", Set: true}}

	res := Build(alpha, req, Choice{Pinned: true})
	assert.Equal(t, []string{"alpha", "-p", "--add-dir", "/tmp/x y", "--", "p"}, res.Argv)
	assert.Empty(t, res.Skipped)

	res = Build(alpha, req, Choice{})
	assert.Equal(t, []string{"alpha", "-p", "--", "p"}, res.Argv)
	require.Len(t, res.Skipped, 1)
	assert.Equal(t, "-- --add-dir /tmp/x y", res.Skipped[0].Spelling)
	assert.Equal(t, "agrouter: warning: skipped 2 raw argument(s) after --: passed through only with --cli",
		res.Skipped[0].Warning)
}

func TestBuildOwnFlagsNeverInArgv(t *testing.T) {
	req := &Request{CLI: "alpha", APIKey: Optional{Value: "sk-secret", Set: true}, Model: "opus",
		ModelSource: SourceFlag, Args: []Arg{flag("verbose", "")}}
	res := Build(fixtureCLI(t, "alpha"), req, Choice{Model: "large-model", Pinned: true})
	assert.Equal(t, []string{"alpha", "-p", "--verbose", "--model", "large-model"}, res.Argv)
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
question        = Which option?
chunk_question  = Which option for this chunk?
relevance       = Is this chunk relevant?
complexity_question = How complex is the project?
complexity_evidence = Does the text describe the project?

[cli.zeta]
command     = zeta-agent
description = A made-up agent.

[cli.zeta.args]
print                     = ["--headless"]
prompt                    = ["--task", "--", "{prompt}"]
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
	assert.Equal(t, []string{"zeta-agent", "--headless", "--emit", "ndjson", "--read-only",
		"--llm=z1", "--think", "high", "--task", "--", "do it"}, res.Argv)
	assert.Equal(t, []string{
		"agrouter: warning: skipped --verbose: maps to nothing for zeta",
		"agrouter: warning: skipped --output-format json: zeta has no mapping for it",
	}, []string{res.Skipped[0].Warning, res.Skipped[1].Warning})
	assert.Equal(t, []string{"--verbose", "--output-format json"}, spellings(res.Skipped))
}

func TestRedacted(t *testing.T) {
	alpha := fixtureCLI(t, "alpha")
	res := Build(alpha, &Request{Prompt: Optional{Value: "secret task text", Set: true},
		Args: []Arg{flag("output-format", "json")}, Raw: []string{"--add-dir", "/private"}},
		Choice{Model: "large-model", Effort: "high", Pinned: true})
	got := res.Redacted()
	assert.Equal(t, "alpha -p --output-format json --model large-model --effort high <2 raw argument(s)> -- <prompt>", got)
	assert.NotContains(t, got, "secret")
	assert.NotContains(t, got, "/private")

	res = Build(fixtureCLI(t, "beta"), &Request{}, Choice{Effort: "low"})
	assert.Equal(t, `beta exec -c "model_reasoning_effort=\"low\""`, res.Redacted())

	assert.Empty(t, Result{}.Redacted())
	assert.Equal(t, `"" "a b" "x\x01"`, Result{Argv: []string{"", "a b", "x\x01"}, promptAt: -1, rawAt: 3}.Redacted())
}
