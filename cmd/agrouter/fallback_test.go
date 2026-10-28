package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SvetlovA/agrouter/pkg/config"
	"github.com/SvetlovA/agrouter/pkg/readiness"
	"github.com/SvetlovA/agrouter/pkg/runner"
)

// defaultsChecker is a checker reporting cli ready with evidence beside the child defaults model and
// effort (empty: not reported).
func defaultsChecker(model, effort string, evidence ...readiness.Evidence) ReadinessChecker {
	return reportChecker(func(cli config.CLI) readiness.Report {
		r := withQuota(cli, append([]readiness.Evidence{free(readiness.WindowFiveHour), free(readiness.WindowWeekly)}, evidence...)...)
		if model != "" {
			r.Defaults.Model = &model
		}
		if effort != "" {
			r.Defaults.Effort = &effort
		}
		return r
	})
}

func verified(value string) map[string]any {
	return map[string]any{"value": value, "source": sourceVerifiedDefault}
}

func unknownDefault(code string) map[string]any {
	return map[string]any{"value": nil, "source": sourceUnknownDefault, "reason": map[string]any{"code": code}}
}

func chosenBy(source, value string) map[string]any {
	return map[string]any{"value": value, "source": source}
}

func TestApp_ExecFallback(t *testing.T) {
	both := map[string]any{"action": fallbackAction, "fields": strs("model", "effort"),
		"message": "Model and effort fall back to the child's defaults, not a classifier selection"}
	effortOnly := map[string]any{"action": fallbackAction, "fields": strs("effort"),
		"message": "Effort falls back to the child's default, not a classifier selection"}
	modelOnly := map[string]any{"action": fallbackAction, "fields": strs("model"),
		"message": "Model falls back to the child's default, not a classifier selection"}
	tests := []struct {
		name           string
		stage          string
		argv           []string
		model, effort  string // the child defaults the probe reports
		warning        string
		fallback       map[string]any
		effective      map[string]any
		childModel     string // the --model the child gets; empty for none
		childHasEffort bool
	}{
		{name: "model stage, verified model and unknown effort", stage: "model", model: "m-2",
			warning: "agrouter: warning: Classifier routing failed at the model stage (classifier_api_error); model and effort fall back to " +
				"alpha defaults, not a classifier selection: model m-2 (verified child default), effort unknown (child did not report it)",
			fallback: both, effective: map[string]any{"model": verified("m-2"), "effort": unknownDefault(defaultNotReported)}},
		{name: "effort stage keeps Jev's model", stage: "effort", effort: "high",
			warning: "agrouter: warning: Classifier routing failed at the effort stage (classifier_api_error); effort falls back to the " +
				"alpha default, not a classifier selection: effort unknown (child reported it for another model)",
			fallback:   effortOnly,
			effective:  map[string]any{"model": chosenBy(sourceRouter, "fast-1"), "effort": unknownDefault(defaultOtherModel)},
			childModel: "fast-1"},
		{name: "effort stage on the child's default model", stage: "effort", model: "fast-1", effort: "high",
			warning: "agrouter: warning: Classifier routing failed at the effort stage (classifier_api_error); effort falls back to the " +
				"alpha default, not a classifier selection: effort high (verified child default)",
			fallback: effortOnly, effective: map[string]any{"model": chosenBy(sourceRouter, "fast-1"), "effort": verified("high")},
			childModel: "fast-1"},
		{name: "caller's model is no fallback", stage: "effort", argv: []string{"--model=strong"},
			warning: "agrouter: warning: Classifier routing failed at the effort stage (classifier_api_error); effort falls back to the " +
				"alpha default, not a classifier selection: effort unknown (child did not report it)",
			fallback:   effortOnly,
			effective:  map[string]any{"model": chosenBy(sourceCaller, "deep-1"), "effort": unknownDefault(defaultNotReported)},
			childModel: "deep-1"},
		{name: "the child's default model reported by alias", stage: "effort", argv: []string{"--model=deep-1"},
			model: "strong", effort: "high",
			warning: "agrouter: warning: Classifier routing failed at the effort stage (classifier_api_error); effort falls back to the " +
				"alpha default, not a classifier selection: effort high (verified child default)",
			fallback:   effortOnly,
			effective:  map[string]any{"model": chosenBy(sourceCaller, "deep-1"), "effort": verified("high")},
			childModel: "deep-1"},
		{name: "caller's effort is no fallback", stage: "model", argv: []string{"--effort=low"}, model: "m-2",
			warning: "agrouter: warning: Classifier routing failed at the model stage (classifier_api_error); model falls back to the " +
				"alpha default, not a classifier selection: model m-2 (verified child default)",
			fallback: modelOnly, effective: map[string]any{"model": verified("m-2"), "effort": chosenBy(sourceCaller, "low")},
			childHasEffort: true},
		{name: "complexity stage", stage: "complexity", model: "m-2", effort: "low",
			warning: "agrouter: warning: Classifier routing failed at the complexity stage (classifier_api_error); model and effort fall " +
				"back to alpha defaults, not a classifier selection: model m-2 (verified child default), effort low (verified child default)",
			fallback: both, effective: map[string]any{"model": verified("m-2"), "effort": verified("low")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := failingEnv(t, tc.stage, func(*fakeJev) {})
			e.checker = defaultsChecker(tc.model, tc.effort)
			argv := append([]string{"exec", "alpha"}, tc.argv...)
			if tc.stage == "complexity" {
				e.jev.failDocs = true
				doc := filepath.Join(e.workDir, "doc.md")
				require.NoError(t, os.WriteFile(doc, []byte("# project\n"), 0o600))
				argv = append(argv, "--doc", doc)
			}
			r := e.run(append(argv, "fix it"), nil)

			require.Equal(t, exitOK, r.code, r.stderr)
			warning, s := fallbackLog(t, r.stderr)
			assert.Equal(t, tc.warning, warning)
			assert.Equal(t, tc.fallback, s["fallback"])
			assert.Equal(t, tc.effective, s["effective"])
			assert.Equal(t, tc.stage, s["reason"].(map[string]any)["stage"])
			assert.NotContains(t, s, "readiness", "nothing was excluded or unverified")
			argv, _, _ = e.child()
			if tc.childModel == "" {
				assert.NotContains(t, argv, "--model", "a fallen-back model passes no flag")
				assert.Nil(t, s["model"])
			} else {
				assert.Contains(t, argv, tc.childModel)
			}
			assert.Equal(t, tc.childHasEffort, strings.Contains(strings.Join(argv, " "), "--effort"), argv)
		})
	}
}

