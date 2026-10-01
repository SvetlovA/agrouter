// Package args holds the parsed agrouter call and turns it into the chosen CLI's argv through
// the [cli.*.args] templates. Parsing the command line stays in cmd/agrouter.
package args

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
	Prompt       Optional // the positional prompt
	Raw          []string // tokens after the first "--", passed through only with --cli
}
