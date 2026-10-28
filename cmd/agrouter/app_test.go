package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SvetlovA/agrouter/cmd/agrouter/mocks"
	"github.com/SvetlovA/agrouter/pkg/classifier"
	"github.com/SvetlovA/agrouter/pkg/config"
	"github.com/SvetlovA/agrouter/pkg/config/defaults"
	"github.com/SvetlovA/agrouter/pkg/readiness"
	"github.com/SvetlovA/agrouter/pkg/router"
	"github.com/SvetlovA/agrouter/pkg/runner"
)

// the test binary doubles as the fake CLI: with GO_WANT_HELPER_PROCESS=1 it records its argv, stdin
// and environment to $AGROUTER_HELPER_OUT and exits $AGROUTER_HELPER_EXIT.
const (
	helperEnv  = "GO_WANT_HELPER_PROCESS"
	helperOut  = "AGROUTER_HELPER_OUT"
	helperExit = "AGROUTER_HELPER_EXIT"
	// with helperProbeArg first, the helper is a readiness probe printing $AGROUTER_HELPER_PROBE
	helperProbe    = "AGROUTER_HELPER_PROBE"
	helperProbeArg = "--readiness-probe"
	testKey        = "test-jev-key"
)

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		os.Exit(helper())
	}
	os.Exit(m.Run())
}

func helper() int {
	if len(os.Args) > 1 && os.Args[1] == helperProbeArg {
		// a json-helper readiness probe: its stdout is the configured evidence
		_, _ = os.Stdout.WriteString(os.Getenv(helperProbe))
		return 0
	}
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

// fakeJev answers each routing stage Choice with pick's part: its CLI, its model section or its
// effort label (pick is an option id, "<section>@<effort>" or "<section>", or a CLI under model
// passthrough), a complexity Score with level normalized to 0 to 10, and every Noul with 0.5, and
// records each request's state. With failDocs it rejects every doc request with a 400, and with
// failStage every request asking that routing stage, as the fail fields say.
type fakeJev struct {
	t         *testing.T
	mu        sync.Mutex
	pick      string
	level     string
	failDocs  bool
	failStage string
	// how failStage fails: with failStatus (0: 400) and, when set, failBody as the JSON error body,
	// with a 200 carrying failAnswer as its answer ("omit" leaves it out), by never answering
	// (failHang) or by dropping the connection (failDrop)
	failStatus int
	failBody   string
	failAnswer string
	failHang   bool
	failDrop   bool
	// usage is the raw JSON the response carries as usage.input_tokens; empty leaves usage out
	usage   string
	states  []string
	longCtx []string // the long-context header each request carried, "" when absent
	stages  []string // the routing stage each Choice request asked, in order
	srv     *httptest.Server
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
			Type         string          `json:"type"`
			Instructions json.RawMessage `json:"instructions"`
			Criteria     json.RawMessage `json:"criteria"`
		} `json:"questions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return // a chunk request the client abandoned mid-upload once another chunk failed
		}
		f.t.Errorf("fake jev: decode request: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.states = append(f.states, string(req.State))
	f.longCtx = append(f.longCtx, r.Header.Get("TypeSafe-Danger-Zone-Long-Context"))
	pick, level, failDocs, failStage, usage := f.pick, f.level, f.failDocs, f.failStage, f.usage
	f.mu.Unlock()
	if failDocs && strings.HasPrefix(string(req.State), `{"doc`) {
		http.Error(w, "doc rejected", http.StatusBadRequest)
		return
	}
	if _, ok := req.Questions[failStage]; ok {
		f.fail(w, r, failStage)
		return
	}

	answers := map[string]any{}
	for id, q := range req.Questions {
		if q.Type == classifier.TypeNoul {
			answers[id] = map[string]any{"type": q.Type, "noul": 0.5}
			continue
		}
		if q.Type == classifier.TypeScore {
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
		choice, err := stagePick(id, pick, q.Instructions)
		if err != nil {
			f.t.Errorf("fake jev: %s stage: decode instructions: %v", id, err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if _, ok := probs[choice]; !ok {
			f.t.Errorf("fake jev: %s stage: pick %q (%q) is not among the criteria sent", id, pick, choice)
		}
		probs[choice] = 1
		answers[id] = map[string]any{"type": q.Type, "choice": choice, "probabilities": probs, "confidence": 0.9}
		f.mu.Lock()
		f.stages = append(f.stages, id)
		f.mu.Unlock()
	}
	body := map[string]any{"model": req.Model, "answers": answers}
	if usage != "" {
		body["usage"] = map[string]any{"input_tokens": json.RawMessage(usage)}
	}
	_ = json.NewEncoder(w).Encode(body)
}

// fail fails a request asking stage as the fail fields say.
func (f *fakeJev) fail(w http.ResponseWriter, r *http.Request, stage string) {
	switch {
	case f.failHang:
		<-r.Context().Done()
	case f.failDrop:
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			f.t.Errorf("fake jev: hijack: %v", err)
			return
		}
		_ = conn.Close()
	case f.failAnswer == "omit":
		_, _ = io.WriteString(w, `{"answers":{}}`)
	case f.failAnswer != "":
		_, _ = io.WriteString(w, `{"answers":{"`+stage+`":`+f.failAnswer+`}}`)
	case f.failBody != "":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(cmp.Or(f.failStatus, http.StatusBadRequest))
		_, _ = io.WriteString(w, f.failBody)
	default:
		http.Error(w, stage+" stage rejected", cmp.Or(f.failStatus, http.StatusBadRequest))
	}
}

// stagePick is pick's criterion name at the stage asked under id. The cli stage finds the CLI
// whose models in the instructions include pick's section; pick itself is a CLI under model passthrough.
func stagePick(id, pick string, instructions json.RawMessage) (string, error) {
	section, effort, _ := strings.Cut(pick, "@")
	switch id {
	case "model":
		return section, nil
	case "effort":
		return effort, nil
	}
	var in struct {
		CLIs map[string]struct {
			Models map[string]json.RawMessage `json:"models"`
		} `json:"clis"`
	}
	if err := json.Unmarshal(instructions, &in); err != nil {
		return "", fmt.Errorf("decode cli stage instructions: %w", err)
	}
	for cli, c := range in.CLIs {
		if _, ok := c.Models[section]; ok {
			return cli, nil
		}
	}
	return pick, nil
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
	return map[string]any{"type": classifier.TypeScore, "score": score, "legend": legend, "probabilities": probs, "confidence": 0.9}, nil
}

func (f *fakeJev) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.states)
}

// longContextHeaders lists the long-context header of every request, one entry per request.
func (f *fakeJev) longContextHeaders() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.longCtx)
}

// asked lists the routing stages asked, one entry per Choice request.
func (f *fakeJev) asked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.stages)
}

// repeated is state sent once per stage request.
func repeated(state string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = state
	}
	return out
}

// env is an isolated agrouter environment: its own global config dir, working directory, fake Jev
// and helper output directory. Its exec readiness checks find the CLI ready unless a test replaces
// the checker.
type env struct {
	t         *testing.T
	configDir string
	workDir   string
	out       string
	jev       *fakeJev
	checker   ReadinessChecker
}

