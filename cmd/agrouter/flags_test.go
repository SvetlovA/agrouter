package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SvetlovA/agrouter/pkg/args"
)

func parseOK(t *testing.T, argv []string) *args.Request {
	t.Helper()
	cmd, err := parseArgs(argv)
	require.NoError(t, err)
	require.NotNil(t, cmd.req)
	return cmd.req
}

func positional(v string) args.Optional { return args.Optional{Value: v, Set: true} }

func TestParseArgs_FlagsAroundPositional(t *testing.T) {
	tests := []struct {
		name string
		argv []string
	}{
		{"prompt first, split form", []string{"fix it", "--model", "m", "--effort", "high", "--verbose"}},
		{"prompt last, = form", []string{"--model=m", "--effort=high", "--verbose", "fix it"}},
		{"prompt in the middle, mixed", []string{"--model", "m", "fix it", "--effort=high", "--verbose"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := parseOK(t, tc.argv)
			assert.Equal(t, args.ModeDecision, req.Mode)
			assert.Equal(t, positional("fix it"), req.Prompt)
			assert.Equal(t, "m", req.Model)
			assert.Equal(t, "high", req.Effort)
			assert.Empty(t, req.Raw)
		})
	}
}

func TestParseArgs_Mode(t *testing.T) {
	tests := []struct {
		name       string
		argv       []string
		wantMode   args.Mode
		wantCLI    string
		wantPrompt args.Optional
	}{
		{"decision, no prompt", []string{"--verbose"}, args.ModeDecision, "", args.Optional{}},
		{"decision with prompt", []string{"hello"}, args.ModeDecision, "", positional("hello")},
		{"exec CLI and prompt", []string{"exec", "tool-a", "hello"}, args.ModeExec, "tool-a", positional("hello")},
		{"exec CLI alone", []string{"exec", "tool-a"}, args.ModeExec, "tool-a", args.Optional{}},
		{"later exec is the positional", []string{"--verbose", "exec"}, args.ModeDecision, "", positional("exec")},
		{"exec as the CLI operand", []string{"exec", "exec", "x"}, args.ModeExec, "exec", positional("x")},
		{"empty positional is still a prompt", []string{""}, args.ModeDecision, "", positional("")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := parseOK(t, tc.argv)
			assert.Equal(t, tc.wantMode, req.Mode)
			assert.Equal(t, tc.wantCLI, req.CLI)
			assert.Equal(t, tc.wantPrompt, req.Prompt)
		})
	}
}

func TestParseArgs_ExecCLI(t *testing.T) {
	t.Run("redundant --cli accepted", func(t *testing.T) {
		req := parseOK(t, []string{"exec", "tool-a", "--cli=tool-a", "x"})
		assert.Equal(t, "tool-a", req.CLI)
	})
	t.Run("--cli in decision mode restricts routing", func(t *testing.T) {
		req := parseOK(t, []string{"--cli", "tool-a", "x"})
		assert.Equal(t, "tool-a", req.CLI)
		req = parseOK(t, []string{"--cli=tool-a", "--cli=", "x"})
		assert.Empty(t, req.CLI, "the last --cli wins, even empty")
	})

	errs := []struct {
		name    string
		argv    []string
		wantErr string
	}{
		{"missing operand", []string{"exec"}, "exec needs the CLI to run"},
		{"empty operand", []string{"exec", "", "x"}, "exec needs the CLI to run"},
		{"option instead of operand", []string{"exec", "-p", "x"}, "exec needs the CLI to run"},
		{"--cli cannot supply the operand", []string{"exec", "--cli=tool-a", "x"}, "exec needs the CLI to run"},
		{"separator instead of operand", []string{"exec", "--", "x"}, "exec needs the CLI to run"},
		{"conflicting --cli", []string{"exec", "tool-a", "--cli", "tool-b", "x"}, "--cli tool-b conflicts with exec tool-a"},
		{"empty --cli conflicts", []string{"exec", "tool-a", "--cli=", "x"}, "conflicts with exec tool-a"},
	}
	for _, tc := range errs {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseArgs(tc.argv)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.NotContains(t, err.Error(), "\n")
		})
	}
}

