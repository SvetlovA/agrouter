package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/SvetlovA/agrouter/pkg/args"
	"github.com/SvetlovA/agrouter/pkg/config"
	"github.com/SvetlovA/agrouter/pkg/readiness"
	"github.com/SvetlovA/agrouter/pkg/router"
	"github.com/SvetlovA/agrouter/pkg/runner"
)

//go:generate go tool moq -out mocks/readiness_checker.go -pkg mocks -skip-ensure . ReadinessChecker

// ReadinessChecker runs the exec CLI's availability checks; readiness.Checker implements it.
type ReadinessChecker interface {
	// Check reports what cli's checks observed. It never fails: a check that cannot verify reports
	// unknown evidence.
	Check(ctx context.Context, cli config.CLI) readiness.Report
}

var _ ReadinessChecker = readiness.Checker{}

// preflight is the exec CLI's readiness check, running while the input is captured.
type preflight struct {
	done   chan struct{}
	report readiness.Report
}

// startPreflight checks cli under ctx in the background. Native arguments that switch the child's
// context make the probes moot: only the executable is looked up, and the rest is unknown.
func (a *app) startPreflight(ctx context.Context, cli config.CLI, raw []string) *preflight {
	p := &preflight{done: make(chan struct{})}
	_, changed := args.ContextFlag(cli, raw)
	probed := cli
	if changed {
		probed.Availability = nil
	}
	go func() {
		defer close(p.done)
		p.report = a.checker.Check(ctx, probed)
		if changed {
			p.report = p.report.ContextChanged(a.clock())
		}
	}()
	return p
}

// wait returns the report once the checks are done.
func (p *preflight) wait() readiness.Report {
	<-p.done
	return p.report
}

// applyPreflight removes the options cli cannot run now from el, and warns once when what is left
// is unverified. When nothing is left it returns the error that stops exec before any Jev request.
func (a *app) applyPreflight(cli config.CLI, models []config.Model, report readiness.Report, el *router.Eligibility) error {
	ev := readiness.Evaluate(report, el.Options, models, unknownPolicy(cli), a.clock())
	a.debug.readiness(report, ev)
	if ev.Blocked != nil {
		return preflightBlock(cli, *ev.Blocked)
	}
	el.Exclude(ev.Excluded)
	if ev.Unknown != "" {
		fmt.Fprintf(a.stderr, "agrouter: warning: %s availability unknown: %s\n", cli.Name, ev.Unknown)
	}
	return nil
}

// unknownPolicy is cli's unknown availability policy; a CLI without availability checks allows.
func unknownPolicy(cli config.CLI) string {
	if cli.Availability == nil {
		return config.UnknownAllow
	}
	return cli.Availability.Unknown
}

// preflightError stops exec before any Jev request: line is the whole stderr line, printed as is, and
// code the exit code.
type preflightError struct {
	line string
	code int
}

func (e *preflightError) Error() string { return e.line }

// preflightBlock is the error for the verdict that left cli nothing to run: a strict unknown block is
// a usage-class exit, a known missing executable, failed auth or exhausted quota is the exit of a
// child that cannot start. Only a quota block is reported in the configured quota wording.
func preflightBlock(cli config.CLI, v readiness.Verdict) *preflightError {
	switch {
	case v.Status != readiness.StatusUnavailable:
		return &preflightError{line: fmt.Sprintf("agrouter: %s availability unverified: %s", cli.Name, v.Reason), code: exitUsage}
	case v.Dimension == readiness.DimensionQuota:
		return &preflightError{line: quotaLine(cli.Availability, v), code: runner.ExitStartFailure}
	}
	return &preflightError{line: fmt.Sprintf("agrouter: %s unavailable: %s", cli.Name, v.Reason), code: runner.ExitStartFailure}
}

// windowNames are the quota windows as the quota line names them.
var windowNames = map[string]string{readiness.WindowFiveHour: "five-hour", readiness.WindowWeekly: "weekly"}

// quotaLine is "<prefix><message>; <windows> subscription quota exhausted[; resets at <RFC 3339>]",
// the message part left out when unset. The reset is the latest of the blocking windows' resets and
// is left out when any is unverified.
func quotaLine(a *config.Availability, v readiness.Verdict) string {
	prefix, message := config.DefaultQuotaErrorPrefix, ""
	if a != nil {
		prefix, message = a.QuotaErrorPrefix, a.QuotaErrorMessage
	}
	windows := make([]string, len(v.Windows))
	for i, w := range v.Windows {
		windows[i] = windowNames[w]
	}
	exhausted := strings.TrimSpace(strings.Join(windows, " and ") + " subscription quota exhausted")
	parts := []string{exhausted}
	if message != "" {
		parts = []string{message, exhausted}
	}
	if !v.ResetAt.IsZero() {
		parts = append(parts, "resets at "+v.ResetAt.UTC().Format(time.RFC3339))
	}
	return prefix + strings.Join(parts, "; ")
}
