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
	budget prompt.Budget
}

// New checks the questions against Jev's budget over the whole catalog and returns a router asking
// client. A question over budget is a config error.
func New(cfg *config.Config, cat *catalog.Catalog, client JevClient) (*Router, error) {
	b, err := budget(cfg, cat)
	if err != nil {
		return nil, err
	}
	return &Router{cfg: cfg, jev: client, budget: b}, nil
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
	// Stages are the routing levels in order, for recording and debug output: all three once routing
	// completes (skipped ones included), the ones completed before a failure otherwise, and none when
	// routing never started.
	Stages []Stage
	// Complexity is how the docs were scored, for recording and debug output; nil unless the complexity stage ran
	// to completion.
	Complexity *ComplexityResult
}

// Choice is the argv choice for args.Build.
func (d Decision) Choice() args.Choice {
	return args.Choice{Model: d.Model, Effort: d.Effort, Pinned: d.Pinned}
}

// Route decides among el's options for the captured prompt, level by level (see decide). With one
// option Jev is not asked and every level is recorded as skipped. When
// the docs have text, the complexity stage scores them first and the routing state carries the
// project complexity. When Jev cannot decide, in either stage, the CLI is used with only the
// caller's fixed --model and --effort if it is known (one CLI left); otherwise Route returns an
// error matching ErrCannotDecide beside a Decision holding only the stages and complexity completed.
func (r *Router) Route(ctx context.Context, el *Eligibility, req *args.Request, captured *prompt.Result) (Decision, error) {
	if len(el.Options) == 1 {
		d := r.chosen(el, el.Options[0])
		d.Stages = skippedStages(el, el.Options[0])
		return d, nil
	}
	if captured.Undecidable != nil {
		return r.cannotDecide(el, req, captured.Undecidable)
	}
	var cx *ComplexityResult
	if captured.HasDocs() {
		var err error
		if cx, err = r.complexity(ctx, captured); err != nil {
			return r.cannotDecide(el, req, fmt.Errorf("complexity stage: %w", err))
		}
		withProject := *captured
		withProject.Project = &prompt.Project{Complexity: cx.Complexity}
		captured = &withProject
	}
	o, stages, err := r.decide(ctx, el, captured)
	if err != nil {
		// partial decisions are discarded: the policy runs against the original eligibility
		d, policyErr := r.cannotDecide(el, req, err)
		d.Stages, d.Complexity = stages, cx
		return d, policyErr
	}
	d := r.chosen(el, o)
	d.Stages, d.Complexity = stages, cx
	return d, nil
}

// chosen is the decision to run o. Pinned comes from the caller's --cli only, never from a CLI Jev chose.
func (r *Router) chosen(el *Eligibility, o catalog.Option) Decision {
	return Decision{CLI: o.CLI, Model: o.Name, Effort: el.EffortFor(o), OptionID: o.ID, Pinned: el.Pinned}
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
