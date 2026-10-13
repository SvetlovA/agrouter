package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SvetlovA/agrouter/pkg/jev"
	"github.com/SvetlovA/agrouter/pkg/router"
)

func TestFailureReason(t *testing.T) {
	secret := errors.New("SECRET body with the prompt")
	stage := func(level string, err error) error { return &router.StageError{Stage: level, Err: err} }
	tests := []struct {
		name string
		err  error
		want *reasonJSON
	}{
		{name: "success", err: nil, want: nil},
		{name: "no key", err: stage(router.LevelCLI, jev.ErrNoKey),
			want: &reasonJSON{reasonNoKey, "cli", "Jev CLI selection failed: no Jev API key is set"}},
		{name: "unauthorized", err: stage(router.LevelModel, &jev.StatusError{Status: http.StatusUnauthorized, Body: "SECRET"}),
			want: &reasonJSON{reasonUnauthorized, "model", "Jev model selection failed: the Jev API key was rejected"}},
		{name: "timeout", err: stage(router.LevelModel, fmt.Errorf("route request: %w", context.DeadlineExceeded)),
			want: &reasonJSON{reasonTimeout, "model", "Jev model selection timed out"}},
		{name: "timeout in the transport", err: stage(router.LevelEffort,
			fmt.Errorf("%w: %w", jev.ErrTransport, context.DeadlineExceeded)),
			want: &reasonJSON{reasonTimeout, "effort", "Jev effort selection timed out"}},
		{name: "canceled", err: stage(router.LevelEffort, context.Canceled),
			want: &reasonJSON{reasonCanceled, "effort", "Jev effort selection was canceled"}},
		{name: "overloaded", err: stage(router.LevelCLI, &jev.StatusError{Status: 529}),
			want: &reasonJSON{reasonAPIError, "cli", "Jev CLI selection failed: Jev returned an error status"}},
		{name: "server error", err: stage(router.LevelComplexity, &jev.StatusError{Status: 500, Body: "SECRET"}),
			want: &reasonJSON{reasonAPIError, "complexity", "Jev project complexity scoring failed: Jev returned an error status"}},
		{name: "transport", err: stage(router.LevelModel, fmt.Errorf("%w: %w", jev.ErrTransport, secret)),
			want: &reasonJSON{reasonTransportError, "model", "Jev model selection failed: Jev could not be reached"}},
		{name: "invalid answer", err: stage(router.LevelModel, fmt.Errorf("%w: %w", jev.ErrInvalidAnswer, secret)),
			want: &reasonJSON{reasonInvalidAnswer, "model", "Jev model selection failed: Jev returned an invalid answer"}},
		{name: "malformed answer", err: stage(router.LevelModel, jev.ErrMalformed),
			want: &reasonJSON{reasonInvalidAnswer, "model", "Jev model selection failed: Jev returned an invalid answer"}},
		{name: "empty answer", err: stage(router.LevelCLI, jev.ErrEmptyAnswer),
			want: &reasonJSON{reasonEmptyAnswer, "cli", "Jev CLI selection failed: Jev returned no answer"}},
		{name: "chunked routing", err: stage(router.LevelModel, fmt.Errorf("route 2 of 3: %w", jev.ErrEmptyAnswer)),
			want: &reasonJSON{reasonEmptyAnswer, "model", "Jev model selection failed: Jev returned no answer"}},
		{name: "through the cannot-decide error", err: fmt.Errorf("%w between a, b: %w", router.ErrCannotDecide,
			stage(router.LevelCLI, &jev.StatusError{Status: 401})),
			want: &reasonJSON{reasonUnauthorized, "cli", "Jev CLI selection failed: the Jev API key was rejected"}},
		{name: "deadline before any stage", err: fmt.Errorf("--doc: %w", context.DeadlineExceeded),
			want: &reasonJSON{reasonTimeout, "", "Jev routing timed out"}},
		{name: "anything else", err: secret,
			want: &reasonJSON{reasonNoDecision, "", "Jev routing failed: Jev could not decide"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := failureReason(tc.err)
			assert.Equal(t, tc.want, got)
			if got != nil {
				assert.NotContains(t, got.Message, "SECRET")
			}
		})
	}
}

