package main

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SvetlovA/agrouter/pkg/catalog"
	"github.com/SvetlovA/agrouter/pkg/config"
	"github.com/SvetlovA/agrouter/pkg/jev"
	"github.com/SvetlovA/agrouter/pkg/readiness"
	"github.com/SvetlovA/agrouter/pkg/router"
)

// debugLines returns the AGROUTER_DEBUG lines of stderr.
func debugLines(stderr string) []string {
	var out []string
	for line := range strings.SplitSeq(stderr, "\n") {
		if rest, ok := strings.CutPrefix(line, "agrouter debug: "); ok {
			out = append(out, rest)
		}
	}
	return out
}

func TestDebug_Redaction(t *testing.T) {
	const secret = "the secret prompt text"
	e := newEnv(t)
	e.fakeCommands()
	t.Setenv(envDebug, "1")
	e.jev.pick = "claude-sonnet-5-5@low"
	r := e.run([]string{"exec", "claude", "--verbose", secret, "--", "--raw-one", "raw-two"}, nil)
	require.Equal(t, 0, r.code, r.stderr)

	lines := debugLines(r.stderr)
	require.Len(t, lines, 13)
	assert.Equal(t, "api key: from env", lines[0])
	assert.Equal(t, []string{
		"readiness claude: executable available",
		"readiness claude: auth available",
		"readiness claude: quota available, window five_hour",
		"readiness claude: quota available, window weekly",
		"readiness claude: defaults model unknown, effort unknown",
	}, lines[1:6])
	assert.Regexp(t, `^readiness claude: 0 of [1-9][0-9]* option\(s\) excluded$`, lines[6])
	assert.Regexp(t, `^eligible: [1-9][0-9]* option\(s\) on claude$`, lines[7])
	assert.Equal(t, "dropped codex: --cli claude", lines[8], "the exec CLI restricts routing like --cli")
	assert.Equal(t, "stage cli: skipped (claude)", lines[9])
	assert.True(t, strings.HasPrefix(lines[10], "stage model: choice claude-sonnet-5-5, confidence 0.900, top [claude-sonnet-5-5 1.000"), lines[10])
	assert.True(t, strings.HasPrefix(lines[11], "stage effort: choice low, confidence 0.900, top [low 1.000"), lines[11])
	assert.Equal(t, "command: "+helperCommand(t)+" -p --model claude-sonnet-5-5 --effort low <2 raw argument(s)> -- <prompt>", lines[12])
	assert.NotContains(t, r.stderr, secret)
	assert.NotContains(t, r.stderr, "raw-two")
	assert.NotContains(t, r.stderr, testKey)
	// debug goes to stderr only: stdout is the child's, empty here
	assert.Empty(t, r.stdout)
}

func TestDebug_Off(t *testing.T) {
	for _, v := range []string{"", "0", "true"} {
		e := newEnv(t)
		t.Setenv(envDebug, v)
		e.jev.pick = "claude-sonnet-5-5@low"
		r := e.run([]string{"fix it"}, nil)
		require.Equal(t, 0, r.code, r.stderr)
		assert.Empty(t, r.stderr, "AGROUTER_DEBUG=%q", v)
	}
}

func TestDebug_JevFailureEchoingKey(t *testing.T) {
	e := newEnv(t)
	t.Setenv(envDebug, "1")
	// a Jev that echoes the Authorization header in its 401 body
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad key: "+r.Header.Get("Authorization"), http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	e.jev.srv = srv

	r := e.run([]string{"--cli=codex", "--jev-api-key=flag-secret-key", "fix it"}, nil)
	require.Equal(t, 0, r.code, r.stderr)
	lines := debugLines(r.stderr)
	require.Len(t, lines, 6, r.stderr)
	assert.Equal(t, "api key: from flag", lines[0])
	assert.Equal(t, "stage cli: skipped (codex)", lines[3], "the stage completed before the failure")
	assert.True(t, strings.HasPrefix(lines[4], "jev failed: model stage: route request: "), lines[4])
	assert.Contains(t, lines[4], "running codex with the caller's fixed values")
	assert.Equal(t, "command: codex exec -- <prompt>", lines[5])
	assert.NotContains(t, r.stderr, "flag-secret-key")
	assert.NotContains(t, r.stdout, "flag-secret-key")
}

func TestDebug_NoKeyAndNoDecision(t *testing.T) {
	e := newEnv(t)
	t.Setenv(envDebug, "1")
	r := e.run([]string{"--jev-api-key=", "fix it"}, nil)
	require.Equal(t, exitUsage, r.code)
	lines := debugLines(r.stderr)
	require.NotEmpty(t, lines)
	assert.Equal(t, "api key: none (flag)", lines[0])
	assert.True(t, strings.HasPrefix(lines[len(lines)-1], "no decision: "), r.stderr)
	assert.Contains(t, r.stderr, "agrouter: jev cannot decide")
}

