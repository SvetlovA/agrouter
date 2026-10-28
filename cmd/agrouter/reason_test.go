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

	"github.com/SvetlovA/agrouter/pkg/classifier"
	"github.com/SvetlovA/agrouter/pkg/router"
)

// tooLongDetail is the fixed detail of the request_too_long reason.
const tooLongDetail = "routing request over the provider's context length; lower request_tokens or bytes_per_token"

func TestFailureReason(t *testing.T) {
	secret := errors.New("SECRET body with the prompt")
	stage := func(level string, err error) error { return &router.StageError{Stage: level, Err: err} }
	tests := []struct {
		name string
		err  error
		want *reasonJSON
	}{
		{name: "success", err: nil, want: nil},
		{name: "no key", err: stage(router.LevelCLI, classifier.ErrNoKey),
			want: &reasonJSON{reasonNoKey, "cli", "Jev CLI selection failed: no Jev API key is set"}},
		{name: "unauthorized", err: stage(router.LevelModel, &classifier.StatusError{Status: http.StatusUnauthorized, Body: "SECRET"}),
			want: &reasonJSON{reasonUnauthorized, "model", "Jev model selection failed: the Jev API key was rejected"}},
		{name: "timeout", err: stage(router.LevelModel, fmt.Errorf("route request: %w", context.DeadlineExceeded)),
			want: &reasonJSON{reasonTimeout, "model", "Jev model selection timed out"}},
		{name: "timeout in the transport", err: stage(router.LevelEffort,
			fmt.Errorf("%w: %w", classifier.ErrTransport, context.DeadlineExceeded)),
			want: &reasonJSON{reasonTimeout, "effort", "Jev effort selection timed out"}},
		{name: "canceled", err: stage(router.LevelEffort, context.Canceled),
			want: &reasonJSON{reasonCanceled, "effort", "Jev effort selection was canceled"}},
		{name: "overloaded", err: stage(router.LevelCLI, &classifier.StatusError{Status: 529}),
			want: &reasonJSON{reasonAPIError, "cli", "Jev CLI selection failed: Jev returned an error status"}},
		{name: "server error", err: stage(router.LevelComplexity, &classifier.StatusError{Status: 500, Body: "SECRET"}),
			want: &reasonJSON{reasonAPIError, "complexity", "Jev project complexity scoring failed: Jev returned an error status"}},
		{name: "request over the context length", err: stage(router.LevelModel,
			&classifier.StatusError{Status: http.StatusBadRequest, ErrorType: "max_tokens_exceeded", Body: "SECRET"}),
			want: &reasonJSON{reasonTooLong, "model", "Jev model selection failed: " + tooLongDetail}},
		{name: "chunk over the context length", err: stage(router.LevelEffort,
			fmt.Errorf("route 2 of 3: estimated 31000 tokens, request_tokens is 30000: %w",
				&classifier.StatusError{Status: http.StatusBadRequest, ErrorType: "max_tokens_exceeded"})),
			want: &reasonJSON{reasonTooLong, "effort", "Jev effort selection failed: " + tooLongDetail}},
		{name: "docs over the context length", err: stage(router.LevelComplexity,
			&classifier.StatusError{Status: http.StatusBadRequest, ErrorType: "max_tokens_exceeded"}),
			want: &reasonJSON{reasonTooLong, "complexity", "Jev project complexity scoring failed: " + tooLongDetail}},
		{name: "overflow through the cannot-decide error", err: fmt.Errorf("%w between a, b: %w", router.ErrCannotDecide,
			stage(router.LevelCLI, &classifier.StatusError{Status: http.StatusBadRequest, ErrorType: "max_tokens_exceeded"})),
			want: &reasonJSON{reasonTooLong, "cli", "Jev CLI selection failed: " + tooLongDetail}},
		{name: "400 of another type", err: stage(router.LevelModel,
			&classifier.StatusError{Status: http.StatusBadRequest, ErrorType: "invalid_request", Body: "SECRET"}),
			want: &reasonJSON{reasonAPIError, "model", "Jev model selection failed: Jev returned an error status"}},
		{name: "400 without a type", err: stage(router.LevelModel, &classifier.StatusError{Status: http.StatusBadRequest, Body: "SECRET"}),
			want: &reasonJSON{reasonAPIError, "model", "Jev model selection failed: Jev returned an error status"}},
		{name: "422 with the overflow type", err: stage(router.LevelModel,
			&classifier.StatusError{Status: http.StatusUnprocessableEntity, ErrorType: "max_tokens_exceeded", Body: "SECRET"}),
			want: &reasonJSON{reasonAPIError, "model", "Jev model selection failed: Jev returned an error status"}},
		{name: "transport", err: stage(router.LevelModel, fmt.Errorf("%w: %w", classifier.ErrTransport, secret)),
			want: &reasonJSON{reasonTransportError, "model", "Jev model selection failed: Jev could not be reached"}},
		{name: "invalid answer", err: stage(router.LevelModel, fmt.Errorf("%w: %w", classifier.ErrInvalidAnswer, secret)),
			want: &reasonJSON{reasonInvalidAnswer, "model", "Jev model selection failed: Jev returned an invalid answer"}},
		{name: "malformed answer", err: stage(router.LevelModel, classifier.ErrMalformed),
			want: &reasonJSON{reasonInvalidAnswer, "model", "Jev model selection failed: Jev returned an invalid answer"}},
		{name: "empty answer", err: stage(router.LevelCLI, classifier.ErrEmptyAnswer),
			want: &reasonJSON{reasonEmptyAnswer, "cli", "Jev CLI selection failed: Jev returned no answer"}},
		{name: "chunked routing", err: stage(router.LevelModel, fmt.Errorf("route 2 of 3: %w", classifier.ErrEmptyAnswer)),
			want: &reasonJSON{reasonEmptyAnswer, "model", "Jev model selection failed: Jev returned no answer"}},
		{name: "through the cannot-decide error", err: fmt.Errorf("%w between a, b: %w", router.ErrCannotDecide,
			stage(router.LevelCLI, &classifier.StatusError{Status: 401})),
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

// Jev error bodies: the structured overflow and a 400 of another type, both echoing text that must
// never reach the output.
const (
	tooLongBody = `{"detail":{"error_type":"max_tokens_exceeded","message":"SECRET stage rejected"}}`
	otherBody   = `{"detail":{"error_type":"invalid_request","message":"SECRET stage rejected"}}`
)

func TestApp_RequestTooLong(t *testing.T) {
	const wantMessage = "Jev model selection failed: " + tooLongDetail
	wantReason := map[string]any{"code": reasonTooLong, "stage": "model", "message": wantMessage}

	t.Run("decision", func(t *testing.T) {
		e := failingEnv(t, "model", func(f *fakeJev) { f.failBody = tooLongBody })
		r := e.run([]string{"--cli=alpha", "SECRET PROMPT fix it"}, nil)

		require.Equal(t, exitOK, r.code, r.stderr)
		d := decision(t, r.stdout)
		assert.Equal(t, wantReason, d["reason"])
		assert.Equal(t, map[string]any{
			"model":  map[string]any{"code": nullRoutingFailed, "message": "Model delegated to the child after routing failed"},
			"effort": map[string]any{"code": nullRoutingFailed, "message": "Effort delegated to the child after routing failed"},
		}, d["null_reasons"])
		assert.Len(t, e.jev.requests(), 1, "the overflow is never resent")
		assert.NotContains(t, r.stdout, "SECRET stage rejected")
		assert.NotContains(t, r.stderr, "SECRET stage rejected")
	})

	t.Run("decision with more than one CLI left", func(t *testing.T) {
		e := failingEnv(t, "cli", func(f *fakeJev) { f.failBody = tooLongBody })
		r := e.run([]string{"fix it"}, nil)

		assert.Equal(t, exitUsage, r.code)
		d := decision(t, r.stdout)
		assert.Equal(t, map[string]any{"code": reasonTooLong, "stage": "cli",
			"message": "Jev CLI selection failed: " + tooLongDetail}, d["reason"])
		assert.Len(t, e.jev.requests(), 1, "the overflow is never resent")
	})

	t.Run("exec fallback", func(t *testing.T) {
		e := failingEnv(t, "model", func(f *fakeJev) { f.failBody = tooLongBody })
		r := e.run([]string{"exec", "alpha", "fix it"}, nil)

		require.Equal(t, exitOK, r.code, r.stderr)
		warning, s := fallbackLog(t, r.stderr)
		assert.Contains(t, warning, "Jev routing failed at the model stage (request_too_long)")
		assert.Equal(t, wantReason, s["reason"])
		assert.Len(t, e.jev.requests(), 1, "the overflow is never resent")
		assert.NotContains(t, r.stderr, "SECRET stage rejected")
	})

	t.Run("other 400 and 422 stay API errors", func(t *testing.T) {
		for name, fail := range map[string]func(*fakeJev){
			"400 of another type": func(f *fakeJev) { f.failBody = otherBody },
			"400 with plain text": func(*fakeJev) {},
			"422":                 func(f *fakeJev) { f.failStatus, f.failBody = http.StatusUnprocessableEntity, tooLongBody },
		} {
			t.Run(name, func(t *testing.T) {
				e := failingEnv(t, "model", fail)
				r := e.run([]string{"--cli=alpha", "fix it"}, nil)

				require.Equal(t, exitOK, r.code, r.stderr)
				reason, _ := decision(t, r.stdout)["reason"].(map[string]any)
				assert.Equal(t, map[string]any{"code": reasonAPIError, "stage": "model",
					"message": failureMessage(reasonAPIError, "model")}, reason)
				assert.Len(t, e.jev.requests(), 1, "a rejected request is not resent")
			})
		}
	})
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
		{name: "request too long", fail: func(f *fakeJev) { f.failBody = tooLongBody }, code: reasonTooLong},
		{name: "400 of another type", fail: func(f *fakeJev) { f.failBody = otherBody }, code: reasonAPIError},
		{name: "422", fail: func(f *fakeJev) {
			f.failStatus, f.failBody = http.StatusUnprocessableEntity, tooLongBody
		}, code: reasonAPIError},
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

func TestApp_EffortWithoutEffortArgs(t *testing.T) {
	// gamma has no effort_args and models without efforts, so a caller's --effort cannot be passed
	gamma := strings.NewReplacer("COMMAND", helperCommand(t)).Replace(`
[cli.gamma]
command     = COMMAND
description = A coding agent without effort levels.
exec_args   = ["-p"]
model_args  = ["--model", "{model}"]
prompt_args = ["--", "{prompt}"]

[model.plain]
cli         = gamma
name        = plain-1
description = The only gamma model.
`)
	const warning = "agrouter: warning: dropped --effort high: gamma has no effort_args to pass it\n"
	tests := []struct {
		name        string
		argv        []string
		wantWarning bool
		wantCode    string
	}{
		{name: "decision, effort dropped", argv: []string{"--cli", "gamma", "--effort", "high", "fix it"},
			wantWarning: true, wantCode: nullEffortDropped},
		{name: "decision, no effort asked", argv: []string{"--cli", "gamma", "fix it"}, wantCode: nullNoEffort},
		{name: "exec, effort dropped", argv: []string{"exec", "gamma", "--effort", "high", "fix it"},
			wantWarning: true, wantCode: nullEffortDropped},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.globalConfig(syntheticCLIs(t) + gamma)
			r := e.run(tc.argv, nil)
			require.Equal(t, 0, r.code, r.stderr)
			assert.Empty(t, e.jev.requests(), "one option: no Jev request")

			stderr := r.stderr
			if tc.wantWarning {
				var ok bool
				stderr, ok = strings.CutPrefix(stderr, warning)
				require.True(t, ok, "one warning first: %s", r.stderr)
			}
			assert.NotContains(t, stderr, "warning")

			out := r.stdout
			if tc.argv[0] == "exec" {
				out = strings.TrimPrefix(stderr, selectionPrefix)
				argv, _, _ := e.child()
				assert.NotContains(t, argv, "high", "the child never gets the effort")
			}
			d := decision(t, out)
			assert.Nil(t, d["effort"], "the selection never reports an effort the child does not get")
			assert.Equal(t, tc.wantCode, d["null_reasons"].(map[string]any)["effort"].(map[string]any)["code"])
		})
	}
}
