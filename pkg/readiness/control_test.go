package readiness

import (
	"bufio"
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SvetlovA/agrouter/pkg/config"
)

// controlHelper answers control requests from the fixture at $READINESS_HELPER_FIXTURE, whose lines
// are {"on": subtype, "reply": response}. Before each reply it writes an unrelated event and a
// response to another request id; it appends every stdin line to $READINESS_HELPER_RECORD, and exits
// when stdin closes or a request has no reply in the fixture.
func controlHelper() int {
	data, err := os.ReadFile(os.Getenv(helperFixture))
	if err != nil {
		return 97
	}
	replies := map[string]map[string]any{}
	for line := range strings.Lines(string(data)) {
		var f struct {
			On    string         `json:"on"`
			Reply map[string]any `json:"reply"`
		}
		if json.Unmarshal([]byte(line), &f) != nil {
			return 96
		}
		replies[f.On] = f.Reply
	}
	record, err := os.OpenFile(os.Getenv(helperRecord), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return 95
	}
	defer record.Close()

	out := json.NewEncoder(os.Stdout)
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		_, _ = record.Write(append(in.Bytes(), '\n'))
		var req struct {
			RequestID string `json:"request_id"`
			Request   struct {
				Subtype string `json:"subtype"`
			} `json:"request"`
		}
		_ = json.Unmarshal(in.Bytes(), &req)
		reply, ok := replies[req.Request.Subtype]
		if !ok {
			return 0
		}
		_ = out.Encode(map[string]any{"type": "system", "subtype": "status"})
		_ = out.Encode(map[string]any{"type": "control_response",
			"response": map[string]any{"subtype": "success", "request_id": "unrelated", "response": map[string]any{}}})
		r := map[string]any{"request_id": req.RequestID}
		maps.Copy(r, reply)
		_ = out.Encode(map[string]any{"type": "control_response", "response": r})
	}
	return 0
}

// controlRun checks a synthetic CLI with one control check against fixture and returns the report
// and the requests the fake received.
func controlRun(t *testing.T, fixture, minVersion string, mutate func(*Checker, *config.CLI)) (Report, []map[string]any) {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("testdata", "control", fixture+".jsonl"))
	require.NoError(t, err)
	record := filepath.Join(t.TempDir(), "requests.jsonl")
	c := checker("control", helperFixture+"="+path, helperRecord+"="+record)
	cli := helperCLI(config.Check{Name: "usage", Transport: config.TransportSDKControlUsage, MinVersion: minVersion})
	if mutate != nil {
		mutate(&c, &cli)
	}
	r := c.Check(context.Background(), cli)

	var requests []map[string]any
	if data, err := os.ReadFile(record); err == nil { //nolint:gosec // test file
		for line := range strings.Lines(string(data)) {
			var m map[string]any
			require.NoError(t, json.Unmarshal([]byte(line), &m), "every request is one JSON line")
			requests = append(requests, m)
		}
	}
	return r, requests
}

