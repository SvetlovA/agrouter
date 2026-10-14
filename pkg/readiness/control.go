package readiness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/SvetlovA/agrouter/pkg/config"
	"github.com/SvetlovA/agrouter/pkg/runner"
)

// Control-stream request subtypes. These reads are the only requests the sdk-control-usage-v1
// transport sends: it never sends a user event, and so never starts inference.
const (
	controlInitialize = "initialize"
	controlVersion    = "get_binary_version"
	controlUsage      = "get_usage"
	controlSettings   = "get_settings"
)

var controlReads = []string{controlInitialize, controlVersion, controlUsage, controlSettings}

// throttleCodes are control error codes that mean the usage read itself was rate limited.
var throttleCodes = []string{"rate_limit_error", "rate_limited", "throttled"}

// exchangeError is a probe exchange that ended without usable evidence, with its reason code.
type exchangeError struct{ reason string }

func (e *exchangeError) Error() string { return "probe exchange: " + e.reason }

// exchangeReason is the unknown reason for an exchange under ctx that failed with err.
func exchangeReason(ctx context.Context, err error) string {
	var ee *exchangeError
	switch {
	case errors.Is(err, runner.ErrProbeResponseTooLarge):
		return ReasonProbeResponseTooLarge
	case errors.As(err, &ee):
		return ee.reason
	case ctx.Err() != nil:
		return ReasonProbeTimeout
	default:
		return ReasonProbeFailed
	}
}

// addExchange records an exchange's result. A failed exchange adds unknown quota for err, unless it
// failed only after its evidence was complete (reading the defaults), which then stands with whatever
// defaults were verified before the failure.
func (r *Report) addExchange(ctx context.Context, check string, evidence []Evidence, defaults Defaults, err error, now time.Time) {
	switch {
	case err == nil:
		r.add(evidence...)
		r.setDefaults(defaults)
	case evidence != nil:
		r.add(evidence...)
		r.setDefaults(defaults)
	default:
		r.add(unknownQuota(check, exchangeReason(ctx, err), now))
	}
}

// controlRequest is one line the transport writes to the probe's stdin.
type controlRequest struct {
	Type      string         `json:"type"`
	RequestID string         `json:"request_id"`
	Request   map[string]any `json:"request"`
}

// controlLine is one stdout line; only control responses are read, everything else is skipped.
type controlLine struct {
	Type     string         `json:"type"`
	Response *controlResult `json:"response"`
}

type controlResult struct {
	Subtype   string          `json:"subtype"` // success or error
	RequestID string          `json:"request_id"`
	Response  json.RawMessage `json:"response"`
	ErrorCode string          `json:"error_code"`
}

// controlSession is one stream-json control connection.
type controlSession struct {
	probe *runner.Probe
	out   *bufio.Reader
	next  int
}

// read is one control request: a read subtype and its other fields.
type read struct {
	subtype string
	fields  map[string]any
}

// call sends the requests in order and returns the results keyed by subtype. Lines that are not a
// control response, and responses to other request ids, are skipped.
func (s *controlSession) call(requests ...read) (map[string]controlResult, error) {
	pending := make(map[string]string, len(requests)) // request id -> subtype
	for _, r := range requests {
		subtype := r.subtype
		if !slices.Contains(controlReads, subtype) {
			return nil, fmt.Errorf("control request %q is not a read", subtype)
		}
		s.next++
		id := "agrouter-" + strconv.Itoa(s.next)
		req := map[string]any{"subtype": subtype}
		maps.Copy(req, r.fields)
		line, err := json.Marshal(controlRequest{Type: "control_request", RequestID: id, Request: req})
		if err != nil {
			return nil, fmt.Errorf("encode control request: %w", err)
		}
		if _, err := s.probe.Stdin.Write(append(line, '\n')); err != nil {
			return nil, fmt.Errorf("write control request: %w", err)
		}
		pending[id] = subtype
	}
	results := make(map[string]controlResult, len(requests))
	for len(pending) > 0 {
		line, err := s.out.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var l controlLine
			if json.Unmarshal(line, &l) == nil && l.Type == "control_response" && l.Response != nil {
				if subtype, ok := pending[l.Response.RequestID]; ok {
					results[subtype] = *l.Response
					delete(pending, l.Response.RequestID)
					continue
				}
			}
		}
		if err != nil {
			return nil, fmt.Errorf("read control response: %w", err)
		}
	}
	return results, nil
}

