package readiness

import (
	"bufio"
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SvetlovA/agrouter/pkg/config"
)

// rpcHelper answers JSON-RPC requests from the fixture at $READINESS_HELPER_FIXTURE, whose lines are
// {"on": method, "cursor": page cursor, "reply": {"result"|"error": ...}}. Before each reply it writes
// a notification, a server request and a response to another id; it appends every stdin line to
// $READINESS_HELPER_RECORD, and exits when stdin closes or a request has no reply in the fixture.
func rpcHelper() int {
	data, err := os.ReadFile(os.Getenv(helperFixture))
	if err != nil {
		return 97
	}
	replies := map[string]map[string]any{}
	for line := range strings.Lines(string(data)) {
		var f struct {
			On     string         `json:"on"`
			Cursor string         `json:"cursor"`
			Reply  map[string]any `json:"reply"`
		}
		if json.Unmarshal([]byte(line), &f) != nil {
			return 96
		}
		replies[f.On+"#"+f.Cursor] = f.Reply
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
			ID     any    `json:"id"`
			Method string `json:"method"`
			Params struct {
				Cursor string `json:"cursor"`
			} `json:"params"`
		}
		_ = json.Unmarshal(in.Bytes(), &req)
		if req.ID == nil {
			continue // a notification
		}
		reply, ok := replies[req.Method+"#"+req.Params.Cursor]
		if !ok {
			return 0
		}
		_ = out.Encode(map[string]any{"method": "account/updated", "params": map[string]any{}})
		_ = out.Encode(map[string]any{"id": "server-1", "method": "item/tool/call", "params": map[string]any{}})
		_ = out.Encode(map[string]any{"id": "unrelated", "result": map[string]any{}})
		r := map[string]any{"id": req.ID}
		maps.Copy(r, reply)
		_ = out.Encode(r)
	}
	return 0
}

// rpcRun checks a synthetic CLI with one JSON-RPC check against fixture and returns the report and
// the messages the fake received.
func rpcRun(t *testing.T, fixture, minVersion string, mutate func(*Checker, *config.CLI)) (Report, []map[string]any) {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("testdata", "rpc", fixture+".jsonl"))
	require.NoError(t, err)
	record := filepath.Join(t.TempDir(), "requests.jsonl")
	c := checker("rpc", helperFixture+"="+path, helperRecord+"="+record)
	cli := helperCLI(config.Check{Name: "limits", Transport: config.TransportJSONRPCRateLimit, MinVersion: minVersion})
	if mutate != nil {
		mutate(&c, &cli)
	}
	r := c.Check(context.Background(), cli)

	var messages []map[string]any
	if data, err := os.ReadFile(record); err == nil { //nolint:gosec // test file
		for line := range strings.Lines(string(data)) {
			var m map[string]any
			require.NoError(t, json.Unmarshal([]byte(line), &m), "every message is one JSON line")
			messages = append(messages, m)
		}
	}
	return r, messages
}

