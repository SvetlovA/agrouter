package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
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

// fakeJev answers every route Choice with pick (which must be among the options sent), a complexity
// Score with level normalized to 0 to 10, and every Noul with 0.5, and records each request's
// state. With failDocs it rejects every doc request with a 400.
type fakeJev struct {
	t        *testing.T
	mu       sync.Mutex
	pick     string
	level    string
	failDocs bool
	states   []string
	srv      *httptest.Server
}

func newFakeJev(t *testing.T) *fakeJev {
	f := &fakeJev{t: t, level: "7"}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeJev) handle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model     string          `json:"model"`
		State     json.RawMessage `json:"state"`
		Questions map[string]struct {
			Type     string          `json:"type"`
			Criteria json.RawMessage `json:"criteria"`
		} `json:"questions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		f.t.Errorf("fake jev: decode request: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.states = append(f.states, string(req.State))
	pick, level, failDocs := f.pick, f.level, f.failDocs
	f.mu.Unlock()
	if failDocs && strings.HasPrefix(string(req.State), `{"doc`) {
		http.Error(w, "doc rejected", http.StatusBadRequest)
		return
	}

	answers := map[string]any{}
	for id, q := range req.Questions {
		if q.Type == jev.TypeNoul {
			answers[id] = map[string]any{"type": q.Type, "noul": 0.5}
			continue
		}
		if q.Type == jev.TypeScore {
			answer, err := fakeScoreAnswer(q.Criteria, level)
			if err != nil {
				f.t.Errorf("fake jev: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			answers[id] = answer
			continue
		}
		var criteria map[string]json.RawMessage
		if err := json.Unmarshal(q.Criteria, &criteria); err != nil {
			f.t.Errorf("fake jev: decode criteria: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		probs := map[string]float64{}
		for name := range criteria {
			probs[name] = 0
		}
		choice := pick
		if _, ok := probs[pick]; !ok {
			choice = level
		}
		if _, ok := probs[choice]; !ok {
			f.t.Errorf("fake jev: neither pick %q nor level %q is among the criteria sent", pick, level)
		}
		probs[choice] = 1
		answers[id] = map[string]any{"type": q.Type, "choice": choice, "probabilities": probs, "confidence": 0.9}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"model": req.Model, "answers": answers})
}

func fakeScoreAnswer(criteria json.RawMessage, level string) (map[string]any, error) {
	var levels []string
	if err := json.Unmarshal(criteria, &levels); err != nil {
		return nil, fmt.Errorf("decode score levels: %w", err)
	}
	rating, err := strconv.ParseFloat(level, 64)
	if err != nil {
		return nil, fmt.Errorf("parse score level: %w", err)
	}
	score := rating * float64(len(levels)-1) / 10
	low := int(math.Floor(score))
	probs, legend := map[string]float64{}, map[string]string{}
	for i, description := range levels {
		key := strconv.Itoa(i)
		legend[key] = description
		probs[key] = 0
		switch i {
		case low:
			probs[key] = 1 - (score - float64(low))
		case low + 1:
			probs[key] = score - float64(low)
		}
	}
	return map[string]any{"type": jev.TypeScore, "score": score, "legend": legend, "probabilities": probs, "confidence": 0.9}, nil
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
	require.True(t, json.Valid([]byte(stdout)), stdout)
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
			"--model", "opus", "--effort", "high", "--print"}, strings.NewReader(task))

		require.Equal(t, 0, r.code, r.stderr)
		assert.Equal(t, map[string]any{"cli": "claude", "model": "claude-opus-5-5", "effort": "high"}, decision(t, r.stderr))
		assert.Empty(t, r.stdout)
		argv, stdin, _ := e.child()
		assert.Equal(t, []string{"-p", "--dangerously-skip-permissions", "--output-format", "stream-json",
			"--model", "claude-opus-5-5", "--effort", "high"}, argv)
		assert.Equal(t, task, string(stdin))
		assert.Empty(t, e.jev.requests())
	})

	t.Run("Jev chooses", func(t *testing.T) {
		e := newEnv(t)
		e.fakeCommands()
		e.jev.pick = "claude-sonnet-5-5@medium"
		r := e.run([]string{"exec", "--cli=claude", "--dangerously-skip-permissions", "--output-format", "stream-json",
			"--print"}, strings.NewReader(task))

		require.Equal(t, 0, r.code, r.stderr)
		assert.Equal(t, map[string]any{"cli": "claude", "model": "claude-sonnet-5-5", "effort": "medium", "confidence": map[string]any{"model_selection": 0.9}}, decision(t, r.stderr))
		argv, stdin, environ := e.child()
		assert.Equal(t, []string{"-p", "--dangerously-skip-permissions", "--output-format", "stream-json",
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
		assert.Equal(t, map[string]any{"cli": "codex", "model": "gpt-6.1-sol", "effort": "medium", "confidence": map[string]any{"model_selection": 0.9}}, decision(t, r.stderr))
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
		assert.Equal(t, map[string]any{"cli": "codex", "model": "gpt-6-astra", "effort": "high", "confidence": map[string]any{"model_selection": 0.9}}, decision(t, r.stderr))
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
			"confidence": map[string]any{"model_selection": 0.9},
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
		r := e.run([]string{"--verbose", "--cli=claude", "--jev-api-key=", "--output-format=json", "fix it"}, nil)

		require.Equal(t, 0, r.code, r.stderr)
		assert.Empty(t, r.stderr)
		decision(t, r.stdout)
		assert.Subset(t, decision(t, r.stdout), map[string]any{"cli": "claude", "model": nil, "effort": nil,
			"argv": strs("claude", "-p", "--output-format", "json", "--", "fix it"), "skipped": []any{}})
		assert.Empty(t, e.jev.requests())
	})

	t.Run("cannot decide keeps the caller's effort", func(t *testing.T) {
		e := newEnv(t)
		t.Setenv(config.EnvAPIKey, "")
		r := e.run([]string{"--verbose", "--cli=claude", "--effort", "high", "fix it"}, nil)

		require.Equal(t, 0, r.code, r.stderr)
		d := decision(t, r.stdout)
		assert.Nil(t, d["model"])
		assert.Equal(t, "high", d["effort"])
		assert.Equal(t, strs("claude", "-p", "--effort", "high", "--", "fix it"), d["argv"])
	})

	t.Run("skipped argument: warning and skipped array", func(t *testing.T) {
		e := newEnv(t)
		e.jev.pick = "gpt-6-luna@low"
		r := e.run([]string{"--verbose", "--cli=codex", "--output-format", "json", "list the files"}, nil)

		require.Equal(t, 0, r.code, r.stderr)
		assert.Equal(t, "agrouter: warning: skipped --output-format json: codex has no mapping for it\n", r.stderr)
		assert.Subset(t, decision(t, r.stdout), map[string]any{
			"cli": "codex", "model": "gpt-6-luna", "effort": "low",
			"argv":    strs("codex", "exec", "--model", "gpt-6-luna", "-c", `model_reasoning_effort="low"`, "--", "list the files"),
			"skipped": strs("--output-format json"),
		})
	})

	t.Run("raw tokens and flags never reach the Jev state", func(t *testing.T) {
		e := newEnv(t)
		e.jev.pick = "claude-opus-5-5@high"
		r := e.run([]string{"--cli", "claude", "--permission-mode", "plan", "--verbose", "plan the migration",
			"--", "--add-dir", "RAWTOKEN"}, strings.NewReader("details on stdin"))

		require.Equal(t, 0, r.code, r.stderr)
		assert.Equal(t, []string{`{"prompt":"plan the migration\n\ndetails on stdin"}`}, e.jev.requests())
		d := decision(t, r.stdout)
		assert.Equal(t, strs("claude", "-p", "--permission-mode", "plan",
			"--model", "claude-opus-5-5", "--effort", "high", "--add-dir", "RAWTOKEN", "--", "plan the migration"), d["argv"])
	})

	t.Run("html characters are not escaped", func(t *testing.T) {
		e := newEnv(t)
		e.jev.pick = "claude-sonnet-5-5@low"
		r := e.run([]string{"--verbose", "a <b> & c"}, nil)

		require.Equal(t, 0, r.code, r.stderr)
		assert.Contains(t, r.stdout, `"a <b> & c"`)
	})
}

func TestApp_VerboseOutput(t *testing.T) {
	for _, mode := range []string{"decision", "exec"} {
		t.Run(mode, func(t *testing.T) {
			e := newSyntheticEnv(t)
			e.jev.pick = "fast@low"
			require.NoError(t, os.WriteFile(filepath.Join(e.workDir, "project.md"), []byte("project doc"), 0o600))
			var states []string
			for _, verbose := range []bool{false, true} {
				argv := []string{"--doc=project.md", "private task"}
				if verbose {
					argv = append([]string{"--verbose"}, argv...)
				}
				if mode == "exec" {
					argv = append([]string{"exec"}, argv...)
				}
				r := e.run(argv, nil)
				require.Equal(t, 0, r.code, r.stderr)
				output := r.stdout
				if mode == "exec" {
					assert.Empty(t, r.stdout)
					output = r.stderr
					childArgv, _, _ := e.child()
					assert.NotContains(t, childArgv, "--verbose")
				} else {
					assert.Empty(t, r.stderr)
				}
				d := decision(t, output)
				assert.Equal(t, "alpha", d["cli"])
				assert.Equal(t, "fast-1", d["model"])
				assert.Equal(t, "low", d["effort"])
				assert.InDelta(t, 7, d["project_complexity"], 1e-9)
				assert.NotContains(t, output, "project doc")
				confidence := d["confidence"].(map[string]any)
				assert.InDelta(t, 0.9, confidence["model_selection"], 1e-9)
				assert.InDelta(t, 0.9, confidence["project_complexity"], 1e-9)
				if verbose {
					assert.Contains(t, confidence, "route")
					assert.Contains(t, confidence, "complexity_chunks")
					assert.Equal(t, "fast@low", confidence["choice"])
					probabilities := confidence["probabilities"].(map[string]any)
					assert.InDelta(t, 1, probabilities["fast@low"], 1e-9)
					chunks := confidence["complexity_chunks"].([]any)
					assert.Equal(t, []any{map[string]any{"index": float64(1), "of": float64(1),
						"project_complexity": float64(7), "evidence": 0.5, "confidence": 0.9}}, chunks)
					assert.Contains(t, d, "options")
					if mode == "decision" {
						assert.Contains(t, d, "argv")
						assert.Equal(t, []any{}, d["skipped"])
						assert.Equal(t, false, d["stdin_required"])
						assert.Contains(t, d["command"], "private task")
						assert.Contains(t, output, "\n  ")
					}
				} else {
					assert.Len(t, confidence, 2)
					assert.Len(t, d, 5)
				}
				if mode == "exec" || !verbose {
					assert.NotContains(t, d, "argv")
					assert.NotContains(t, d, "skipped")
					assert.NotContains(t, output, "private task")
				}
				requests := e.jev.requests()
				if states == nil {
					states = requests
				} else {
					assert.Equal(t, states, requests[len(states):], "verbosity must not affect routing requests")
				}
			}
		})
	}
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
		e.globalConfig("[agrouter]\ntimeout = 0s\n[model.synthetic]\ncli = claude\n")
		r := e.run([]string{"fix it"}, nil)

		assert.Equal(t, 2, r.code)
		assert.Empty(t, r.stdout)
		assert.Equal(t, "agrouter: config: [agrouter] timeout = 0s: must be positive\n"+
			"agrouter: config: [model.synthetic] name: required\n", r.stderr)
	})

	t.Run("no enabled options", func(t *testing.T) {
		e := newEnv(t)
		e.globalConfig("[cli.claude]\nenabled = false\n[cli.codex]\nenabled = false\n")
		oneLine(t, e.run([]string{"fix it"}, nil), "agrouter: config: no enabled options")
	})

	t.Run("unknown --cli is a warning", func(t *testing.T) {
		e := newEnv(t)
		e.jev.pick = "claude-sonnet-5-5@low"
		r := e.run([]string{"--verbose", "--cli=nope", "fix it"}, nil)

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
		r := e.run([]string{"--verbose", "--cli=nope", "--sandbox", "read-only", "--output-format", "json", "fix it"}, nil)

		require.Equal(t, 0, r.code, r.stderr)
		assert.Equal(t, "agrouter: warning: skipped --cli nope: not an enabled CLI; routing across every CLI\n"+
			"agrouter: warning: skipped --sandbox read-only: claude has no mapping for it\n", r.stderr)
		var out map[string]any
		require.NoError(t, json.Unmarshal([]byte(r.stdout), &out))
		assert.Equal(t, strs("--cli nope", "--sandbox read-only"), out["skipped"])
	})
}

