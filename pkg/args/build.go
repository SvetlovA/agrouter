package args

import (
	"slices"
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
	// promptAt is the {prompt} index, -1 when absent. Native options and the child's own separator
	// and positional tokens occupy separate ranges around the generated selection arguments.
	promptAt int
	rawAt    int
	rawLen   int
	tailAt   int
	tailLen  int
}

// Build emits native options before the configured model and effort arguments, so agrouter's values
// take precedence. The child's own "--" and positional tokens follow the selection. PromptArgs are
// last and only emitted for a command-line prompt.
func Build(cli config.CLI, req *Request, choice Choice) Result {
	res := Result{promptAt: -1}
	res.Argv = append(res.Argv, cli.Command)
	res.Argv = append(res.Argv, cli.ExecArgs...)
	raw := nativeOptions(req.Raw)
	end := slices.Index(raw, config.EndOfOptions)
	if end < 0 {
		end = len(raw)
	}
	res.rawAt, res.rawLen = len(res.Argv), end
	res.Argv = append(res.Argv, raw[:end]...)
	if choice.Model != "" {
		res.Argv = appendTemplate(res.Argv, cli.ModelArgs, config.PlaceholderModel, choice.Model)
	}
	if choice.Effort != "" {
		res.Argv = appendTemplate(res.Argv, cli.EffortArgs, config.PlaceholderEffort, choice.Effort)
	}
	res.tailAt, res.tailLen = len(res.Argv), len(raw)-end
	res.Argv = append(res.Argv, raw[end:]...)
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
