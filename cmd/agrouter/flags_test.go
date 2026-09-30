package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SvetlovA/agrouter/pkg/args"
)

func noEnv(string) string { return "" }

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func parseOK(t *testing.T, argv []string, getenv func(string) string) *args.Request {
	t.Helper()
	cmd, err := parseArgs(argv, getenv)
	require.NoError(t, err)
	require.NotNil(t, cmd.req)
	return cmd.req
}

func prompt(v string) args.Optional { return args.Optional{Value: v, Set: true} }

func TestParseArgs_FlagsAroundPositional(t *testing.T) {
	want := []args.Arg{
		{Spelling: "--output-format json", Key: "output-format.json"},
		{Spelling: "--permission-mode plan", Key: "permission-mode.plan"},
		{Spelling: "--verbose", Key: "verbose"},
	}
	tests := []struct {
		name string
		argv []string
	}{
		{"prompt first, split form", []string{"fix it", "--output-format", "json", "--permission-mode", "plan", "--verbose"}},
		{"prompt last, = form", []string{"--output-format=json", "--permission-mode=plan", "--verbose", "fix it"}},
		{"prompt in the middle, mixed", []string{"--output-format", "json", "fix it", "--permission-mode=plan", "--verbose"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := parseOK(t, tc.argv, noEnv)
			assert.Equal(t, args.ModeDecision, req.Mode)
			assert.Equal(t, prompt("fix it"), req.Prompt)
			assert.Equal(t, want, req.Args)
			assert.Empty(t, req.Raw)
		})
	}
}

func TestParseArgs_Mode(t *testing.T) {
	tests := []struct {
		name       string
		argv       []string
		wantMode   args.Mode
		wantPrompt args.Optional
	}{
		{"decision, no prompt", []string{"--verbose"}, args.ModeDecision, args.Optional{}},
		{"decision with prompt", []string{"hello"}, args.ModeDecision, prompt("hello")},
		{"exec first", []string{"exec", "hello"}, args.ModeExec, prompt("hello")},
		{"exec alone", []string{"exec"}, args.ModeExec, args.Optional{}},
		{"later exec is the positional", []string{"--verbose", "exec"}, args.ModeDecision, prompt("exec")},
		{"exec then exec as the prompt", []string{"exec", "exec"}, args.ModeExec, prompt("exec")},
		{"empty positional is still a prompt", []string{""}, args.ModeDecision, prompt("")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := parseOK(t, tc.argv, noEnv)
			assert.Equal(t, tc.wantMode, req.Mode)
			assert.Equal(t, tc.wantPrompt, req.Prompt)
		})
	}
}

func TestParseArgs_OwnFlags(t *testing.T) {
	t.Run("-p and --print accepted, never mapped", func(t *testing.T) {
		for _, f := range []string{"-p", "--print"} {
			req := parseOK(t, []string{f, "x"}, noEnv)
			assert.Empty(t, req.Args)
			assert.Equal(t, prompt("x"), req.Prompt)
		}
	})
	t.Run("AGROUTER_CLI used without --cli", func(t *testing.T) {
		req := parseOK(t, []string{"x"}, envOf(map[string]string{cliEnv: "codex"}))
		assert.Equal(t, "codex", req.CLI)
	})
	t.Run("--cli beats AGROUTER_CLI", func(t *testing.T) {
		req := parseOK(t, []string{"--cli=claude", "x"}, envOf(map[string]string{cliEnv: "codex"}))
		assert.Equal(t, "claude", req.CLI)
	})
	t.Run("explicit empty --cli beats AGROUTER_CLI", func(t *testing.T) {
		req := parseOK(t, []string{"--cli=", "x"}, envOf(map[string]string{cliEnv: "codex"}))
		assert.Empty(t, req.CLI)
	})
	t.Run("--jev-api-key absent", func(t *testing.T) {
		req := parseOK(t, []string{"x"}, noEnv)
		assert.Equal(t, args.Optional{}, req.APIKey)
	})
	t.Run("--jev-api-key= clears", func(t *testing.T) {
		req := parseOK(t, []string{"--jev-api-key=", "x"}, noEnv)
		assert.Equal(t, args.Optional{Set: true}, req.APIKey)
	})
	t.Run("--jev-api-key split form", func(t *testing.T) {
		req := parseOK(t, []string{"--jev-api-key", "k-1", "x"}, noEnv)
		assert.Equal(t, args.Optional{Value: "k-1", Set: true}, req.APIKey)
		assert.Empty(t, req.Args)
	})
	t.Run("--model and --effort are constraints, not mapped args", func(t *testing.T) {
		req := parseOK(t, []string{"--model", "opus", "--effort=high", "x"}, noEnv)
		assert.Equal(t, "opus", req.Model)
		assert.Equal(t, args.SourceFlag, req.ModelSource)
		assert.Equal(t, "high", req.Effort)
		assert.Equal(t, args.SourceFlag, req.EffortSource)
		assert.Empty(t, req.Args)
	})
	t.Run("--help", func(t *testing.T) {
		cmd, err := parseArgs([]string{"--verbose", "--help"}, noEnv)
		require.NoError(t, err)
		assert.Nil(t, cmd.req)
		assert.Contains(t, cmd.help, "TYPESAFE_API_KEY")
		assert.Contains(t, cmd.help, "process")
	})
	t.Run("--version", func(t *testing.T) {
		cmd, err := parseArgs([]string{"--version"}, noEnv)
		require.NoError(t, err)
		assert.True(t, cmd.version)
		assert.Nil(t, cmd.req)
	})
}