func TestRPC_Evidence(t *testing.T) {
	reset1 := time.Date(2026, 10, 13, 15, 0, 0, 0, time.UTC)
	reset2 := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	q := func(status Status, reason, window string, reset time.Time, models ...string) Evidence {
		return Evidence{Dimension: DimensionQuota, Status: status, Reason: reason, Check: "limits",
			Scope: Scope{Models: models}, Window: window, ObservedAt: observed, ResetAt: reset}
	}
	authOK := Evidence{Dimension: DimensionAuth, Status: StatusAvailable, Check: "limits", ObservedAt: observed}
	fiveHour := q(StatusAvailable, "", WindowFiveHour, reset1)
	weekly := q(StatusAvailable, "", WindowWeekly, reset2)
	verified := Defaults{Model: new("m-2"), Effort: new("e-1")}
	tests := []struct {
		fixture  string
		evidence []Evidence
		defaults Defaults
	}{
		{fixture: "available", evidence: []Evidence{authOK, fiveHour, weekly}, defaults: verified},
		{fixture: "weekly_exhausted", evidence: []Evidence{authOK, fiveHour,
			q(StatusUnavailable, ReasonQuotaExhausted, WindowWeekly, reset2)}, defaults: verified},
		{fixture: "windows_by_duration", evidence: []Evidence{authOK, weekly, fiveHour}, defaults: verified},
		{fixture: "scoped", evidence: []Evidence{authOK, fiveHour, weekly,
			q(StatusUnavailable, ReasonQuotaExhausted, WindowWeekly, reset2, "M-3"),
			q(StatusAvailable, "", WindowFiveHour, reset1, "Bucket C")}, defaults: verified},
		{fixture: "denied", evidence: []Evidence{authOK, q(StatusUnavailable, ReasonQuotaDenied, "", time.Time{}),
			fiveHour, weekly}, defaults: verified},
		{fixture: "reached_type", evidence: []Evidence{authOK, fiveHour, weekly,
			q(StatusUnavailable, ReasonQuotaDenied, "", time.Time{})}, defaults: verified},
		{fixture: "spend_control", evidence: []Evidence{authOK, fiveHour, weekly,
			q(StatusUnavailable, ReasonQuotaDenied, "", time.Time{})}, defaults: verified},
		{fixture: "permission_missing", evidence: []Evidence{authOK,
			q(StatusUnknown, ReasonPermissionUnknown, "", time.Time{}), fiveHour, weekly}, defaults: verified},
		{fixture: "credits", evidence: []Evidence{authOK, fiveHour,
			q(StatusUnknown, ReasonExtraUsageEnabled, WindowWeekly, reset2)}, defaults: verified},
		{fixture: "missing_windows", evidence: []Evidence{authOK,
			q(StatusUnknown, ReasonNotReported, WindowFiveHour, time.Time{}),
			q(StatusUnknown, ReasonNotReported, WindowWeekly, time.Time{})}, defaults: verified},
		{fixture: "other_duration", evidence: []Evidence{authOK,
			q(StatusUnavailable, ReasonQuotaExhausted, "", reset1), weekly,
			q(StatusUnknown, ReasonNotReported, WindowFiveHour, time.Time{})}, defaults: verified},
		{fixture: "api_key", evidence: []Evidence{q(StatusNotApplicable, ReasonNotSubscription, "", time.Time{})},
			defaults: verified},
		{fixture: "custom_provider", evidence: []Evidence{q(StatusNotApplicable, ReasonNotSubscription, "", time.Time{})},
			defaults: verified},
		{fixture: "logged_out", evidence: []Evidence{
			{Dimension: DimensionAuth, Status: StatusUnavailable, Reason: ReasonAuthFailed, Check: "limits", ObservedAt: observed},
			{Dimension: DimensionQuota, Status: StatusUnknown, Reason: ReasonNotReported, ObservedAt: observed}},
			defaults: verified},
		{fixture: "limits_error", evidence: []Evidence{authOK, q(StatusUnknown, ReasonProbeFailed, "", time.Time{})},
			defaults: verified},
		{fixture: "limits_throttled", evidence: []Evidence{authOK, q(StatusUnknown, ReasonProbeThrottled, "", time.Time{})},
			defaults: verified},
		{fixture: "limits_throttled_http", evidence: []Evidence{authOK,
			q(StatusUnknown, ReasonProbeThrottled, "", time.Time{})}, defaults: verified},
		{fixture: "limits_malformed", evidence: []Evidence{authOK, q(StatusUnknown, ReasonProbeMalformed, "", time.Time{})},
			defaults: verified},
		{fixture: "bad_window", evidence: []Evidence{authOK, q(StatusUnknown, ReasonProbeMalformed, "", time.Time{})},
			defaults: verified},
		{fixture: "config_error", evidence: []Evidence{authOK, fiveHour, weekly}},
		// a server that exits before the defaults read keeps the quota it already reported
		{fixture: "no_config_reply", evidence: []Evidence{authOK, fiveHour,
			q(StatusUnavailable, ReasonQuotaExhausted, WindowWeekly, reset2)}},
		{fixture: "config_no_model", evidence: []Evidence{authOK, fiveHour, weekly}},
		{fixture: "models_paged", evidence: []Evidence{authOK, fiveHour, weekly},
			defaults: Defaults{Model: new("m-hidden"), Effort: new("e-2")}},
		{fixture: "models_not_listed", evidence: []Evidence{authOK, fiveHour, weekly},
			defaults: Defaults{Model: new("m-hidden")}},
		// a server that exits before the model list keeps the configured model, its effort unknown
		{fixture: "no_models_reply", evidence: []Evidence{authOK, fiveHour, weekly},
			defaults: Defaults{Model: new("m-hidden")}},
		{fixture: "models_error", evidence: []Evidence{authOK, fiveHour, weekly},
			defaults: Defaults{Model: new("m-hidden")}},
		{fixture: "models_no_effort", evidence: []Evidence{authOK, fiveHour, weekly},
			defaults: Defaults{Model: new("m-hidden")}},
		// a cursor that never ends stops at the page bound instead of running into the deadline
		{fixture: "models_endless", evidence: []Evidence{authOK, fiveHour, weekly},
			defaults: Defaults{Model: new("m-hidden")}},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			r, _ := rpcRun(t, tt.fixture, "1.0.0", nil)
			want := append([]Evidence{{Dimension: DimensionExecutable, Status: StatusAvailable, ObservedAt: observed}},
				tt.evidence...)
			assert.Equal(t, want, r.Evidence)
			assert.Equal(t, tt.defaults, r.Defaults)
		})
	}
}

