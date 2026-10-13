package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SvetlovA/agrouter/cmd/agrouter/mocks"
	"github.com/SvetlovA/agrouter/pkg/config"
	"github.com/SvetlovA/agrouter/pkg/readiness"
	"github.com/SvetlovA/agrouter/pkg/runner"
)

// testNow is the app clock in tests; resets are relative to it.
var testNow = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

// reportChecker is a checker returning report(cli) for the CLI it is asked about.
func reportChecker(report func(cli config.CLI) readiness.Report) *mocks.ReadinessCheckerMock {
	return &mocks.ReadinessCheckerMock{CheckFunc: func(_ context.Context, cli config.CLI) readiness.Report {
		return report(cli)
	}}
}

// readyReport is a CLI found, logged in and with both subscription windows free.
func readyReport(cli config.CLI) readiness.Report {
	return withQuota(cli,
		readiness.Evidence{Dimension: readiness.DimensionQuota, Status: readiness.StatusAvailable, Window: readiness.WindowFiveHour},
		readiness.Evidence{Dimension: readiness.DimensionQuota, Status: readiness.StatusAvailable, Window: readiness.WindowWeekly})
}

// withQuota is a report of cli with its executable found, auth available, and the given evidence.
func withQuota(cli config.CLI, evidence ...readiness.Evidence) readiness.Report {
	r := readiness.Report{CLI: cli.Name, Evidence: []readiness.Evidence{
		{Dimension: readiness.DimensionExecutable, Status: readiness.StatusAvailable, ObservedAt: testNow},
		{Dimension: readiness.DimensionAuth, Status: readiness.StatusAvailable, ObservedAt: testNow},
	}}
	for _, e := range evidence {
		e.ObservedAt = testNow
		r.Evidence = append(r.Evidence, e)
	}
	return r
}

func exhausted(window string, reset time.Duration, models ...string) readiness.Evidence {
	e := readiness.Evidence{Dimension: readiness.DimensionQuota, Status: readiness.StatusUnavailable,
		Reason: readiness.ReasonQuotaExhausted, Window: window, Scope: readiness.Scope{Models: models}}
	if reset != 0 {
		e.ResetAt = testNow.Add(reset)
	}
	return e
}

func free(window string) readiness.Evidence {
	return readiness.Evidence{Dimension: readiness.DimensionQuota, Status: readiness.StatusAvailable, Window: window}
}

func unknownQuota(reason string) readiness.Evidence {
	return readiness.Evidence{Dimension: readiness.DimensionQuota, Status: readiness.StatusUnknown, Reason: reason}
}

// availability is an [cli.alpha.availability] section with the given extra lines.
func availability(lines ...string) string {
	return "\n[cli.alpha.availability]\n" + strings.Join(lines, "\n") + "\n"
}

// noChild is a runner that fails the test if a child starts.
func noChild(t *testing.T) *mocks.CommandRunnerMock {
	return &mocks.CommandRunnerMock{RunFunc: func(context.Context, runner.Command) (int, error) {
		t.Error("no child may start")
		return 0, nil
	}}
}