func newEnv(t *testing.T) *env {
	e := &env{t: t, configDir: t.TempDir(), workDir: t.TempDir(), out: t.TempDir(), jev: newFakeJev(t),
		checker: reportChecker(readyReport)}
	t.Setenv(config.EnvConfigDir, e.configDir)
	t.Setenv(config.EnvAPIKey, testKey)
	t.Setenv(legacyCLIEnv, "")
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
	code := e.app(stdin, &stdout, &stderr, child).run(argv)
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// app is an invocation wired to the environment, for tests that run one app more than once.
func (e *env) app(stdin io.Reader, stdout, stderr io.Writer, child CommandRunner) *app {
	return &app{
		stdin:    stdin,
		stdout:   stdout,
		stderr:   stderr,
		getenv:   os.Getenv,
		workDir:  e.workDir,
		embedded: defaults.Config,
		newClassifier: func(_, key string, longContext bool) router.Classifier {
			return newClassifierClient(e.jev.srv.URL, key, longContext)
		},
		runner:  child,
		checker: e.checker,
		now:     func() time.Time { return testNow },
	}
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

// selectionLog decodes the exec selection line: the prefix, then one JSON object.
func selectionLog(t *testing.T, stderr string) map[string]any {
	t.Helper()
	line, ok := strings.CutPrefix(stderr, selectionPrefix)
	require.True(t, ok, stderr)
	return decision(t, line)
}

// fallbackLog splits exec's stderr after a routing failure into its two lines: the one fallback
// warning, then the decoded selection line.
func fallbackLog(t *testing.T, stderr string) (string, map[string]any) {
	t.Helper()
	warning, rest, ok := strings.Cut(stderr, "\n")
	require.True(t, ok, stderr)
	require.True(t, strings.HasPrefix(warning, "agrouter: warning: Classifier routing failed"), stderr)
	require.Equal(t, 1, strings.Count(rest, "\n"), stderr)
	return warning, selectionLog(t, rest)
}

func strs(v ...string) []any {
	out := make([]any, len(v))
	for i, s := range v {
		out[i] = s
	}
	return out
}

func TestApp_RalphexClaudeMode(t *testing.T) {
	// the prompt comes on stdin and ralphex appends the CLI's own flags after "--"
	const task = "implement task 3\nof the plan\n"
	native := []string{"--dangerously-skip-permissions", "--output-format", "stream-json", "--verbose", "--print"}

	t.Run("model and effort fixed: no Jev call", func(t *testing.T) {
		e := newEnv(t)
		e.fakeCommands()
		r := e.run(slices.Concat([]string{"exec", "claude", "--model", "opus", "--effort", "high", "--"}, native),
			strings.NewReader(task))

		require.Equal(t, 0, r.code, r.stderr)
		assert.Equal(t, map[string]any{"cli": "claude", "model": "claude-opus-5-5", "effort": "high"}, selectionLog(t, r.stderr))
		assert.Empty(t, r.stdout)
		argv, stdin, _ := e.child()
		assert.Equal(t, slices.Concat([]string{"-p"}, native, []string{"--model", "claude-opus-5-5", "--effort", "high"}), argv)
		assert.Equal(t, task, string(stdin))
		assert.Empty(t, e.jev.requests())
	})

	t.Run("Jev chooses", func(t *testing.T) {
		e := newEnv(t)
		e.fakeCommands()
		e.jev.pick = "claude-sonnet-5-5@medium"
		r := e.run(slices.Concat([]string{"exec", "claude", "--"}, native), strings.NewReader(task))

		require.Equal(t, 0, r.code, r.stderr)
		assert.Equal(t, map[string]any{"cli": "claude", "model": "claude-sonnet-5-5", "effort": "medium", "confidence": map[string]any{"model": 0.9, "effort": 0.9}}, selectionLog(t, r.stderr))
		argv, stdin, environ := e.child()
		assert.Equal(t, slices.Concat([]string{"-p"}, native, []string{"--model", "claude-sonnet-5-5", "--effort", "medium"}), argv)
		assert.Equal(t, task, string(stdin), "stdin replayed byte for byte, the prompt never in argv")
		assert.Equal(t, repeated(`{"prompt":"implement task 3\nof the plan\n"}`, 2), e.jev.requests())
		assert.Equal(t, []string{"model", "effort"}, e.jev.asked(), "exec fixes the cli stage")
		for _, kv := range environ {
			assert.False(t, strings.HasPrefix(strings.ToUpper(kv), config.EnvAPIKey+"="), "API key reached the child")
		}
	})
}

func TestApp_RalphexCodexMode(t *testing.T) {
	const task = "review the diff"
	desc := `agents.reviewer.description="general code review specialist; behavior driven by the task argument"`
	native := []string{
		"-c", "features.multi_agent=true",
		"-c", desc,
		"-c", `project_doc_fallback_filenames=["CLAUDE.md"]`,
		"--dangerously-bypass-approvals-and-sandbox",
		"--sandbox", "danger-full-access",
		"-c", "stream_idle_timeout_ms=3600000",
		"-c", `project_doc="/tmp/doc.md"`,
	}

	t.Run("executor", func(t *testing.T) {
		e := newEnv(t)
		e.fakeCommands()
		e.jev.pick = "gpt-6.1-sol@medium"
		r := e.run(slices.Concat([]string{"exec", "codex", "--"}, native), strings.NewReader(task))

		require.Equal(t, 0, r.code, r.stderr)
		assert.Equal(t, map[string]any{"cli": "codex", "model": "gpt-6.1-sol", "effort": "medium", "confidence": map[string]any{"model": 0.9, "effort": 0.9}}, selectionLog(t, r.stderr))
		argv, stdin, _ := e.child()
		assert.Equal(t, slices.Concat([]string{"exec"}, native, []string{"--model", "gpt-6.1-sol", "-c", `model_reasoning_effort="medium"`}), argv)
		assert.Equal(t, task, string(stdin))
	})

	t.Run("model and effort fixed: no Jev call", func(t *testing.T) {
		e := newEnv(t)
		e.fakeCommands()
		r := e.run([]string{"exec", "codex", "--model", "gpt-6.1-sol", "--effort", "xhigh",
			"--", "-c", "stream_idle_timeout_ms=3600000", "--sandbox", "workspace-write"}, strings.NewReader(task))

		require.Equal(t, 0, r.code, r.stderr)
		assert.Equal(t, map[string]any{"cli": "codex", "model": "gpt-6.1-sol", "effort": "xhigh"}, selectionLog(t, r.stderr))
		argv, _, _ := e.child()
		assert.Equal(t, []string{"exec", "-c", "stream_idle_timeout_ms=3600000", "--sandbox", "workspace-write", "--model", "gpt-6.1-sol", "-c", `model_reasoning_effort="xhigh"`}, argv)
		assert.Empty(t, e.jev.requests())
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
			"confidence": map[string]any{"cli": 0.9, "model": 0.9, "effort": 0.9},
		}, decision(t, r.stdout))
		assert.Equal(t, repeated(`{"prompt":"fix the typo in README"}`, 3), e.jev.requests())
		assert.Equal(t, []string{"cli", "model", "effort"}, e.jev.asked())
	})

	t.Run("mentioned file sent to Jev", func(t *testing.T) {
		e := newEnv(t)
		require.NoError(t, os.WriteFile(filepath.Join(e.workDir, "notes.md"), []byte("use opus\n"), 0o600))
		e.jev.pick = "claude-sonnet-5-5@low"
		r := e.run([]string{"summarize notes.md"}, nil)

		require.Equal(t, 0, r.code, r.stderr)
		assert.Equal(t, repeated(`{"prompt":"summarize notes.md","files":["use opus\n"]}`, 3), e.jev.requests())
	})

	t.Run("model without efforts: effort null", func(t *testing.T) {
		e := newEnv(t)
		e.globalConfig(syntheticCLIs(t) + "\n[model.quick]\ncli = alpha\nname = quick-1\nefforts =\ndescription = No effort levels.\n")
		e.jev.pick = "quick"
		r := e.run([]string{"-p", "rename x to y"}, nil)

		require.Equal(t, 0, r.code, r.stderr)
		d := decision(t, r.stdout)
		assert.Equal(t, "quick-1", d["model"])
		assert.Nil(t, d["effort"])
		assert.Contains(t, r.stdout, `"effort":null`)
	})

	t.Run("cannot decide with the CLI known: null model and effort", func(t *testing.T) {
		e := newEnv(t)
		r := e.run([]string{"--verbose", "--cli=claude", "--api-key=", "fix it"}, nil)

		require.Equal(t, 0, r.code, r.stderr)
		assert.Empty(t, r.stderr)
		assert.Subset(t, decision(t, r.stdout), map[string]any{"cli": "claude", "model": nil, "effort": nil,
			"argv": strs("claude", "-p", "--", "fix it")})
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

	t.Run("native tokens never reach the Jev state", func(t *testing.T) {
		e := newEnv(t)
		e.fakeCommands()
		e.jev.pick = "claude-opus-5-5@high"
		r := e.run([]string{"exec", "claude", "--verbose", "plan the migration", "--", "--add-dir", "RAWTOKEN"},
			strings.NewReader("details on stdin"))

		require.Equal(t, 0, r.code, r.stderr)
		assert.Equal(t, repeated(`{"prompt":"plan the migration\n\ndetails on stdin"}`, 2), e.jev.requests())
		argv, _, _ := e.child()
		assert.Equal(t, []string{"-p", "--add-dir", "RAWTOKEN", "--model", "claude-opus-5-5", "--effort", "high", "--", "plan the migration"}, argv)
	})

	t.Run("html characters are not escaped", func(t *testing.T) {
		e := newEnv(t)
		e.jev.pick = "claude-sonnet-5-5@low"
		r := e.run([]string{"--verbose", "a <b> & c"}, nil)

		require.Equal(t, 0, r.code, r.stderr)
		assert.Contains(t, r.stdout, `"a <b> & c"`)
	})
}

func TestNewApp_LongContextReachesClient(t *testing.T) {
	a := newApp(nil, io.Discard, io.Discard)
	for _, want := range []bool{false, true} {
		c, ok := a.newClassifier("http://classifier.test", "key", want).(*classifier.Client)
		require.True(t, ok)
		assert.Equal(t, want, c.LongContext)
		assert.Equal(t, "http://classifier.test", c.URL)
	}
}

func TestApp_LongContext(t *testing.T) {
	tests := []struct {
		name   string
		config string
		header string
	}{
		{name: "off by default"},
		{name: "off when set to false", config: "[classifier]\nlong_context = false\n"},
		{name: "on when set", config: "[classifier]\nlong_context = true\n", header: "true"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.jev.pick = "claude-sonnet-5-5@low"
			if tt.config != "" {
				e.globalConfig(tt.config)
			}
			r := e.run([]string{"fix the typo in README"}, nil)

			require.Equal(t, 0, r.code, r.stderr)
			headers := e.jev.longContextHeaders()
			assert.Equal(t, repeated(tt.header, 3), headers, "every cli, model and effort request alike")
		})
	}
}

// the classifier key resolves flag > env > local > global > embedded, and the source is reported
func TestApp_ClassifierAPIKeyPrecedence(t *testing.T) {
	tests := []struct {
		name     string
		global   string
		local    string
		env      string
		argv     []string
		wantKey  string
		wantExit int
		wantLine string // the key line of the debug output
	}{
		{name: "global config", global: "global-key", env: "", wantKey: "global-key", wantLine: "api key: from global"},
		{name: "local over global", global: "global-key", local: "local-key", env: "", wantKey: "local-key", wantLine: "api key: from local"},
		{name: "env over config", global: "global-key", local: "local-key", env: "env-key", wantKey: "env-key", wantLine: "api key: from env"},
		{name: "flag over env", local: "local-key", env: "env-key", argv: []string{"--api-key=flag-key"},
			wantKey: "flag-key", wantLine: "api key: from flag"},
		{name: "empty flag clears the key", local: "local-key", env: "env-key", argv: []string{"--api-key="},
			wantKey: "", wantLine: "api key: none (flag)", wantExit: exitUsage},
		{name: "embedded without a key", env: "", wantKey: "", wantLine: "api key: none (embedded)", wantExit: exitUsage},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.jev.pick = "claude-sonnet-5-5@low"
			t.Setenv(config.EnvAPIKey, tc.env)
			t.Setenv(envDebug, "1")
			if tc.global != "" {
				e.globalConfig("[classifier]\napi_key = " + tc.global + "\n")
			}
			if tc.local != "" {
				require.NoError(t, os.MkdirAll(filepath.Join(e.workDir, ".agrouter"), 0o750))
				require.NoError(t, os.WriteFile(filepath.Join(e.workDir, ".agrouter", "config"),
					[]byte("[classifier]\napi_key = "+tc.local+"\n"), 0o600))
			}
			var stdout, stderr bytes.Buffer
			a := e.app(nil, &stdout, &stderr, runner.Runner{})
			var keys []string
			factory := a.newClassifier
			a.newClassifier = func(url, key string, longContext bool) router.Classifier {
				keys = append(keys, key)
				return factory(url, key, longContext)
			}

			code := a.run(append(slices.Clone(tc.argv), "fix the typo in README"))

			require.Equal(t, tc.wantExit, code, stderr.String())
			assert.Equal(t, []string{tc.wantKey}, keys)
			assert.Contains(t, stderr.String(), "agrouter debug: "+tc.wantLine+"\n")
		})
	}
}