// runControl runs an sdk-control-usage-v1 check: initialize, the version when the check has a
// min_version, then the usage and settings reads. The usage read skips the transcript scan.
func (c Checker) runControl(ctx context.Context, cli config.CLI, check config.Check, r *Report) {
	p, err := runner.StartProbe(ctx, c.probeCommand(cli, check))
	if err != nil {
		r.add(unknownQuota(check.Name, ReasonProbeMissing, c.now()))
		return
	}
	s := &controlSession{probe: p, out: bufio.NewReader(p.Stdout)}
	evidence, defaults, err := c.controlExchange(s, check)
	if err != nil {
		p.Kill()
	}
	_, _ = p.Wait() // the replies, not the exit, decide: a read exits 0 even with quota exhausted
	r.addExchange(ctx, check.Name, evidence, defaults, err, c.now())
}

func (c Checker) controlExchange(s *controlSession, check config.Check) ([]Evidence, Defaults, error) {
	first := []read{{subtype: controlInitialize}}
	if check.MinVersion != "" {
		first = append(first, read{subtype: controlVersion})
	}
	res, err := s.call(first...)
	if err != nil {
		return nil, Defaults{}, err
	}
	if res[controlInitialize].Subtype != "success" {
		return nil, Defaults{}, &exchangeError{ReasonProbeFailed}
	}
	if check.MinVersion != "" && !versionSupported(res[controlVersion], check.MinVersion) {
		return nil, Defaults{}, &exchangeError{ReasonVersionUnsupported}
	}

	res, err = s.call(read{subtype: controlUsage, fields: map[string]any{"skip_behaviors": true}})
	if err != nil {
		return nil, Defaults{}, err
	}
	usage := res[controlUsage]
	switch {
	case usage.Subtype == "error" && slices.Contains(throttleCodes, usage.ErrorCode):
		return nil, Defaults{}, &exchangeError{ReasonProbeThrottled}
	case usage.Subtype != "success":
		return nil, Defaults{}, &exchangeError{ReasonProbeFailed}
	}
	evidence, err := usageEvidence(usage.Response, check.Name, c.now())
	if err != nil {
		return nil, Defaults{}, &exchangeError{ReasonProbeMalformed}
	}
	// the settings are read after the usage, so a connection lost reading them keeps the evidence
	res, err = s.call(read{subtype: controlSettings})
	if err != nil {
		return evidence, Defaults{}, err
	}
	return evidence, settingsDefaults(res[controlSettings]), nil
}

// versionSupported reports whether the version read answered with a version at or above lowest.
func versionSupported(res controlResult, lowest string) bool {
	var v struct {
		Version string `json:"version"`
	}
	if res.Subtype != "success" || json.Unmarshal(res.Response, &v) != nil {
		return false
	}
	cmp, ok := compareVersions(v.Version, lowest)
	return ok && cmp >= 0
}