func TestApp_ExecPreflightBlocks(t *testing.T) {
	both := []readiness.Evidence{exhausted(readiness.WindowFiveHour, time.Hour), exhausted(readiness.WindowWeekly, 72*time.Hour)}
	tests := []struct {
		name     string
		config   string
		evidence []readiness.Evidence
		auth     readiness.Status // replaces auth available when set
		missing  bool             // the executable is missing
		code     int
		line     string
	}{
		{name: "quota, default prefix, latest reset", config: availability("quota_error_message = Limit reached"),
			evidence: both, code: runner.ExitStartFailure,
			line: "agrouter: Limit reached; five-hour and weekly subscription quota exhausted; resets at 2026-10-16T12:00:00Z"},
		{name: "quota, severity prefix", config: availability(`quota_error_prefix = "ERROR: agrouter: "`, "quota_error_message = Limit reached"),
			evidence: []readiness.Evidence{free(readiness.WindowFiveHour), exhausted(readiness.WindowWeekly, 2*time.Hour)},
			code:     runner.ExitStartFailure,
			line:     "ERROR: agrouter: Limit reached; weekly subscription quota exhausted; resets at 2026-10-13T14:00:00Z"},
		{name: "quota, unverified reset left out", config: availability(),
			evidence: []readiness.Evidence{exhausted(readiness.WindowFiveHour, time.Hour), exhausted(readiness.WindowWeekly, 0)},
			code:     runner.ExitStartFailure, line: "agrouter: five-hour and weekly subscription quota exhausted"},
		{name: "weekly survives a five-hour reset", config: availability(),
			evidence: []readiness.Evidence{free(readiness.WindowFiveHour), exhausted(readiness.WindowWeekly, 24*time.Hour)},
			code:     runner.ExitStartFailure, line: "agrouter: weekly subscription quota exhausted; resets at 2026-10-14T12:00:00Z"},
		{name: "auth failure", auth: readiness.StatusUnavailable, evidence: both[:1], code: runner.ExitStartFailure,
			line: "agrouter: alpha unavailable: auth_failed"},
		{name: "missing executable", missing: true, code: runner.ExitStartFailure,
			line: "agrouter: alpha unavailable: executable_missing"},
		{name: "strict unknown", config: availability("unknown = block"),
			evidence: []readiness.Evidence{unknownQuota(readiness.ReasonProbeThrottled)}, code: exitUsage,
			line: "agrouter: alpha availability unverified: probe_throttled"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.globalConfig(syntheticCLIs(t) + tc.config)
			doc := filepath.Join(e.workDir, "doc.md")
			require.NoError(t, os.WriteFile(doc, []byte("# project\n"), 0o600))
			e.checker = reportChecker(func(cli config.CLI) readiness.Report {
				if tc.missing {
					return readiness.Report{CLI: cli.Name, Evidence: []readiness.Evidence{{Dimension: readiness.DimensionExecutable,
						Status: readiness.StatusUnavailable, Reason: readiness.ReasonExecutableMissing}}}
				}
				r := withQuota(cli, tc.evidence...)
				if tc.auth != "" {
					r.Evidence[1].Status, r.Evidence[1].Reason = tc.auth, readiness.ReasonAuthFailed
				}
				return r
			})
			r := e.runWithRunner([]string{"exec", "alpha", "--doc", doc}, strings.NewReader("fix the bug\n"), noChild(t))

			assert.Equal(t, tc.code, r.code)
			assert.Empty(t, r.stdout)
			assert.Equal(t, tc.line+"\n", r.stderr, "one line, no selection line")
			assert.Empty(t, e.jev.requests(), "no complexity or selection request")
			if tc.evidence == nil || tc.code == exitUsage || tc.auth != "" {
				assert.NotContains(t, r.stderr, "quota")
			}
		})
	}
}

func TestApp_ExecPreflightUnknownAllowed(t *testing.T) {
	tests := []struct {
		name     string
		evidence []readiness.Evidence
		reason   string
	}{
		{name: "throttled probe", evidence: []readiness.Evidence{unknownQuota(readiness.ReasonProbeThrottled)}, reason: "probe_throttled"},
		{name: "one window not reported", evidence: []readiness.Evidence{free(readiness.WindowFiveHour)}, reason: "window_not_reported"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newSyntheticEnv(t)
			e.checker = reportChecker(func(cli config.CLI) readiness.Report { return withQuota(cli, tc.evidence...) })
			const input = "prompt on stdin\x00\r\n"
			r := e.run([]string{"exec", "alpha", "--model=fast-1", "--effort=low"}, strings.NewReader(input))

			require.Equal(t, 0, r.code, r.stderr)
			warning, rest, ok := strings.Cut(r.stderr, "\n")
			require.True(t, ok)
			assert.Equal(t, "agrouter: warning: alpha availability unknown: "+tc.reason, warning)
			assert.Equal(t, "fast-1", selectionLog(t, rest)["model"])
			assert.NotContains(t, r.stderr, "quota")
			_, stdin, _ := e.child()
			assert.Equal(t, input, string(stdin), "stdin is replayed unchanged")
		})
	}
}

