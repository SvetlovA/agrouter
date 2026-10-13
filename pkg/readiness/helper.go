package readiness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"time"

	"github.com/SvetlovA/agrouter/pkg/config"
)

// HelperSchema is the version of the evidence JSON a json-helper prints on stdout.
const HelperSchema = "agrouter.readiness/v1"

// reasonRe is a reason code a helper may report; anything else could carry raw text to stderr.
var reasonRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// helperOutput is the json-helper stdout. Every section is optional; a missing one is unknown.
type helperOutput struct {
	Schema     string          `json:"schema"`
	Auth       *helperStatus   `json:"auth"`
	Quota      []helperWindow  `json:"quota"`
	Capability *helperCapacity `json:"capability"`
	Defaults   *helperDefaults `json:"defaults"`
}

type helperStatus struct {
	Status Status `json:"status"`
	Reason string `json:"reason"`
}

type helperWindow struct {
	Window  string   `json:"window"`
	Status  Status   `json:"status"`
	Reason  string   `json:"reason"`
	Models  []string `json:"models"`
	ResetAt string   `json:"reset_at"`
}

type helperCapacity struct {
	UnsupportedModels []string `json:"unsupported_models"`
}

type helperDefaults struct {
	Model  *string `json:"model"`
	Effort *string `json:"effort"`
}

// runHelper runs a json-helper check with empty stdin and adds what its stdout reports. Any failure,
// including output that does not match the schema, is unknown quota.
func (c Checker) runHelper(ctx context.Context, cli config.CLI, check config.Check, r *Report) {
	out, reason := c.probe(ctx, cli, check, nil)
	if reason != "" {
		r.add(unknownQuota(check.Name, reason, c.now()))
		return
	}
	evidence, defaults, err := parseHelper(out, check.Name, c.now())
	if err != nil {
		r.add(unknownQuota(check.Name, ReasonProbeMalformed, c.now()))
		return
	}
	r.add(evidence...)
	if defaults.Model != nil {
		r.Defaults.Model = defaults.Model
	}
	if defaults.Effort != nil {
		r.Defaults.Effort = defaults.Effort
	}
}

// parseHelper decodes and validates one json-helper response. The error never quotes the response.
func parseHelper(data []byte, check string, now time.Time) ([]Evidence, Defaults, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var out helperOutput
	if err := dec.Decode(&out); err != nil {
		return nil, Defaults{}, errors.New("not the helper evidence JSON")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, Defaults{}, errors.New("trailing data after the helper evidence JSON")
	}
	if out.Schema != HelperSchema {
		return nil, Defaults{}, fmt.Errorf("schema is not %s", HelperSchema)
	}

	var evidence []Evidence
	if out.Auth != nil {
		reason, err := statusReason(out.Auth.Status, out.Auth.Reason, ReasonAuthFailed)
		if err != nil {
			return nil, Defaults{}, fmt.Errorf("auth: %w", err)
		}
		evidence = append(evidence, Evidence{Dimension: DimensionAuth, Status: out.Auth.Status, Reason: reason,
			Check: check, ObservedAt: now})
	}
	for i, w := range out.Quota {
		e, err := w.evidence(check, now)
		if err != nil {
			return nil, Defaults{}, fmt.Errorf("quota[%d]: %w", i, err)
		}
		evidence = append(evidence, e)
	}
	if out.Capability != nil && len(out.Capability.UnsupportedModels) > 0 {
		if err := checkModels(out.Capability.UnsupportedModels); err != nil {
			return nil, Defaults{}, fmt.Errorf("capability: %w", err)
		}
		evidence = append(evidence, Evidence{Dimension: DimensionCapability, Status: StatusUnavailable,
			Reason: ReasonUnsupportedModel, Check: check, Scope: Scope{Models: out.Capability.UnsupportedModels}, ObservedAt: now})
	}
	var defaults Defaults
	if d := out.Defaults; d != nil {
		if (d.Model != nil && *d.Model == "") || (d.Effort != nil && *d.Effort == "") {
			return nil, Defaults{}, errors.New("defaults: empty value (use null for unknown)")
		}
		defaults = Defaults{Model: d.Model, Effort: d.Effort}
	}
	return evidence, defaults, nil
}

func (w helperWindow) evidence(check string, now time.Time) (Evidence, error) {
	if w.Window != WindowFiveHour && w.Window != WindowWeekly {
		return Evidence{}, fmt.Errorf("window must be %s or %s", WindowFiveHour, WindowWeekly)
	}
	if err := checkModels(w.Models); err != nil {
		return Evidence{}, err
	}
	reason, err := statusReason(w.Status, w.Reason, ReasonQuotaExhausted)
	if err != nil {
		return Evidence{}, err
	}
	e := Evidence{Dimension: DimensionQuota, Status: w.Status, Reason: reason, Check: check,
		Scope: Scope{Models: w.Models}, Window: w.Window, ObservedAt: now}
	if w.ResetAt != "" {
		if e.ResetAt, err = time.Parse(time.RFC3339, w.ResetAt); err != nil {
			return Evidence{}, errors.New("reset_at is not an RFC 3339 time")
		}
		e.ResetAt = e.ResetAt.UTC()
	}
	return e, nil
}

// statusReason validates status and reason, defaulting an unavailable status's reason to blocked and
// an unknown one's to ReasonNotReported.
func statusReason(status Status, reason, blocked string) (string, error) {
	switch status {
	case StatusAvailable, StatusNotApplicable:
	case StatusUnavailable:
		if reason == "" {
			reason = blocked
		}
	case StatusUnknown:
		if reason == "" {
			reason = ReasonNotReported
		}
	default:
		return "", errors.New("status must be available, unavailable, unknown or not_applicable")
	}
	if reason != "" && !reasonRe.MatchString(reason) {
		return "", errors.New("reason must be a lowercase code")
	}
	return reason, nil
}

func checkModels(models []string) error {
	if slices.Contains(models, "") {
		return errors.New("empty model name")
	}
	return nil
}
