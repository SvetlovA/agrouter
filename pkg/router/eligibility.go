// Package router decides which catalog option runs a prompt: eligibility from the caller's
// arguments, then Jev.
package router

import (
	"fmt"
	"slices"

	"github.com/SvetlovA/agrouter/pkg/args"
	"github.com/SvetlovA/agrouter/pkg/catalog"
	"github.com/SvetlovA/agrouter/pkg/config"
)

// Drop is a CLI eligibility removed, and why, for debug output.
type Drop struct {
	CLI    string
	Reason string
}

// Eligibility is what the filters leave for Jev to choose from.
type Eligibility struct {
	Options []catalog.Option // never empty, in catalog order
	// Pinned is true when a valid --cli fixed the CLI.
	Pinned bool
	// ModelPassthrough: the caller's --model was not among the remaining options, so every option
	// carries it unresolved (or resolved to its catalog name) with one option per CLI.
	ModelPassthrough bool
	// EffortPassthrough: no remaining option has the caller's --effort, so the options carry no
	// effort and the caller's value is emitted as given.
	EffortPassthrough bool
	Dropped           []Drop
	Warnings          []string // stderr lines, without a trailing newline
	effort            string
}

// EffortFor returns the effort to emit for o: its own, or the caller's passed-through value.
func (e *Eligibility) EffortFor(o catalog.Option) string {
	if o.Effort != "" {
		return o.Effort
	}
	return e.effort
}

// CLIs returns the distinct CLIs still eligible, in catalog order.
func (e *Eligibility) CLIs() []string {
	return catalog.CLIs(e.Options)
}

// Eligible applies the filters in design order: --cli, --model, --effort, then the mapped-argument
// preference. No step ever empties the list: an unusable --cli is skipped with a warning, and a
// --model or --effort nothing satisfies is passed through.
func Eligible(cfg *config.Config, cat *catalog.Catalog, req *args.Request) *Eligibility {
	e := &Eligibility{Options: slices.Clone(cat.Options), effort: req.Effort}
	e.filterCLI(req.CLI)
	e.filterModel(cat, req.Model)
	e.filterEffort(req.Effort)
	e.prefer(cfg, req)
	return e
}

func (e *Eligibility) filterCLI(name string) {
	if name == "" {
		return
	}
	kept := catalog.ByCLI(e.Options, name)
	if len(kept) == 0 {
		e.Warnings = append(e.Warnings,
			fmt.Sprintf("agrouter: warning: skipped --cli %s: not an enabled CLI; routing across every CLI", name))
		return
	}
	e.drop(kept, "--cli "+name)
	e.Options, e.Pinned = kept, true
}

func (e *Eligibility) filterModel(cat *catalog.Catalog, value string) {
	if value == "" {
		return
	}
	if m, ok := cat.LookupModel(value); ok {
		if kept := catalog.ByModel(e.Options, m.Section); len(kept) > 0 {
			e.drop(kept, "--model "+value)
			e.Options = kept
			return
		}
	}
	// outside the catalog, or a catalog model of a CLI already dropped: one option per remaining CLI
	// with the model fixed, for that CLI to validate
	name, _ := cat.ResolveModel(value)
	var opts []catalog.Option
	for _, cli := range catalog.CLIs(e.Options) {
		opts = append(opts, catalog.Option{ID: cli, CLI: cli, Name: name})
	}
	e.Options, e.ModelPassthrough = opts, true
}

func (e *Eligibility) filterEffort(effort string) {
	if effort == "" {
		return
	}
	if e.ModelPassthrough {
		e.EffortPassthrough = true
		return
	}
	if kept := catalog.ByEffort(e.Options, effort); len(kept) > 0 {
		e.drop(kept, "--effort "+effort)
		e.Options = kept
		return
	}
	// nothing has it: keep each model once, without an effort, and pass the value through
	var opts []catalog.Option
	for _, o := range e.Options {
		if !slices.ContainsFunc(opts, func(k catalog.Option) bool { return k.Section == o.Section }) {
			opts = append(opts, catalog.Option{ID: o.Section, CLI: o.CLI, Section: o.Section, Name: o.Name})
		}
	}
	e.Options, e.EffortPassthrough = opts, true
}

// prefer keeps the CLIs that would skip the fewest of the caller's mapped arguments (a missing key
// and a [] mapping both count), and every CLI tied at that count.
func (e *Eligibility) prefer(cfg *config.Config, req *args.Request) {
	clis := catalog.CLIs(e.Options)
	if len(clis) < 2 {
		return
	}
	skips := make(map[string]int, len(clis))
	for _, name := range clis {
		cli, _ := cfg.CLIByName(name)
		// Pinned so raw passthrough, the same for every CLI, is not counted
		skips[name] = len(args.Build(cli, req, args.Choice{Effort: req.Effort, Pinned: true}).Skipped)
	}
	fewest := skips[clis[0]]
	for _, name := range clis {
		fewest = min(fewest, skips[name])
	}
	var kept []catalog.Option
	for _, o := range e.Options {
		if skips[o.CLI] == fewest {
			kept = append(kept, o)
		}
	}
	for _, name := range clis {
		if n := skips[name]; n > fewest {
			e.Dropped = append(e.Dropped, Drop{CLI: name,
				Reason: fmt.Sprintf("would skip %d mapped argument(s), another CLI skips %d", n, fewest)})
		}
	}
	e.Options = kept
}

// drop records the CLIs of the current options that kept no longer has.
func (e *Eligibility) drop(kept []catalog.Option, reason string) {
	remaining := catalog.CLIs(kept)
	for _, cli := range catalog.CLIs(e.Options) {
		if !slices.Contains(remaining, cli) {
			e.Dropped = append(e.Dropped, Drop{CLI: cli, Reason: reason})
		}
	}
}