func TestApp_ExecPreflightFiltersOptions(t *testing.T) {
	deepBlocked := func(cli config.CLI) readiness.Report {
		return withQuota(cli, free(readiness.WindowFiveHour), free(readiness.WindowWeekly),
			exhausted(readiness.WindowWeekly, time.Hour, "strong"))
	}

	t.Run("a scoped block leaves the other model", func(t *testing.T) {
		e := newSyntheticEnv(t)
		e.checker = reportChecker(deepBlocked)
		e.jev.pick = "fast@high"
		r := e.run([]string{"exec", "alpha", "fix it"}, nil)

		require.Equal(t, 0, r.code, r.stderr)
		assert.Equal(t, map[string]any{"cli": "alpha", "model": "fast-1", "effort": "high",
			"confidence": map[string]any{"effort": 0.9},
			"readiness":  map[string]any{"excluded": map[string]any{"quota_exhausted": float64(2)}}}, selectionLog(t, r.stderr))
		assert.Equal(t, []string{"effort"}, e.jev.asked(), "one model left: only the effort stage asks Jev")
	})

	t.Run("a fixed model is never broadened", func(t *testing.T) {
		e := newSyntheticEnv(t)
		e.checker = reportChecker(deepBlocked)
		r := e.runWithRunner([]string{"exec", "alpha", "--model=strong", "fix it"}, nil, noChild(t))

		assert.Equal(t, runner.ExitStartFailure, r.code)
		assert.Equal(t, "agrouter: weekly subscription quota exhausted; resets at 2026-10-13T13:00:00Z\n", r.stderr)
		assert.Empty(t, e.jev.requests())
	})

	t.Run("no switch to another CLI", func(t *testing.T) {
		e := newSyntheticEnv(t)
		checker := reportChecker(func(cli config.CLI) readiness.Report {
			return withQuota(cli, exhausted(readiness.WindowFiveHour, time.Hour), free(readiness.WindowWeekly))
		})
		e.checker = checker
		r := e.runWithRunner([]string{"exec", "alpha", "fix it"}, nil, noChild(t))

		assert.Equal(t, runner.ExitStartFailure, r.code)
		require.Len(t, checker.CheckCalls(), 1)
		assert.Equal(t, "alpha", checker.CheckCalls()[0].Cli.Name, "only the exec CLI is checked")
		assert.Empty(t, e.jev.requests())
	})

	t.Run("a single candidate is still checked", func(t *testing.T) {
		e := newSyntheticEnv(t)
		checker := reportChecker(readyReport)
		e.checker = checker
		r := e.run([]string{"exec", "alpha", "--model=fast-1", "--effort=low", "fix it"}, nil)

		require.Equal(t, 0, r.code, r.stderr)
		assert.Len(t, checker.CheckCalls(), 1)
		assert.Empty(t, e.jev.requests())
		assert.Equal(t, map[string]any{"cli": "alpha", "model": "fast-1", "effort": "low"}, selectionLog(t, r.stderr),
			"no warning when everything is verified")
	})
}

func TestApp_ExecPreflightContextFlag(t *testing.T) {
	e := newEnv(t)
	e.globalConfig(syntheticCLIs(t) + "\n[cli.alpha]\ncontext_flags = --profile\n" + availability())
	checker := reportChecker(func(cli config.CLI) readiness.Report {
		assert.Nil(t, cli.Availability, "no probe runs for another context")
		return readiness.Report{CLI: cli.Name, Evidence: []readiness.Evidence{
			{Dimension: readiness.DimensionExecutable, Status: readiness.StatusAvailable},
			unknownQuota(readiness.ReasonNotConfigured),
		}}
	})
	e.checker = checker
	r := e.run([]string{"exec", "alpha", "--model=fast-1", "--effort=low", "fix it", "--", "--profile", "work"}, nil)

	require.Equal(t, 0, r.code, r.stderr)
	assert.True(t, strings.HasPrefix(r.stderr, "agrouter: warning: alpha availability unknown: context_changed\n"), r.stderr)
	assert.Len(t, checker.CheckCalls(), 1)
}

func TestApp_ExecPreflightDeadline(t *testing.T) {
	e := newEnv(t)
	e.globalConfig(syntheticCLIs(t) + "\n[agrouter]\ntimeout = 200ms\n")
	e.checker = &mocks.ReadinessCheckerMock{CheckFunc: func(ctx context.Context, cli config.CLI) readiness.Report {
		<-ctx.Done() // a probe that never answers is cut off by the routing deadline
		return withQuota(cli, unknownQuota(readiness.ReasonProbeTimeout))
	}}
	start := time.Now()
	r := e.run([]string{"exec", "alpha", "--model=fast-1", "--effort=low"}, strings.NewReader("fix it\n"))

	require.Equal(t, 0, r.code, r.stderr)
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.True(t, strings.HasPrefix(r.stderr, "agrouter: warning: alpha availability unknown: probe_timeout\n"), r.stderr)
	_, stdin, _ := e.child()
	assert.Equal(t, "fix it\n", string(stdin))
}

