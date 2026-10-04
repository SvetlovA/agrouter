package router

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/SvetlovA/agrouter/pkg/args"
	"github.com/SvetlovA/agrouter/pkg/catalog"
	"github.com/SvetlovA/agrouter/pkg/config"
	"github.com/SvetlovA/agrouter/pkg/jev"
	"github.com/SvetlovA/agrouter/pkg/prompt"
)

//go:generate go tool moq -out mocks/jev_client.go -pkg mocks -skip-ensure . JevClient

// JevClient asks Jev typed questions about a state; *jev.Client implements it.
type JevClient interface {
	Ask(ctx context.Context, req jev.Request) (map[string]jev.Answer, error)
}

// ErrCannotDecide means Jev could not decide and more than one CLI is still eligible, so there is
// nothing to run: exit 2.
var ErrCannotDecide = errors.New("jev cannot decide")

// Router chooses among the eligible options with Jev.
type Router struct {
	cfg    *config.Config
	jev    JevClient
	enc    Encoding
	budget prompt.Budget
}

// New checks the questions against Jev's budget over the whole catalog and returns a router asking
// client. A question over budget is a config error.
func New(cfg *config.Config, cat *catalog.Catalog, client JevClient, enc Encoding) (*Router, error) {
	b, err := budget(cfg, cat, enc)
	if err != nil {
		return nil, err
	}
	return &Router{cfg: cfg, jev: client, enc: enc, budget: b}, nil
}

// Budget is the state budget the questions leave.
func (r *Router) Budget() prompt.Budget {
	return r.budget
}

// Decision is what runs: the CLI, and the model and effort to emit through its templates.
type Decision struct {
	CLI    string
	Model  string // empty: no model argument, the CLI's default applies (null in decision JSON)
	Effort string // empty: no effort argument (null in decision JSON)
	// OptionID is the chosen option; empty when Jev could not decide.
	OptionID string
	// Pinned is true when a valid --cli fixed the CLI, so raw passthrough is appended.
	Pinned bool
	// Undecided is why Jev could not decide when the CLI was known anyway; nil otherwise.
	Undecided error
	// Answer is Jev's route answer to a single request, for debug output; nil without one.
	Answer *jev.Answer
	// Pooled is how a split state was decided, for debug output; nil unless it was split.
	Pooled *Pooled
}

// outcome is what Jev decided: the option, with the single answer or the pooled chunks.
type outcome struct {
	option catalog.Option
	answer *jev.Answer
	pooled *Pooled
}

// Choice is the argv choice for args.Build.
func (d Decision) Choice() args.Choice {
	return args.Choice{Model: d.Model, Effort: d.Effort, Pinned: d.Pinned}
}

// Route decides among el's options for the captured prompt. With one option Jev is not asked. When
// Jev cannot decide, the CLI is used with only the caller's fixed --model and --effort if it is
// known (one CLI left); otherwise Route returns an error matching ErrCannotDecide.
func (r *Router) Route(ctx context.Context, el *Eligibility, req *args.Request, captured *prompt.Result) (Decision, error) {
	if len(el.Options) == 1 {
		return r.chosen(el, el.Options[0], nil), nil
	}
	out, err := r.decide(ctx, el, captured)
	if err != nil {
		return r.cannotDecide(el, req, err)
	}
	d := r.chosen(el, out.option, out.answer)
	d.Pooled = out.pooled
	return d, nil
}

// decide sends the state whole when it fits, and otherwise one request per chunk, pooled.
func (r *Router) decide(ctx context.Context, el *Eligibility, captured *prompt.Result) (outcome, error) {
	if captured.Undecidable != nil {
		return outcome{}, captured.Undecidable
	}
	split, err := captured.Split(r.budget)
	if err != nil {
		return outcome{}, fmt.Errorf("split the state: %w", err)
	}
	if split == nil {
		return r.single(ctx, el, captured)
	}
	return r.pooled(ctx, el, split, false)
}

// ask sends state in one Choice request and returns the chosen option.
func (r *Router) ask(ctx context.Context, el *Eligibility, state prompt.State) (catalog.Option, *jev.Answer, error) {
	req := jev.Request{
		Model: r.cfg.Agrouter.JevModel,
		State: state,
		Questions: map[string]jev.Question{
			questionRoute: routeQuestion(r.cfg, r.cfg.Agrouter.Question, el.Options, el.effort, r.enc),
		},
	}
	answers, err := r.jev.Ask(ctx, req)
	if err != nil {
		return catalog.Option{}, nil, fmt.Errorf("route request: %w", err)
	}
	answer, ok := answers[questionRoute]
	if !ok {
		return catalog.Option{}, nil, fmt.Errorf("%w: no %q answer", jev.ErrMalformed, questionRoute)
	}
	for _, o := range el.Options {
		if o.ID == answer.Choice {
			return o, &answer, nil
		}
	}
	return catalog.Option{}, nil, fmt.Errorf("%w: choice %q is not among the options sent", jev.ErrMalformed, answer.Choice)
}

func (r *Router) chosen(el *Eligibility, o catalog.Option, answer *jev.Answer) Decision {
	return Decision{CLI: o.CLI, Model: o.Name, Effort: el.EffortFor(o), OptionID: o.ID, Pinned: el.Pinned, Answer: answer}
}

// cannotDecide applies the policy: with one CLI left (--cli, implied by --model, or the only one
// eligible) it runs with only what the caller fixed; with more, there is nothing to run.
func (r *Router) cannotDecide(el *Eligibility, req *args.Request, cause error) (Decision, error) {
	clis := el.CLIs()
	if len(clis) > 1 {
		return Decision{}, fmt.Errorf("%w between %s: %w; pass --cli to run one without Jev",
			ErrCannotDecide, strings.Join(clis, ", "), cause)
	}
	d := Decision{CLI: clis[0], Pinned: el.Pinned, Undecided: cause, Effort: req.Effort}
	if req.Model != "" {
		// every option carries the caller's model, resolved to its catalog name when it has one
		d.Model = el.Options[0].Name
	}
	return d, nil
}
