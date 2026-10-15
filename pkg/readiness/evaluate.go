package readiness

import (
	"slices"
	"strings"
	"time"

	"github.com/SvetlovA/agrouter/pkg/catalog"
	"github.com/SvetlovA/agrouter/pkg/config"
)

// Evaluation reason codes, beside the evidence's own.
const (
	ReasonResetPassed      = "quota_reset_passed"         // a block whose verified reset is already past: stale
	ReasonDelegatedModel   = "delegated_model_unverified" // no model is set and some model has its own block
	ReasonScopeUnmatched   = "quota_scope_unmatched"      // a model-scoped block whose label names no catalog model
	ReasonModelUncataloged = "model_uncataloged"          // a model outside the catalog beside another model's block
	ReasonEvidenceInvalid  = "evidence_invalid"           // a status or window the evaluation does not know
	ReasonWindowNotChecked = "window_not_reported"        // no evidence covers a subscription window
)

// Verdict is whether one option, or one model, can run on what the checks observed.
type Verdict struct {
	Status Status // available, unavailable or unknown
	// Dimension and Reason say what decided an unavailable or unknown verdict.
	Dimension Dimension
	Reason    string
	// Check is the check whose evidence decided an unavailable verdict: for a quota block, the first
	// blocking window's; empty for the executable lookup.
	Check string
	// Windows are the exhausted or denied quota windows of an unavailable quota verdict, in evidence
	// order; an account-wide denial without a window adds nothing.
	Windows []string
	// ResetAt is the earliest recovery of an unavailable quota verdict: the latest reset of its
	// blocking windows, zero when any of them is unverified.
	ResetAt time.Time
}

// OptionVerdict is the verdict of one eligible option.
type OptionVerdict struct {
	ID string
	Verdict
}

// Evaluation applies a report to the exec CLI's eligible options under the unknown policy.
type Evaluation struct {
	CLI      string
	Options  []OptionVerdict // in eligibility order
	Excluded []string        // ids of the options not to route over: unavailable, or unknown under block
	// Unknown is the reason of the first kept option whose availability is unknown, for the one
	// warning line; empty when every kept option is verified.
	Unknown string
	// Blocked explains why nothing is left when every option is excluded: an unavailable verdict when
	// any option is known unavailable (known blocks win over unknowns), otherwise the unknown verdict
	// that the block policy refused. Nil while any option remains.
	Blocked *Verdict

	report Report
	models []config.Model
	now    time.Time
}

// Evaluate judges opts, the eligible options of r.CLI, against r. Model-scoped evidence matches an
// option when its label equals, ignoring case, the option's model name, section or one of the
// model's aliases. A model-scoped block whose label names no catalog model of the CLI cannot be
// placed, so it leaves every other model unknown. Evaluate never adds an option, so caller-fixed
// constraints are never broadened.
func Evaluate(r Report, opts []catalog.Option, models []config.Model, unknown string, now time.Time) *Evaluation {
	ev := &Evaluation{CLI: r.CLI, report: r, models: models, now: now}
	for _, o := range opts {
		v := ev.judge(ev.names(o.Section, o.Name), false)
		ev.Options = append(ev.Options, OptionVerdict{ID: o.ID, Verdict: v})
		switch {
		case v.Status == StatusUnavailable, v.Status == StatusUnknown && unknown == config.UnknownBlock:
			ev.Excluded = append(ev.Excluded, o.ID)
		case v.Status == StatusUnknown && ev.Unknown == "":
			ev.Unknown = v.Reason
		}
	}
	if len(opts) > 0 && len(ev.Excluded) == len(opts) {
		ev.Blocked = ev.blocked()
	}
	return ev
}

// Model judges a model the child would run outside the eligible options, such as its verified
// default. An empty name is a delegated model: unknown when any model-scoped block exists, since the
// child's choice cannot be matched against it. A name outside the catalog is matched only by its own
// label, so a block on a catalog model leaves it unknown: the label may be another spelling of it.
func (ev *Evaluation) Model(name string) Verdict {
	if name == "" {
		return ev.judge(nil, true)
	}
	return ev.judge(ev.names(ev.section(name), name), false)
}

