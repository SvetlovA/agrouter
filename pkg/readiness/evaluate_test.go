package readiness

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SvetlovA/agrouter/pkg/catalog"
	"github.com/SvetlovA/agrouter/pkg/config"
)

var evalNow = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

// evalFixture is a synthetic CLI with two models; nothing here depends on the shipped catalog.
func evalFixture(t *testing.T) ([]catalog.Option, []config.Model) {
	t.Helper()
	cfg := &config.Config{
		CLIs: []config.CLI{{Name: "tool-a", Command: "tool-a"}},
		Models: []config.Model{
			{Section: "big", CLI: "tool-a", Name: "big-model", Aliases: []string{"heavy"}, Efforts: []string{"low", "high"}},
			{Section: "small", CLI: "tool-a", Name: "small-model"},
		},
	}
	cat, err := catalog.Build(cfg)
	require.NoError(t, err)
	return cat.Options, cfg.Models
}

func quotaEv(window string, status Status, reason string, reset time.Duration, models ...string) Evidence {
	e := Evidence{Dimension: DimensionQuota, Status: status, Reason: reason, Window: window,
		Scope: Scope{Models: models}, ObservedAt: evalNow}
	if reset != 0 {
		e.ResetAt = evalNow.Add(reset)
	}
	return e
}

func free(window string) Evidence {
	return quotaEv(window, StatusAvailable, "", 0)
}

func exhausted(window string, reset time.Duration, models ...string) Evidence {
	return quotaEv(window, StatusUnavailable, ReasonQuotaExhausted, reset, models...)
}

func bothFree() []Evidence { return []Evidence{free(WindowFiveHour), free(WindowWeekly)} }