// compareVersions compares the leading dotted numeric parts of a and b ("2.1.3-beta" is 2.1.3);
// ok is false when a has none.
func compareVersions(a, b string) (int, bool) {
	pa, pb := versionParts(a), versionParts(b)
	if len(pa) == 0 {
		return 0, false
	}
	for len(pa) < len(pb) {
		pa = append(pa, 0)
	}
	for len(pb) < len(pa) {
		pb = append(pb, 0)
	}
	for i := range pa {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

func versionParts(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexFunc(v, func(r rune) bool { return r != '.' && !unicode.IsDigit(r) }); i >= 0 {
		v = v[:i]
	}
	var parts []int
	for p := range strings.SplitSeq(v, ".") {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil
		}
		parts = append(parts, n)
	}
	return parts
}

// usageReply is the get_usage success payload; the three fields are required, even when null.
type usageReply struct {
	SubscriptionType    *string     `json:"subscription_type"`
	RateLimitsAvailable *bool       `json:"rate_limits_available"`
	RateLimits          *rateLimits `json:"rate_limits"`
}

type rateLimits struct {
	FiveHour    *usageWindow   `json:"five_hour"`
	SevenDay    *usageWindow   `json:"seven_day"`
	ModelScoped []scopedWindow `json:"model_scoped"`
	ExtraUsage  *extraUsage    `json:"extra_usage"`
}

type usageWindow struct {
	Utilization  *float64        `json:"utilization"` // percent, 0-100
	ResetsAt     *string         `json:"resets_at"`
	LockedReason json.RawMessage `json:"locked_reason"` // any non-null value is a denial; never printed
}

type scopedWindow struct {
	DisplayName string   `json:"display_name"` // the server's label for the model bucket
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

type extraUsage struct {
	IsEnabled         bool `json:"is_enabled"`
	SpendLimitReached bool `json:"spend_limit_reached"`
}

// usageEvidence normalizes a get_usage payload. Both the five-hour and the weekly window are always
// reported, unknown when missing or null; each model-scoped weekly window is scoped to the server's
// label for its model bucket.
func usageEvidence(data json.RawMessage, check string, now time.Time) ([]Evidence, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, errors.New("usage reply is not an object")
	}
	for _, k := range []string{"subscription_type", "rate_limits_available", "rate_limits"} {
		if _, ok := fields[k]; !ok {
			return nil, fmt.Errorf("usage reply has no %s", k)
		}
	}
	var u usageReply
	if err := json.Unmarshal(data, &u); err != nil {
		return nil, fmt.Errorf("usage reply: %w", err)
	}
	quota := func(status Status, reason string) Evidence {
		return Evidence{Dimension: DimensionQuota, Status: status, Reason: reason, Check: check, ObservedAt: now}
	}
	switch {
	case u.SubscriptionType == nil && (u.RateLimitsAvailable == nil || !*u.RateLimitsAvailable):
		// API key or another provider: subscription windows do not apply
		return []Evidence{quota(StatusNotApplicable, ReasonNotSubscription)}, nil
	case u.RateLimits == nil:
		return []Evidence{quota(StatusUnknown, ReasonNotReported)}, nil
	}

	rl := u.RateLimits
	overage := rl.ExtraUsage != nil && rl.ExtraUsage.IsEnabled && !rl.ExtraUsage.SpendLimitReached
	evidence := []Evidence{{Dimension: DimensionAuth, Status: StatusAvailable, Check: check, ObservedAt: now}}
	for _, w := range []struct {
		name string
		w    *usageWindow
	}{{WindowFiveHour, rl.FiveHour}, {WindowWeekly, rl.SevenDay}} {
		e := quota(StatusUnknown, ReasonNotReported)
		e.Window = w.name
		if w.w != nil {
			locked := len(w.w.LockedReason) > 0 && string(w.w.LockedReason) != "null"
			if err := e.fromWindow(w.w.Utilization, w.w.ResetsAt, locked, overage); err != nil {
				return nil, fmt.Errorf("%s: %w", w.name, err)
			}
		}
		evidence = append(evidence, e)
	}
	for i, w := range rl.ModelScoped {
		if strings.TrimSpace(w.DisplayName) == "" {
			return nil, fmt.Errorf("model_scoped[%d]: no display_name", i)
		}
		e := quota(StatusUnknown, ReasonNotReported)
		e.Window, e.Scope = WindowWeekly, Scope{Models: []string{w.DisplayName}}
		if err := e.fromWindow(w.Utilization, w.ResetsAt, false, overage); err != nil {
			return nil, fmt.Errorf("model_scoped[%d]: %w", i, err)
		}
		evidence = append(evidence, e)
	}
	return evidence, nil
}

// fromWindow sets e's status from a window: an explicit lock is unavailable whatever the
// utilization; a full window is unavailable unless an overage path may still pay for the run, which
// is unknown; a null utilization stays unknown. An unparsable reset is left unverified.
func (e *Evidence) fromWindow(utilization *float64, resetsAt *string, locked, overage bool) error {
	if utilization != nil && *utilization < 0 {
		return errors.New("negative utilization")
	}
	if resetsAt != nil {
		if t, err := time.Parse(time.RFC3339, *resetsAt); err == nil {
			e.ResetAt = t.UTC()
		}
	}
	switch {
	case locked:
		e.Status, e.Reason = StatusUnavailable, ReasonQuotaDenied
	case utilization == nil:
	case *utilization >= 100 && overage:
		e.Status, e.Reason = StatusUnknown, ReasonExtraUsageEnabled
	case *utilization >= 100:
		e.Status, e.Reason = StatusUnavailable, ReasonQuotaExhausted
	default:
		e.Status, e.Reason = StatusAvailable, ""
	}
	return nil
}

// settingsDefaults reads the model and effort the session would apply from a get_settings result.
// Anything missing, failed or malformed leaves that default unknown.
func settingsDefaults(res controlResult) Defaults {
	var s struct {
		Applied *struct {
			Model  *string `json:"model"`
			Effort *string `json:"effort"`
		} `json:"applied"`
	}
	if res.Subtype != "success" || json.Unmarshal(res.Response, &s) != nil || s.Applied == nil {
		return Defaults{}
	}
	return Defaults{Model: safeValue(s.Applied.Model), Effort: safeValue(s.Applied.Effort)}
}

// safeValue keeps v when it is a short token without spaces or control characters, safe to print.
func safeValue(v *string) *string {
	if v == nil || *v == "" || len(*v) > 128 || strings.ContainsFunc(*v, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) {
		return nil
	}
	return v
}