// TestApp_ClassifierURL pins that the configured url, not a built-in endpoint, reaches the client.
func TestApp_ClassifierURL(t *testing.T) {
	e := newEnv(t)
	e.jev.pick = "claude-sonnet-5-5@low"
	e.globalConfig("[classifier]\nurl = https://classifier.example.test/v1/route\n")
	var stdout, stderr bytes.Buffer
	a := e.app(nil, &stdout, &stderr, runner.Runner{})
	var urls []string
	factory := a.newClassifier
	a.newClassifier = func(url, key string, longContext bool) router.Classifier {
		urls = append(urls, url)
		return factory(url, key, longContext)
	}

	a.run([]string{"fix the typo in README"})

	assert.Equal(t, []string{"https://classifier.example.test/v1/route"}, urls)
}

// TestApp_ClassifierAPIKeyEnvName pins the env var name: the literal name is read, and the name used
// by previous versions is not.
func TestApp_ClassifierAPIKeyEnvName(t *testing.T) {
	tests := []struct {
		name    string
		env     string
		wantKey string
	}{
		{name: "classifier env var", env: "AGROUTER_CLASSIFIER_API_KEY", wantKey: "env-key"},
		{name: "previous env var is not read", env: "TYPESAFE_API_KEY", wantKey: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.jev.pick = "claude-sonnet-5-5@low"
			t.Setenv(config.EnvAPIKey, "")
			t.Setenv(tc.env, "env-key")
			var stdout, stderr bytes.Buffer
			a := e.app(nil, &stdout, &stderr, runner.Runner{})
			var keys []string
			factory := a.newClassifier
			a.newClassifier = func(url, key string, longContext bool) router.Classifier {
				keys = append(keys, key)
				return factory(url, key, longContext)
			}

			a.run([]string{"fix the typo in README"})

			assert.Equal(t, []string{tc.wantKey}, keys)
		})
	}
}