func TestEvaluate(t *testing.T) {
	opts, models := evalFixture(t)
	all := []string{"big@low", "big@high", "small"}
	quotaBlock := func(reason string, reset time.Duration, windows ...string) *Verdict {
		v := &Verdict{Status: StatusUnavailable, Dimension: DimensionQuota, Reason: reason, Windows: windows}
		if reset != 0 {
			v.ResetAt = evalNow.Add(reset)
		}
		return v
	}
	cases := []struct {
		name     string
		evidence []Evidence
		unknown  string
		excluded []string
		warn     string
		blocked  *Verdict
	}{
		{name: "both windows available", evidence: bothFree()},
		{name: "five-hour exhausted",
			evidence: []Evidence{exhausted(WindowFiveHour, time.Hour), free(WindowWeekly)},
			excluded: all, blocked: quotaBlock(ReasonQuotaExhausted, time.Hour, WindowFiveHour)},
		{name: "weekly exhausted with five-hour available",
			evidence: []Evidence{free(WindowFiveHour), exhausted(WindowWeekly, 72*time.Hour)},
			excluded: all, blocked: quotaBlock(ReasonQuotaExhausted, 72*time.Hour, WindowWeekly)},
		{name: "both exhausted recover at the latest reset",
			evidence: []Evidence{exhausted(WindowFiveHour, time.Hour), exhausted(WindowWeekly, 72*time.Hour)},
			excluded: all, blocked: quotaBlock(ReasonQuotaExhausted, 72*time.Hour, WindowFiveHour, WindowWeekly)},
		{name: "weekly survives a passed five-hour reset",
			evidence: []Evidence{exhausted(WindowFiveHour, -time.Minute), exhausted(WindowWeekly, 72*time.Hour)},
			excluded: all, blocked: quotaBlock(ReasonQuotaExhausted, 72*time.Hour, WindowWeekly)},
		{name: "an unverified reset is omitted",
			evidence: []Evidence{exhausted(WindowFiveHour, time.Hour), exhausted(WindowWeekly, 0)},
			excluded: all, blocked: quotaBlock(ReasonQuotaExhausted, 0, WindowFiveHour, WindowWeekly)},
		{name: "a stale block is unknown, allowed",
			evidence: []Evidence{exhausted(WindowFiveHour, -time.Minute), free(WindowWeekly)},
			warn:     ReasonResetPassed},
		{name: "a stale block is unknown, blocked",
			evidence: []Evidence{exhausted(WindowFiveHour, -time.Minute), free(WindowWeekly)},
			unknown:  config.UnknownBlock, excluded: all,
			blocked: &Verdict{Status: StatusUnknown, Dimension: DimensionQuota, Reason: ReasonResetPassed}},
		{name: "an account-wide denial without a window",
			evidence: append(bothFree(), quotaEv("", StatusUnavailable, ReasonQuotaDenied, 0)),
			excluded: all, blocked: quotaBlock(ReasonQuotaDenied, 0)},
		{name: "a known block wins over an unrelated unknown",
			evidence: []Evidence{quotaEv(WindowFiveHour, StatusUnknown, ReasonProbeThrottled, 0), exhausted(WindowWeekly, time.Hour)},
			unknown:  config.UnknownAllow, excluded: all, blocked: quotaBlock(ReasonQuotaExhausted, time.Hour, WindowWeekly)},
		{name: "throttled is unknown, never a block",
			evidence: []Evidence{quotaEv(WindowFiveHour, StatusUnknown, ReasonProbeThrottled, 0), free(WindowWeekly)},
			warn:     ReasonProbeThrottled},
		{name: "throttled under block",
			evidence: []Evidence{quotaEv("", StatusUnknown, ReasonProbeThrottled, 0)},
			unknown:  config.UnknownBlock, excluded: all,
			blocked: &Verdict{Status: StatusUnknown, Dimension: DimensionQuota, Reason: ReasonProbeThrottled}},
		{name: "a missing window is unknown", evidence: []Evidence{free(WindowFiveHour)}, warn: ReasonWindowNotChecked},
		{name: "no evidence at all is unknown", warn: ReasonWindowNotChecked},
		{name: "not applicable needs no windows",
			evidence: []Evidence{quotaEv("", StatusNotApplicable, ReasonNotSubscription, 0)}},
		{name: "model-scoped not applicable still needs windows",
			evidence: []Evidence{quotaEv("", StatusNotApplicable, ReasonNotSubscription, 0, "big-model")},
			warn:     ReasonWindowNotChecked},
		{name: "a malformed status is unknown",
			evidence: append(bothFree(), quotaEv(WindowWeekly, "partly", "", 0)), warn: ReasonEvidenceInvalid},
		{name: "a malformed window is unknown",
			evidence: append(bothFree(), exhausted("monthly", time.Hour)), warn: ReasonEvidenceInvalid},
		{name: "a scoped block excludes only its model, matched by name ignoring case",
			evidence: append(bothFree(), exhausted(WindowWeekly, time.Hour, "BIG-MODEL")),
			excluded: []string{"big@low", "big@high"}},
		{name: "a scoped block matched by alias",
			evidence: append(bothFree(), exhausted(WindowWeekly, time.Hour, "Heavy")),
			excluded: []string{"big@low", "big@high"}},
		{name: "a scoped block matched by section",
			evidence: append(bothFree(), exhausted(WindowWeekly, time.Hour, "small")),
			excluded: []string{"small"}},
		{name: "a scoped block of another catalog model changes nothing for the rest",
			evidence: append(bothFree(), exhausted(WindowWeekly, time.Hour, "Small-Model")),
			excluded: []string{"small"}},
		{name: "a scoped block no catalog model matches is unknown, allowed",
			evidence: append(bothFree(), exhausted(WindowWeekly, time.Hour, "elsewhere")),
			warn:     ReasonScopeUnmatched},
		{name: "a scoped block no catalog model matches is unknown, blocked",
			evidence: append(bothFree(), exhausted(WindowWeekly, time.Hour, "elsewhere")),
			unknown:  config.UnknownBlock, excluded: all,
			blocked: &Verdict{Status: StatusUnknown, Dimension: DimensionQuota, Reason: ReasonScopeUnmatched}},
		{name: "a scoped unknown no catalog model matches changes nothing",
			evidence: append(bothFree(), quotaEv(WindowWeekly, StatusUnknown, ReasonExtraUsageEnabled, 0, "elsewhere"))},
		{name: "scoped blocks on every model recover with the first one",
			evidence: append(bothFree(), exhausted(WindowWeekly, 5*time.Hour, "big-model"), exhausted(WindowWeekly, time.Hour, "small-model")),
			excluded: all, blocked: quotaBlock(ReasonQuotaExhausted, time.Hour, WindowWeekly)},
		{name: "a scoped unknown warns only for its model",
			evidence: append(bothFree(), quotaEv(WindowWeekly, StatusUnknown, ReasonExtraUsageEnabled, 0, "small-model")),
			warn:     ReasonExtraUsageEnabled},
		{name: "a missing executable blocks everything",
			evidence: []Evidence{{Dimension: DimensionExecutable, Status: StatusUnavailable, Reason: ReasonExecutableMissing},
				quotaEv("", StatusUnknown, ReasonNotConfigured, 0)},
			excluded: all, blocked: &Verdict{Status: StatusUnavailable, Dimension: DimensionExecutable, Reason: ReasonExecutableMissing}},
		{name: "failed auth blocks everything before quota",
			evidence: append([]Evidence{{Dimension: DimensionAuth, Status: StatusUnavailable, Reason: ReasonAuthFailed}},
				exhausted(WindowWeekly, time.Hour)),
			excluded: all, blocked: &Verdict{Status: StatusUnavailable, Dimension: DimensionAuth, Reason: ReasonAuthFailed}},
		{name: "unknown auth does not stop the child",
			evidence: append(bothFree(), Evidence{Dimension: DimensionAuth, Status: StatusUnknown, Reason: ReasonNotReported})},
		{name: "a malformed auth status is unknown",
			evidence: append(bothFree(), Evidence{Dimension: DimensionAuth, Status: "maybe"}), warn: ReasonEvidenceInvalid},
		{name: "an unsupported model is excluded alone",
			evidence: append(bothFree(), Evidence{Dimension: DimensionCapability, Status: StatusUnavailable,
				Reason: ReasonUnsupportedModel, Scope: Scope{Models: []string{"small-model"}}}),
			excluded: []string{"small"}},
		{name: "a quota block with a reset is reported before an unsupported model",
			evidence: append(bothFree(), Evidence{Dimension: DimensionCapability, Status: StatusUnavailable,
				Reason: ReasonUnsupportedModel, Scope: Scope{Models: []string{"small-model"}}},
				exhausted(WindowWeekly, time.Hour, "big-model")),
			excluded: all, blocked: quotaBlock(ReasonQuotaExhausted, time.Hour, WindowWeekly)},
		{name: "a later quota block with a reset is reported before an earlier unsupported model",
			evidence: append(bothFree(), Evidence{Dimension: DimensionCapability, Status: StatusUnavailable,
				Reason: ReasonUnsupportedModel, Scope: Scope{Models: []string{"big-model"}}},
				exhausted(WindowWeekly, time.Hour, "small-model")),
			excluded: all, blocked: quotaBlock(ReasonQuotaExhausted, time.Hour, WindowWeekly)},
		{name: "a later known block wins over an earlier refused unknown",
			evidence: append(bothFree(), quotaEv(WindowWeekly, StatusUnknown, ReasonExtraUsageEnabled, 0, "big-model"),
				exhausted(WindowWeekly, time.Hour, "small-model")),
			unknown: config.UnknownBlock, excluded: all, blocked: quotaBlock(ReasonQuotaExhausted, time.Hour, WindowWeekly)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := tc.unknown
			if policy == "" {
				policy = config.UnknownAllow
			}
			ev := Evaluate(Report{CLI: "tool-a", Evidence: tc.evidence}, opts, models, policy, evalNow)
			assert.Equal(t, "tool-a", ev.CLI)
			assert.Equal(t, tc.excluded, ev.Excluded)
			assert.Equal(t, tc.warn, ev.Unknown)
			assert.Equal(t, tc.blocked, ev.Blocked)
			require.Len(t, ev.Options, len(opts))
			for i, o := range opts {
				assert.Equal(t, o.ID, ev.Options[i].ID, "eligibility order")
			}
		})
	}
}