// blockedAfterRouting splits exec's stderr after a block that followed routing: the selection routing
// produced, the run-skipped line, then the block line, each with its newline.
func blockedAfterRouting(t *testing.T, stderr string) (map[string]any, string, string) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(stderr, "\n"), "\n")
	require.Len(t, lines, 3, stderr)
	return selectionLog(t, lines[0]+"\n"), lines[1] + "\n", lines[2] + "\n"
}

func TestApp_ExecFallbackDefaultsAndReadiness(t *testing.T) {
	deepBlocked := exhausted(readiness.WindowWeekly, time.Hour, "strong")

	t.Run("native settings do not change the configured probe defaults", func(t *testing.T) {
		e := newEnv(t)
		e.globalConfig(syntheticCLIs(t) + "\n[agrouter]\ntimeout = 300ms\n")
		e.jev.pick, e.jev.failStage = "fast@low", "model"
		e.checker = defaultsChecker("m-2", "high")
		r := e.run([]string{"exec", "alpha", "fix it", "--", "--profile", "work"}, nil)
		require.Equal(t, exitOK, r.code, r.stderr)
		warning, selection := fallbackLog(t, r.stderr)
		assert.Contains(t, warning, "model m-2 (verified child default), effort high (verified child default)")
		assert.Equal(t, map[string]any{"model": verified("m-2"), "effort": verified("high")}, selection["effective"])
		child, _, _ := e.child()
		assert.Equal(t, []string{"-p", "--profile", "work", "--", "fix it"}, child)
	})

	t.Run("a blocked verified default stops the fallback", func(t *testing.T) {
		e := failingEnv(t, "effort", func(*fakeJev) {})
		e.checker = defaultsChecker("deep-1", "", deepBlocked)
		r := e.runWithRunner([]string{"exec", "alpha", "fix it"}, nil, noChild(t))

		assert.Equal(t, runner.ExitStartFailure, r.code)
		assert.Empty(t, r.stdout)
		sel, skipped, block := blockedAfterRouting(t, r.stderr)
		assert.Equal(t, runSkipped("subscription quota exhausted"), skipped)
		assert.Equal(t, "agrouter: weekly subscription quota exhausted; resets at 2026-10-13T13:00:00Z\n", block,
			"the quota line keeps its wording")
		assert.Equal(t, "alpha", sel["cli"])
		assert.Nil(t, sel["model"])
		assert.Nil(t, sel["effort"])
		assert.NotNil(t, sel["reason"], "the selection routing produced says why it is undecided")
		for _, key := range []string{"effective", "fallback", "readiness"} {
			assert.NotContains(t, sel, key, "the line claims nothing about a run")
		}
	})

	t.Run("a verified default on an unsupported model stops the fallback", func(t *testing.T) {
		e := failingEnv(t, "effort", func(*fakeJev) {})
		e.checker = defaultsChecker("deep-1", "", readiness.Evidence{Dimension: readiness.DimensionCapability,
			Status: readiness.StatusUnavailable, Reason: readiness.ReasonUnsupportedModel, Check: "models",
			Scope: readiness.Scope{Models: []string{"strong"}}})
		r := e.runWithRunner([]string{"exec", "alpha", "fix it"}, nil, noChild(t))

		assert.Equal(t, runner.ExitStartFailure, r.code)
		assert.Empty(t, r.stdout)
		sel, skipped, block := blockedAfterRouting(t, r.stderr)
		assert.Equal(t, runSkipped("model not supported"), skipped)
		assert.Equal(t, "agrouter: alpha unavailable: unsupported_model\n", block)
		assert.Nil(t, sel["model"])
	})

	t.Run("an unknown delegated default under a scoped block", func(t *testing.T) {
		e := failingEnv(t, "effort", func(*fakeJev) {})
		e.checker = defaultsChecker("", "", deepBlocked)
		r := e.run([]string{"exec", "alpha", "fix it"}, nil)

		require.Equal(t, exitOK, r.code, r.stderr)
		first, rest, _ := strings.Cut(r.stderr, "\n")
		assert.Equal(t, "agrouter: warning: alpha availability unknown: delegated_model_unverified", first)
		_, s := fallbackLog(t, rest)
		assert.Equal(t, map[string]any{"excluded": map[string]any{"quota_exhausted": float64(2)},
			"unknown": "delegated_model_unverified"}, s["readiness"], "the fallback ran over the filtered options")
		assert.Equal(t, strs("model", "effort"), s["fallback"].(map[string]any)["fields"])
		assert.Len(t, e.jev.requests(), 1, "one model was left after filtering: only the effort stage asked")
	})

	t.Run("an unknown delegated default under unknown = block", func(t *testing.T) {
		e := failingEnv(t, "effort", func(*fakeJev) {})
		e.globalConfig(syntheticCLIs(t) + availability("unknown = block"))
		e.checker = defaultsChecker("", "", deepBlocked)
		r := e.runWithRunner([]string{"exec", "alpha", "fix it"}, nil, noChild(t))

		assert.Equal(t, exitUsage, r.code)
		assert.Empty(t, r.stdout)
		sel, skipped, block := blockedAfterRouting(t, r.stderr)
		assert.Equal(t, runSkipped("availability unverified"), skipped)
		assert.Equal(t, "agrouter: alpha availability unverified: delegated_model_unverified\n", block)
		assert.Equal(t, "alpha", sel["cli"])
	})

	t.Run("a verified default outside the block runs", func(t *testing.T) {
		e := failingEnv(t, "effort", func(*fakeJev) {})
		e.checker = defaultsChecker("fast-1", "", deepBlocked)
		r := e.run([]string{"exec", "alpha", "fix it"}, nil)

		require.Equal(t, exitOK, r.code, r.stderr)
		_, s := fallbackLog(t, r.stderr)
		assert.Equal(t, verified("fast-1"), s["effective"].(map[string]any)["model"])
	})
}