func TestParseArgs_Config(t *testing.T) {
	t.Run("value containing = kept whole", func(t *testing.T) {
		req := parseOK(t, []string{"-c", `project_doc="a=b.md"`, "x"}, noEnv)
		assert.Equal(t, []args.Arg{{
			Spelling: `-c project_doc="a=b.md"`, Key: "config.project_doc", Value: `project_doc="a=b.md"`,
		}}, req.Args)
	})
	t.Run("--config long form and no =", func(t *testing.T) {
		req := parseOK(t, []string{"--config=approval_policy=never", "-c", "bare", "x"}, noEnv)
		assert.Equal(t, []args.Arg{
			{Spelling: "-c approval_policy=never", Key: "config.approval_policy", Value: "approval_policy=never"},
			{Spelling: "-c bare", Key: "config.bare", Value: "bare"},
		}, req.Args)
	})
	t.Run("-c model and effort become constraints, TOML quotes removed", func(t *testing.T) {
		req := parseOK(t, []string{"-c", `model="gpt-6-sol"`, "-c", "model_reasoning_effort=high", "x"}, noEnv)
		assert.Equal(t, "gpt-6-sol", req.Model)
		assert.Equal(t, args.SourceConfig, req.ModelSource)
		assert.Equal(t, "high", req.Effort)
		assert.Equal(t, args.SourceConfig, req.EffortSource)
		assert.Empty(t, req.Args)
	})
	t.Run("literal TOML quotes and last one wins", func(t *testing.T) {
		req := parseOK(t, []string{"-c", "model='a'", "-c", "model=b", "x"}, noEnv)
		assert.Equal(t, "b", req.Model)
	})
	t.Run("-c model beside --model stays a config.model key", func(t *testing.T) {
		req := parseOK(t, []string{"-c", `model="gpt-6"`, "--model", "gpt-6-sol",
			"-c", "model_reasoning_effort=low", "--effort", "high", "x"}, noEnv)
		assert.Equal(t, "gpt-6-sol", req.Model)
		assert.Equal(t, args.SourceFlag, req.ModelSource)
		assert.Equal(t, "high", req.Effort)
		assert.Equal(t, args.SourceFlag, req.EffortSource)
		assert.Equal(t, []args.Arg{
			{Spelling: `-c model="gpt-6"`, Key: "config.model", Value: `model="gpt-6"`},
			{Spelling: "-c model_reasoning_effort=low", Key: "config.model_reasoning_effort", Value: "model_reasoning_effort=low"},
		}, req.Args)
	})
}

func TestParseArgs_BypassAliasesShareKey(t *testing.T) {
	req := parseOK(t, []string{"--dangerously-skip-permissions", "--dangerously-bypass-approvals-and-sandbox",
		"--permission-mode", "bypassPermissions", "x"}, noEnv)
	require.Len(t, req.Args, 3)
	for _, a := range req.Args {
		assert.Equal(t, "permission-mode.bypassPermissions", a.Key)
	}
	assert.Equal(t, "--dangerously-bypass-approvals-and-sandbox", req.Args[1].Spelling)
}

func TestParseArgs_RalphexClaude(t *testing.T) {
	// claude_args, then ralphex's own --model/--effort and --print, prompt on stdin
	argv := []string{"exec", "--cli=claude", "--dangerously-skip-permissions", "--output-format", "stream-json",
		"--verbose", "--model", "opus", "--effort", "high", "--print"}
	req := parseOK(t, argv, noEnv)
	assert.Equal(t, args.ModeExec, req.Mode)
	assert.Equal(t, "claude", req.CLI)
	assert.Equal(t, "opus", req.Model)
	assert.Equal(t, "high", req.Effort)
	assert.False(t, req.Prompt.Set)
	assert.Equal(t, []args.Arg{
		{Spelling: "--dangerously-skip-permissions", Key: "permission-mode.bypassPermissions"},
		{Spelling: "--output-format stream-json", Key: "output-format.stream-json"},
		{Spelling: "--verbose", Key: "verbose"},
	}, req.Args)
}