func TestApp_VerboseOutput(t *testing.T) {
	e := newSyntheticEnv(t)
	e.jev.pick = "fast@low"
	e.jev.level = "5.1"
	require.NoError(t, os.WriteFile(filepath.Join(e.workDir, "project.md"), []byte("project doc"), 0o600))
	var states []string
	for _, verbose := range []bool{false, true} {
		argv := []string{"--doc=project.md", "private task"}
		if verbose {
			argv = append([]string{"--verbose"}, argv...)
		}
		r := e.run(argv, nil)
		require.Equal(t, 0, r.code, r.stderr)
		assert.Empty(t, r.stderr)
		output := r.stdout
		d := decision(t, output)
		assert.Equal(t, "alpha", d["cli"])
		assert.Equal(t, "fast-1", d["model"])
		assert.Equal(t, "low", d["effort"])
		assert.Equal(t, "5.1/10", d["project_complexity"])
		assert.NotContains(t, output, "project doc")
		confidence := d["confidence"].(map[string]any)
		for _, level := range []string{"cli", "model", "effort"} {
			assert.InDelta(t, 0.9, confidence[level], 1e-9, level)
		}
		assert.InDelta(t, 0.9, confidence["project_complexity"], 1e-9)
		if verbose {
			assert.Contains(t, confidence, "complexity_chunks")
			stages := confidence["stages"].([]any)
			require.Len(t, stages, 3)
			for i, want := range [][2]string{{"cli", "alpha"}, {"model", "fast"}, {"effort", "low"}} {
				stage := stages[i].(map[string]any)
				assert.Subset(t, stage, map[string]any{"level": want[0], "choice": want[1], "skipped": false, "confidence": 0.9})
				assert.InDelta(t, 1, stage["probabilities"].(map[string]any)[want[1]], 1e-9)
			}
			chunks := confidence["complexity_chunks"].([]any)
			require.Len(t, chunks, 1)
			assert.Subset(t, chunks[0], map[string]any{"index": float64(1), "of": float64(1),
				"project_complexity": "5.1/10", "evidence": 0.5, "confidence": 0.9})
			assert.InDelta(t, 5.1, chunks[0].(map[string]any)["score"], 1e-9)
			assert.Contains(t, d, "options")
			assert.Contains(t, d, "argv")
			assert.NotContains(t, d, "skipped")
			assert.Equal(t, false, d["stdin_required"])
			assert.Contains(t, d["command"], "private task")
			assert.Contains(t, output, "\n  ")
		} else {
			assert.Len(t, confidence, 4)
			assert.Len(t, d, 5)
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
}

func TestApp_ExecVerbosity(t *testing.T) {
	// --verbose before "--" is agrouter's and never changes the concise selection line; after "--" it is
	// the child's own option and is forwarded as given
	tests := []struct {
		name                   string
		agrouterVerbose, child bool
	}{
		{"neither", false, false},
		{"agrouter only", true, false},
		{"child only", false, true},
		{"both", true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newSyntheticEnv(t)
			e.jev.pick = "fast@low"
			e.jev.level = "5.1"
			require.NoError(t, os.WriteFile(filepath.Join(e.workDir, "project.md"), []byte("project doc"), 0o600))
			argv := []string{"exec", "alpha", "--doc=project.md", "private task"}
			if tc.agrouterVerbose {
				argv = append(argv, "--verbose")
			}
			if tc.child {
				argv = append(argv, "--", "--verbose")
			}
			r := e.run(argv, nil)

			require.Equal(t, 0, r.code, r.stderr)
			assert.Empty(t, r.stdout)
			assert.Equal(t, map[string]any{"cli": "alpha", "model": "fast-1", "effort": "low", "project_complexity": "5.1/10",
				"confidence": map[string]any{"model": 0.9, "effort": 0.9, "project_complexity": 0.9}}, selectionLog(t, r.stderr))
			assert.NotContains(t, r.stderr, "private task")
			assert.NotContains(t, r.stderr, "project doc")
			childArgv, _, _ := e.child()
			if tc.child {
				assert.Equal(t, []string{"-p", "--verbose", "--model", "fast-1", "--effort", "low", "--", "private task"}, childArgv)
			} else {
				assert.NotContains(t, childArgv, "--verbose")
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
		oneLine(t, e.run([]string{"exec", "claude"}, strings.NewReader("")), "agrouter: no prompt")
		oneLine(t, e.run([]string{"exec", "claude", "--", "fix it"}, strings.NewReader("")), "agrouter: no prompt")
	})

	t.Run("cannot decide with the CLI unknown", func(t *testing.T) {
		e := newEnv(t)
		r := e.run([]string{"--api-key=", "fix it"}, nil)
		assert.Equal(t, 2, r.code)
		assert.Equal(t, 1, strings.Count(r.stderr, "\n"), r.stderr)
		assert.True(t, strings.HasPrefix(r.stderr, "agrouter: classifier cannot decide between claude, codex"), r.stderr)
		d := decision(t, r.stdout)
		assert.Subset(t, d, map[string]any{"cli": nil, "model": nil, "effort": nil})
		assert.Equal(t, map[string]any{"code": "classifier_no_key", "stage": "cli",
			"message": "Classifier CLI selection failed: no classifier API key is set"}, d["reason"])
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
		assert.NotContains(t, out, "skipped")
	})

	t.Run("exec CLI must be configured and enabled", func(t *testing.T) {
		e := newSyntheticEnv(t)
		e.globalConfig(syntheticCLIs(t) + "\n[cli.beta]\nenabled = false\n")
		for _, name := range []string{"nope", "beta"} {
			oneLine(t, e.run([]string{"exec", name, "fix it"}, nil), "agrouter: exec "+name+": not a configured, enabled CLI")
		}
		assert.Empty(t, e.jev.requests())
		assert.NoFileExists(t, filepath.Join(e.out, "argv.json"), "the child must not run")
	})

	t.Run("exec usage errors", func(t *testing.T) {
		e := newSyntheticEnv(t)
		oneLine(t, e.run([]string{"exec"}, nil), "agrouter: exec needs the CLI to run")
		oneLine(t, e.run([]string{"exec", "alpha", "--cli=beta", "fix it"}, nil), "agrouter: --cli beta conflicts with exec alpha")
		oneLine(t, e.run([]string{"exec", "alpha", "--sandbox", "read-only", "fix it"}, nil), "agrouter: unknown flag `sandbox'")
		oneLine(t, e.run([]string{"fix it", "--"}, nil), `agrouter: "--" is only accepted with exec`)
		assert.Empty(t, e.jev.requests())
	})
}

func TestApp_LegacyCLIEnvIgnored(t *testing.T) {
	const warning = "agrouter: warning: AGROUTER_CLI is ignored: use exec CLI to run a CLI, or --cli=NAME to restrict a decision\n"

	t.Run("decision", func(t *testing.T) {
		e := newSyntheticEnv(t)
		t.Setenv(legacyCLIEnv, "beta")
		e.jev.pick = "fast@low"
		r := e.run([]string{"fix it"}, nil)

		require.Equal(t, 0, r.code, r.stderr)
		assert.Equal(t, warning, r.stderr)
		assert.Equal(t, "alpha", decision(t, r.stdout)["cli"], "routing still spans every CLI")
		assert.Equal(t, []string{"cli", "model", "effort"}, e.jev.asked())
	})

	t.Run("exec", func(t *testing.T) {
		e := newSyntheticEnv(t)
		t.Setenv(legacyCLIEnv, "beta")
		e.jev.pick = "fast@low"
		r := e.run([]string{"exec", "alpha", "fix it"}, nil)

		require.Equal(t, 0, r.code, r.stderr)
		log, ok := strings.CutPrefix(r.stderr, warning)
		require.True(t, ok, r.stderr)
		assert.Equal(t, "alpha", selectionLog(t, log)["cli"])
	})
}

// syntheticCLIs disables the shipped CLIs and defines two made-up ones, both running the helper with
// -p/--model/--effort descriptors: alpha (fast@low|high and deep@low|high, deep aliased "strong") and
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
command               = COMMAND
description           = A coding agent that exists only in this test.
exec_args             = ["-p"]
model_args            = ["--model", "{model}"]
effort_args           = ["--effort", "{effort}"]
prompt_args           = ["--", "{prompt}"]

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
				assert.Equal(t, repeated(jevPrompt(t, tc.wantJev), 2), e.jev.requests())
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
		assert.Equal(t, repeated(jevPrompt(t, "flag text\n\npositional text\n\nfile text\n\n\nstdin text"), 2), e.jev.requests())
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
		assert.Equal(t, repeated(`{"prompt":"follow notes.md","files":["use opus\n"]}`, 2), e.jev.requests())
	})

	t.Run("exec: native child gets the explicit prompt in argv and stdin replayed", func(t *testing.T) {
		e := newSyntheticEnv(t)
		writeFile(t, e.workDir, "task.md", "line one\nline two\n")
		r := e.run([]string{"exec", "alpha", "--model=strong", "--effort=low", "-p", "flag", "--prompt-file", "task.md"},
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
		assert.Equal(t, append([]string{`{"docs":["DOC TEXT, see notes.md\n"]}`},
			repeated(`{"prompt":"fix it","project":{"complexity":8}}`, 2)...),
			e.jev.requests(), "the docs are not read for mentions and not resent")
		assert.Equal(t, "fast-1", decision(t, r.stdout)["model"])
		assert.NotContains(t, r.stdout, "DOC TEXT")
		assert.Equal(t, "8.0/10", decision(t, r.stdout)["project_complexity"])
		assert.Equal(t, map[string]any{"model": 0.9, "effort": 0.9, "project_complexity": 0.9}, decision(t, r.stdout)["confidence"])
	})

	t.Run("decision: no docs, no complexity stage and no project", func(t *testing.T) {
		e := newSyntheticEnv(t)
		e.jev.pick = "fast@low"
		r := e.run([]string{"--cli=alpha", "fix it"}, nil)

		require.Equal(t, 0, r.code, r.stderr)
		assert.Equal(t, repeated(jevPrompt(t, "fix it"), 2), e.jev.requests())
	})

	t.Run("decision: an empty doc has nothing to score", func(t *testing.T) {
		e := newSyntheticEnv(t)
		writeFile(t, e.workDir, "empty.md", "")
		e.jev.pick = "fast@low"
		r := e.run([]string{"--cli=alpha", "--doc", "empty.md", "fix it"}, nil)

		require.Equal(t, 0, r.code, r.stderr)
		assert.Equal(t, repeated(jevPrompt(t, "fix it"), 2), e.jev.requests())
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
		r := e.run([]string{"exec", "alpha", "--doc", "project.md", "fix it"}, nil)

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
		assert.Equal(t, 1, strings.Count(r.stderr, "\n"), r.stderr)
		assert.True(t, strings.HasPrefix(r.stderr, "agrouter: "), r.stderr)
		assert.NotContains(t, r.stderr, "DOC TEXT")
		assert.NotContains(t, r.stdout, "DOC TEXT")
		d := decision(t, r.stdout)
		assert.Nil(t, d["cli"])
		assert.Equal(t, map[string]any{"code": "classifier_api_error", "stage": "complexity",
			"message": "Classifier project complexity scoring failed: the classifier returned an error status"}, d["reason"])
	})

	t.Run("exec: the deadline passing while reading a doc runs the known CLI with its defaults", func(t *testing.T) {
		e := newEnv(t)
		e.globalConfig(syntheticCLIs(t) + "\n[agrouter]\ntimeout = 1ns\n")
		writeFile(t, e.workDir, "project.md", "DOC TEXT\n")
		r := e.run([]string{"exec", "alpha", "--doc", "project.md", "fix it"}, nil)

		require.Equal(t, 0, r.code, r.stderr)
		argv, _, _ := e.child()
		assert.Equal(t, []string{"-p", "--", "fix it"}, argv)
		assert.Empty(t, e.jev.requests())
	})

	t.Run("exec: a missing doc after the deadline passed is still a bad doc", func(t *testing.T) {
		e := newEnv(t)
		e.globalConfig(syntheticCLIs(t) + "\n[agrouter]\ntimeout = 1ns\n")
		writeFile(t, e.workDir, "project.md", "DOC TEXT\n")
		r := e.run([]string{"exec", "alpha", "--doc", "project.md", "--doc", "nope.md", "fix it"}, nil)

		assert.Equal(t, 2, r.code)
		assert.Equal(t, "agrouter: --doc: nope.md: no such file\n", r.stderr)
		assert.NoFileExists(t, filepath.Join(e.out, "argv.json"), "the child must not run")
	})

	t.Run("exec: the child gets neither the docs nor their paths", func(t *testing.T) {
		e := newSyntheticEnv(t)
		writeFile(t, e.workDir, "a.md", "DOC A\n")
		writeFile(t, e.workDir, "b.md", "DOC B\n")
		r := e.run([]string{"exec", "alpha", "--model=strong", "--effort=low", "--doc", "a.md", "--doc=b.md", "-p", "flag"},
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
	pinned := []string{"exec", "alpha", "--model=strong", "--effort=low"}

	tests := []struct {
		name   string
		argv   []string
		stdin  io.Reader
		prefix string
	}{
		{"all empty", []string{"-p", "", ""}, strings.NewReader(""), "agrouter: no prompt: give -p, a positional prompt, --prompt-file or stdin"},
		{"second -p", []string{"-p", "a", "-p", "b"}, nil, "agrouter: -p/--prompt/--print given more than once"},
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
		assert.Equal(t, want, selectionLog(t, c.Stderr.(*bytes.Buffer).String()), "selection must be logged before the child starts")
		assert.Empty(t, c.Stdout.(*bytes.Buffer).String())
		assert.Contains(t, c.Argv, "private prompt")
		assert.Contains(t, c.Argv, "private raw token")
		_, err := fmt.Fprintln(c.Stdout, `{"child":"output"}`)
		require.NoError(t, err)
		_, err = fmt.Fprintln(c.Stderr, "child diagnostic")
		require.NoError(t, err)
		return 7, nil
	}}
	r := e.runWithRunner([]string{"exec", "acme", "--model=preferred", "--effort=thorough",
		"--api-key=private-api-key", "private prompt", "--", "private raw token"}, nil, child)
	assert.Equal(t, 7, r.code)
	assert.JSONEq(t, `{"child":"output"}`, r.stdout)
	line, rest, ok := strings.Cut(r.stderr, "\n")
	require.True(t, ok)
	assert.Equal(t, want, selectionLog(t, line+"\n"))
	assert.Equal(t, "child diagnostic\n", rest)
	assert.Len(t, child.RunCalls(), 1)
	assert.Empty(t, e.jev.requests())
}

func TestApp_Exec(t *testing.T) {
	t.Run("child exit code", func(t *testing.T) {
		e := newEnv(t)
		e.fakeCommands()
		t.Setenv(helperExit, "7")
		r := e.run([]string{"exec", "claude", "--model=opus", "--effort=low", "fix it"}, nil)

		assert.Equal(t, 7, r.code)
		assert.Empty(t, r.stdout)
		assert.Equal(t, map[string]any{"cli": "claude", "model": "claude-opus-5-5", "effort": "low"},
			selectionLog(t, r.stderr))
		argv, stdin, _ := e.child()
		assert.Equal(t, []string{"-p", "--model", "claude-opus-5-5", "--effort", "low", "--", "fix it"}, argv)
		assert.Empty(t, stdin)
	})

	t.Run("missing command: 127", func(t *testing.T) {
		e := newEnv(t)
		e.globalConfig("[cli.claude]\ncommand = agrouter-test-no-such-command\n")
		r := e.run([]string{"exec", "claude", "--model=opus", "--effort=low", "fix it"}, nil)

		assert.Equal(t, runner.ExitStartFailure, r.code)
		assert.Empty(t, r.stdout)
		line, rest, ok := strings.Cut(r.stderr, "\n")
		require.True(t, ok)
		assert.Equal(t, "claude", selectionLog(t, line+"\n")["cli"])
		assert.Equal(t, 1, strings.Count(rest, "\n"), rest)
		assert.True(t, strings.HasPrefix(rest, "agrouter: cannot start "), rest)
	})

	t.Run("large stdin is routed in chunks and reaches the child whole", func(t *testing.T) {
		e := newEnv(t)
		e.fakeCommands()
		e.jev.pick = "claude-sonnet-5-5@low"
		input := strings.Repeat("line of a long ralphex prompt\n", 100_000) // 3 MB, no capture limit
		r := e.run([]string{"exec", "claude"}, strings.NewReader(input))

		require.Equal(t, 0, r.code, r.stderr)
		_, stdin, _ := e.child()
		assert.Equal(t, input, string(stdin))
		assert.Greater(t, len(e.jev.requests()), 1, "one request per chunk")
		log := selectionLog(t, r.stderr)
		confidence, ok := log["confidence"].(map[string]any)
		require.True(t, ok)
		assert.InDelta(t, 0.9, confidence["model"], 1e-9, "the mean of the chunk confidences")
		assert.InDelta(t, 0.9, confidence["effort"], 1e-9)
		assert.NotContains(t, confidence, "cli", "exec fixes the cli stage")
		assert.NotContains(t, confidence, "project_complexity", "no document answers were recorded")
		assert.Subset(t, log, map[string]any{"cli": "claude", "model": "claude-sonnet-5-5", "effort": "low", "confidence": confidence})
	})
}

func TestApp_ExecNativeTail(t *testing.T) {
	tests := []struct {
		name   string
		native []string
	}{
		{"separator with empty tail", []string{}},
		{"empty tokens, spaces, repeats and a later --", []string{"", "two words", "-v", "-v", "--", "--print", "/abs"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newSyntheticEnv(t)
			r := e.run(slices.Concat([]string{"exec", "alpha", "--model=strong", "--effort=low", "--"}, tc.native),
				strings.NewReader("prompt on stdin\x00\r\n"))

			require.Equal(t, 0, r.code, r.stderr)
			argv, stdin, _ := e.child()
			prefix, tail := tc.native, []string(nil)
			if i := slices.Index(prefix, "--"); i >= 0 {
				prefix, tail = prefix[:i], prefix[i:]
			}
			assert.Equal(t, slices.Concat([]string{"-p"}, prefix, []string{"--model", "deep-1", "--effort", "low"}, tail), argv)
			assert.Equal(t, "prompt on stdin\x00\r\n", string(stdin))
			assert.Empty(t, e.jev.requests())
		})
	}
}

func TestApp_ExecNativeSelections(t *testing.T) {
	tests := []struct {
		name              string
		own, native, want []string
		jev               bool
	}{
		{name: "native model never constrains Jev", native: []string{"-m", "native-model"},
			want: []string{"--model", "fast-1", "--effort", "low"}, jev: true},
		{name: "caller short model wins", own: []string{"-m", "strong", "--effort=low"}, native: []string{"-m", "native-model"},
			want: []string{"--model", "deep-1", "--effort", "low"}},
		{name: "caller model outside catalog wins", own: []string{"-m", "model1", "--effort=low"}, native: []string{"-m", "model2", "--effort=high"},
			want: []string{"--model", "model1", "--effort", "low"}},
		{name: "equals and attached selections replaced", native: []string{"--model=native", "-mother", "--effort=native", "--json"},
			want: []string{"--json", "--model", "fast-1", "--effort", "low"}, jev: true},
		{name: "native settings and config kept", native: []string{"--profile", "work", "-c", `model="native"`},
			want: []string{"--profile", "work", "-c", `model="native"`, "--model", "fast-1", "--effort", "low"}, jev: true},
		{name: "selection-shaped positionals after child separator", native: []string{"--json", "--", "--model", "text"},
			want: []string{"--json", "--model", "fast-1", "--effort", "low", "--", "--model", "text"}, jev: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newSyntheticEnv(t)
			e.jev.pick = "fast@low"
			r := e.run(slices.Concat([]string{"exec", "alpha"}, tc.own, []string{"--"}, tc.native), strings.NewReader("task on stdin"))
			require.Equal(t, exitOK, r.code, r.stderr)
			child, stdin, _ := e.child()
			assert.Equal(t, slices.Concat([]string{"-p"}, tc.want), child)
			assert.Equal(t, "task on stdin", string(stdin))
			if tc.jev {
				assert.Equal(t, repeated(`{"prompt":"task on stdin"}`, 2), e.jev.requests())
			} else {
				assert.Empty(t, e.jev.requests())
			}
			assert.NotContains(t, r.stderr, "native-model")
		})
	}
	t.Run("prompt argument and native separator remain a usage error", func(t *testing.T) {
		e := newSyntheticEnv(t)
		r := e.run([]string{"exec", "alpha", "fix it", "--", "--json", "--", "x"}, nil)
		assert.Equal(t, 2, r.code)
		assert.Contains(t, r.stderr, `the native arguments after "--" hold another "--"`)
		assert.Empty(t, e.jev.requests())
		assert.NoFileExists(t, filepath.Join(e.out, "argv.json"))
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
exec_args   = ["run", "--quiet"]
model_args  = ["--llm", "{model}"]
effort_args = ["--think={effort}"]
prompt_args = ["--", "{prompt}"]

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
			r := e.run([]string{"exec", tc.cli, "do it now", "--", "--events", "ndjson", "--mode", "read"},
				strings.NewReader(stdin))
			require.Equal(t, 0, r.code, r.stderr)
			log := r.stderr
			assert.Equal(t, map[string]any{"cli": tc.cli, "model": tc.model, "effort": tc.effort,
				"confidence": map[string]any{"model": 0.9, "effort": 0.9}}, selectionLog(t, log))
			got, gotStdin, _ := e.child()
			assert.Equal(t, []string{"run", "--quiet", "--events", "ndjson", "--mode", "read", "--llm", tc.model, "--think=" + tc.effort, "--", "do it now"}, got)
			assert.Equal(t, stdin, string(gotStdin))
			require.Len(t, e.jev.requests(), 2, "one CLI: model and effort asked")

			r = e.run([]string{"--verbose", "--cli", tc.cli, "--model", "preferred", "--effort", tc.effort, "do it"}, nil)
			require.Equal(t, 0, r.code, r.stderr)
			d := decision(t, r.stdout)
			assert.Equal(t, tc.cli, d["cli"])
			assert.Equal(t, tc.model, d["model"])
			assert.Equal(t, tc.effort, d["effort"])
			assert.Equal(t, strs(helperCommand(t), "run", "--quiet", "--llm", tc.model,
				"--think="+tc.effort, "--", "do it"), d["argv"])
			assert.Len(t, e.jev.requests(), 2, "a model alias plus an effort selects one option without Jev")
		})
	}
}

func TestApp_HelpHasKeyCautions(t *testing.T) {
	e := newEnv(t)
	r := e.run([]string{"--help"}, nil)

	require.Equal(t, 0, r.code)
	for _, want := range []string{"AGROUTER_CLASSIFIER_API_KEY", "process", "listings", ".agrouter/config"} {
		assert.Contains(t, r.stdout, want)
	}
}

func TestApp_InstallsConfig(t *testing.T) {
	t.Run("first run", func(t *testing.T) {
		e := newEnv(t)
		// passthrough values fix all stages without relying on the shipped model catalog
		r := e.run([]string{"--cli", defaultsCLI(t), "--model=custom-model", "--effort=custom-effort", "do it"}, nil)
		require.Equal(t, exitOK, r.code, r.stderr)
		assert.Empty(t, r.stderr)
		assert.True(t, json.Valid([]byte(r.stdout)))

		path := filepath.Join(e.configDir, "config")
		data, err := os.ReadFile(path) //nolint:gosec // isolated test config
		require.NoError(t, err)
		assert.NotEmpty(t, data)
		for line := range strings.SplitSeq(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			assert.True(t, trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";"), line)
		}
		assert.NoFileExists(t, filepath.Join(e.workDir, ".agrouter", "config"), "only the global config is installed")
	})
	for _, flag := range []string{"--help", "--version"} {
		t.Run(flag, func(t *testing.T) {
			e := newEnv(t)
			r := e.run([]string{flag}, nil)
			require.Equal(t, exitOK, r.code, r.stderr)
			assert.NoFileExists(t, filepath.Join(e.configDir, "config"))
		})
	}
	t.Run("installation error", func(t *testing.T) {
		e := newEnv(t)
		blocked := filepath.Join(e.configDir, "blocked")
		require.NoError(t, os.WriteFile(blocked, []byte("file"), 0o600))
		t.Setenv(config.EnvConfigDir, blocked)
		r := e.run([]string{"do it"}, nil)
		assert.Equal(t, 2, r.code)
		assert.Empty(t, r.stdout)
		assert.True(t, strings.HasPrefix(r.stderr, "agrouter: config: create global config directory:"), r.stderr)
		assert.Equal(t, 1, strings.Count(r.stderr, "\n"))
		assert.Empty(t, e.jev.requests())
	})
}

