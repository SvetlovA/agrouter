package args

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SvetlovA/agrouter/pkg/config"
)

// fixtureCLI supplies invocation descriptors for behavior tests independently of embedded defaults.
func fixtureCLI(t *testing.T, name string) config.CLI {
	t.Helper()
	switch name {
	case "alpha":
		return config.CLI{Name: name, Command: name,
			ExecArgs:   []string{"-p"},
			ModelArgs:  []string{"--model", "{model}"},
			EffortArgs: []string{"--effort", "{effort}"},
			PromptArgs: []string{"--", "{prompt}"},
		}
	case "beta":
		return config.CLI{Name: name, Command: name,
			ExecArgs:   []string{"exec"},
			ModelArgs:  []string{"--model", "{model}"},
			EffortArgs: []string{"-c", `model_reasoning_effort="{effort}"`},
			PromptArgs: []string{"--", "{prompt}"},
		}
	}
	t.Fatalf("unknown CLI fixture %q", name)
	return config.CLI{}
}

func TestBuild(t *testing.T) {
	alpha, beta := fixtureCLI(t, "alpha"), fixtureCLI(t, "beta")
	prompt := "fix \"it\" in 'a b'\nthen\t$HOME & %PATH%"
	tests := []struct {
		name   string
		cli    config.CLI
		req    Request
		choice Choice
		want   []string
	}{
		{name: "model and effort set", cli: alpha, req: Request{Prompt: Optional{Value: prompt, Set: true}},
			choice: Choice{Model: "everyday-model", Effort: "low"},
			want:   []string{"alpha", "-p", "--model", "everyday-model", "--effort", "low", "--", prompt}},
		{name: "model unset", cli: alpha, choice: Choice{Effort: "high"},
			want: []string{"alpha", "-p", "--effort", "high"}},
		{name: "effort unset", cli: alpha, choice: Choice{Model: "m"},
			want: []string{"alpha", "-p", "--model", "m"}},
		{name: "both unset: the child's defaults", cli: alpha, want: []string{"alpha", "-p"}},
		{name: "placeholder inside a token", cli: beta, choice: Choice{Effort: "high"},
			want: []string{"beta", "exec", "-c", `model_reasoning_effort="high"`}},
		{name: "native tokens before the prompt, unchanged", cli: beta,
			req:    Request{Prompt: Optional{Value: prompt, Set: true}, Raw: []string{"--search", "", "a b", "--search", "--", "x"}},
			choice: Choice{Model: "worker-model", Effort: "high"},
			want:   []string{"beta", "exec", "--search", "", "a b", "--search", "--model", "worker-model", "-c", `model_reasoning_effort="high"`, "--", "x", "--", prompt}},
		{name: "no prompt: prompt_args dropped, \"--\" included", cli: alpha, req: Request{Raw: []string{"--json"}},
			want: []string{"alpha", "-p", "--json"}},
		{name: "an empty prompt is never an empty token", cli: alpha, req: Request{Prompt: Optional{Set: true}},
			want: []string{"alpha", "-p"}},
		{name: "empty sources and the file path never reach argv", cli: alpha,
			req:  Request{PromptFlag: Optional{Set: true}, Prompt: Optional{Set: true}, PromptFile: Optional{Value: "f", Set: true}},
			want: []string{"alpha", "-p"}},
		{name: "empty exec_args", cli: config.CLI{Command: "gamma", ExecArgs: []string{}, PromptArgs: []string{"{prompt}"}},
			req: Request{Prompt: Optional{Value: "go", Set: true}}, want: []string{"gamma", "go"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Build(tc.cli, &tc.req, tc.choice).Argv)
		})
	}
}