func TestApp_ExecPreflightEarlyFailureStopsProbe(t *testing.T) {
	e := newSyntheticEnv(t)
	canceled := make(chan struct{})
	e.checker = &mocks.ReadinessCheckerMock{CheckFunc: func(ctx context.Context, cli config.CLI) readiness.Report {
		<-ctx.Done()
		close(canceled)
		return withQuota(cli)
	}}
	r := e.runWithRunner([]string{"exec", "alpha", "--doc", filepath.Join(e.workDir, "missing.md"), "fix it"}, nil, noChild(t))

	assert.Equal(t, exitUsage, r.code)
	assert.Contains(t, r.stderr, "--doc")
	select {
	case <-canceled:
	default:
		t.Error("the probe must be stopped before agrouter exits")
	}
}

func TestApp_DecisionNeverChecksReadiness(t *testing.T) {
	e := newEnv(t)
	e.globalConfig(strings.ReplaceAll(syntheticCLIs(t), helperCommand(t), filepath.Join(t.TempDir(), "no-such-cli")) +
		availability("unknown = block"))
	doc := filepath.Join(e.workDir, "doc.md")
	require.NoError(t, os.WriteFile(doc, []byte("# project\n"), 0o600))
	e.checker = &mocks.ReadinessCheckerMock{CheckFunc: func(context.Context, config.CLI) readiness.Report {
		t.Error("decision mode must not check readiness")
		return readiness.Report{}
	}}
	e.jev.pick = "fast@low"
	r := e.run([]string{"--doc", doc, "fix it"}, nil)

	require.Equal(t, 0, r.code, r.stderr)
	assert.Equal(t, "alpha", decision(t, r.stdout)["cli"])
}

func TestApp_ExecPreflightWithJSONHelper(t *testing.T) {
	probe := func(weekly string) string {
		return `{"schema":"agrouter.readiness/v1","auth":{"status":"available"},"quota":[` +
			`{"window":"five_hour","status":"available"},` + weekly + `]}`
	}
	tests := []struct {
		name   string
		output string
		code   int
		stderr string
	}{
		{name: "ready", output: probe(`{"window":"weekly","status":"available"}`)},
		{name: "weekly exhausted", code: runner.ExitStartFailure,
			output: probe(`{"window":"weekly","status":"unavailable","reason":"quota_exhausted","reset_at":"2026-10-14T00:00:00Z"}`),
			stderr: "agrouter: Limit reached; weekly subscription quota exhausted; resets at 2026-10-14T00:00:00Z\n"},
		{name: "malformed output", output: "not json", stderr: "agrouter: warning: alpha availability unknown: probe_malformed\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.globalConfig(syntheticCLIs(t) + availability("checks = usage", "quota_error_message = Limit reached") +
				"\n[cli.alpha.availability.usage]\ntransport = json-helper\nargs = [\"" + helperProbeArg + "\"]\n")
			e.checker = readiness.Checker{Dir: e.workDir}
			t.Setenv(helperProbe, tc.output)
			r := e.run([]string{"exec", "alpha", "--model=fast-1", "--effort=low", "fix it"}, nil)

			assert.Equal(t, tc.code, r.code, r.stderr)
			if tc.code != 0 {
				assert.Equal(t, tc.stderr, r.stderr)
				return
			}
			assert.True(t, strings.HasPrefix(r.stderr, tc.stderr+selectionPrefix), r.stderr)
			argv, _, _ := e.child()
			assert.NotContains(t, argv, helperProbeArg, "the child is the real run, not the probe")
		})
	}
}

func TestQuotaLine(t *testing.T) {
	reset := testNow.In(time.FixedZone("x", 3*3600))
	tests := []struct {
		name string
		a    *config.Availability
		v    readiness.Verdict
		want string
	}{
		{name: "no availability section", v: readiness.Verdict{Windows: []string{readiness.WindowWeekly}},
			want: "agrouter: weekly subscription quota exhausted"},
		{name: "account denial without a window", a: &config.Availability{QuotaErrorPrefix: "", QuotaErrorMessage: "Limit"},
			v: readiness.Verdict{}, want: "Limit; subscription quota exhausted"},
		{name: "whitespace preserved, reset in UTC", a: &config.Availability{QuotaErrorPrefix: "WARN  ", QuotaErrorMessage: " x "},
			v:    readiness.Verdict{Windows: []string{readiness.WindowFiveHour}, ResetAt: reset},
			want: "WARN   x ; five-hour subscription quota exhausted; resets at 2026-10-13T12:00:00Z"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, quotaLine(tc.a, tc.v))
		})
	}
}