// defaultsCLI picks an enabled CLI without pinning the contents of the embedded catalog.
func defaultsCLI(t *testing.T) string {
	t.Helper()
	cfg, err := config.Load(config.Sources{Embedded: defaults.Config})
	require.NoError(t, err)
	require.NotEmpty(t, cfg.CLIs)
	return cfg.CLIs[0].Name
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

// mirrorPath is the console log in the default log directory of the environment's work directory.
func (e *env) mirrorPath() string {
	return filepath.Join(e.workDir, ".agrouter", "logs", logFileName)
}

var logStamp = regexp.MustCompile(`^\S+ pid=\d+ `)

// mirrored reads the console log at path and returns its lines without their stamps, which must be there.
func mirrored(t *testing.T, path string) string {
	t.Helper()
	var out strings.Builder
	for line := range strings.Lines(readFile(t, path)) {
		loc := logStamp.FindStringIndex(line)
		require.NotNil(t, loc, "unstamped log line %q", line)
		out.WriteString(line[loc[1]:])
	}
	return out.String()
}

// skippedCLIWarning is the warning for an unknown --cli, a line every invocation can produce.
func skippedCLIWarning(name string) string {
	return "agrouter: warning: skipped --cli " + name + ": not an enabled CLI; routing across every CLI\n"
}

func TestApp_MirrorLog(t *testing.T) {
	skipWarning := skippedCLIWarning("nope")
	nopeArgv := []string{"--cli=nope", "fix it"}

	t.Run("agrouter's lines are copied, the child's and stdout are not", func(t *testing.T) {
		const (
			promptMarker = "PROMPT-MARKER"
			argvMarker   = "ARGV-MARKER"
			keyMarker    = "KEY-MARKER"
		)
		e := newEnv(t)
		e.globalConfig(madeUpConfig(t, "acme", "test-child", "large", "future-model", "thorough"))
		t.Setenv(envDebug, "1")
		t.Setenv(legacyCLIEnv, "alpha")
		child := &mocks.CommandRunnerMock{RunFunc: func(_ context.Context, c runner.Command) (int, error) {
			_, _ = fmt.Fprintln(c.Stdout, "CHILD-STDOUT")
			_, _ = fmt.Fprintln(c.Stderr, "CHILD-STDERR")
			return 3, nil
		}}
		r := e.runWithRunner([]string{"exec", "acme", "--model=preferred", "--effort=thorough",
			"--api-key=" + keyMarker, promptMarker, "--", argvMarker}, nil, child)
		require.Equal(t, 3, r.code, r.stderr)
		assert.Equal(t, "CHILD-STDOUT\n", r.stdout)

		log := mirrored(t, e.mirrorPath())
		assert.Equal(t, strings.Replace(r.stderr, "CHILD-STDERR\n", "", 1), log,
			"every line agrouter wrote, in order: warning, debug, selection")
		for _, want := range []string{"agrouter: warning: " + legacyCLIEnv, "agrouter debug: ", selectionPrefix} {
			assert.Contains(t, log, want)
		}
		raw := readFile(t, e.mirrorPath())
		for _, secret := range []string{promptMarker, argvMarker, keyMarker, "CHILD-STDOUT", "CHILD-STDERR"} {
			assert.NotContains(t, raw, secret)
		}
		assert.True(t, strings.HasPrefix(raw, testNow.UTC().Format(time.RFC3339)+" pid="+strconv.Itoa(os.Getpid())+" "), raw)
	})

	t.Run("decision JSON stays on stdout", func(t *testing.T) {
		e := newSyntheticEnv(t)
		e.jev.pick = "fast@low"
		r := e.run([]string{"--verbose", "--cli=nope", "PROMPT-MARKER"}, nil)
		require.Equal(t, 0, r.code, r.stderr)
		require.Contains(t, r.stdout, "PROMPT-MARKER", "the verbose decision carries the argv")

		assert.Equal(t, skipWarning, mirrored(t, e.mirrorPath()))
		raw := readFile(t, e.mirrorPath())
		assert.NotContains(t, raw, "PROMPT-MARKER")
		assert.NotContains(t, raw, `"argv"`)
	})

	t.Run("a caller that drops stderr still has the selection in the log", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git is not on PATH")
		}
		e := newEnv(t)
		e.globalConfig(madeUpConfig(t, "acme", "test-child", "large", "future-model", "thorough"))
		git := func(args ...string) error {
			cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", e.workDir}, args...)...)
			cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
			return cmd.Run()
		}
		require.NoError(t, git("init", "--quiet"))
		require.NoError(t, os.MkdirAll(filepath.Join(e.workDir, ".agrouter"), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(e.workDir, ".agrouter", "config"), []byte("[agrouter]\n"), 0o600))

		child := &mocks.CommandRunnerMock{RunFunc: func(context.Context, runner.Command) (int, error) { return 0, nil }}
		var stdout bytes.Buffer
		code := e.app(nil, &stdout, io.Discard, child).run([]string{"exec", "acme", "--model=preferred", "--effort=thorough", "fix it"})
		require.Equal(t, exitOK, code)

		assert.Contains(t, mirrored(t, e.mirrorPath()), selectionPrefix)
		require.NoError(t, git("check-ignore", "--quiet", filepath.Join(".agrouter", "logs", logFileName)),
			"the log stays out of git")
		var exitErr *exec.ExitError
		require.ErrorAs(t, git("check-ignore", "--quiet", filepath.Join(".agrouter", "config")), &exitErr,
			"the local config stays trackable")
		assert.Equal(t, 1, exitErr.ExitCode(), "exit 1 means not ignored")
	})

	t.Run("readiness blocks are copied", func(t *testing.T) {
		tests := []struct {
			name string
			env  func(t *testing.T) *env
			want string
		}{
			{"before routing", func(t *testing.T) *env {
				e := newSyntheticEnv(t)
				e.globalConfig(syntheticCLIs(t) + usageCheck("error_message = Limit reached"))
				e.checker = reportChecker(func(cli config.CLI) readiness.Report {
					return withQuota(cli, free(readiness.WindowFiveHour), exhausted(readiness.WindowWeekly, 24*time.Hour))
				})
				return e
			}, routingSkipped("subscription quota exhausted")},
			{"after routing", func(t *testing.T) *env {
				e := failingEnv(t, "effort", func(*fakeJev) {})
				e.checker = defaultsChecker("deep-1", "", exhausted(readiness.WindowWeekly, time.Hour, "strong"))
				return e
			}, runSkipped("subscription quota exhausted")},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				e := tc.env(t)
				r := e.runWithRunner([]string{"exec", "alpha", "fix it"}, nil, noChild(t))
				require.Equal(t, runner.ExitStartFailure, r.code, r.stderr)
				assert.Empty(t, r.stdout)

				assert.Equal(t, r.stderr, mirrored(t, e.mirrorPath()))
				assert.Contains(t, r.stderr, tc.want)
			})
		}
	})

	t.Run("invocations that fail before the config is ready write nothing", func(t *testing.T) {
		tests := []struct {
			name   string
			config string
			argv   []string
		}{
			{"usage error", "", []string{"--no-such-flag"}},
			{"invalid config", "\n[classifier]\nrequest_tokens = 0\n", []string{"fix it"}},
			{"request_tokens over the provider limit", "\n[classifier]\nrequest_tokens = 40000\n", []string{"fix it"}},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				e := newEnv(t)
				e.globalConfig(syntheticCLIs(t) + tc.config)
				r := e.run(tc.argv, nil)
				assert.Equal(t, exitUsage, r.code)
				assert.NotEmpty(t, r.stderr)
				assert.NoDirExists(t, filepath.Join(e.workDir, ".agrouter"))
			})
		}
	})

	t.Run("one app run repeatedly does not stack mirrors or warnings", func(t *testing.T) {
		e := newSyntheticEnv(t)
		e.jev.pick = "fast@low"
		var stdout, stderr bytes.Buffer
		a := e.app(nil, &stdout, &stderr, runner.Runner{})
		for range 3 {
			require.Equal(t, exitOK, a.run(nopeArgv), stderr.String())
		}

		assert.Equal(t, strings.Repeat(skipWarning, 3), stderr.String())
		assert.Equal(t, strings.Repeat(skipWarning, 3), mirrored(t, e.mirrorPath()), "each run copies its own line once")
		assert.Same(t, &stderr, a.stderr, "the console is restored")
		assert.Nil(t, a.childStderr)
		assert.Equal(t, ".gitignore\nlogs/\n", readFile(t, filepath.Join(e.workDir, ".agrouter", ".gitignore")),
			"the ignore rules are written once")
		assert.NoFileExists(t, filepath.Join(e.workDir, ".agrouter", "config"))
	})

	t.Run("log_dir", func(t *testing.T) {
		abs := filepath.Join(t.TempDir(), "elsewhere")
		tests := []struct {
			name    string
			logDir  string
			dir     func(e *env) string
			ignored bool
		}{
			{"relative", "custom/logs", func(e *env) string { return filepath.Join(e.workDir, "custom", "logs") }, false},
			{"absolute", abs, func(*env) string { return abs }, false},
			{"another directory under .agrouter", ".agrouter/other", func(e *env) string {
				return filepath.Join(e.workDir, ".agrouter", "other")
			}, false},
			{"the default directory spelled out", "./.agrouter/logs", func(e *env) string {
				return filepath.Join(e.workDir, ".agrouter", "logs")
			}, true},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				e := newEnv(t)
				e.globalConfig(syntheticCLIs(t) + "\n[agrouter]\nlog_dir = " + tc.logDir + "\n")
				e.jev.pick = "fast@low"
				r := e.run(nopeArgv, nil)
				require.Equal(t, exitOK, r.code, r.stderr)

				dir := tc.dir(e)
				assert.Equal(t, skipWarning, mirrored(t, filepath.Join(dir, logFileName)))
				ignore := filepath.Join(e.workDir, ".agrouter", ".gitignore")
				if tc.ignored {
					assert.FileExists(t, ignore)
				} else {
					assert.NoFileExists(t, ignore, "a custom log_dir is the user's to manage")
					assert.NoFileExists(t, filepath.Join(dir, ".gitignore"))
				}
			})
		}
	})

	t.Run("concurrent invocations keep their lines intact", func(t *testing.T) {
		e := newSyntheticEnv(t)
		e.jev.pick = "fast@low"
		const invocations = 12
		var wg sync.WaitGroup
		for i := range invocations {
			wg.Go(func() {
				r := e.run([]string{"--cli=nope" + strconv.Itoa(i), "fix it"}, nil)
				assert.Equal(t, exitOK, r.code, r.stderr)
			})
		}
		wg.Wait()

		want := make([]string, 0, invocations)
		for i := range invocations {
			want = append(want, strings.TrimSuffix(skippedCLIWarning("nope"+strconv.Itoa(i)), "\n"))
		}
		got := strings.Split(strings.TrimSuffix(mirrored(t, e.mirrorPath()), "\n"), "\n")
		assert.ElementsMatch(t, want, got)
		// first runs racing on a fresh directory may each append the rules; only the rules are there
		ignore := readFile(t, filepath.Join(e.workDir, ".agrouter", ".gitignore"))
		assert.True(t, endsWithLines([]byte(ignore), logIgnoreRules), ignore)
		for line := range strings.Lines(ignore) {
			assert.Contains(t, logIgnoreRules, strings.TrimSuffix(line, "\n"), ignore)
		}
	})
}

