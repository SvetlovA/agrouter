// Package readiness checks, without a prompt, whether the exec CLI can run: its executable,
// authentication, subscription quota and model capability, as reported by the probes its
// [cli.<name>.availability] section configures. It returns evidence; it never routes.
package readiness

import (
	"context"
	"errors"
	"time"

	"github.com/SvetlovA/agrouter/pkg/config"
	"github.com/SvetlovA/agrouter/pkg/runner"
)

// Status is one dimension's verdict.
type Status string

// Statuses. Unknown is never ready: it is what a check that could not verify reports.
const (
	StatusAvailable     Status = "available"
	StatusUnavailable   Status = "unavailable"
	StatusUnknown       Status = "unknown"
	StatusNotApplicable Status = "not_applicable" // e.g. quota under API-key or custom-provider auth
)

// Dimension is what a piece of evidence is about. Auth success never implies quota success.
type Dimension string

// Dimensions.
const (
	DimensionExecutable Dimension = "executable"
	DimensionAuth       Dimension = "auth"
	DimensionQuota      Dimension = "quota"
	DimensionCapability Dimension = "capability"
)

// Quota windows.
const (
	WindowFiveHour = "five_hour"
	WindowWeekly   = "weekly"
)

// Reason codes. They are controlled values: safe to print, never a raw response or error body.
const (
	ReasonNotConfigured         = "availability_not_configured"
	ReasonDisabled              = "availability_disabled"
	ReasonExecutableMissing     = "executable_missing"
	ReasonProbeMissing          = "probe_missing"
	ReasonProbeTimeout          = "probe_timeout"
	ReasonProbeFailed           = "probe_failed"
	ReasonProbeResponseTooLarge = "probe_response_too_large"
	ReasonProbeMalformed        = "probe_malformed"
	ReasonProbeThrottled        = "probe_throttled"
	ReasonTransportUnsupported  = "transport_unsupported"
	ReasonNotReported           = "not_reported"
	ReasonQuotaExhausted        = "quota_exhausted"
	ReasonAuthFailed            = "auth_failed"
	ReasonUnsupportedModel      = "unsupported_model"
)

// Scope is what a piece of evidence applies to: the whole account when Models is empty, otherwise
// only those models.
type Scope struct {
	Models []string
}

// Account reports whether the evidence applies to every model.
func (s Scope) Account() bool { return len(s.Models) == 0 }

// Evidence is one observation, kept in memory only.
type Evidence struct {
	Dimension  Dimension
	Status     Status
	Reason     string // a Reason* code or a validated code from a helper
	Check      string // the check that observed it; empty for the executable lookup
	Scope      Scope
	Window     string // WindowFiveHour or WindowWeekly for quota, when known
	ObservedAt time.Time
	ResetAt    time.Time // zero when unverified
}

// Defaults is the child's effective default model and effort as a probe reported them; nil is unknown.
type Defaults struct {
	Model  *string
	Effort *string
}

// Report is everything one CLI's checks observed.
type Report struct {
	CLI      string
	Evidence []Evidence
	Defaults Defaults
}

// Checker runs a CLI's availability checks.
type Checker struct {
	Env         []string         // the child's environment before filtering; nil means os.Environ()
	Dir         string           // the real working directory the child would run in; empty for the current one
	Now         func() time.Time // nil means time.Now
	MaxResponse int64            // probe stdout limit; zero for the runner's default
}

// Check runs cli's checks once, under ctx and the availability timeout, and reports what they found.
// A check that cannot verify reports unknown evidence; nothing it does returns an error.
func (c Checker) Check(ctx context.Context, cli config.CLI) Report {
	r := Report{CLI: cli.Name}
	now := c.now()
	if _, err := runner.LookPath(cli.Command); err != nil {
		r.add(Evidence{Dimension: DimensionExecutable, Status: StatusUnavailable, Reason: ReasonExecutableMissing, ObservedAt: now})
		return r
	}
	r.add(Evidence{Dimension: DimensionExecutable, Status: StatusAvailable, ObservedAt: now})

	a := cli.Availability
	switch {
	case a == nil || len(a.Checks) == 0:
		r.add(unknownQuota("", ReasonNotConfigured, now))
		return r
	case !a.Enabled:
		r.add(unknownQuota("", ReasonDisabled, now))
		return r
	}
	ctx, cancel := context.WithTimeout(ctx, a.Timeout)
	defer cancel()
	for _, check := range a.Checks {
		switch check.Transport {
		case config.TransportJSONHelper:
			c.runHelper(ctx, cli, check, &r)
		default:
			r.add(unknownQuota(check.Name, ReasonTransportUnsupported, c.now()))
		}
	}
	if !r.has(DimensionQuota) {
		r.add(unknownQuota("", ReasonNotReported, c.now()))
	}
	return r
}

// probe runs check's command (the CLI's own when unset) with stdin as its whole input. A probe that
// cannot run, times out, overflows or exits nonzero returns its unknown reason instead of a response.
func (c Checker) probe(ctx context.Context, cli config.CLI, check config.Check, stdin []byte) ([]byte, string) {
	command := check.Command
	if command == "" {
		command = cli.Command
	}
	res, err := runner.RunProbe(ctx, runner.ProbeCommand{
		Argv:        append([]string{command}, check.Args...),
		Env:         c.Env,
		Dir:         c.Dir,
		MaxResponse: c.MaxResponse,
	}, stdin)
	var startErr *runner.StartError
	switch {
	case err != nil && ctx.Err() != nil:
		return nil, ReasonProbeTimeout
	case errors.As(err, &startErr):
		return nil, ReasonProbeMissing
	case errors.Is(err, runner.ErrProbeResponseTooLarge):
		return nil, ReasonProbeResponseTooLarge
	case err != nil:
		return nil, ReasonProbeTimeout
	case res.ExitCode != 0:
		return nil, ReasonProbeFailed
	}
	return res.Stdout, ""
}

func (c Checker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (r *Report) add(e ...Evidence) { r.Evidence = append(r.Evidence, e...) }

func (r *Report) has(d Dimension) bool {
	for _, e := range r.Evidence {
		if e.Dimension == d {
			return true
		}
	}
	return false
}

func unknownQuota(check, reason string, now time.Time) Evidence {
	return Evidence{Dimension: DimensionQuota, Status: StatusUnknown, Reason: reason, Check: check, ObservedAt: now}
}