// SameModel reports whether a and b name the same model: the same spelling ignoring case, or a name
// or alias of the same catalog model of the CLI.
func (ev *Evaluation) SameModel(a, b string) bool {
	if strings.EqualFold(a, b) {
		return true
	}
	section := ev.section(a)
	return section != "" && section == ev.section(b)
}

// section is the section of the CLI's catalog model named or aliased name, ignoring case; empty when
// there is none.
func (ev *Evaluation) section(name string) string {
	for _, m := range ev.models {
		if m.CLI == ev.CLI && (strings.EqualFold(m.Name, name) || slices.ContainsFunc(m.Aliases, func(a string) bool {
			return strings.EqualFold(a, name)
		})) {
			return m.Section
		}
	}
	return ""
}

// names are the labels model-scoped evidence may use for a model: its name, section and aliases.
func (ev *Evaluation) names(section, name string) []string {
	out := []string{name}
	for _, m := range ev.models {
		if section != "" && m.Section == section {
			out = append(out, m.Section, m.Name)
			out = append(out, m.Aliases...)
		}
	}
	return out
}

// cataloged reports whether any label names a catalog model of the CLI by name, section or alias.
func (ev *Evaluation) cataloged(labels []string) bool {
	return slices.ContainsFunc(ev.models, func(m config.Model) bool {
		if m.CLI != ev.CLI {
			return false
		}
		names := append([]string{m.Section, m.Name}, m.Aliases...)
		return slices.ContainsFunc(labels, func(l string) bool {
			return slices.ContainsFunc(names, func(n string) bool { return n != "" && strings.EqualFold(n, l) })
		})
	})
}

// applies reports whether s covers a model known by names; a delegated model is covered only by
// account-wide evidence.
func applies(s Scope, names []string, delegated bool) bool {
	if s.Account() {
		return true
	}
	if delegated {
		return false
	}
	return slices.ContainsFunc(s.Models, func(label string) bool {
		return slices.ContainsFunc(names, func(n string) bool { return n != "" && strings.EqualFold(n, label) })
	})
}

// judge is the verdict for one model. Known blocks win: a missing executable, then failed auth, then
// an unsupported model, then a blocked quota window. Only then is anything unknown reported. A
// delegated model is never matched against an unsupported-model list: missing entries are no denial.
func (ev *Evaluation) judge(names []string, delegated bool) Verdict {
	var unknown *Verdict
	setUnknown := func(d Dimension, reason string) {
		if unknown == nil {
			unknown = &Verdict{Status: StatusUnknown, Dimension: d, Reason: reason}
		}
	}
	for _, d := range []Dimension{DimensionExecutable, DimensionAuth, DimensionCapability} {
		for _, e := range ev.report.Evidence {
			if e.Dimension != d {
				continue
			}
			if !applies(e.Scope, names, delegated) {
				continue
			}
			switch e.Status {
			case StatusUnavailable:
				return Verdict{Status: StatusUnavailable, Dimension: d, Reason: e.Reason, Check: e.Check}
			case StatusAvailable, StatusNotApplicable, StatusUnknown:
				// an unverified executable, auth or capability does not stop the child; it reports its own error
			default:
				setUnknown(d, ReasonEvidenceInvalid)
			}
		}
	}
	quota := ev.quota(names, delegated)
	if quota.Status == StatusUnavailable {
		return quota
	}
	if unknown != nil {
		return *unknown
	}
	return quota
}