func TestDebug_SingleOption(t *testing.T) {
	e := newEnv(t)
	t.Setenv(envDebug, "1")
	r := e.run([]string{"--model", "claude-opus-5-5", "--effort", "high", "fix it"}, nil)
	require.Equal(t, 0, r.code, r.stderr)
	assert.Contains(t, debugLines(r.stderr), "one option, jev not asked: claude-opus-5-5@high")
	assert.Empty(t, e.jev.requests())
}

func TestDebug_Docs(t *testing.T) {
	e := newSyntheticEnv(t)
	t.Setenv(envDebug, "1")
	require.NoError(t, os.WriteFile(filepath.Join(e.workDir, "project.md"), []byte("SECRET DOC TEXT\n"), 0o600))
	e.jev.pick = "fast@low"
	r := e.run([]string{"--cli=alpha", "--doc", "project.md", "fix it"}, nil)
	require.Equal(t, 0, r.code, r.stderr)

	lines := debugLines(r.stderr)
	assert.Contains(t, lines, "doc 1/1: score 7.000, evidence 0.500, confidence 0.900")
	assert.Contains(t, lines, "project complexity: 7.0")
	assert.NotContains(t, r.stderr, "SECRET DOC TEXT")
}

func TestDebug_StagesBeforeAFailureWithTwoCLIs(t *testing.T) {
	// exec always names its one CLI, so only decision mode can fail with two left
	e := newSyntheticEnv(t)
	t.Setenv(envDebug, "1")
	e.jev.pick = "fast@low"
	e.jev.failStage = "model"
	r := e.run([]string{"--verbose", "fix it"}, nil)

	assert.Equal(t, exitUsage, r.code)
	assert.Equal(t, map[string]any{"code": "jev_api_error", "stage": "model",
		"message": "Jev model selection failed: Jev returned an error status"}, decision(t, r.stdout)["reason"])
	assert.Equal(t, []string{"cli"}, e.jev.asked(), "the cli stage was answered")
	lines := debugLines(r.stderr)
	require.Len(t, lines, 4, r.stderr)
	assert.Equal(t, "eligible: 6 option(s) on alpha, beta", lines[1])
	assert.Equal(t, "stage cli: choice alpha, confidence 0.900, top [alpha 1.000, beta 0.000]", lines[2])
	assert.True(t, strings.HasPrefix(lines[3], "no decision: jev cannot decide between alpha, beta: model stage: "), lines[3])
	assert.Contains(t, lines[3], "model stage rejected")
	assert.True(t, strings.HasSuffix(r.stderr, "\nagrouter: "+strings.TrimPrefix(lines[3], "no decision: ")+"\n"),
		"the one error line follows the debug lines: %s", r.stderr)
}

func TestDebug_FailureBeforeRoutingPrintsNoStage(t *testing.T) {
	e := newSyntheticEnv(t)
	t.Setenv(envDebug, "1")
	r := e.run([]string{"--prompt-file", filepath.Join(t.TempDir(), "missing.md")}, nil)

	assert.Equal(t, exitUsage, r.code)
	assert.Empty(t, r.stdout)
	assert.Empty(t, e.jev.asked())
	lines := debugLines(r.stderr)
	require.NotEmpty(t, lines, r.stderr)
	for _, line := range lines {
		assert.False(t, strings.HasPrefix(line, "stage ") || strings.HasPrefix(line, "one option"), line)
	}
	assert.True(t, strings.HasPrefix(lines[len(lines)-1], "no decision: "), r.stderr)
}

