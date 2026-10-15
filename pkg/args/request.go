// Package args holds the parsed agrouter call and turns it into the chosen CLI's argv through
// the [cli.*] invocation descriptors. Parsing the command line stays in cmd/agrouter.
package args

import "github.com/SvetlovA/agrouter/pkg/prompt"

// Mode selects what agrouter does with its decision.
type Mode int

// Modes of an agrouter call.
const (
	ModeDecision Mode = iota // print the decision JSON
	ModeExec                 // run the chosen CLI
)

// Optional is a string that distinguishes "not given" from "given empty".
type Optional struct {
	Value string
	Set   bool
}

// Request is one parsed agrouter call.
type Request struct {
	Mode       Mode
	CLI        string   // the exec CLI, or --cli in decision mode; empty routes across every CLI
	APIKey     Optional // --jev-api-key; Set with an empty Value clears any other key
	Model      string   // --model constraint, as given (resolved against the catalog later)
	Effort     string   // --effort constraint, as given
	PromptFlag Optional // -p, --prompt, --print
	Prompt     Optional // the positional prompt
	PromptFile Optional // --prompt-file path, as given
	FileText   string   // the --prompt-file contents, read by the caller before capture
	Docs       []string // --doc paths, as given, in order; routing only, never in the child's argv
	Raw        []string // exec only: native tokens after the first "--", passed to the child except reserved model/effort selections
}

// ArgvPrompt is the {prompt} text: the prompt texts given on the command line joined in prompt
// order (-p, the positional prompt, then the --prompt-file contents), like the prompt Jev sees but
// without stdin, which the child gets replayed instead.
func (r *Request) ArgvPrompt() string {
	return prompt.Join(r.PromptFlag.Value, r.Prompt.Value, r.FileText)
}