// syntheticCLIs disables the shipped CLIs and defines two made-up ones, both running the helper with
// -p/--model/--effort mappings: alpha (fast@low|high and deep@low|high, deep aliased "strong") and
// beta (other@low|high).
func syntheticCLIs(t *testing.T) string {
	t.Helper()
	cfg, err := config.Load(config.Sources{Embedded: defaults.Config})
	require.NoError(t, err)
	var disabled strings.Builder
	for _, cli := range cfg.CLIs {
		fmt.Fprintf(&disabled, "[cli.%s]\nenabled = false\n", cli.Name)
	}
	var clis strings.Builder
	for _, name := range []string{"alpha", "beta"} {
		clis.WriteString(strings.NewReplacer("CLI_NAME", name, "COMMAND", helperCommand(t)).Replace(`
[cli.CLI_NAME]
command     = COMMAND
description = A coding agent that exists only in this test.

[cli.CLI_NAME.args]
print  = ["-p"]
prompt = ["--", "{prompt}"]
model  = ["--model", "{model}"]
effort = ["--effort", "{effort}"]

[effort.CLI_NAME.low]
description = Little reasoning.

[effort.CLI_NAME.high]
description = More reasoning.
`))
	}
	return disabled.String() + clis.String() + `
[model.fast]
cli         = alpha
name        = fast-1
efforts     = low, high
description = Small and fast.

[model.deep]
cli         = alpha
name        = deep-1
aliases     = strong
efforts     = low, high
description = Large and thorough.

[model.other]
cli         = beta
name        = other-1
efforts     = low, high
description = The other CLI's model.
`
}