func TestApp_MirrorLogFailure(t *testing.T) {
	const argvFix = "fix it"
	// control is the console output of the same invocation with a working log
	control := func(t *testing.T) result {
		e := newSyntheticEnv(t)
		e.jev.pick = "fast@low"
		return e.run([]string{argvFix}, nil)
	}
	tests := []struct {
		name     string
		breakLog func(t *testing.T, e *env)
	}{
		{"log_dir below a file", func(t *testing.T, e *env) {
			require.NoError(t, os.WriteFile(filepath.Join(e.workDir, "blocked"), []byte("file"), 0o600))
			e.globalConfig(syntheticCLIs(t) + "\n[agrouter]\nlog_dir = blocked/logs\n")
		}},
		{"ignore file is a directory", func(t *testing.T, e *env) {
			require.NoError(t, os.MkdirAll(filepath.Join(e.workDir, ".agrouter", ".gitignore"), 0o750))
		}},
		{"ignore file is a symlink", func(t *testing.T, e *env) {
			linkTo(t, filepath.Join(t.TempDir(), "victim"), filepath.Join(e.workDir, ".agrouter", ".gitignore"))
		}},
		{"log directory is a symlink", func(t *testing.T, e *env) {
			require.NoError(t, os.MkdirAll(filepath.Join(e.workDir, ".agrouter"), 0o750))
			linkTo(t, t.TempDir(), filepath.Join(e.workDir, ".agrouter", "logs"))
		}},
		{"read-only work directory", func(t *testing.T, e *env) {
			if runtime.GOOS == "windows" || os.Geteuid() == 0 {
				t.Skip("directory permissions do not stop this user")
			}
			require.NoError(t, os.Chmod(e.workDir, 0o500))       //nolint:gosec // a read-only directory is the case under test
			t.Cleanup(func() { _ = os.Chmod(e.workDir, 0o700) }) //nolint:gosec // restored so the temporary directory can be removed
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			want := control(t)
			require.Equal(t, exitOK, want.code, want.stderr)
			require.Empty(t, want.stderr)

			e := newSyntheticEnv(t)
			e.jev.pick = "fast@low"
			tc.breakLog(t, e)
			var stdout, stderr bytes.Buffer
			a := e.app(nil, &stdout, &stderr, runner.Runner{})
			for range 2 {
				require.Equal(t, exitOK, a.run([]string{argvFix}), stderr.String())
			}

			lines := strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n")
			require.Len(t, lines, 2, "one warning per invocation, none from the failed mirror itself")
			for _, line := range lines {
				assert.True(t, strings.HasPrefix(line, "agrouter: warning: log: "), line)
			}
			assert.Equal(t, strings.Repeat(want.stdout, 2), stdout.String(), "the decision is unchanged")
		})
	}
}

// linkTo makes link a symlink to target, creating link's directory. It skips where symlinks need
// privileges the test user lacks.
func linkTo(t *testing.T, target, link string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o750))
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

func TestApp_MirrorLogSymlinkTargetUntouched(t *testing.T) {
	e := newSyntheticEnv(t)
	e.jev.pick = "fast@low"
	victim := filepath.Join(t.TempDir(), "gitconfig")
	require.NoError(t, os.WriteFile(victim, []byte("[user]\n"), 0o600))
	linkTo(t, victim, filepath.Join(e.workDir, ".agrouter", ".gitignore"))

	var stdout, stderr bytes.Buffer
	a := e.app(nil, &stdout, &stderr, runner.Runner{})
	require.Equal(t, exitOK, a.run([]string{"fix it"}), stderr.String())
	assert.Contains(t, stderr.String(), "symlink")
	assert.Equal(t, "[user]\n", readFile(t, victim))
}