func TestDebugLog(t *testing.T) {
	t.Run("nil prints nothing", func(t *testing.T) {
		var l *debugLog
		l.apiKey("k", "env")
		l.eligibility(&router.Eligibility{})
		l.decision(router.Decision{})
		l.failed(errors.New("x"))
		assert.Nil(t, newDebugLog("", &bytes.Buffer{}))
	})

	t.Run("key replaced in every line", func(t *testing.T) {
		var buf bytes.Buffer
		l := newDebugLog("1", &buf)
		l.apiKey("sk-123", "local")
		l.failed(errors.New("body echoed sk-123 back"))
		assert.Equal(t, "agrouter debug: api key: from local\nagrouter debug: no decision: body echoed <redacted> back\n", buf.String())
	})

	t.Run("passthrough and pooled", func(t *testing.T) {
		var buf bytes.Buffer
		l := newDebugLog("1", &buf)
		l.eligibility(&router.Eligibility{Options: []catalog.Option{{ID: "x", CLI: "made-up"}},
			ModelPassthrough: true, EffortPassthrough: true})
		l.decision(router.Decision{OptionID: "b", Stages: []router.Stage{
			{Level: "cli", Choice: "made-up", Skipped: true},
			{Level: "model", Choice: "b", Pooled: &router.Pooled{
				Chunks: []router.ChunkResult{{Field: "prompt", Index: 1, Of: 2, Relevance: 0.8, Confidence: 0.6,
					Top: []router.Score{{ID: "b", Score: 0.6}, {ID: "a", Score: 0.4}}}},
				Top: []router.Score{{ID: "b", Score: 0.55}},
			}},
		}})
		assert.Equal(t, `agrouter debug: eligible: 1 option(s) on made-up
agrouter debug: model: passed through
agrouter debug: effort: passed through
agrouter debug: stage cli: skipped (made-up)
agrouter debug: stage model chunk prompt 1/2: relevance 0.800, confidence 0.600, top [b 0.600, a 0.400]
agrouter debug: stage model: pooled choice b, confidence 0.600, top [b 0.550]
`, buf.String())
	})

	t.Run("complexity before the routing outcome", func(t *testing.T) {
		var buf bytes.Buffer
		newDebugLog("1", &buf).decision(router.Decision{CLI: "made-up", Undecided: errors.New("boom"),
			Complexity: &router.ComplexityResult{Complexity: 6.9, Chunks: []router.DocScore{
				{Index: 1, Of: 2, Score: 8.25, Evidence: 0.9, Confidence: 0.8}, {Index: 2, Of: 2, Score: 1, Evidence: 0.1, Confidence: 0.2},
			}}})
		assert.Equal(t, `agrouter debug: doc 1/2: score 8.250, evidence 0.900, confidence 0.800
agrouter debug: doc 2/2: score 1.000, evidence 0.100, confidence 0.200
agrouter debug: project complexity: 6.9
agrouter debug: jev failed: boom; running made-up with the caller's fixed values
`, buf.String())
	})

	t.Run("stages completed before a failure, and a pool without chunks", func(t *testing.T) {
		var buf bytes.Buffer
		newDebugLog("1", &buf).decision(router.Decision{Stages: []router.Stage{
			{Level: "cli", Choice: "made-up", Skipped: true},
			{Level: "model", Choice: "b", Pooled: &router.Pooled{}},
		}})
		assert.Equal(t, `agrouter debug: stage cli: skipped (made-up)
agrouter debug: stage model: pooled choice b, confidence none, top []
`, buf.String(), "no decision: neither the one-option line nor a jev failure")
	})

	t.Run("top probabilities: highest first, ties by id, at most three", func(t *testing.T) {
		got := topProbabilities(map[string]float64{"d": 0.1, "c": 0.3, "a": 0.3, "b": 0.2, "e": 0.1})
		assert.Equal(t, "[a 0.300, c 0.300, b 0.200]", got)
		assert.Equal(t, "[]", topProbabilities(nil))
	})

	t.Run("answer", func(t *testing.T) {
		var buf bytes.Buffer
		newDebugLog("1", &buf).decision(router.Decision{OptionID: "a", Stages: []router.Stage{{Level: "cli",
			Choice: "a", Answer: &jev.Answer{Choice: "a", Confidence: 0.5, Probabilities: map[string]float64{"a": 0.5, "b": 0.5}}}}})
		assert.Equal(t, "agrouter debug: stage cli: choice a, confidence 0.500, top [a 0.500, b 0.500]\n", buf.String())
	})
}

func TestDebug_Readiness(t *testing.T) {
	e := newSyntheticEnv(t)
	t.Setenv(envDebug, "1")
	model := "fast-1"
	e.checker = reportChecker(func(cli config.CLI) readiness.Report {
		r := withQuota(cli, free(readiness.WindowFiveHour), exhausted(readiness.WindowWeekly, time.Hour, "strong"),
			unknownQuota(readiness.ReasonProbeThrottled))
		r.Evidence[2].Check = "usage"
		r.Defaults.Model = &model
		return r
	})
	r := e.run([]string{"exec", "alpha", "--model=fast-1", "--effort=low", "--jev-api-key=" + testKey, "fix it"}, nil)
	require.Equal(t, 0, r.code, r.stderr)

	var lines []string
	for _, l := range debugLines(r.stderr) {
		if strings.HasPrefix(l, "readiness ") {
			lines = append(lines, l)
		}
	}
	assert.Equal(t, []string{
		"readiness alpha: executable available",
		"readiness alpha: auth available",
		"readiness alpha: quota available, window five_hour, check usage",
		"readiness alpha: quota unavailable (quota_exhausted), window weekly, models strong, resets 2026-10-13T13:00:00Z",
		"readiness alpha: quota unknown (probe_throttled)",
		"readiness alpha: defaults model fast-1, effort unknown",
		"readiness alpha: 0 of 1 option(s) excluded",
	}, lines)
	assert.NotContains(t, r.stderr, "fix it")
	assert.NotContains(t, r.stderr, testKey)
}