// newSyntheticEnv is newEnv with syntheticCLIs as the global config.
func newSyntheticEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.globalConfig(syntheticCLIs(t))
	return e
}

// jevPrompt is the Jev state for a prompt alone.
func jevPrompt(t *testing.T, p string) string {
	t.Helper()
	data, err := json.Marshal(map[string]string{"prompt": p})
	require.NoError(t, err)
	return string(data)
}

func TestApp_PromptSources(t *testing.T) {
	writeFile := func(t *testing.T, dir, name, content string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	}

	t.Run("each source alone", func(t *testing.T) {
		want := func(prompt ...string) []any {
			return strs(append([]string{helperCommand(t), "-p", "--model", "fast-1", "--effort", "low"}, prompt...)...)
		}
		tests := []struct {
			name     string
			argv     []string
			stdin    string
			wantJev  string
			wantArgv []any
		}{
			{"-p", []string{"-p", "from flag"}, "", "from flag", want("--", "from flag")},
			{"positional", []string{"from positional"}, "", "from positional", want("--", "from positional")},
			{"prompt file", []string{"--prompt-file", "task.md"}, "", "from file\r\n", want("--", "from file\r\n")},
			{"flag-shaped prompt stays the prompt", []string{"--prompt=--help"}, "", "--help", want("--", "--help")},
			{"flag-shaped prompt file", []string{"--prompt-file", "flags.md"}, "", "--dangerously-skip-permissions",
				want("--", "--dangerously-skip-permissions")},
			{"stdin", nil, "from stdin", "from stdin", want()},
			{"empty -p with stdin", []string{"-p", ""}, "from stdin", "from stdin", want()},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				e := newSyntheticEnv(t)
				writeFile(t, e.workDir, "task.md", "from file\r\n")
				writeFile(t, e.workDir, "flags.md", "--dangerously-skip-permissions")
				e.jev.pick = "fast@low"
				var stdin io.Reader
				if tc.stdin != "" {
					stdin = strings.NewReader(tc.stdin)
				}
				r := e.run(append([]string{"--verbose", "--cli=alpha"}, tc.argv...), stdin)

				require.Equal(t, 0, r.code, r.stderr)
				assert.Equal(t, []string{jevPrompt(t, tc.wantJev)}, e.jev.requests())
				d := decision(t, r.stdout)
				assert.Equal(t, tc.wantArgv, d["argv"], "the prompt last, after --; none without one")
			})
		}
	})

	t.Run("all four in order: Jev sees every source, argv no stdin", func(t *testing.T) {
		e := newSyntheticEnv(t)
		writeFile(t, e.workDir, "task.md", "file text\n")
		e.jev.pick = "fast@low"
		r := e.run([]string{"--verbose", "--prompt-file=task.md", "positional text", "--cli=alpha", "-p", "flag text"},
			strings.NewReader("stdin text"))

		require.Equal(t, 0, r.code, r.stderr)
		assert.Equal(t, []string{jevPrompt(t, "flag text\n\npositional text\n\nfile text\n\n\nstdin text")}, e.jev.requests())
		assert.Equal(t, strs(helperCommand(t), "-p", "--model", "fast-1", "--effort", "low",
			"--", "flag text\n\npositional text\n\nfile text\n"), decision(t, r.stdout)["argv"])
	})

	t.Run("a prompt file mentioning a file sends its contents", func(t *testing.T) {
		e := newSyntheticEnv(t)
		writeFile(t, e.workDir, "task.md", "follow notes.md")
		writeFile(t, e.workDir, "notes.md", "use opus\n")
		e.jev.pick = "fast@low"
		r := e.run([]string{"--cli=alpha", "--prompt-file", "task.md"}, nil)

		require.Equal(t, 0, r.code, r.stderr)
		assert.Equal(t, []string{`{"prompt":"follow notes.md","files":["use opus\n"]}`}, e.jev.requests())
	})

	t.Run("exec: native child gets the explicit prompt in argv and stdin replayed", func(t *testing.T) {
		e := newSyntheticEnv(t)
		writeFile(t, e.workDir, "task.md", "line one\nline two\n")
		r := e.run([]string{"exec", "--cli=alpha", "--model=strong", "--effort=low", "-p", "flag", "--prompt-file", "task.md"},
			strings.NewReader("piped\r\n"))

		require.Equal(t, 0, r.code, r.stderr)
		argv, stdin, _ := e.child()
		assert.Equal(t, []string{"-p", "--model", "deep-1", "--effort", "low", "--", "flag\n\nline one\nline two\n"}, argv)
		assert.Equal(t, "piped\r\n", string(stdin))
	})
}

