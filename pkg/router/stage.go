package router

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/SvetlovA/agrouter/pkg/catalog"
	"github.com/SvetlovA/agrouter/pkg/jev"
	"github.com/SvetlovA/agrouter/pkg/prompt"
)

// Routing levels, in the order they are asked. Each name is its stage request's question id and its
// confidence key in the decision JSON.
const (
	LevelCLI    = "cli"
	LevelModel  = "model"
	LevelEffort = "effort"
)

// level is one routing stage: the options are grouped by key, Jev chooses among the groups' labels,
// and the options are narrowed to the winning group.
type level struct {
	name     string                      // question id, confidence key and debug label
	key      func(catalog.Option) string // group key
	label    func(catalog.Option) string // criterion name
	question string                      // the stage question
}

// routeLevels are the routing stages in order: CLI, then model, then effort.
var routeLevels = [...]level{
	{name: LevelCLI, key: optionCLI, label: optionCLI, question: cliText},
	{name: LevelModel, key: optionSection, label: modelLabel, question: modelText},
	// effort labels are unique within one model, the only one left when the effort stage is asked
	{name: LevelEffort, key: optionID, label: optionEffortLabel, question: effortText},
}

func optionCLI(o catalog.Option) string         { return o.CLI }
func optionSection(o catalog.Option) string     { return o.Section }
func optionID(o catalog.Option) string          { return o.ID }
func optionEffortLabel(o catalog.Option) string { return o.Effort }

// modelLabel is the model stage's criterion name: the model section, or the model name under
// model passthrough, where the options have no section.
func modelLabel(o catalog.Option) string { return cmp.Or(o.Section, o.Name) }

// Stage is how one routing level was decided, for recording and debug output.
type Stage struct {
	Level string // "cli", "model" or "effort"
	// Choice is the criterion name chosen, or the value the options share when the level was skipped:
	// the CLI, the model section (the model name under passthrough), or the effort, empty without one.
	Choice  string
	Skipped bool        // one group: Jev not asked
	Answer  *jev.Answer // the whole-state answer; nil when skipped or pooled
	Pooled  *Pooled     // the split-state answers; nil otherwise
}

// group is the options sharing one level's key.
type group struct {
	label string
	opts  []catalog.Option
}

// groups splits opts by lv's key, in catalog order.
func groups(opts []catalog.Option, lv level) []group {
	var out []group
	index := map[string]int{}
	for _, o := range opts {
		k := lv.key(o)
		i, ok := index[k]
		if !ok {
			i = len(out)
			index[k] = i
			out = append(out, group{label: lv.label(o)})
		}
		out[i].opts = append(out[i].opts, o)
	}
	return out
}

// groupLabels are the criterion names of gs, in order.
func groupLabels(gs []group) []string {
	out := make([]string, len(gs))
	for i, g := range gs {
		out[i] = g.label
	}
	return out
}

// skipped records lv as skipped, with the value every option left shares, o among them: its label,
// or for the effort level the effort o runs at, the passed one included.
func skipped(el *Eligibility, lv level, o catalog.Option) Stage {
	choice := lv.label(o)
	if lv.name == LevelEffort {
		choice = el.EffortFor(o)
	}
	return Stage{Level: lv.name, Choice: choice, Skipped: true}
}

// skippedStages records every level as skipped for a single option, chosen without Jev.
func skippedStages(el *Eligibility, o catalog.Option) []Stage {
	out := make([]Stage, len(routeLevels))
	for i, lv := range routeLevels {
		out[i] = skipped(el, lv, o)
	}
	return out
}

// routeState is the routing state as sent at each level: whole, or split into chunks.
type routeState struct {
	captured *prompt.Result
	split    *prompt.Split // nil: sent whole
	resplit  bool          // the chunks come from re-splitting a whole state Jev rejected
}