func TestEvaluateConstraints(t *testing.T) {
	opts, models := evalFixture(t)
	evidence := append(bothFree(), exhausted(WindowWeekly, time.Hour, "big-model"))

	// a caller-fixed model: its options are all blocked and nothing else is added
	fixed := catalog.ByModel(opts, "big")
	ev := Evaluate(Report{CLI: "tool-a", Evidence: evidence}, fixed, models, config.UnknownAllow, evalNow)
	assert.Equal(t, []string{"big@low", "big@high"}, ev.Excluded)
	require.NotNil(t, ev.Blocked)
	assert.Equal(t, StatusUnavailable, ev.Blocked.Status)

	// an out-of-catalog model passed through: matched by its own name only
	passthrough := []catalog.Option{{ID: "tool-a", CLI: "tool-a", Name: "custom-x"}}
	ev = Evaluate(Report{CLI: "tool-a", Evidence: evidence}, passthrough, models, config.UnknownAllow, evalNow)
	assert.Empty(t, ev.Excluded)
	assert.Nil(t, ev.Blocked)
	ev = Evaluate(Report{CLI: "tool-a", Evidence: append(bothFree(), exhausted(WindowWeekly, time.Hour, "Custom-X"))},
		passthrough, models, config.UnknownAllow, evalNow)
	assert.Equal(t, []string{"tool-a"}, ev.Excluded)

	// a catalog model fixed with an effort it lacks: one option per model, no effort
	noEffort := []catalog.Option{{ID: "big", CLI: "tool-a", Section: "big", Name: "big-model"}}
	ev = Evaluate(Report{CLI: "tool-a", Evidence: evidence}, noEffort, models, config.UnknownAllow, evalNow)
	assert.Equal(t, []string{"big"}, ev.Excluded)

	// no options: nothing to judge, nothing blocked
	ev = Evaluate(Report{CLI: "tool-a", Evidence: evidence}, nil, models, config.UnknownBlock, evalNow)
	assert.Empty(t, ev.Excluded)
	assert.Nil(t, ev.Blocked)
}