func TestParseArgs_RalphexCodex(t *testing.T) {
	desc := `agents.reviewer.description="general code review specialist; behavior driven by the task argument"`
	argv := []string{"exec",
		"-c", "features.multi_agent=true",
		"-c", desc,
		"-c", `project_doc_fallback_filenames=["CLAUDE.md"]`,
		"--dangerously-bypass-approvals-and-sandbox",
		"--sandbox", "danger-full-access",
		"-c", `model="gpt-6-sol"`,
		"-c", "model_reasoning_effort=xhigh",
		"-c", "stream_idle_timeout_ms=3600000",
		"-c", `project_doc="/tmp/doc.md"`,
	}
	req := parseOK(t, argv, noEnv)
	assert.Equal(t, args.ModeExec, req.Mode)
	assert.Empty(t, req.CLI)
	assert.False(t, req.Prompt.Set)
	assert.Equal(t, "gpt-6-sol", req.Model)
	assert.Equal(t, args.SourceConfig, req.ModelSource)
	assert.Equal(t, "xhigh", req.Effort)
	assert.Equal(t, args.SourceConfig, req.EffortSource)
	assert.Equal(t, []args.Arg{
		{Spelling: "-c features.multi_agent=true", Key: "config.features.multi_agent", Value: "features.multi_agent=true"},
		{Spelling: "-c " + desc, Key: "config.agents.reviewer.description", Value: desc},
		{Spelling: `-c project_doc_fallback_filenames=["CLAUDE.md"]`, Key: "config.project_doc_fallback_filenames",
			Value: `project_doc_fallback_filenames=["CLAUDE.md"]`},
		{Spelling: "--dangerously-bypass-approvals-and-sandbox", Key: "permission-mode.bypassPermissions"},
		{Spelling: "--sandbox danger-full-access", Key: "sandbox.danger-full-access"},
		{Spelling: "-c stream_idle_timeout_ms=3600000", Key: "config.stream_idle_timeout_ms", Value: "stream_idle_timeout_ms=3600000"},
		{Spelling: `-c project_doc="/tmp/doc.md"`, Key: "config.project_doc", Value: `project_doc="/tmp/doc.md"`},
	}, req.Args)
}

func TestParseArgs_Raw(t *testing.T) {
	t.Run("raw tokens kept apart from the positional", func(t *testing.T) {
		req := parseOK(t, []string{"--cli", "claude", "fix it", "--", "--add-dir", "../x", "--", "extra"}, noEnv)
		assert.Equal(t, prompt("fix it"), req.Prompt)
		assert.Equal(t, []string{"--add-dir", "../x", "--", "extra"}, req.Raw)
		assert.Empty(t, req.Args)
	})
	t.Run("a positional-looking raw token is not a second positional", func(t *testing.T) {
		req := parseOK(t, []string{"fix it", "--", "other"}, noEnv)
		assert.Equal(t, prompt("fix it"), req.Prompt)
		assert.Equal(t, []string{"other"}, req.Raw)
	})
	t.Run("-- with nothing after", func(t *testing.T) {
		req := parseOK(t, []string{"x", "--"}, noEnv)
		assert.Empty(t, req.Raw)
	})
}

func TestParseArgs_SlashTokens(t *testing.T) {
	// go-flags on Windows takes "/x" as an option; prompts and values starting with "/" must survive
	req := parseOK(t, []string{"--model", "/models/m", "--sandbox=/weird", "/review the diff"}, noEnv)
	assert.Equal(t, prompt("/review the diff"), req.Prompt)
	assert.Equal(t, "/models/m", req.Model)
	assert.Equal(t, []args.Arg{{Spelling: "--sandbox /weird", Key: "sandbox./weird"}}, req.Args)
}

func TestParseArgs_Errors(t *testing.T) {
	tests := []struct {
		name    string
		argv    []string
		wantErr string
	}{
		{"unknown long flag", []string{"--no-such-flag", "x"}, "no-such-flag"},
		{"unknown short flag", []string{"-z", "x"}, "z"},
		{"prompt-looking flag", []string{"-fix it"}, "unknown flag"},
		{"second positional", []string{"one", "two"}, "more than one positional"},
		{"second positional around flags", []string{"exec", "one", "--verbose", "two"}, "more than one positional"},
		{"missing value", []string{"x", "--model"}, "model"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseArgs(tc.argv, noEnv)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.NotContains(t, err.Error(), "\n")
			assert.NotContains(t, err.Error(), slashEscape)
		})
	}
}

func TestRun_SecondPositional(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"one", "two"}, strings.NewReader(""), &stdout, &stderr)

	assert.Equal(t, 2, code)
	assert.Empty(t, stdout.String())
	assert.Equal(t, "agrouter: more than one positional argument: pass the prompt as one quoted argument\n", stderr.String())
}

func TestUnquoteTOML(t *testing.T) {
	tests := map[string]string{
		`"a"`:     "a",
		`'a'`:     "a",
		`"a\"b"`:  `a"b`,
		`"a\qb"`:  `a\qb`,
		`a`:       "a",
		`"`:       `"`,
		`""`:      "",
		`"a'`:     `"a'`,
		`gpt-6"x`: `gpt-6"x`,
	}
	for in, want := range tests {
		assert.Equal(t, want, unquoteTOML(in), in)
	}
}