func TestParseArgs_OwnFlags(t *testing.T) {
	t.Run("--verbose before -- belongs only to agrouter", func(t *testing.T) {
		cmd, err := parseArgs([]string{"--verbose", "x"})
		require.NoError(t, err)
		assert.True(t, cmd.verbose)
		cmd, err = parseArgs([]string{"exec", "tool-a", "x", "--", "--verbose"})
		require.NoError(t, err)
		assert.False(t, cmd.verbose)
		assert.Equal(t, []string{"--verbose"}, cmd.req.Raw)
	})
	t.Run("--jev-api-key absent", func(t *testing.T) {
		req := parseOK(t, []string{"x"})
		assert.Equal(t, args.Optional{}, req.APIKey)
	})
	t.Run("--jev-api-key= clears", func(t *testing.T) {
		req := parseOK(t, []string{"--jev-api-key=", "x"})
		assert.Equal(t, args.Optional{Set: true}, req.APIKey)
	})
	t.Run("--jev-api-key split form", func(t *testing.T) {
		req := parseOK(t, []string{"--jev-api-key", "k-1", "x"})
		assert.Equal(t, args.Optional{Value: "k-1", Set: true}, req.APIKey)
	})
	t.Run("--model and --effort are constraints", func(t *testing.T) {
		req := parseOK(t, []string{"--model", "m-1", "--effort=high", "x"})
		assert.Equal(t, "m-1", req.Model)
		assert.Equal(t, "high", req.Effort)
	})
	t.Run("--help", func(t *testing.T) {
		cmd, err := parseArgs([]string{"--verbose", "--help"})
		require.NoError(t, err)
		assert.Nil(t, cmd.req)
		assert.Contains(t, cmd.help, "TYPESAFE_API_KEY")
		assert.Contains(t, cmd.help, "process")
		assert.Contains(t, cmd.help, "--cli=NAME")
		assert.Contains(t, cmd.help, "exec CLI")
		assert.Contains(t, cmd.help, "-p, --prompt=TEXT")
		assert.Contains(t, cmd.help, "--print=TEXT")
		assert.Contains(t, cmd.help, "--prompt-file=PATH")
		assert.Contains(t, cmd.help, "--doc=PATH")
		assert.NotContains(t, cmd.help, "/cli")
		assert.NotContains(t, cmd.help, "--config")
		assert.NotContains(t, cmd.help, "AGROUTER_CLI")
	})
	t.Run("--version", func(t *testing.T) {
		cmd, err := parseArgs([]string{"--version"})
		require.NoError(t, err)
		assert.True(t, cmd.version)
		assert.Nil(t, cmd.req)
	})
}

func TestParseArgs_PrintIsPromptAlias(t *testing.T) {
	for _, mode := range [][]string{nil, {"exec", "tool-a"}} {
		for _, spelling := range [][]string{{"--print", "fix it"}, {"--print=fix it"}} {
			want := parseOK(t, append(slices.Clone(mode), "-p", "fix it"))
			got := parseOK(t, append(slices.Clone(mode), spelling...))
			assert.Equal(t, want, got, spelling)
			assert.Equal(t, positional("fix it"), got.PromptFlag)
			assert.False(t, got.Prompt.Set)
		}
	}
	t.Run("--print after -- goes to the child", func(t *testing.T) {
		req := parseOK(t, []string{"exec", "tool-a", "-p", "x", "--", "--print"})
		assert.Equal(t, positional("x"), req.PromptFlag)
		assert.Equal(t, []string{"--print"}, req.Raw)
	})
}

func TestParseArgs_PromptSources(t *testing.T) {
	tests := []struct {
		name              string
		argv              []string
		wantFlag, wantPos args.Optional
		wantFile          args.Optional
	}{
		{name: "-p alone", argv: []string{"-p", "fix it"}, wantFlag: positional("fix it")},
		{name: "--prompt = form", argv: []string{"--prompt=fix it"}, wantFlag: positional("fix it")},
		{name: "-p attached", argv: []string{"-pfix"}, wantFlag: positional("fix")},
		{name: "empty -p is set", argv: []string{"-p", ""}, wantFlag: positional("")},
		{name: "--prompt-file alone", argv: []string{"--prompt-file", "task.md"}, wantFile: positional("task.md")},
		{name: "every source", argv: []string{"--prompt-file=task.md", "positional", "-p", "flag", "--verbose"},
			wantFlag: positional("flag"), wantPos: positional("positional"), wantFile: positional("task.md")},
		{name: "slash values survive", argv: []string{"-p", "/review", "--prompt-file", "/tmp/task.md"},
			wantFlag: positional("/review"), wantFile: positional("/tmp/task.md")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := parseOK(t, tc.argv)
			assert.Equal(t, tc.wantFlag, req.PromptFlag)
			assert.Equal(t, tc.wantPos, req.Prompt)
			assert.Equal(t, tc.wantFile, req.PromptFile)
			assert.Empty(t, req.FileText, "the file is read by the app, not the parser")
		})
	}
}

func TestParseArgs_Docs(t *testing.T) {
	tests := []struct {
		name string
		argv []string
		want []string
	}{
		{name: "none", argv: []string{"fix it"}},
		{name: "one", argv: []string{"--doc", "CLAUDE.md", "fix it"}, want: []string{"CLAUDE.md"}},
		{name: "repeated, order kept", argv: []string{"--doc=b.md", "fix it", "--doc", "a.md", "--doc", "b.md"},
			want: []string{"b.md", "a.md", "b.md"}},
		{name: "slash path survives", argv: []string{"--doc", "/repo/AGENTS.md", "fix it"}, want: []string{"/repo/AGENTS.md"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := parseOK(t, tc.argv)
			assert.Equal(t, tc.want, req.Docs)
			assert.Equal(t, positional("fix it"), req.Prompt)
		})
	}
}