func TestControl_Evidence(t *testing.T) {
	reset1 := time.Date(2026, 10, 13, 15, 0, 0, 123456000, time.UTC)
	reset2 := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	q := func(status Status, reason, window string, reset time.Time, models ...string) Evidence {
		return Evidence{Dimension: DimensionQuota, Status: status, Reason: reason, Check: "usage",
			Scope: Scope{Models: models}, Window: window, ObservedAt: observed, ResetAt: reset}
	}
	authOK := Evidence{Dimension: DimensionAuth, Status: StatusAvailable, Check: "usage", ObservedAt: observed}
	tests := []struct {
		fixture    string
		evidence   []Evidence
		unverified bool // defaults stay unknown
	}{
		{fixture: "available", evidence: []Evidence{authOK,
			q(StatusAvailable, "", WindowFiveHour, reset1), q(StatusAvailable, "", WindowWeekly, reset2)}},
		{fixture: "weekly_exhausted", evidence: []Evidence{authOK,
			q(StatusAvailable, "", WindowFiveHour, reset1), q(StatusUnavailable, ReasonQuotaExhausted, WindowWeekly, reset2)}},
		{fixture: "both_exhausted", evidence: []Evidence{authOK,
			q(StatusUnavailable, ReasonQuotaExhausted, WindowFiveHour, reset1),
			q(StatusUnavailable, ReasonQuotaExhausted, WindowWeekly, reset2)}},
		{fixture: "missing_windows", evidence: []Evidence{authOK,
			q(StatusUnknown, ReasonNotReported, WindowFiveHour, time.Time{}),
			q(StatusUnknown, ReasonNotReported, WindowWeekly, time.Time{})}},
		{fixture: "model_scoped", evidence: []Evidence{authOK,
			q(StatusAvailable, "", WindowFiveHour, reset1), q(StatusAvailable, "", WindowWeekly, reset2),
			q(StatusUnavailable, ReasonQuotaExhausted, WindowWeekly, reset2, "M-1"),
			q(StatusAvailable, "", WindowWeekly, time.Time{}, "M-3")}},
		{fixture: "extra_usage", evidence: []Evidence{authOK,
			q(StatusAvailable, "", WindowFiveHour, reset1), q(StatusUnknown, ReasonExtraUsageEnabled, WindowWeekly, reset2)}},
		{fixture: "extra_usage_spent", evidence: []Evidence{authOK,
			q(StatusAvailable, "", WindowFiveHour, reset1), q(StatusUnavailable, ReasonQuotaExhausted, WindowWeekly, reset2)}},
		{fixture: "locked", evidence: []Evidence{authOK,
			q(StatusUnavailable, ReasonQuotaDenied, WindowFiveHour, reset1), q(StatusAvailable, "", WindowWeekly, reset2)}},
		{fixture: "api_key", evidence: []Evidence{q(StatusNotApplicable, ReasonNotSubscription, "", time.Time{})},
			unverified: true},
		{fixture: "fetch_failed", evidence: []Evidence{q(StatusUnknown, ReasonNotReported, "", time.Time{})}},
		// a CLI that exits before the settings read keeps the quota it already reported
		{fixture: "no_settings_reply", evidence: []Evidence{authOK,
			q(StatusAvailable, "", WindowFiveHour, reset1), q(StatusUnavailable, ReasonQuotaExhausted, WindowWeekly, reset2)},
			unverified: true},
		{fixture: "settings_error", evidence: []Evidence{authOK,
			q(StatusAvailable, "", WindowFiveHour, reset1), q(StatusAvailable, "", WindowWeekly, reset2)},
			unverified: true},
		{fixture: "unsafe_defaults", evidence: []Evidence{authOK,
			q(StatusAvailable, "", WindowFiveHour, reset1), q(StatusAvailable, "", WindowWeekly, reset2)},
			unverified: true},
	}
	verified := Defaults{Model: new("m-2"), Effort: new("e-1")}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			r, _ := controlRun(t, tt.fixture, "2.1.0", nil)
			want := append([]Evidence{{Dimension: DimensionExecutable, Status: StatusAvailable, ObservedAt: observed}},
				tt.evidence...)
			assert.Equal(t, want, r.Evidence)
			if tt.unverified {
				assert.Equal(t, Defaults{}, r.Defaults)
			} else {
				assert.Equal(t, verified, r.Defaults)
			}
		})
	}
}

