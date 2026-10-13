package readiness

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseHelper(t *testing.T) {
	const schema = `"schema":"agrouter.readiness/v1"`
	reset := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		in       string
		want     []Evidence
		defaults Defaults
	}{
		{name: "schema only", in: `{` + schema + `}`},
		{name: "auth not applicable", in: `{` + schema + `,"auth":{"status":"not_applicable"}}`,
			want: []Evidence{{Dimension: DimensionAuth, Status: StatusNotApplicable, Check: "c", ObservedAt: observed}}},
		{name: "auth unavailable gets a default reason", in: `{` + schema + `,"auth":{"status":"unavailable"}}`,
			want: []Evidence{{Dimension: DimensionAuth, Status: StatusUnavailable, Reason: ReasonAuthFailed, Check: "c", ObservedAt: observed}}},
		{name: "unknown quota gets a default reason", in: `{` + schema + `,"quota":[{"window":"weekly","status":"unknown"}]}`,
			want: []Evidence{{Dimension: DimensionQuota, Status: StatusUnknown, Reason: ReasonNotReported, Check: "c",
				Window: WindowWeekly, ObservedAt: observed}}},
		{name: "helper reason and reset with offset",
			in: `{` + schema + `,"quota":[{"window":"five_hour","status":"unavailable","reason":"probe_throttled","reset_at":"2026-10-13T15:00:00+03:00"}]}`,
			want: []Evidence{{Dimension: DimensionQuota, Status: StatusUnavailable, Reason: ReasonProbeThrottled, Check: "c",
				Window: WindowFiveHour, ObservedAt: observed, ResetAt: reset}}},
		{name: "empty capability list", in: `{` + schema + `,"capability":{"unsupported_models":[]}}`},
		{name: "defaults", in: `{` + schema + `,"defaults":{"model":null,"effort":"e-1"}}`,
			defaults: Defaults{Effort: new("e-1")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, defaults, err := parseHelper([]byte(tt.in), "c", observed)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.defaults, defaults)
		})
	}
}

func TestParseHelper_Malformed(t *testing.T) {
	const schema = `"schema":"agrouter.readiness/v1"`
	tests := []struct {
		name string
		in   string
	}{
		{name: "empty", in: ``},
		{name: "not JSON", in: `available`},
		{name: "array", in: `[]`},
		{name: "missing schema", in: `{"auth":{"status":"available"}}`},
		{name: "other schema version", in: `{"schema":"agrouter.readiness/v2"}`},
		{name: "unknown field", in: `{` + schema + `,"credits":1}`},
		{name: "trailing data", in: `{` + schema + `} {}`},
		{name: "bad status", in: `{` + schema + `,"auth":{"status":"ok"}}`},
		{name: "missing status", in: `{` + schema + `,"quota":[{"window":"weekly"}]}`},
		{name: "bad window", in: `{` + schema + `,"quota":[{"window":"daily","status":"available"}]}`},
		{name: "free-text reason", in: `{` + schema + `,"quota":[{"window":"weekly","status":"unavailable","reason":"You hit a limit"}]}`},
		{name: "bad reset", in: `{` + schema + `,"quota":[{"window":"weekly","status":"unavailable","reset_at":"tomorrow"}]}`},
		{name: "empty scoped model", in: `{` + schema + `,"quota":[{"window":"weekly","status":"unavailable","models":[""]}]}`},
		{name: "empty unsupported model", in: `{` + schema + `,"capability":{"unsupported_models":[""]}}`},
		{name: "empty default", in: `{` + schema + `,"defaults":{"model":""}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := parseHelper([]byte(tt.in), "c", observed)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "You hit a limit", "the error never quotes the response")
		})
	}
}