func TestApp_ExecNoFallback(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		e := newSyntheticEnv(t)
		e.jev.pick = "fast@low"
		e.checker = defaultsChecker("m-2", "high")
		r := e.run([]string{"exec", "alpha", "fix it"}, nil)

		require.Equal(t, exitOK, r.code, r.stderr)
		s := selectionLog(t, r.stderr)
		for _, key := range []string{"fallback", "effective", "readiness", "reason"} {
			assert.NotContains(t, s, key)
		}
	})

	t.Run("a model without efforts", func(t *testing.T) {
		e := newEnv(t)
		e.globalConfig(syntheticCLIs(t) + "\n[model.plain]\ncli = alpha\nname = plain-1\ndescription = No efforts.\n")
		e.checker = defaultsChecker("", "high")
		r := e.run([]string{"exec", "alpha", "--model=plain-1", "fix it"}, nil)

		require.Equal(t, exitOK, r.code, r.stderr)
		s := selectionLog(t, r.stderr)
		assert.NotContains(t, s, "fallback")
		assert.NotContains(t, s, "effective", "no value is left to the child")
	})

	t.Run("an effort left to a passed-through model", func(t *testing.T) {
		e := newSyntheticEnv(t)
		e.checker = defaultsChecker("made-up-9", "high")
		r := e.run([]string{"exec", "alpha", "--model=made-up-9", "fix it"}, nil)

		require.Equal(t, exitOK, r.code, r.stderr)
		s := selectionLog(t, r.stderr)
		assert.NotContains(t, s, "fallback", "an intentional omission is no fallback")
		assert.Equal(t, map[string]any{"model": chosenBy(sourceCaller, "made-up-9"), "effort": verified("high")}, s["effective"])
	})

	t.Run("a default effort of another model is unknown", func(t *testing.T) {
		e := newSyntheticEnv(t)
		e.checker = defaultsChecker("m-2", "high")
		r := e.run([]string{"exec", "alpha", "--model=made-up-9", "fix it"}, nil)

		require.Equal(t, exitOK, r.code, r.stderr)
		s := selectionLog(t, r.stderr)
		assert.Equal(t, map[string]any{"model": chosenBy(sourceCaller, "made-up-9"), "effort": unknownDefault(defaultOtherModel)},
			s["effective"])
	})

	t.Run("chunked routing keeps the pooled model", func(t *testing.T) {
		e := failingEnv(t, "effort", func(*fakeJev) {})
		e.globalConfig(syntheticCLIs(t))
		input := strings.Repeat("line of a long ralphex prompt\n", 100_000)
		r := e.run([]string{"exec", "alpha"}, strings.NewReader(input))

		require.Equal(t, exitOK, r.code, r.stderr)
		_, s := fallbackLog(t, r.stderr)
		assert.Equal(t, "fast-1", s["model"])
		assert.Equal(t, strs("effort"), s["fallback"].(map[string]any)["fields"])
		_, stdin, _ := e.child()
		assert.Equal(t, input, string(stdin))
	})
}

func TestFallbackWarning(t *testing.T) {
	m2 := "m-2"
	eff := &effectiveJSON{Model: &effectiveValueJSON{Value: &m2, Source: sourceVerifiedDefault},
		Effort: &effectiveValueJSON{Source: sourceUnknownDefault, Reason: &codeJSON{defaultNotReported}}}
	assert.Equal(t, "agrouter: warning: Classifier routing failed (classifier_timeout); model and effort fall back to tool-a defaults, "+
		"not a classifier selection: model m-2 (verified child default), effort unknown (child did not report it)",
		fallbackWarning("tool-a", &reasonJSON{Code: reasonTimeout}, []string{"model", "effort"}, eff),
		"no stage when the capture ran past the deadline")
	assert.Empty(t, capitalize(""))
}