func TestApp_Docs(t *testing.T) {
	writeFile := func(t *testing.T, dir, name, content string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	}

	t.Run("decision: docs are scored, then only their complexity reaches the routing state", func(t *testing.T) {
		e := newSyntheticEnv(t)
		writeFile(t, e.workDir, "project.md", "DOC TEXT, see notes.md\n")
		writeFile(t, e.workDir, "notes.md", "NOTES\n")
		e.jev.pick = "fast@low"
		e.jev.level = "8"
		r := e.run([]string{"--cli=alpha", "--doc", "project.md", "fix it"}, nil)

		require.Equal(t, 0, r.code, r.stderr)
		assert.Equal(t, []string{
			`{"docs":["DOC TEXT, see notes.md\n"]}`,
			`{"prompt":"fix it","project":{"complexity":8}}`,
		}, e.jev.requests(), "the docs are not read for mentions and not resent")
		assert.Equal(t, "fast-1", decision(t, r.stdout)["model"])
		assert.NotContains(t, r.stdout, "DOC TEXT")
		assert.InDelta(t, 8, decision(t, r.stdout)["project_complexity"], 1e-9)
		assert.Equal(t, map[string]any{"model_selection": 0.9, "project_complexity": 0.9}, decision(t, r.stdout)["confidence"])
	})

	t.Run("decision: no docs, no complexity stage and no project", func(t *testing.T) {
		e := newSyntheticEnv(t)
		e.jev.pick = "fast@low"
		r := e.run([]string{"--cli=alpha", "fix it"}, nil)

		require.Equal(t, 0, r.code, r.stderr)
		assert.Equal(t, []string{jevPrompt(t, "fix it")}, e.jev.requests())
	})

	t.Run("decision: an empty doc has nothing to score", func(t *testing.T) {
		e := newSyntheticEnv(t)
		writeFile(t, e.workDir, "empty.md", "")
		e.jev.pick = "fast@low"
		r := e.run([]string{"--cli=alpha", "--doc", "empty.md", "fix it"}, nil)

		require.Equal(t, 0, r.code, r.stderr)
		assert.Equal(t, []string{jevPrompt(t, "fix it")}, e.jev.requests())
	})

	t.Run("decision: one eligible option asks Jev nothing, docs or not", func(t *testing.T) {
		e := newSyntheticEnv(t)
		writeFile(t, e.workDir, "project.md", "DOC TEXT\n")
		r := e.run([]string{"--model=deep-1", "--effort=high", "--doc", "project.md", "fix it"}, nil)

		require.Equal(t, 0, r.code, r.stderr)
		assert.Empty(t, e.jev.requests())
	})

	t.Run("exec: a failed complexity stage runs the known CLI with its defaults", func(t *testing.T) {
		e := newSyntheticEnv(t)
		writeFile(t, e.workDir, "project.md", "DOC TEXT\n")
		e.jev.failDocs = true
		r := e.run([]string{"exec", "--cli=alpha", "--doc", "project.md", "fix it"}, nil)

		require.Equal(t, 0, r.code, r.stderr)
		argv, _, _ := e.child()
		assert.Equal(t, []string{"-p", "--", "fix it"}, argv, "no model or effort chosen")
		assert.Len(t, e.jev.requests(), 1, "only the doc request")
		assert.NotContains(t, r.stderr, "DOC TEXT")
	})

	t.Run("decision: a failed complexity stage with more than one CLI left exits 2", func(t *testing.T) {
		e := newSyntheticEnv(t)
		writeFile(t, e.workDir, "project.md", "DOC TEXT\n")
		e.jev.failDocs = true
		r := e.run([]string{"--doc", "project.md", "fix it"}, nil)

		assert.Equal(t, 2, r.code)
		assert.Empty(t, r.stdout)
		assert.Equal(t, 1, strings.Count(r.stderr, "\n"), r.stderr)
		assert.True(t, strings.HasPrefix(r.stderr, "agrouter: "), r.stderr)
		assert.NotContains(t, r.stderr, "DOC TEXT")
	})

	t.Run("exec: the deadline passing while reading a doc runs the known CLI with its defaults", func(t *testing.T) {
		e := newEnv(t)
		e.globalConfig(syntheticCLIs(t) + "\n[agrouter]\ntimeout = 1ns\n")
		writeFile(t, e.workDir, "project.md", "DOC TEXT\n")
		r := e.run([]string{"exec", "--cli=alpha", "--doc", "project.md", "fix it"}, nil)

		require.Equal(t, 0, r.code, r.stderr)
		argv, _, _ := e.child()
		assert.Equal(t, []string{"-p", "--", "fix it"}, argv)
		assert.Empty(t, e.jev.requests())
	})

	t.Run("exec: a missing doc after the deadline passed is still a bad doc", func(t *testing.T) {
		e := newEnv(t)
		e.globalConfig(syntheticCLIs(t) + "\n[agrouter]\ntimeout = 1ns\n")
		writeFile(t, e.workDir, "project.md", "DOC TEXT\n")
		r := e.run([]string{"exec", "--cli=alpha", "--doc", "project.md", "--doc", "nope.md", "fix it"}, nil)

		assert.Equal(t, 2, r.code)
		assert.Equal(t, "agrouter: --doc: nope.md: no such file\n", r.stderr)
		assert.NoFileExists(t, filepath.Join(e.out, "argv.json"), "the child must not run")
	})

	t.Run("exec: the child gets neither the docs nor their paths", func(t *testing.T) {
		e := newSyntheticEnv(t)
		writeFile(t, e.workDir, "a.md", "DOC A\n")
		writeFile(t, e.workDir, "b.md", "DOC B\n")
		r := e.run([]string{"exec", "--cli=alpha", "--model=strong", "--effort=low", "--doc", "a.md", "--doc=b.md", "-p", "flag"},
			strings.NewReader("piped"))

		require.Equal(t, 0, r.code, r.stderr)
		argv, stdin, _ := e.child()
		assert.Equal(t, []string{"-p", "--model", "deep-1", "--effort", "low", "--", "flag"}, argv)
		assert.Equal(t, "piped", string(stdin))
		assert.NotContains(t, r.stderr, "DOC")
	})
}

