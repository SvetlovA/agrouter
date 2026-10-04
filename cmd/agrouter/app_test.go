package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SvetlovA/agrouter/cmd/agrouter/mocks"
	"github.com/SvetlovA/agrouter/pkg/config"
	"github.com/SvetlovA/agrouter/pkg/config/defaults"
	"github.com/SvetlovA/agrouter/pkg/jev"
	"github.com/SvetlovA/agrouter/pkg/router"
	"github.com/SvetlovA/agrouter/pkg/runner"
)

// the test binary doubles as the fake CLI: with GO_WANT_HELPER_PROCESS=1 it records its argv, stdin
// and environment to $AGROUTER_HELPER_OUT and exits $AGROUTER_HELPER_EXIT.
const (
	helperEnv  = "GO_WANT_HELPER_PROCESS"
	helperOut  = "AGROUTER_HELPER_OUT"
	helperExit = "AGROUTER_HELPER_EXIT"
	testKey    = "test-jev-key"
)

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		os.Exit(helper())
	}
	os.Exit(m.Run())
}

func helper() int {
	out := os.Getenv(helperOut)
	argv, _ := json.Marshal(os.Args[1:])
	stdin, _ := io.ReadAll(os.Stdin)
	env, _ := json.Marshal(os.Environ())
	_ = os.WriteFile(filepath.Join(out, "argv.json"), argv, 0o600)
	_ = os.WriteFile(filepath.Join(out, "stdin.bin"), stdin, 0o600)
	_ = os.WriteFile(filepath.Join(out, "env.json"), env, 0o600)
	code, _ := strconv.Atoi(os.Getenv(helperExit))
	return code
}

// fakeJev answers every Choice with pick (which must be among the options sent) and every Noul with
// 0.5, and records each request's state.
type fakeJev struct {
	t      *testing.T
	mu     sync.Mutex
	pick   string
	states []string
	srv    *httptest.Server
}