func TestEvaluationModel(t *testing.T) {
	opts, models := evalFixture(t)
	cases := []struct {
		name     string
		evidence []Evidence
		model    string
		want     Verdict
	}{
		{name: "delegated without scoped blocks", evidence: bothFree(), want: Verdict{Status: StatusAvailable}},
		{name: "delegated with a scoped block is unknown",
			evidence: append(bothFree(), exhausted(WindowWeekly, time.Hour, "big-model")),
			want:     Verdict{Status: StatusUnknown, Dimension: DimensionQuota, Reason: ReasonDelegatedModel}},
		{name: "delegated under an account block is blocked",
			evidence: []Evidence{free(WindowFiveHour), exhausted(WindowWeekly, time.Hour), exhausted(WindowWeekly, 2*time.Hour, "big-model")},
			want: Verdict{Status: StatusUnavailable, Dimension: DimensionQuota, Reason: ReasonQuotaExhausted,
				Windows: []string{WindowWeekly}, ResetAt: evalNow.Add(time.Hour)}},
		{name: "delegated ignores scoped capability lists",
			evidence: append(bothFree(), Evidence{Dimension: DimensionCapability, Status: StatusUnavailable,
				Reason: ReasonUnsupportedModel, Scope: Scope{Models: []string{"big-model"}}}),
			want: Verdict{Status: StatusAvailable}},
		{name: "a verified default matched by alias ignoring case",
			evidence: append(bothFree(), exhausted(WindowWeekly, time.Hour, "big-model")), model: "HEAVY",
			want: Verdict{Status: StatusUnavailable, Dimension: DimensionQuota, Reason: ReasonQuotaExhausted,
				Windows: []string{WindowWeekly}, ResetAt: evalNow.Add(time.Hour)}},
		{name: "a verified default the block does not cover",
			evidence: append(bothFree(), exhausted(WindowWeekly, time.Hour, "big-model")), model: "small-model",
			want: Verdict{Status: StatusAvailable}},
		{name: "a verified default beside a block no catalog model matches",
			evidence: append(bothFree(), exhausted(WindowWeekly, time.Hour, "elsewhere")), model: "small-model",
			want: Verdict{Status: StatusUnknown, Dimension: DimensionQuota, Reason: ReasonScopeUnmatched}},
		{name: "a verified default outside the catalog beside a catalog model's block",
			evidence: append(bothFree(), exhausted(WindowWeekly, time.Hour, "big-model")), model: "other",
			want: Verdict{Status: StatusUnknown, Dimension: DimensionQuota, Reason: ReasonModelUncataloged}},
		{name: "a verified default outside the catalog without scoped blocks",
			evidence: bothFree(), model: "other", want: Verdict{Status: StatusAvailable}},
		{name: "a verified default outside the catalog",
			evidence: append(bothFree(), exhausted(WindowWeekly, time.Hour, "other")), model: "other",
			want: Verdict{Status: StatusUnavailable, Dimension: DimensionQuota, Reason: ReasonQuotaExhausted,
				Windows: []string{WindowWeekly}, ResetAt: evalNow.Add(time.Hour)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := Evaluate(Report{CLI: "tool-a", Evidence: tc.evidence}, opts, models, config.UnknownAllow, evalNow)
			assert.Equal(t, tc.want, ev.Model(tc.model))
		})
	}
}

func TestEvaluationSameModel(t *testing.T) {
	opts, models := evalFixture(t)
	ev := Evaluate(Report{CLI: "tool-a", Evidence: bothFree()}, opts, models, config.UnknownAllow, evalNow)
	cases := []struct {
		a, b string
		want bool
	}{
		{a: "big-model", b: "BIG-MODEL", want: true},
		{a: "heavy", b: "big-model", want: true},
		{a: "big-model", b: "Heavy", want: true},
		{a: "big-model", b: "small-model"},
		{a: "other", b: "big-model"},
		{a: "other", b: "else"},
		{a: "other", b: "OTHER", want: true},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, ev.SameModel(tc.a, tc.b), "%s vs %s", tc.a, tc.b)
	}
}