func TestNullReasons(t *testing.T) {
	value := "v"
	routingFailed := func(field string) *nullReasonJSON {
		return &nullReasonJSON{nullRoutingFailed, field + " delegated to the child after routing failed"}
	}
	tests := []struct {
		name     string
		s        selectionJSON
		failed   bool
		noEffort bool
		want     *nullReasonsJSON
	}{
		{name: "all set", s: selectionJSON{CLI: &value, Model: &value, Effort: &value}},
		{name: "no CLI after a failure", s: selectionJSON{}, failed: true, want: &nullReasonsJSON{
			CLI:    &nullReasonJSON{nullUnresolved, "No CLI chosen: routing failed with more than one CLI left"},
			Model:  &nullReasonJSON{nullUnresolved, "No model chosen: no CLI was chosen"},
			Effort: &nullReasonJSON{nullUnresolved, "No effort chosen: no CLI was chosen"},
		}},
		{name: "failure with the CLI known", s: selectionJSON{CLI: &value}, failed: true,
			want: &nullReasonsJSON{Model: routingFailed("Model"), Effort: routingFailed("Effort")}},
		{name: "failure keeps the caller's effort", s: selectionJSON{CLI: &value, Effort: &value}, failed: true,
			want: &nullReasonsJSON{Model: routingFailed("Model")}},
		{name: "model without efforts", s: selectionJSON{CLI: &value, Model: &value}, noEffort: true,
			want: &nullReasonsJSON{Effort: &nullReasonJSON{nullNoEffort, "The chosen model has no effort levels"}}},
		{name: "effort left to the child", s: selectionJSON{CLI: &value, Model: &value},
			want: &nullReasonsJSON{Effort: &nullReasonJSON{nullDelegated, "Effort left to the child's default"}}},
		{name: "model left to the child", s: selectionJSON{CLI: &value, Effort: &value},
			want: &nullReasonsJSON{Model: &nullReasonJSON{nullDelegated, "Model left to the child's default"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, nullReasons(tc.s, tc.failed, tc.noEffort))
		})
	}
}

// failingEnv is a synthetic env whose fake Jev fails stage as fail sets up, under a short routing
// deadline so a hanging Jev times out quickly.
func failingEnv(t *testing.T, stage string, fail func(*fakeJev)) *env {
	t.Helper()
	e := newEnv(t)
	e.globalConfig(syntheticCLIs(t) + "\n[agrouter]\ntimeout = 300ms\n")
	e.jev.pick = "fast@low"
	e.jev.failStage = stage
	fail(e.jev)
	return e
}

