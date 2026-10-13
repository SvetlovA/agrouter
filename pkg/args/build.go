package args

import (
	"strings"

	"github.com/SvetlovA/agrouter/pkg/config"
)

// Choice is what the argv is built for: the model and effort to emit through the CLI's descriptors.
// An empty Model or Effort emits no model or effort argument, so the CLI's default applies.
type Choice struct {
	Model  string // the model's name, or a value outside the catalog passed through
	Effort string
}

// Result is the child's argv.
type Result struct {
	Argv []string
	// promptAt is the index of the {prompt} token in Argv, -1 when there is none; native passthrough is
	// Argv[rawAt:rawAt+rawLen]. They serve the redacted rendering.
	promptAt int
	rawAt    int
	rawLen   int
}

// Build turns req into the argv for cli: command, exec_args, model_args and effort_args for the
// selected values, the native tokens after "--" unchanged, then prompt_args, emitted only when there
// is a command-line prompt. The prompt goes last, so nothing after it is parsed as a child option.
func Build(cli config.CLI, req *Request, choice Choice) Result {
	res := Result{promptAt: -1}
	res.Argv = append(res.Argv, cli.Command)
	res.Argv = append(res.Argv, cli.ExecArgs...)
	if choice.Model != "" {
		res.Argv = appendTemplate(res.Argv, cli.ModelArgs, config.PlaceholderModel, choice.Model)
	}
	if choice.Effort != "" {
		res.Argv = appendTemplate(res.Argv, cli.EffortArgs, config.PlaceholderEffort, choice.Effort)
	}
	res.rawAt, res.rawLen = len(res.Argv), len(req.Raw)
	res.Argv = append(res.Argv, req.Raw...)
	if p := req.ArgvPrompt(); p != "" {
		for _, tok := range cli.PromptArgs {
			if tok == config.PlaceholderPrompt {
				res.promptAt = len(res.Argv)
				res.Argv = append(res.Argv, p)
				continue
			}
			res.Argv = append(res.Argv, tok)
		}
	}
	return res
}

// appendTemplate appends tmpl to argv with placeholder replaced by value in every token.
func appendTemplate(argv, tmpl []string, placeholder, value string) []string {
	for _, tok := range tmpl {
		argv = append(argv, strings.ReplaceAll(tok, placeholder, value))
	}
	return argv
}