func TestApp_PromptSourceErrors(t *testing.T) {
	oneLine := func(t *testing.T, r result, prefix string) {
		t.Helper()
		assert.Equal(t, 2, r.code)
		assert.Empty(t, r.stdout)
		assert.Equal(t, 1, strings.Count(r.stderr, "\n"), r.stderr)
		assert.True(t, strings.HasPrefix(r.stderr, prefix), r.stderr)
	}
	// model and effort fixed leave one eligible option: a bad prompt file still fails, though Jev is not asked
	pinned := []string{"exec", "--cli=alpha", "--model=strong", "--effort=low"}

	tests := []struct {
		name   string
		argv   []string
		stdin  io.Reader
		prefix string
	}{
		{"all empty", []string{"-p", "", ""}, strings.NewReader(""), "agrouter: no prompt: give -p, a positional prompt, --prompt-file or stdin"},
		{"second -p", []string{"-p", "a", "-p", "b"}, nil, "agrouter: -p/--prompt given more than once"},
		{"second --prompt-file", []string{"--prompt-file", "task.md", "--prompt-file", "task.md"}, nil,
			"agrouter: --prompt-file given more than once"},
		{"-p swallowing a flag", []string{"-p", "--model", "x"}, nil, "agrouter: expected argument"},
		{"missing prompt file", append(slices.Clone(pinned), "--prompt-file", "nope.md"), nil,
			"agrouter: --prompt-file: nope.md: no such file"},
		{"directory prompt file", append(slices.Clone(pinned), "--prompt-file", "sub"), nil,
			"agrouter: --prompt-file: sub: not a regular file"},
		{"binary prompt file", append(slices.Clone(pinned), "--prompt-file", "blob.bin", "fix it"), nil,
			"agrouter: --prompt-file: blob.bin: binary file"},
		{"missing doc", append(slices.Clone(pinned), "--doc", "task.md", "--doc", "nope.md", "fix it"), nil,
			"agrouter: --doc: nope.md: no such file"},
		{"directory doc", append(slices.Clone(pinned), "--doc", "sub", "fix it"), nil,
			"agrouter: --doc: sub: not a regular file"},
		{"binary doc", append(slices.Clone(pinned), "--doc", "blob.bin", "fix it"), nil,
			"agrouter: --doc: blob.bin: binary file"},
		{"bad doc in decision mode", []string{"--doc", "nope.md", "fix it"}, nil,
			"agrouter: --doc: nope.md: no such file"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newSyntheticEnv(t)
			require.NoError(t, os.WriteFile(filepath.Join(e.workDir, "task.md"), []byte("x"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(e.workDir, "blob.bin"), []byte{0x89, 'P', 'N', 'G', 0, 0, 1}, 0o600))
			require.NoError(t, os.Mkdir(filepath.Join(e.workDir, "sub"), 0o700))

			oneLine(t, e.run(tc.argv, tc.stdin), tc.prefix)
			assert.Empty(t, e.jev.requests())
			assert.NoFileExists(t, filepath.Join(e.out, "argv.json"), "the child must not run")
		})
	}
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
		assert.Equal(t, []string{"-p", "--model", "claude-opus-5-5", "--effort", "low", "--", "fix it"}, argv)
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

	t.Run("large stdin is routed in chunks and reaches the child whole", func(t *testing.T) {
		e := newEnv(t)
		e.fakeCommands()
		e.jev.pick = "claude-sonnet-5-5@low"
		input := strings.Repeat("line of a long ralphex prompt\n", 100_000) // 3 MB, no capture limit
		r := e.run([]string{"exec", "--verbose", "--cli=claude"}, strings.NewReader(input))

		require.Equal(t, 0, r.code, r.stderr)
		_, stdin, _ := e.child()
		assert.Equal(t, input, string(stdin))
		assert.Greater(t, len(e.jev.requests()), 1, "one request per chunk")
		log := decision(t, r.stderr)
		confidence, ok := log["confidence"].(map[string]any)
		require.True(t, ok)
		assert.Nil(t, confidence["route"], "a pooled decision has no whole-request Jev confidence")
		assert.InDelta(t, 0.9, confidence["model_selection"], 1e-9)
		assert.NotContains(t, confidence, "project_complexity", "no document answers were recorded")
		chunks, ok := confidence["routing_chunks"].([]any)
		require.True(t, ok)
		require.Len(t, chunks, len(e.jev.requests()))
		for i, chunk := range chunks {
			assert.Subset(t, chunk, map[string]any{"field": "prompt", "index": float64(i + 1),
				"of": float64(len(chunks)), "confidence": 0.9, "choice": e.jev.pick, "relevance": 0.5})
			probabilities := chunk.(map[string]any)["probabilities"].(map[string]any)
			assert.InDelta(t, 1, probabilities[e.jev.pick], 1e-9)
		}
		assert.Subset(t, log, map[string]any{"cli": "claude", "model": "claude-sonnet-5-5", "effort": "low", "confidence": confidence})
	})

	t.Run("skip warnings in exec mode", func(t *testing.T) {
		e := newEnv(t)
		e.fakeCommands()
		r := e.run([]string{"exec", "--cli=codex", "--model=gpt-6-luna", "--effort=low", "--output-format=json", "fix it"}, nil)

		require.Equal(t, 0, r.code, r.stderr)
		log, ok := strings.CutPrefix(r.stderr, "agrouter: warning: skipped --output-format json: codex has no mapping for it\n")
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
print                     = ["run", "--quiet"]
prompt                    = ["--", "{prompt}"]
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
	argv := []string{"exec", "--output-format", "stream-json", "--permission-mode", "plan", "do it now"}

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
			log := r.stderr
			assert.Equal(t, map[string]any{"cli": tc.cli, "model": tc.model, "effort": tc.effort,
				"confidence": map[string]any{"model_selection": 0.9}}, decision(t, log))
			got, gotStdin, _ := e.child()
			assert.Equal(t, []string{"run", "--quiet", "--events", "ndjson", "--mode", "read",
				"--llm", tc.model, "--think=" + tc.effort, "--", "do it now"}, got)
			assert.Equal(t, stdin, string(gotStdin))
			require.Len(t, e.jev.requests(), 1)

			r = e.run([]string{"--verbose", "--cli", tc.cli, "--model", "preferred", "--effort", tc.effort, "do it"}, nil)
			require.Equal(t, 0, r.code, r.stderr)
			d := decision(t, r.stdout)
			assert.Equal(t, tc.cli, d["cli"])
			assert.Equal(t, tc.model, d["model"])
			assert.Equal(t, tc.effort, d["effort"])
			assert.Equal(t, strs(helperCommand(t), "run", "--quiet", "--llm", tc.model,
				"--think="+tc.effort, "--", "do it"), d["argv"])
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