func TestApp_FailureReasons(t *testing.T) {
	const prompt = "SECRET PROMPT fix it"
	causes := []struct {
		name string
		fail func(*fakeJev)
		argv []string
		code string
	}{
		{name: "missing key", fail: func(*fakeJev) {}, argv: []string{"--jev-api-key="}, code: reasonNoKey},
		{name: "unauthorized", fail: func(f *fakeJev) { f.failStatus = http.StatusUnauthorized }, code: reasonUnauthorized},
		{name: "API error", fail: func(f *fakeJev) { f.failStatus = http.StatusInternalServerError }, code: reasonAPIError},
		{name: "timeout", fail: func(f *fakeJev) { f.failHang = true }, code: reasonTimeout},
		{name: "transport error", fail: func(f *fakeJev) { f.failDrop = true }, code: reasonTransportError},
		{name: "invalid answer", fail: func(f *fakeJev) {
			f.failAnswer = `{"choice":"made-up","probabilities":{"made-up":1},"confidence":0.5}`
		}, code: reasonInvalidAnswer},
		{name: "null answer", fail: func(f *fakeJev) { f.failAnswer = "null" }, code: reasonEmptyAnswer},
		{name: "missing answer", fail: func(f *fakeJev) { f.failAnswer = "omit" }, code: reasonEmptyAnswer},
		{name: "empty answer", fail: func(f *fakeJev) { f.failAnswer = "{}" }, code: reasonEmptyAnswer},
	}
	// noLeak checks that out carries neither the prompt, the key nor Jev's response body
	noLeak := func(t *testing.T, out string) {
		t.Helper()
		assert.NotContains(t, out, "SECRET")
		assert.NotContains(t, out, testKey)
		assert.NotContains(t, out, "stage rejected")
	}
	for _, cause := range causes {
		for _, verbose := range []bool{false, true} {
			var flags []string
			if verbose {
				flags = []string{"--verbose"}
			}
			argv := func(mode ...string) []string {
				return append(append(append(mode, flags...), cause.argv...), prompt)
			}
			name := fmt.Sprintf("%s/verbose=%v", cause.name, verbose)

			t.Run(name+"/decision, more than one CLI", func(t *testing.T) {
				e := failingEnv(t, "cli", cause.fail)
				r := e.run(argv(), nil)

				assert.Equal(t, exitUsage, r.code)
				assert.Equal(t, 1, strings.Count(r.stderr, "\n"), r.stderr)
				assert.True(t, strings.HasPrefix(r.stderr, "agrouter: jev cannot decide between alpha, beta"), r.stderr)
				d := decision(t, r.stdout)
				assert.Subset(t, d, map[string]any{"cli": nil, "model": nil, "effort": nil})
				reason, _ := d["reason"].(map[string]any)
				assert.Equal(t, cause.code, reason["code"])
				assert.Equal(t, "cli", reason["stage"])
				nulls, _ := d["null_reasons"].(map[string]any)
				for _, field := range []string{"cli", "model", "effort"} {
					assert.Equal(t, nullUnresolved, nulls[field].(map[string]any)["code"], field)
				}
				for _, key := range []string{"argv", "command", "stdin_required", "options"} {
					assert.NotContains(t, d, key, "nothing to run")
				}
				noLeak(t, r.stdout)
			})

			t.Run(name+"/decision, CLI known", func(t *testing.T) {
				e := failingEnv(t, "model", cause.fail)
				r := e.run(argv("--cli=alpha"), nil)

				require.Equal(t, exitOK, r.code, r.stderr)
				d := decision(t, r.stdout)
				assert.Subset(t, d, map[string]any{"cli": "alpha", "model": nil, "effort": nil})
				reason, _ := d["reason"].(map[string]any)
				assert.Equal(t, cause.code, reason["code"])
				assert.Equal(t, "model", reason["stage"])
				assert.Equal(t, map[string]any{
					"model":  map[string]any{"code": nullRoutingFailed, "message": "Model delegated to the child after routing failed"},
					"effort": map[string]any{"code": nullRoutingFailed, "message": "Effort delegated to the child after routing failed"},
				}, d["null_reasons"])
				// verbose argv carries the prompt by design; the reasons never do
				reasons, err := json.Marshal([]any{d["reason"], d["null_reasons"]})
				require.NoError(t, err)
				noLeak(t, string(reasons))
			})

			t.Run(name+"/exec", func(t *testing.T) {
				e := failingEnv(t, "model", cause.fail)
				r := e.run(argv("exec", "alpha"), nil)

				require.Equal(t, exitOK, r.code, r.stderr)
				_, s := fallbackLog(t, r.stderr)
				assert.Subset(t, s, map[string]any{"cli": "alpha", "model": nil, "effort": nil})
				reason, _ := s["reason"].(map[string]any)
				assert.Equal(t, cause.code, reason["code"])
				assert.Equal(t, "model", reason["stage"])
				assert.Contains(t, s, "null_reasons")
				assert.NotContains(t, s, "options", "the selection line stays concise")
				noLeak(t, r.stderr)
			})
		}
	}
}