// quota judges the subscription windows for one model: any applicable block is unavailable, an
// account-wide not_applicable needs no windows, and otherwise both the five-hour and weekly windows
// must be verified available.
func (ev *Evaluation) quota(names []string, delegated bool) Verdict {
	t := quotaTally{block: Verdict{Status: StatusUnavailable, Dimension: DimensionQuota}, verified: map[string]bool{}}
	for _, e := range ev.report.Evidence {
		switch {
		case e.Dimension != DimensionQuota:
		case !e.Scope.Account() && delegated:
			if e.Status == StatusUnavailable {
				t.setUnknown(ReasonDelegatedModel)
			}
		case !applies(e.Scope, names, delegated):
			switch {
			case e.Status != StatusUnavailable:
			case !ev.cataloged(e.Scope.Models):
				t.setUnknown(ReasonScopeUnmatched)
			case !ev.cataloged(names):
				t.setUnknown(ReasonModelUncataloged)
			}
		case e.Window != "" && e.Window != WindowFiveHour && e.Window != WindowWeekly:
			t.setUnknown(ReasonEvidenceInvalid)
		default:
			t.add(e, ev.now)
		}
	}
	switch {
	case t.block.Reason != "":
		if t.unverifiedReset {
			t.block.ResetAt = time.Time{}
		}
		return t.block
	case t.notApplicable:
		return Verdict{Status: StatusAvailable}
	case t.unknown != nil:
		return *t.unknown
	case !t.verified[WindowFiveHour] || !t.verified[WindowWeekly]:
		return Verdict{Status: StatusUnknown, Dimension: DimensionQuota, Reason: ReasonWindowNotChecked}
	}
	return Verdict{Status: StatusAvailable}
}

// quotaTally collects the quota evidence that applies to one model.
type quotaTally struct {
	block           Verdict
	unknown         *Verdict // the first unknown, if any
	verified        map[string]bool
	notApplicable   bool
	unverifiedReset bool
}

func (t *quotaTally) setUnknown(reason string) {
	if t.unknown == nil {
		t.unknown = &Verdict{Status: StatusUnknown, Dimension: DimensionQuota, Reason: reason}
	}
}

func (t *quotaTally) add(e Evidence, now time.Time) {
	switch e.Status {
	case StatusUnavailable:
		if !e.ResetAt.IsZero() && !e.ResetAt.After(now) {
			// the window has reset since: no longer a known block, and not known free either
			t.setUnknown(ReasonResetPassed)
			return
		}
		if t.block.Reason == "" {
			t.block.Reason, t.block.Check = e.Reason, e.Check
		}
		if e.Window != "" && !slices.Contains(t.block.Windows, e.Window) {
			t.block.Windows = append(t.block.Windows, e.Window)
		}
		if e.ResetAt.IsZero() {
			t.unverifiedReset = true
		} else if e.ResetAt.After(t.block.ResetAt) {
			t.block.ResetAt = e.ResetAt
		}
	case StatusUnknown:
		t.setUnknown(e.Reason)
	case StatusNotApplicable:
		t.notApplicable = t.notApplicable || e.Scope.Account()
	case StatusAvailable:
		if e.Window != "" {
			t.verified[e.Window] = true
		}
	default:
		t.setUnknown(ReasonEvidenceInvalid)
	}
}

// blocked picks the verdict that explains why every option is excluded. Among unavailable verdicts
// it prefers the one the CLI recovers from first: a quota block with the earliest verified reset,
// then any other, in option order.
func (ev *Evaluation) blocked() *Verdict {
	var best *Verdict
	for i := range ev.Options {
		v := &ev.Options[i].Verdict
		switch {
		case v.Status != StatusUnavailable:
		case best == nil, recoversFirst(v, best):
			best = v
		}
	}
	if best == nil {
		best = &ev.Options[0].Verdict
	}
	out := *best
	return &out
}

// recoversFirst reports whether v ends before best: a non-quota block is permanent for this run, so a
// quota block with a known reset always wins, and of two resets the earlier one.
func recoversFirst(v, best *Verdict) bool {
	known := func(x *Verdict) bool { return x.Dimension == DimensionQuota && !x.ResetAt.IsZero() }
	switch {
	case !known(v):
		return false
	case !known(best):
		return true
	}
	return v.ResetAt.Before(best.ResetAt)
}