// TestBuildPromptStaysLiteral pins that prompt text shaped like a flag reaches the child as the last
// argument, right after prompt_args' "--", from every prompt source and with native tokens.
func TestBuildPromptStaysLiteral(t *testing.T) {
	alpha := fixtureCLI(t, "alpha")
	for _, text := range []string{"--help", "--model", "-", "--", "-p\nsecond line"} {
		for name, req := range map[string]Request{
			"-p":            {PromptFlag: Optional{Value: text, Set: true}},
			"positional":    {Prompt: Optional{Value: text, Set: true}},
			"--prompt-file": {PromptFile: Optional{Value: "task.md", Set: true}, FileText: text},
		} {
			t.Run(name+" "+text, func(t *testing.T) {
				req.Raw = []string{"--add-dir", "x"}
				res := Build(alpha, &req, Choice{Model: "m", Effort: "high"})
				assert.Equal(t, []string{"alpha", "-p", "--add-dir", "x", "--model", "m", "--effort", "high", "--", text},
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

func TestBuildOwnFlagsNeverInArgv(t *testing.T) {
	req := &Request{CLI: "alpha", APIKey: Optional{Value: "sk-secret", Set: true}, Model: "opus",
		Docs: []string{"AGENTS.md"}}
	res := Build(fixtureCLI(t, "alpha"), req, Choice{Model: "large-model"})
	assert.Equal(t, []string{"alpha", "-p", "--model", "large-model"}, res.Argv)
	for _, tok := range res.Argv {
		assert.NotContains(t, tok, "sk-secret")
		assert.NotContains(t, tok, "--cli")
		assert.NotContains(t, tok, "--api-key")
		assert.NotContains(t, tok, "AGENTS.md")
	}
}

func TestBuildMadeUpCLI(t *testing.T) {
	const ini = `
[agrouter]
timeout         = 10s
routing_policy  = Prefer the cheapest option.
complexity_policy = Judge the whole codebase.
log_dir           = logs

[classifier]
url               = https://classifier.example.test/v1
model             = test-classifier
request_tokens    = 20000
bytes_per_token   = 3

[cli.zeta]
command     = zeta-agent
description = A made-up agent.
exec_args   = ["--headless"]
model_args  = ["--llm={model}"]
effort_args = ["--think", "{effort}"]
prompt_args = ["--task", "--", "{prompt}"]

[model.zeta-one]
cli     = zeta
name    = z1
efforts = low, high
`
	cfg, err := config.Load(config.Sources{Embedded: []byte(ini)})
	require.NoError(t, err)
	zeta, ok := cfg.CLIByName("zeta")
	require.True(t, ok)

	req := &Request{Prompt: Optional{Value: "do it", Set: true}, Raw: []string{"--emit", "ndjson"}}
	res := Build(zeta, req, Choice{Model: "z1", Effort: "high"})
	assert.Equal(t, []string{"zeta-agent", "--headless", "--emit", "ndjson", "--llm=z1", "--think", "high",
		"--task", "--", "do it"}, res.Argv)
}

func TestRedacted(t *testing.T) {
	alpha := fixtureCLI(t, "alpha")
	res := Build(alpha, &Request{Prompt: Optional{Value: "secret task text", Set: true}, Raw: []string{"--add-dir", "/private"}},
		Choice{Model: "large-model", Effort: "high"})
	got := res.Redacted()
	assert.Equal(t, "alpha -p <2 raw argument(s)> --model large-model --effort high -- <prompt>", got)
	assert.NotContains(t, got, "secret")
	assert.NotContains(t, got, "/private")

	res = Build(fixtureCLI(t, "beta"), &Request{}, Choice{Effort: "low"})
	assert.Equal(t, `beta exec -c "model_reasoning_effort=\"low\""`, res.Redacted())

	assert.Empty(t, Result{}.Redacted())
	assert.Equal(t, `"" "a b" "x\x01"`, Result{Argv: []string{"", "a b", "x\x01"}, promptAt: -1, rawAt: 3}.Redacted())
}

func TestBuildSelectionPrecedence(t *testing.T) {
	cli := fixtureCLI(t, "beta")
	raw := []string{"-m", "native-model", "--effort=native", "-c", `model_reasoning_effort="native"`, "--profile", "work"}
	req := Request{Raw: raw}
	res := Build(cli, &req, Choice{Model: "chosen-model", Effort: "chosen-effort"})
	assert.Equal(t, []string{"beta", "exec", "-c", `model_reasoning_effort="native"`, "--profile", "work",
		"--model", "chosen-model", "-c", `model_reasoning_effort="chosen-effort"`}, res.Argv)
	assert.Equal(t, raw, req.Raw)
	assert.Equal(t, []string{"beta", "exec", "--json"}, Build(cli,
		&Request{Raw: []string{"--model", "native", "--effort", "native", "--json"}}, Choice{}).Argv,
		"fallback uses the child's defaults instead of a native selection")
}

func TestRedactedNativeSeparator(t *testing.T) {
	res := Build(fixtureCLI(t, "alpha"), &Request{Raw: []string{"--private", "secret", "-mnative", "--", "--model", "secret-position"}},
		Choice{Model: "chosen", Effort: "chosen-effort"})
	assert.Equal(t, []string{"alpha", "-p", "--private", "secret", "--model", "chosen", "--effort", "chosen-effort",
		"--", "--model", "secret-position"}, res.Argv)
	assert.Equal(t, "alpha -p <2 raw argument(s)> --model chosen --effort chosen-effort <3 raw argument(s)>", res.Redacted())
	assert.NotContains(t, res.Redacted(), "secret")
	assert.NotContains(t, res.Redacted(), "native")
}