func TestApp_FailureStages(t *testing.T) {
	tests := []struct {
		name      string
		stage     string
		argv      []string
		wantModel any
		wantNulls []string
	}{
		{name: "cli stage", stage: "cli", argv: []string{"fix it"}, wantNulls: []string{"cli", "model", "effort"}},
		{name: "model stage", stage: "model", argv: []string{"--cli=alpha", "fix it"}, wantNulls: []string{"model", "effort"}},
		// the model is the caller's, so it is kept; only the effort falls back
		{name: "effort stage", stage: "effort", argv: []string{"--model=fast-1", "fix it"}, wantModel: "fast-1",
			wantNulls: []string{"effort"}},
	}
	for _, tc := range tests {
		for _, mode := range []string{"decision", "exec"} {
			if mode == "exec" && tc.stage == "cli" {
				continue // exec names its one CLI
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				e := failingEnv(t, tc.stage, func(*fakeJev) {})
				argv := tc.argv
				if mode == "exec" {
					argv = append([]string{"exec", "alpha"}, argv...)
				}
				r := e.run(argv, nil)

				var d map[string]any
				if mode == "exec" {
					require.Equal(t, exitOK, r.code, r.stderr)
					_, d = fallbackLog(t, r.stderr)
				} else {
					d = decision(t, r.stdout)
				}
				assert.Equal(t, tc.wantModel, d["model"])
				reason, _ := d["reason"].(map[string]any)
				assert.Equal(t, map[string]any{"code": reasonAPIError, "stage": tc.stage,
					"message": failureMessage(reasonAPIError, tc.stage)}, reason)
				nulls, _ := d["null_reasons"].(map[string]any)
				assert.ElementsMatch(t, tc.wantNulls, keys(nulls))
			})
		}
	}
}

func TestApp_ReasonsOnSuccessAndOmissions(t *testing.T) {
	t.Run("success omits both fields", func(t *testing.T) {
		for _, argv := range [][]string{{"fix it"}, {"--verbose", "fix it"}} {
			e := newSyntheticEnv(t)
			e.jev.pick = "fast@low"
			r := e.run(argv, nil)

			require.Equal(t, exitOK, r.code, r.stderr)
			d := decision(t, r.stdout)
			assert.NotContains(t, d, "reason")
			assert.NotContains(t, d, "null_reasons")
		}
		e := newSyntheticEnv(t)
		e.jev.pick = "fast@low"
		r := e.run([]string{"exec", "alpha", "fix it"}, nil)
		require.Equal(t, exitOK, r.code, r.stderr)
		s := selectionLog(t, r.stderr)
		assert.NotContains(t, s, "reason")
		assert.NotContains(t, s, "null_reasons")
	})

	t.Run("caller's effort kept after a failure", func(t *testing.T) {
		e := failingEnv(t, "model", func(*fakeJev) {})
		r := e.run([]string{"--cli=alpha", "--effort=high", "fix it"}, nil)

		require.Equal(t, exitOK, r.code, r.stderr)
		d := decision(t, r.stdout)
		assert.Equal(t, "high", d["effort"])
		assert.Equal(t, []string{"model"}, keys(d["null_reasons"].(map[string]any)))
	})

	t.Run("a model without efforts", func(t *testing.T) {
		e := newEnv(t)
		e.globalConfig(syntheticCLIs(t) + `
[model.plain]
cli         = alpha
name        = plain-1
description = A model without efforts.
`)
		r := e.run([]string{"--model=plain-1", "fix it"}, nil)

		require.Equal(t, exitOK, r.code, r.stderr)
		d := decision(t, r.stdout)
		assert.Equal(t, "plain-1", d["model"])
		assert.NotContains(t, d, "reason")
		assert.Equal(t, map[string]any{"effort": map[string]any{"code": nullNoEffort,
			"message": "The chosen model has no effort levels"}}, d["null_reasons"])
	})

	t.Run("an effort left to the child of a passed-through model", func(t *testing.T) {
		e := newSyntheticEnv(t)
		r := e.run([]string{"--cli=alpha", "--model=made-up-9", "fix it"}, nil)

		require.Equal(t, exitOK, r.code, r.stderr)
		d := decision(t, r.stdout)
		assert.Equal(t, "made-up-9", d["model"])
		assert.NotContains(t, d, "reason")
		assert.Equal(t, map[string]any{"effort": map[string]any{"code": nullDelegated,
			"message": "Effort left to the child's default"}}, d["null_reasons"])
	})
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
