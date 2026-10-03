// Package catalog derives the routing options (model × effort) from the enabled config, builds their
// ids and resolves model names and aliases.
package catalog

import (
	"errors"
	"fmt"
	"slices"

	"github.com/SvetlovA/agrouter/pkg/config"
)

// MaxOptions is Jev's limit on the options of one Choice question.
const MaxOptions = 255

// idSep separates the model section from the effort in an option id; it is reserved in section names.
const idSep = "@"

// Option is one (cli, model, effort) the router can choose.
type Option struct {
	ID      string // "<model section>@<effort>", or "<model section>" for a model without efforts
	CLI     string
	Section string // model section name without the "model." prefix
	Name    string // model name, the value substituted into {model}
	Effort  string // empty for a model without efforts
}

// Catalog is the ordered option list together with the models it came from.
type Catalog struct {
	Options []Option // catalog order: models in config order, efforts in their listed order
	models  []config.Model
}

// Build derives the options from the enabled models of cfg. No enabled options and more than
// MaxOptions options are config errors.
func Build(cfg *config.Config) (*Catalog, error) {
	c := &Catalog{models: cfg.Models}
	for _, m := range cfg.Models {
		if len(m.Efforts) == 0 {
			c.Options = append(c.Options, Option{ID: m.Section, CLI: m.CLI, Section: m.Section, Name: m.Name})
			continue
		}
		for _, eff := range m.Efforts {
			c.Options = append(c.Options, Option{
				ID: m.Section + idSep + eff, CLI: m.CLI, Section: m.Section, Name: m.Name, Effort: eff,
			})
		}
	}
	switch n := len(c.Options); {
	case n == 0:
		return nil, errors.New("config: no enabled options: enable at least one [model.*] section")
	case n > MaxOptions:
		return nil, fmt.Errorf("config: %d options, more than Jev's limit of %d: disable models with enabled = false",
			n, MaxOptions)
	}
	return c, nil
}

// LookupModel finds the enabled model whose name or one of whose aliases equals value.
// ok is false when value is not in the catalog; the caller then passes value through unchanged.
func (c *Catalog) LookupModel(value string) (config.Model, bool) {
	for _, m := range c.models {
		if m.Name == value || slices.Contains(m.Aliases, value) {
			return m, true
		}
	}
	return config.Model{}, false
}

// ByCLI keeps the options of the named CLI.
func ByCLI(opts []Option, cli string) []Option {
	return filter(opts, func(o Option) bool { return o.CLI == cli })
}

// ByModel keeps the options of the model section.
func ByModel(opts []Option, section string) []Option {
	return filter(opts, func(o Option) bool { return o.Section == section })
}

// ByEffort keeps the options with the given effort.
func ByEffort(opts []Option, effort string) []Option {
	return filter(opts, func(o Option) bool { return o.Effort == effort })
}

// CLIs returns the distinct CLIs of opts in the order they first appear.
func CLIs(opts []Option) []string {
	var clis []string
	for _, o := range opts {
		if !slices.Contains(clis, o.CLI) {
			clis = append(clis, o.CLI)
		}
	}
	return clis
}

func filter(opts []Option, keep func(Option) bool) []Option {
	var out []Option
	for _, o := range opts {
		if keep(o) {
			out = append(out, o)
		}
	}
	return out
}