// decide asks the levels in order over a private copy of el's options. A level with one group is
// skipped; otherwise Jev chooses a group, by one request for a whole state or by pooling one request
// per chunk, and every later level is asked over that group alone. It returns the last option left
// and every stage decided, also on error: then the stages completed before the failing one.
func (r *Router) decide(ctx context.Context, el *Eligibility, captured *prompt.Result) (catalog.Option, []Stage, error) {
	split, err := captured.Split(r.budget)
	if err != nil {
		return catalog.Option{}, nil, fmt.Errorf("split the state: %w", err)
	}
	st := &routeState{captured: captured, split: split}
	opts := slices.Clone(el.Options)
	stages := make([]Stage, 0, len(routeLevels))
	for _, lv := range routeLevels {
		gs := groups(opts, lv)
		if len(gs) == 1 {
			stages = append(stages, skipped(el, lv, opts[0]))
			continue
		}
		winner, stage, err := r.stage(ctx, el, lv, gs, st)
		if err != nil {
			return catalog.Option{}, stages, fmt.Errorf("%s stage: %w", lv.name, err)
		}
		stages = append(stages, stage)
		opts = gs[winner].opts
	}
	return opts[0], stages, nil
}

// stage asks one level over gs and returns the winning group's index. A whole state goes in one
// request; a 422 on it re-splits it at half the chunk budget, once, and the chunks are pooled for
// this level and every later one.
func (r *Router) stage(ctx context.Context, el *Eligibility, lv level, gs []group, st *routeState) (int, Stage, error) {
	if st.split == nil {
		i, answer, err := r.ask(ctx, el, lv, gs, st.captured.State())
		if err == nil {
			return i, Stage{Level: lv.name, Choice: gs[i].label, Answer: answer}, nil
		}
		if !errors.Is(err, jev.ErrUnprocessable) {
			return 0, Stage{}, err
		}
		if st.captured.StateTokens() < prompt.MinStateTokens {
			return 0, Stage{}, fmt.Errorf("%w: %w", errUnsplittable, err)
		}
		// State 0 forces the split even though the state fit the full budget
		split, splitErr := st.captured.Split(prompt.Budget{Chunk: r.budget.Chunk / 2})
		if splitErr != nil {
			return 0, Stage{}, fmt.Errorf("re-split after %w: %w", err, splitErr)
		}
		st.split, st.resplit = split, true
	}
	i, p, err := r.pooled(ctx, el, lv, gs, st.split, st.resplit)
	if err != nil {
		return 0, Stage{}, err
	}
	return i, Stage{Level: lv.name, Choice: gs[i].label, Pooled: p}, nil
}

// ask sends state in one Choice request over gs and returns the chosen group's index.
func (r *Router) ask(ctx context.Context, el *Eligibility, lv level, gs []group, state prompt.State) (int, *jev.Answer, error) {
	req := jev.Request{
		Model:     r.cfg.Agrouter.JevModel,
		State:     state,
		Questions: map[string]jev.Question{lv.name: stageQuestion(r.cfg, lv, wholeGuide, gs, el.effort)},
	}
	answers, err := r.jev.Ask(ctx, req)
	if err != nil {
		return 0, nil, fmt.Errorf("route request: %w", err)
	}
	answer, ok := answers[lv.name]
	if !ok {
		return 0, nil, fmt.Errorf("%w: no %q answer", jev.ErrMalformed, lv.name)
	}
	names := groupLabels(gs)
	if _, err := scoresOf(answer, names); err != nil {
		return 0, nil, err
	}
	return slices.Index(names, answer.Choice), &answer, nil
}

// scoresOf returns answer's probability for every name, in order. A missing probability, or a
// choice outside names, is malformed.
func scoresOf(answer jev.Answer, names []string) ([]float64, error) {
	if !slices.Contains(names, answer.Choice) {
		return nil, fmt.Errorf("%w: choice %q is not among the criteria sent", jev.ErrMalformed, answer.Choice)
	}
	scores := make([]float64, len(names))
	for i, name := range names {
		p, ok := answer.Probabilities[name]
		if !ok {
			return nil, fmt.Errorf("%w: no probability for %q", jev.ErrMalformed, name)
		}
		scores[i] = p
	}
	return scores, nil
}