func TestRPC_Unknown(t *testing.T) {
	tests := []struct {
		name       string
		fixture    string
		minVersion string
		mutate     func(*Checker, *config.CLI)
		reason     string
	}{
		{name: "unsupported version", fixture: "old_version", minVersion: "1.0.0", reason: ReasonVersionUnsupported},
		{name: "version not reported", fixture: "no_version", minVersion: "1.0.0", reason: ReasonVersionUnsupported},
		{name: "initialize error", fixture: "init_error", reason: ReasonProbeFailed},
		{name: "account error", fixture: "account_error", reason: ReasonProbeFailed},
		{name: "account read throttled", fixture: "account_throttled", reason: ReasonProbeThrottled},
		{name: "account shape", fixture: "account_malformed", reason: ReasonProbeMalformed},
		{name: "account without a type", fixture: "account_no_type", reason: ReasonProbeMalformed},
		{name: "unrecognized account type", fixture: "account_unknown_type", reason: ReasonProbeMalformed},
		{name: "exits before replying", fixture: "no_limits_reply", reason: ReasonProbeFailed},
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
			r, _ := rpcRun(t, tt.fixture, tt.minVersion, tt.mutate)
			quota := evidenceOf(r, DimensionQuota)
			require.Len(t, quota, 1)
			assert.Equal(t, StatusUnknown, quota[0].Status, "an unverified check is never ready")
			assert.Equal(t, tt.reason, quota[0].Reason)
			assert.Equal(t, "limits", quota[0].Check)
			assert.Empty(t, evidenceOf(r, DimensionAuth))
			assert.Equal(t, Defaults{}, r.Defaults)
		})
	}
}