func TestControl_Unknown(t *testing.T) {
	tests := []struct {
		name       string
		fixture    string
		minVersion string
		mutate     func(*Checker, *config.CLI)
		reason     string
	}{
		{name: "unsupported version", fixture: "old_version", minVersion: "2.1.0", reason: ReasonVersionUnsupported},
		{name: "version not reported", fixture: "no_version", minVersion: "2.1.0", reason: ReasonVersionUnsupported},
		{name: "unknown shape", fixture: "unknown_shape", reason: ReasonProbeMalformed},
		{name: "invalid window", fixture: "bad_window", reason: ReasonProbeMalformed},
		{name: "throttled", fixture: "throttled", reason: ReasonProbeThrottled},
		{name: "usage read error", fixture: "usage_error", reason: ReasonProbeFailed},
		{name: "initialize error", fixture: "init_error", reason: ReasonProbeFailed},
		{name: "exits before replying", fixture: "no_usage_reply", reason: ReasonProbeFailed},
		{name: "response too large", fixture: "available", reason: ReasonProbeResponseTooLarge,
			mutate: func(c *Checker, _ *config.CLI) { c.MaxResponse = 64 }},
		{name: "missing probe", fixture: "available", reason: ReasonProbeMissing,
			mutate: func(_ *Checker, cli *config.CLI) {
				cli.Availability.Checks[0].Command = filepath.Join(os.TempDir(), "no-such-probe-x")
			}},
		{name: "timeout", fixture: "available", reason: ReasonProbeTimeout,
			mutate: func(c *Checker, cli *config.CLI) {
				c.Env = append(c.Env, helperMode+"=sleep")
				cli.Availability.Timeout = 200 * time.Millisecond
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := controlRun(t, tt.fixture, tt.minVersion, tt.mutate)
			quota := evidenceOf(r, DimensionQuota)
			require.Len(t, quota, 1)
			assert.Equal(t, StatusUnknown, quota[0].Status, "an unverified check is never ready")
			assert.Equal(t, tt.reason, quota[0].Reason)
			assert.Equal(t, "usage", quota[0].Check)
			assert.Empty(t, evidenceOf(r, DimensionAuth))
			assert.Equal(t, Defaults{}, r.Defaults)
		})
	}
}

// the fake must only ever see read-only control requests: no user event, no prompt, no inference
func TestControl_RequestsAreReads(t *testing.T) {
	tests := []struct {
		fixture    string
		minVersion string
		subtypes   []string
	}{
		{fixture: "available", subtypes: []string{"initialize", "get_usage", "get_settings"}},
		{fixture: "available", minVersion: "2.1.0",
			subtypes: []string{"initialize", "get_binary_version", "get_usage", "get_settings"}},
		{fixture: "old_version", minVersion: "2.1.0", subtypes: []string{"initialize", "get_binary_version"}},
	}
	for _, tt := range tests {
		t.Run(tt.fixture+"/"+tt.minVersion, func(t *testing.T) {
			_, requests := controlRun(t, tt.fixture, tt.minVersion, nil)
			var subtypes []string
			for _, m := range requests {
				assert.Equal(t, "control_request", m["type"], "never a user event")
				assert.NotEmpty(t, m["request_id"])
				req, ok := m["request"].(map[string]any)
				require.True(t, ok)
				subtype, _ := req["subtype"].(string)
				subtypes = append(subtypes, subtype)
				assert.Contains(t, controlReads, subtype)
				if subtype == controlUsage {
					assert.Equal(t, map[string]any{"subtype": controlUsage, "skip_behaviors": true}, req,
						"usage skips the transcript scan and carries nothing else")
				} else {
					assert.Equal(t, map[string]any{"subtype": subtype}, req)
				}
			}
			assert.Equal(t, tt.subtypes, subtypes)
		})
	}
}

func TestControlSession_RejectsWrites(t *testing.T) {
	_, err := (&controlSession{}).call(read{subtype: "set_model"})
	require.Error(t, err)
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		cmp  int
		ok   bool
	}{
		{"2.1.296", "2.1.0", 1, true},
		{"2.1", "2.1.0", 0, true},
		{"2.0.9", "2.1.0", -1, true},
		{"v3.0.0-beta.1", "3", 0, true},
		{"2.1.296 (build)", "2.1.296", 0, true},
		{"10.0", "9.9", 1, true},
		{"", "1", 0, false},
		{"dev", "1", 0, false},
	}
	for _, tt := range tests {
		cmp, ok := compareVersions(tt.a, tt.b)
		assert.Equal(t, tt.ok, ok, tt.a)
		assert.Equal(t, tt.cmp, cmp, "%s vs %s", tt.a, tt.b)
	}
}
