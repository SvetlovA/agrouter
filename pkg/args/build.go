package args

import (
	"fmt"
	"strings"

	"github.com/SvetlovA/agrouter/pkg/config"
)

// Choice is what the argv is built for: the model and effort to emit through the CLI's templates.
// An empty Model or Effort emits no model or effort argument, so the CLI's default applies.
type Choice struct {
	Model  string // the model's name, or a value outside the catalog passed through
	Effort string
	Pinned bool // the CLI was fixed by a valid --cli: raw passthrough is appended only then
}

// Skip is one argument left out of the argv.
type Skip struct {
	Spelling string // as the caller gave it, for the decision JSON "skipped" array
	Warning  string // the stderr line, without a trailing newline
}

// Result is the child's argv and what was left out of it.
type Result struct {
	Argv    []string
	Skipped []Skip
	// promptAt is the index of the {prompt} token in Argv, -1 when there is none; rawAt is where raw
	// passthrough starts, len(Argv) when there is none. Both serve the redacted rendering.
	promptAt int
	rawAt    int
}

// Build turns req into the argv for cli: command, print, mapped arguments in the caller's order,
// model, effort, prompt, then raw passthrough (only when choice.Pinned). An argument the CLI does
// not map, or maps to [], is skipped with a warning; arguments hitting the same mapping key (and,
// for config.* keys, the same key=value) are emitted once.
func Build(cli config.CLI, req *Request, choice Choice) Result {
	b := builder{cli: cli, prompt: req.ArgvPrompt(), res: Result{promptAt: -1}}
	b.res.Argv = append(b.res.Argv, cli.Command)
	b.emit(config.KeyPrint, "")

	seen := map[string]bool{}
	for _, a := range req.Args {
		id := a.Key + "\x00" + a.Value
		if seen[id] {
			continue
		}
		seen[id] = true
		b.mapped(a.Key, a.Value, a.Spelling)
	}

	if choice.Model != "" {
		b.model = choice.Model
		b.mapped(config.KeyModel, "", "--model "+choice.Model)
	}
	if choice.Effort != "" {
		b.effort = choice.Effort
		spelling := "--effort " + choice.Effort
		if req.EffortSpell != "" && choice.Effort == req.Effort {
			spelling = req.EffortSpell // the caller's constraint, as the caller gave it
		}
		b.mapped(config.KeyEffort, "", spelling)
	}
	b.emit(config.KeyPrompt, "")

	b.res.rawAt = len(b.res.Argv)
	if len(req.Raw) > 0 {
		if choice.Pinned {
			b.res.Argv = append(b.res.Argv, req.Raw...)
		} else {
			b.res.Skipped = append(b.res.Skipped, Skip{
				Spelling: strings.Join(append([]string{"--"}, req.Raw...), " "),
				Warning: fmt.Sprintf("agrouter: warning: skipped %d raw argument(s) after --: passed through only with --cli",
					len(req.Raw)),
			})
		}
	}
	return b.res
}

// builder accumulates one Build call.
type builder struct {
	cli           config.CLI
	prompt        string
	model, effort string
	res           Result
}

// mapped emits key through the CLI's template, or records it as skipped.
func (b *builder) mapped(key, value, spelling string) {
	tmpl, ok := b.cli.Args[key]
	switch {
	case !ok:
		b.skip(spelling, b.cli.Name+" has no mapping for it")
	case len(tmpl) == 0:
		b.skip(spelling, "maps to nothing for "+b.cli.Name)
	default:
		b.emit(key, value)
	}
}

func (b *builder) skip(spelling, reason string) {
	b.res.Skipped = append(b.res.Skipped, Skip{
		Spelling: spelling,
		Warning:  fmt.Sprintf("agrouter: warning: skipped %s: %s", spelling, reason),
	})
}

// emit appends key's template with placeholders substituted. A missing key emits nothing. The
// {prompt} token becomes the explicit prompt (Request.ArgvPrompt), or zero tokens when it is empty.
func (b *builder) emit(key, value string) {
	r := strings.NewReplacer(config.PlaceholderModel, b.model, config.PlaceholderEffort, b.effort, config.PlaceholderValue, value)
	for _, tok := range b.cli.Args[key] {
		if tok == config.PlaceholderPrompt {
			if b.prompt == "" {
				continue
			}
			b.res.promptAt = len(b.res.Argv)
			b.res.Argv = append(b.res.Argv, b.prompt)
			continue
		}
		b.res.Argv = append(b.res.Argv, r.Replace(tok))
	}
}