func TestParseArgs_Raw(t *testing.T) {
	tests := []struct {
		name string
		argv []string
		want []string
	}{
		{"tokens kept apart from the positional, later -- kept",
			[]string{"exec", "tool-a", "fix it", "--", "--add-dir", "../x", "--", "extra"},
			[]string{"--add-dir", "../x", "--", "extra"}},
		{"a positional-looking token is not a second positional", []string{"exec", "tool-a", "fix it", "--", "other"},
			[]string{"other"}},
		{"-- with nothing after", []string{"exec", "tool-a", "x", "--"}, []string{}},
		{"empty tokens, spaces and repeats kept", []string{"exec", "tool-a", "x", "--", "", "a b", "-v", "-v", "/abs"},
			[]string{"", "a b", "-v", "-v", "/abs"}},
		{"prompt text after -- is not a prompt", []string{"exec", "tool-a", "--", "fix it"}, []string{"fix it"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := parseOK(t, tc.argv)
			assert.Equal(t, tc.want, req.Raw)
			if tc.name == "prompt text after -- is not a prompt" {
				assert.False(t, req.Prompt.Set)
				assert.False(t, req.PromptFlag.Set)
			} else {
				assert.True(t, req.Prompt.Set)
			}
		})
	}
}

func TestParseArgs_SlashTokens(t *testing.T) {
	// go-flags on Windows takes "/x" as an option; prompts and values starting with "/" must survive
	req := parseOK(t, []string{"--model", "/models/m", "--doc=/weird", "/review the diff"})
	assert.Equal(t, positional("/review the diff"), req.Prompt)
	assert.Equal(t, "/models/m", req.Model)
	assert.Equal(t, []string{"/weird"}, req.Docs)
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
		{"second positional around flags", []string{"exec", "tool-a", "one", "--verbose", "two"}, "more than one positional"},
		{"missing value", []string{"x", "--model"}, "model"},
		{"second -p", []string{"-p", "a", "--prompt", "b"}, "-p/--prompt/--print given more than once"},
		{"-p and --print", []string{"-p", "a", "--print", "b"}, "-p/--prompt/--print given more than once"},
		{"bare --print", []string{"x", "--print"}, "print"},
		{"bare --print before --", []string{"exec", "tool-a", "x", "--print", "--", "y"}, "print"},
		{"-- in decision mode", []string{"x", "--", "--add-dir"}, `"--" is only accepted with exec`},
		{"empty -- in decision mode", []string{"x", "--"}, `"--" is only accepted with exec`},
		{"removed --permission-mode", []string{"--permission-mode", "plan", "x"}, "permission-mode"},
		{"removed --dangerously-skip-permissions", []string{"--dangerously-skip-permissions", "x"}, "dangerously-skip-permissions"},
		{"removed --dangerously-bypass-approvals-and-sandbox", []string{"exec", "tool-a",
			"--dangerously-bypass-approvals-and-sandbox", "x"}, "dangerously-bypass-approvals-and-sandbox"},
		{"removed --output-format", []string{"--output-format=json", "x"}, "output-format"},
		{"removed --sandbox", []string{"--sandbox", "read-only", "x"}, "sandbox"},
		{"removed -c", []string{"exec", "tool-a", "-c", "k=v", "x"}, "unknown flag"},
		{"removed --config", []string{"--config=k=v", "x"}, "config"},
		{"second --prompt-file", []string{"--prompt-file", "a", "--prompt-file=b"}, "--prompt-file given more than once"},
		{"-p value looks like a flag", []string{"-p", "--model", "x"}, "expected argument"},
		{"-p without value", []string{"x", "-p"}, "prompt"},
		{"--prompt-file without value", []string{"x", "--prompt-file"}, "prompt-file"},
		{"--doc without value", []string{"x", "--doc"}, "doc"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseArgs(tc.argv)
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

func TestPosixHelp(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"value option", "      /cli:NAME      restrict", "      --cli=NAME     restrict"},
		{"short and long", "  /p, /prompt:TEXT        the", "  -p, --prompt=TEXT       the"},
		{"long flag", "      /verbose       verbose", "      --verbose      verbose"},
		{"windows-only help line dropped", "x\n  /?                 Show\ny", "x\ny"},
		{"description continuation kept", "                     CLI (env)", "                     CLI (env)"},
		{"prose untouched", "a local .agrouter/config", "a local .agrouter/config"},
		{"posix help untouched", "      --cli=NAME     restrict", "      --cli=NAME     restrict"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, posixHelp(tt.in))
		})
	}
}