// the fake must only ever see reads and the initialized notification: no thread, no turn, no login
// or configuration write
func TestRPC_MessagesAreReads(t *testing.T) {
	reads := []string{"initialize", "initialized", "account/read", "account/rateLimits/read", "config/read"}
	tests := []struct {
		fixture    string
		minVersion string
		methods    []string
	}{
		{fixture: "available", methods: reads},
		{fixture: "available", minVersion: "1.0.0", methods: reads},
		{fixture: "old_version", minVersion: "1.0.0", methods: []string{"initialize"}},
		{fixture: "config_no_model", methods: reads}, // the list's default model is never read as the child's
		{fixture: "models_paged", methods: append(slices.Clone(reads), "model/list", "model/list")},
	}
	for _, tt := range tests {
		t.Run(tt.fixture+"/"+tt.minVersion, func(t *testing.T) {
			_, messages := rpcRun(t, tt.fixture, tt.minVersion, nil)
			var methods []string
			for _, m := range messages {
				method, _ := m["method"].(string)
				methods = append(methods, method)
				assert.NotContains(t, method, "thread/")
				assert.NotContains(t, method, "turn/")
				if method == rpcInitialized {
					assert.NotContains(t, m, "id", "a notification has no id")
					continue
				}
				assert.Contains(t, rpcReads, method)
				assert.NotEmpty(t, m["id"])
			}
			assert.Equal(t, tt.methods, methods)
		})
	}
}

func TestRPC_RequestParams(t *testing.T) {
	dir := t.TempDir()
	_, messages := rpcRun(t, "models_paged", "", func(c *Checker, _ *config.CLI) { c.Dir = dir })
	byMethod := map[string][]map[string]any{}
	for _, m := range messages {
		method, _ := m["method"].(string)
		params, _ := m["params"].(map[string]any)
		byMethod[method] = append(byMethod[method], params)
	}
	require.Len(t, byMethod[rpcConfig], 1)
	assert.Equal(t, map[string]any{"cwd": dir}, byMethod[rpcConfig][0], "the effective configuration of the real directory")
	assert.Equal(t, map[string]any{"excludeResetCreditDetails": true}, byMethod[rpcRateLimits][0])
	assert.Equal(t, []map[string]any{{"includeHidden": true}, {"includeHidden": true, "cursor": "p2"}}, byMethod[rpcModels])
}

func TestRPCSession_RejectsWrites(t *testing.T) {
	s := &rpcSession{}
	for _, method := range []string{"thread/start", "turn/start", "account/logout", "config/value/write"} {
		_, err := s.call(rpcCall{method: method})
		require.Error(t, err, method)
	}
	require.Error(t, s.notify("turn/interrupt"))
}

func TestRPCError_Throttled(t *testing.T) {
	tests := []struct {
		err  rpcError
		want bool
	}{
		{rpcError{Code: 429}, true},
		{rpcError{Code: -32603, Data: json.RawMessage(`"rateLimitExceeded"`)}, true},
		{rpcError{Code: -32603, Data: json.RawMessage(`[{"info":"rateLimitExceeded"}]`)}, true},
		{rpcError{Code: -32603, Data: json.RawMessage(`{"responseStreamConnectionFailed":{"httpStatusCode":429}}`)}, true},
		{rpcError{Code: -32603, Data: json.RawMessage(`{"httpConnectionFailed":{"httpStatusCode":500}}`)}, false},
		{rpcError{Code: -32603, Data: json.RawMessage(`{"count":429}`)}, false},
		{rpcError{Code: -32603, Data: json.RawMessage(`"usageLimitExceeded"`)}, false},
		{rpcError{Code: -32603, Data: json.RawMessage(`not json`)}, false},
		{rpcError{Code: -32603}, false},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, tt.err.throttled(), string(tt.err.Data))
	}
}

func TestUserAgentSupported(t *testing.T) {
	tests := []struct {
		agent string
		want  bool
	}{
		{"client/1.2.3 (os; arch) term", true},
		{"client/1.0.0", true},
		{"client/0.9.9 (os)", false},
		{"client", false},
		{"client/dev", false},
		{"", false},
	}
	for _, tt := range tests {
		result, err := json.Marshal(map[string]string{"userAgent": tt.agent})
		require.NoError(t, err)
		assert.Equal(t, tt.want, userAgentSupported(result, "1.0.0"), tt.agent)
	}
	assert.False(t, userAgentSupported(json.RawMessage(`[]`), "1.0.0"))
}