func newFakeJev(t *testing.T) *fakeJev {
	f := &fakeJev{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeJev) handle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model     string          `json:"model"`
		State     json.RawMessage `json:"state"`
		Questions map[string]struct {
			Type     string                     `json:"type"`
			Criteria map[string]json.RawMessage `json:"criteria"`
		} `json:"questions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		f.t.Errorf("fake jev: decode request: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.states = append(f.states, string(req.State))
	pick := f.pick
	f.mu.Unlock()

	answers := map[string]any{}
	for id, q := range req.Questions {
		if q.Type == jev.TypeNoul {
			answers[id] = map[string]any{"type": q.Type, "noul": 0.5}
			continue
		}
		probs := map[string]float64{}
		for name := range q.Criteria {
			probs[name] = 0
		}
		if _, ok := probs[pick]; !ok {
			f.t.Errorf("fake jev: pick %q is not among the options sent", pick)
		}
		probs[pick] = 1
		answers[id] = map[string]any{"type": q.Type, "choice": pick, "probabilities": probs, "confidence": 0.9}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"model": req.Model, "answers": answers})
}

func (f *fakeJev) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.states)
}

// env is an isolated agrouter environment: its own global config dir, working directory, fake Jev
// and helper output directory.
type env struct {
	t         *testing.T
	configDir string
	workDir   string
	out       string
	jev       *fakeJev
}

func newEnv(t *testing.T) *env {
	e := &env{t: t, configDir: t.TempDir(), workDir: t.TempDir(), out: t.TempDir(), jev: newFakeJev(t)}
	t.Setenv(config.EnvConfigDir, e.configDir)
	t.Setenv(config.EnvAPIKey, testKey)
	t.Setenv(cliEnv, "")
	t.Setenv(envDebug, "")
	t.Setenv(helperEnv, "1")
	t.Setenv(helperOut, e.out)
	t.Setenv(helperExit, "0")
	return e
}

// helperCommand is the fake CLI's executable.
func helperCommand(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	return exe
}

// globalConfig writes the global config file.
func (e *env) globalConfig(content string) {
	e.t.Helper()
	require.NoError(e.t, os.WriteFile(filepath.Join(e.configDir, "config"), []byte(content), 0o600))
}

// fakeCommands points the shipped CLIs at the helper, keeping their mappings.
func (e *env) fakeCommands() {
	exe := helperCommand(e.t)
	e.globalConfig("[cli.claude]\ncommand = " + exe + "\n[cli.codex]\ncommand = " + exe + "\n")
}

type result struct {
	code           int
	stdout, stderr string
}

func (e *env) run(argv []string, stdin io.Reader) result {
	return e.runWithRunner(argv, stdin, runner.Runner{})
}

func (e *env) runWithRunner(argv []string, stdin io.Reader, child CommandRunner) result {
	var stdout, stderr bytes.Buffer
	a := &app{
		stdin:    stdin,
		stdout:   &stdout,
		stderr:   &stderr,
		getenv:   os.Getenv,
		workDir:  e.workDir,
		embedded: defaults.Config,
		newJev: func(key string) router.JevClient {
			c := jev.New(key)
			c.URL = e.jev.srv.URL
			return c
		},
		runner: child,
	}
	code := a.run(argv)
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// child reads what the helper recorded.
func (e *env) child() (argv []string, stdin []byte, environ []string) {
	e.t.Helper()
	data, err := os.ReadFile(filepath.Join(e.out, "argv.json"))
	require.NoError(e.t, err, "the child did not run")
	require.NoError(e.t, json.Unmarshal(data, &argv))
	stdin, err = os.ReadFile(filepath.Join(e.out, "stdin.bin"))
	require.NoError(e.t, err)
	data, err = os.ReadFile(filepath.Join(e.out, "env.json"))
	require.NoError(e.t, err)
	require.NoError(e.t, json.Unmarshal(data, &environ))
	return argv, stdin, environ
}

// decision decodes the one decision-mode JSON line.
func decision(t *testing.T, stdout string) map[string]any {
	t.Helper()
	require.True(t, strings.HasSuffix(stdout, "\n"), stdout)
	require.Equal(t, 1, strings.Count(stdout, "\n"), stdout)
	var d map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &d))
	return d
}

func strs(v ...string) []any {
	out := make([]any, len(v))
	for i, s := range v {
		out[i] = s
	}
	return out
}

func TestApp_RalphexClaudeMode(t *testing.T) {
	const task = "implement task 3\nof the plan\n"

	t.Run("model and effort fixed: no Jev call", func(t *testing.T) {
		e := newEnv(t)
		e.fakeCommands()
		r := e.run([]string{"exec", "--cli=claude", "--dangerously-skip-permissions", "--output-format", "stream-json",
			"--verbose", "--model", "opus", "--effort", "high", "--print"}, strings.NewReader(task))

		require.Equal(t, 0, r.code, r.stderr)
		assert.Equal(t, map[string]any{"cli": "claude", "model": "claude-opus-5-5", "effort": "high"}, decision(t, r.stderr))
		assert.Empty(t, r.stdout)
		argv, stdin, _ := e.child()
		assert.Equal(t, []string{"-p", "--dangerously-skip-permissions", "--output-format", "stream-json", "--verbose",
			"--model", "claude-opus-5-5", "--effort", "high"}, argv)
		assert.Equal(t, task, string(stdin))
		assert.Empty(t, e.jev.requests())
	})

	t.Run("Jev chooses", func(t *testing.T) {
		e := newEnv(t)
		e.fakeCommands()
		e.jev.pick = "claude-sonnet-5-5@medium"
		r := e.run([]string{"exec", "--cli=claude", "--dangerously-skip-permissions", "--output-format", "stream-json",
			"--verbose", "--print"}, strings.NewReader(task))

		require.Equal(t, 0, r.code, r.stderr)
		assert.Equal(t, map[string]any{"cli": "claude", "model": "claude-sonnet-5-5", "effort": "medium"}, decision(t, r.stderr))
		argv, stdin, environ := e.child()
		assert.Equal(t, []string{"-p", "--dangerously-skip-permissions", "--output-format", "stream-json", "--verbose",
			"--model", "claude-sonnet-5-5", "--effort", "medium"}, argv)
		assert.Equal(t, task, string(stdin))
		assert.Equal(t, []string{`{"prompt":"implement task 3\nof the plan\n"}`}, e.jev.requests())
		for _, kv := range environ {
			assert.False(t, strings.HasPrefix(strings.ToUpper(kv), config.EnvAPIKey+"="), "API key reached the child")
		}
	})
}

func TestApp_RalphexCodexMode(t *testing.T) {
	const task = "review the diff"
	desc := `agents.reviewer.description="general code review specialist; behavior driven by the task argument"`

	t.Run("executor", func(t *testing.T) {
		e := newEnv(t)
		e.fakeCommands()
		e.jev.pick = "gpt-6.1-sol@medium"
		r := e.run([]string{"exec",
			"-c", "features.multi_agent=true",
			"-c", desc,
			"-c", `project_doc_fallback_filenames=["CLAUDE.md"]`,
			"--dangerously-bypass-approvals-and-sandbox",
			"--sandbox", "danger-full-access",
			"-c", "stream_idle_timeout_ms=3600000",
			"-c", `project_doc="/tmp/doc.md"`,
		}, strings.NewReader(task))

		require.Equal(t, 0, r.code, r.stderr)
		assert.Equal(t, map[string]any{"cli": "codex", "model": "gpt-6.1-sol", "effort": "medium"}, decision(t, r.stderr))
		argv, stdin, _ := e.child()
		assert.Equal(t, []string{"exec",
			"-c", "features.multi_agent=true",
			"-c", desc,
			"-c", `project_doc_fallback_filenames=["CLAUDE.md"]`,
			"--dangerously-bypass-approvals-and-sandbox", "--skip-git-repo-check",
			"--sandbox", "danger-full-access",
			"-c", "stream_idle_timeout_ms=3600000",
			"-c", `project_doc="/tmp/doc.md"`,
			"--model", "gpt-6.1-sol", "-c", `model_reasoning_effort="medium"`,
		}, argv)
		assert.Equal(t, task, string(stdin))
	})

	t.Run("model constraints from -c: no Jev call", func(t *testing.T) {
		e := newEnv(t)
		e.fakeCommands()
		r := e.run([]string{"exec", "-c", `model="gpt-6.1-sol"`, "-c", "model_reasoning_effort=xhigh",
			"-c", "stream_idle_timeout_ms=3600000", "--sandbox", "workspace-write"}, strings.NewReader(task))

		require.Equal(t, 0, r.code, r.stderr)
		assert.Equal(t, map[string]any{"cli": "codex", "model": "gpt-6.1-sol", "effort": "xhigh"}, decision(t, r.stderr))
		argv, _, _ := e.child()
		assert.Equal(t, []string{"exec", "-c", "stream_idle_timeout_ms=3600000", "--sandbox", "workspace-write",
			"--model", "gpt-6.1-sol", "-c", `model_reasoning_effort="xhigh"`}, argv)
		assert.Empty(t, e.jev.requests())
	})

	t.Run("external review", func(t *testing.T) {
		e := newEnv(t)
		e.fakeCommands()
		e.jev.pick = "gpt-6-astra@high"
		r := e.run([]string{"exec", "-c", "stream_idle_timeout_ms=3600000", "--sandbox", "read-only"},
			strings.NewReader(task))

		require.Equal(t, 0, r.code, r.stderr)
		assert.Equal(t, map[string]any{"cli": "codex", "model": "gpt-6-astra", "effort": "high"}, decision(t, r.stderr))
		argv, stdin, _ := e.child()
		assert.Equal(t, []string{"exec", "-c", "stream_idle_timeout_ms=3600000", "--sandbox", "read-only",
			"--model", "gpt-6-astra", "-c", `model_reasoning_effort="high"`}, argv)
		assert.Equal(t, task, string(stdin))
	})
}

func TestApp_Decision(t *testing.T) {
	t.Run("prompt only", func(t *testing.T) {
		e := newEnv(t)
		e.jev.pick = "claude-sonnet-5-5@low"
		r := e.run([]string{"fix the typo in README"}, nil)

		require.Equal(t, 0, r.code, r.stderr)
		assert.Empty(t, r.stderr)
		assert.Equal(t, map[string]any{
			"cli": "claude", "model": "claude-sonnet-5-5", "effort": "low",
			"argv":    strs("claude", "-p", "fix the typo in README", "--model", "claude-sonnet-5-5", "--effort", "low"),
			"skipped": []any{},
		}, decision(t, r.stdout))
		assert.Equal(t, []string{`{"prompt":"fix the typo in README"}`}, e.jev.requests())
	})

	t.Run("mentioned file sent to Jev", func(t *testing.T) {
		e := newEnv(t)
		require.NoError(t, os.WriteFile(filepath.Join(e.workDir, "notes.md"), []byte("use opus\n"), 0o600))
		e.jev.pick = "claude-sonnet-5-5@low"
		r := e.run([]string{"summarize notes.md"}, nil)

		require.Equal(t, 0, r.code, r.stderr)
		assert.Equal(t, []string{`{"prompt":"summarize notes.md","files":["use opus\n"]}`}, e.jev.requests())
	})

	t.Run("model without efforts: effort null", func(t *testing.T) {
		e := newEnv(t)
		e.jev.pick = "claude-haiku-4-5"
		r := e.run([]string{"-p", "rename x to y"}, nil)

		require.Equal(t, 0, r.code, r.stderr)
		d := decision(t, r.stdout)
		assert.Equal(t, "claude-haiku-4-5", d["model"])
		assert.Nil(t, d["effort"])
		assert.Contains(t, r.stdout, `"effort":null`)
	})

	t.Run("cannot decide with the CLI known: null model and effort", func(t *testing.T) {
		e := newEnv(t)
		r := e.run([]string{"--cli=claude", "--jev-api-key=", "--output-format=json", "fix it"}, nil)

		require.Equal(t, 0, r.code, r.stderr)
		assert.Empty(t, r.stderr)
		decision(t, r.stdout)
		assert.JSONEq(t, `{"cli":"claude","model":null,"effort":null,"argv":["claude","-p","fix it","--output-format","json"],"skipped":[]}`,
			r.stdout)
		assert.Empty(t, e.jev.requests())
	})

	t.Run("cannot decide keeps the caller's effort", func(t *testing.T) {
		e := newEnv(t)
		t.Setenv(config.EnvAPIKey, "")
		r := e.run([]string{"--cli=claude", "--effort", "high", "fix it"}, nil)

		require.Equal(t, 0, r.code, r.stderr)
		d := decision(t, r.stdout)
		assert.Nil(t, d["model"])
		assert.Equal(t, "high", d["effort"])
		assert.Equal(t, strs("claude", "-p", "fix it", "--effort", "high"), d["argv"])
	})

	t.Run("skipped argument: warning and skipped array", func(t *testing.T) {
		e := newEnv(t)
		e.jev.pick = "gpt-6-luna@low"
		r := e.run([]string{"--cli=codex", "--output-format", "json", "list the files"}, nil)

		require.Equal(t, 0, r.code, r.stderr)
		assert.Equal(t, "agrouter: warning: skipped --output-format json: codex has no mapping for it\n", r.stderr)
		assert.Equal(t, map[string]any{
			"cli": "codex", "model": "gpt-6-luna", "effort": "low",
			"argv":    strs("codex", "exec", "list the files", "--model", "gpt-6-luna", "-c", `model_reasoning_effort="low"`),
			"skipped": strs("--output-format json"),
		}, decision(t, r.stdout))
	})

	t.Run("raw tokens and flags never reach the Jev state", func(t *testing.T) {
		e := newEnv(t)
		e.jev.pick = "claude-opus-5-5@high"
		r := e.run([]string{"--cli", "claude", "--permission-mode", "plan", "--verbose", "plan the migration",
			"--", "--add-dir", "RAWTOKEN"}, strings.NewReader("details on stdin"))

		require.Equal(t, 0, r.code, r.stderr)
		assert.Equal(t, []string{`{"prompt":"plan the migration\n\ndetails on stdin"}`}, e.jev.requests())
		d := decision(t, r.stdout)
		assert.Equal(t, strs("claude", "-p", "plan the migration", "--permission-mode", "plan", "--verbose",
			"--model", "claude-opus-5-5", "--effort", "high", "--add-dir", "RAWTOKEN"), d["argv"])
	})

	t.Run("html characters are not escaped", func(t *testing.T) {
		e := newEnv(t)
		e.jev.pick = "claude-sonnet-5-5@low"
		r := e.run([]string{"a <b> & c"}, nil)

		require.Equal(t, 0, r.code, r.stderr)
		assert.Contains(t, r.stdout, `"a <b> & c"`)
	})
}

func TestApp_Errors(t *testing.T) {
	oneLine := func(t *testing.T, r result, prefix string) {
		t.Helper()
		assert.Equal(t, 2, r.code)
		assert.Empty(t, r.stdout)
		assert.Equal(t, 1, strings.Count(r.stderr, "\n"), r.stderr)
		assert.True(t, strings.HasPrefix(r.stderr, prefix), r.stderr)
	}

	t.Run("no prompt", func(t *testing.T) {
		e := newEnv(t)
		oneLine(t, e.run([]string{"--verbose"}, nil), "agrouter: no prompt")
		oneLine(t, e.run([]string{"exec", "--print"}, strings.NewReader("")), "agrouter: no prompt")
	})

	t.Run("cannot decide with the CLI unknown", func(t *testing.T) {
		e := newEnv(t)
		oneLine(t, e.run([]string{"--jev-api-key=", "fix it"}, nil), "agrouter: jev cannot decide between claude, codex")
	})

	t.Run("unknown flag and second positional", func(t *testing.T) {
		e := newEnv(t)
		oneLine(t, e.run([]string{"--frobnicate", "x"}, nil), "agrouter: ")
		oneLine(t, e.run([]string{"one", "two"}, nil), "agrouter: more than one positional")
	})

	t.Run("config error: one line per violation", func(t *testing.T) {
		e := newEnv(t)
		e.globalConfig("[agrouter]\nmax_chunks = 0\nchunk_parallel = 0\n")
		r := e.run([]string{"fix it"}, nil)

		assert.Equal(t, 2, r.code)
		assert.Empty(t, r.stdout)
		assert.Equal(t, "agrouter: config: [agrouter] max_chunks = 0: must be at least 1\n"+
			"agrouter: config: [agrouter] chunk_parallel = 0: must be at least 1\n", r.stderr)
	})

	t.Run("no enabled options", func(t *testing.T) {
		e := newEnv(t)
		e.globalConfig("[cli.claude]\nenabled = false\n[cli.codex]\nenabled = false\n")
		oneLine(t, e.run([]string{"fix it"}, nil), "agrouter: config: no enabled options")
	})

	t.Run("unknown --cli is a warning", func(t *testing.T) {
		e := newEnv(t)
		e.jev.pick = "claude-sonnet-5-5@low"
		r := e.run([]string{"--cli=nope", "fix it"}, nil)

		require.Equal(t, 0, r.code, r.stderr)
		assert.Equal(t, "agrouter: warning: skipped --cli nope: not an enabled CLI; routing across every CLI\n", r.stderr)
		var out map[string]any
		require.NoError(t, json.Unmarshal([]byte(r.stdout), &out))
		assert.Equal(t, strs("--cli nope"), out["skipped"])
	})

	t.Run("eligibility skips come before argv skips", func(t *testing.T) {
		e := newEnv(t)
		e.jev.pick = "claude-sonnet-5-5@low"
		// claude skips --sandbox and codex skips --output-format json, so both stay eligible
		r := e.run([]string{"--cli=nope", "--sandbox", "read-only", "--output-format", "json", "fix it"}, nil)

		require.Equal(t, 0, r.code, r.stderr)
		assert.Equal(t, "agrouter: warning: skipped --cli nope: not an enabled CLI; routing across every CLI\n"+
			"agrouter: warning: skipped --sandbox read-only: claude has no mapping for it\n", r.stderr)
		var out map[string]any
		require.NoError(t, json.Unmarshal([]byte(r.stdout), &out))
		assert.Equal(t, strs("--cli nope", "--sandbox read-only"), out["skipped"])
	})
}

func TestApp_ExecSelectionBeforeChild(t *testing.T) {
	e := newEnv(t)
	e.globalConfig(madeUpConfig(t, "acme", "test-child", "large", "future-model", "thorough"))
	want := map[string]any{"cli": "acme", "model": "future-model", "effort": "thorough"}
	child := &mocks.CommandRunnerMock{RunFunc: func(_ context.Context, c runner.Command) (int, error) {
		assert.Equal(t, want, decision(t, c.Stderr.(*bytes.Buffer).String()), "selection must be logged before the child starts")
		assert.Empty(t, c.Stdout.(*bytes.Buffer).String())
		assert.Contains(t, c.Argv, "private prompt")
		assert.Contains(t, c.Argv, "private raw token")
		_, err := fmt.Fprintln(c.Stdout, `{"child":"output"}`)
		require.NoError(t, err)
		_, err = fmt.Fprintln(c.Stderr, "child diagnostic")
		require.NoError(t, err)
		return 7, nil
	}}
	r := e.runWithRunner([]string{"exec", "--cli=acme", "--model=preferred", "--effort=thorough",
		"--jev-api-key=private-api-key", "private prompt", "--", "private raw token"}, nil, child)
	assert.Equal(t, 7, r.code)
	assert.JSONEq(t, `{"child":"output"}`, r.stdout)
	line, rest, ok := strings.Cut(r.stderr, "\n")
	require.True(t, ok)
	assert.Equal(t, want, decision(t, line+"\n"))
	assert.Equal(t, "child diagnostic\n", rest)
	assert.Len(t, child.RunCalls(), 1)
	assert.Empty(t, e.jev.requests())
}

func TestApp_Exec(t *testing.T) {
	t.Run("child exit code", func(t *testing.T) {
		e := newEnv(t)
		e.fakeCommands()
		t.Setenv(helperExit, "7")
		r := e.run([]string{"exec", "--cli=claude", "--model=opus", "--effort=low", "fix it"}, nil)

		assert.Equal(t, 7, r.code)
		assert.Empty(t, r.stdout)
		assert.Equal(t, map[string]any{"cli": "claude", "model": "claude-opus-5-5", "effort": "low"},
			decision(t, r.stderr))
		argv, stdin, _ := e.child()
		assert.Equal(t, []string{"-p", "fix it", "--model", "claude-opus-5-5", "--effort", "low"}, argv)
		assert.Empty(t, stdin)
	})

	t.Run("missing command: 127", func(t *testing.T) {
		e := newEnv(t)
		e.globalConfig("[cli.claude]\ncommand = agrouter-test-no-such-command\n")
		r := e.run([]string{"exec", "--cli=claude", "--model=opus", "--effort=low", "fix it"}, nil)

		assert.Equal(t, runner.ExitStartFailure, r.code)
		assert.Empty(t, r.stdout)
		line, rest, ok := strings.Cut(r.stderr, "\n")
		require.True(t, ok)
		assert.Equal(t, "claude", decision(t, line+"\n")["cli"])
		assert.Equal(t, 1, strings.Count(rest, "\n"), rest)
		assert.True(t, strings.HasPrefix(rest, "agrouter: cannot start "), rest)
	})

	t.Run("stdin over the capture limit reaches the child whole", func(t *testing.T) {
		e := newEnv(t)
		exe := helperCommand(t)
		e.globalConfig("[agrouter]\nmax_chunks = 1\n[cli.claude]\ncommand = " + exe + "\n[cli.codex]\ncommand = " + exe + "\n")
		input := strings.Repeat("line of a long ralphex prompt\n", 20_000) // 600 KB, past one chunk
		r := e.run([]string{"exec", "--cli=claude"}, strings.NewReader(input))

		require.Equal(t, 0, r.code, r.stderr)
		argv, stdin, _ := e.child()
		assert.Equal(t, []string{"-p"}, argv, "cannot decide: the known CLI runs with its defaults")
		assert.Equal(t, input, string(stdin))
		assert.Empty(t, e.jev.requests())
		assert.Equal(t, map[string]any{"cli": "claude", "model": nil, "effort": nil}, decision(t, r.stderr))
	})

	t.Run("skip warnings in exec mode", func(t *testing.T) {
		e := newEnv(t)
		e.fakeCommands()
		r := e.run([]string{"exec", "--cli=codex", "--model=gpt-6-luna", "--effort=low", "--verbose", "fix it"}, nil)

		require.Equal(t, 0, r.code, r.stderr)
		log, ok := strings.CutPrefix(r.stderr, "agrouter: warning: skipped --verbose: maps to nothing for codex\n")
		require.True(t, ok, r.stderr)
		assert.Equal(t, map[string]any{"cli": "codex", "model": "gpt-6-luna", "effort": "low"}, decision(t, log))
	})
}

// madeUpConfig defines custom names in config and disables the shipped CLIs dynamically.
func madeUpConfig(t *testing.T, name, command, section, model, effort string) string {
	t.Helper()
	cfg, err := config.Load(config.Sources{Embedded: defaults.Config})
	require.NoError(t, err)
	var disabled strings.Builder
	for _, cli := range cfg.CLIs {
		fmt.Fprintf(&disabled, "[cli.%s]\nenabled = false\n", cli.Name)
	}
	return disabled.String() + strings.NewReplacer("CLI_NAME", name, "COMMAND", command,
		"MODEL_SECTION", section, "MODEL_NAME", model, "EFFORT_NAME", effort).Replace(`
[cli.CLI_NAME]
command     = COMMAND
description = A coding agent that exists only in this test.

[cli.CLI_NAME.args]
print                     = ["run", "--quiet", "{prompt}"]
model                     = ["--llm", "{model}"]
effort                    = ["--think={effort}"]
output-format.stream-json = ["--events", "ndjson"]
permission-mode.plan      = ["--mode", "read"]
verbose                   = []

[model.small]
cli         = CLI_NAME
name        = small-1
efforts     = quick, EFFORT_NAME
description = Small and fast.

[model.MODEL_SECTION]
cli         = CLI_NAME
name        = MODEL_NAME
aliases     = preferred
efforts     = quick, EFFORT_NAME
description = Large and thorough.

[effort.CLI_NAME.EFFORT_NAME]
description = Custom reasoning effort.
`)
}

func TestApp_ConfigDrivenNames(t *testing.T) {
	stdin := "line one\r\nline two\n\ttabbed \"quoted\" & 100%\n"
	argv := []string{"exec", "--output-format", "stream-json", "--verbose", "--permission-mode", "plan", "do it now"}

	tests := []struct {
		cli     string
		section string
		model   string
		effort  string
	}{
		{cli: "acme", section: "big", model: "big-1", effort: "deep"},
		{cli: "zeta", section: "big", model: "big-1", effort: "deep"},
		{cli: "nova", section: "new-reasoner", model: "future-2027", effort: "thorough"},
	}
	for _, tc := range tests {
		t.Run(tc.cli, func(t *testing.T) {
			e := newEnv(t)
			e.globalConfig(madeUpConfig(t, tc.cli, helperCommand(t), tc.section, tc.model, tc.effort))
			e.jev.pick = tc.section + "@" + tc.effort
			r := e.run(argv, strings.NewReader(stdin))
			require.Equal(t, 0, r.code, r.stderr)
			log, ok := strings.CutPrefix(r.stderr, "agrouter: warning: skipped --verbose: maps to nothing for "+tc.cli+"\n")
			require.True(t, ok, r.stderr)
			assert.Equal(t, map[string]any{"cli": tc.cli, "model": tc.model, "effort": tc.effort}, decision(t, log))
			got, gotStdin, _ := e.child()
			assert.Equal(t, []string{"run", "--quiet", "do it now", "--events", "ndjson", "--mode", "read",
				"--llm", tc.model, "--think=" + tc.effort}, got)
			assert.Equal(t, stdin, string(gotStdin))
			require.Len(t, e.jev.requests(), 1)

			r = e.run([]string{"--cli", tc.cli, "--model", "preferred", "--effort", tc.effort, "do it"}, nil)
			require.Equal(t, 0, r.code, r.stderr)
			d := decision(t, r.stdout)
			assert.Equal(t, tc.cli, d["cli"])
			assert.Equal(t, tc.model, d["model"])
			assert.Equal(t, tc.effort, d["effort"])
			assert.Equal(t, strs(helperCommand(t), "run", "--quiet", "do it", "--llm", tc.model,
				"--think="+tc.effort), d["argv"])
			assert.Len(t, e.jev.requests(), 1, "a model alias plus an effort selects one option without Jev")
		})
	}
}

func TestApp_HelpHasKeyCautions(t *testing.T) {
	e := newEnv(t)
	r := e.run([]string{"--help"}, nil)

	require.Equal(t, 0, r.code)
	for _, want := range []string{"TYPESAFE_API_KEY", "process", "listings", ".agrouter/config"} {
		assert.Contains(t, r.stdout, want)
	}
}

func TestStdinReader(t *testing.T) {
	assert.Nil(t, stdinReader(nil))
	r := strings.NewReader("x")
	assert.Same(t, r, stdinReader(r))

	f, err := os.CreateTemp(t.TempDir(), "stdin")
	require.NoError(t, err)
	defer f.Close()
	assert.Equal(t, f, stdinReader(f))
}
