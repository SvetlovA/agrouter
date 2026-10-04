// Package args holds the parsed agrouter call and turns it into the chosen CLI's argv through
// the [cli.*.args] templates. Parsing the command line stays in cmd/agrouter.
package args

import "github.com/SvetlovA/agrouter/pkg/prompt"

// Mode selects what agrouter does with its decision.
type Mode int

// Modes of an agrouter call.
const (
	ModeDecision Mode = iota // print the decision JSON
	ModeExec                 // run the chosen CLI
)

// Source says where a model or effort constraint came from.
type Source int

// Sources of a constraint.
const (
	SourceNone   Source = iota // no constraint
	SourceFlag                 // --model / --effort
	SourceConfig               // -c model=... / -c model_reasoning_effort=...
)

// Arg is one mapped argument, in the caller's order.
type Arg struct {
	Spelling string // how the caller gave it, for warnings and "skipped": "--output-format json"
	Key      string // mapping key: "output-format.json", "verbose", "config.features.multi_agent"
	Value    string // {value}: the whole key=value of a -c argument, empty otherwise
}

// Optional is a string that distinguishes "not given" from "given empty".
type Optional struct {
	Value string
	Set   bool
}

// Request is one parsed agrouter call.
type Request struct {
	Mode         Mode
	CLI          string   // --cli or AGROUTER_CLI; empty routes across every CLI
	APIKey       Optional // --jev-api-key; Set with an empty Value clears any other key
	Model        string   // model constraint, as given (resolved against the catalog later)
	ModelSource  Source
	Effort       string // effort constraint, as given
	EffortSource Source
	EffortSpell  string   // how the caller gave the effort constraint, for a warning when the CLI cannot map it
	Args         []Arg    // mapped arguments in the caller's order
	PromptFlag   Optional // -p, --prompt
	Prompt       Optional // the positional prompt
	PromptFile   Optional // --prompt-file path, as given
	FileText     string   // the --prompt-file contents, read by the caller before capture
	Raw          []string // tokens after the first "--", passed through only with --cli
}

// Explicit returns the prompt texts given on the command line, in prompt order: -p, the positional
// prompt, then the --prompt-file contents. Stdin is not among them.
func (r *Request) Explicit() []string {
	return []string{r.PromptFlag.Value, r.Prompt.Value, r.FileText}
}

// ArgvPrompt is the {prompt} text: the explicit texts joined like the prompt Jev sees, without stdin,
// which the child gets replayed instead.
func (r *Request) ArgvPrompt() string {
	return prompt.Join(r.Explicit()...)
}
