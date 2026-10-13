// Package config loads agrouter's INI configuration: the embedded defaults, the global file and the
// local .agrouter/config, merged per section and per key.
package config

import "time"

// Layer names identify where a config value came from, for error messages and debug output.
const (
	LayerEmbedded = "embedded"
	LayerGlobal   = "global"
	LayerLocal    = "local"
	LayerEnv      = "env"
	LayerFlag     = "flag"
)

// Config is the merged configuration with disabled sections removed.
type Config struct {
	Agrouter Agrouter
	CLIs     []CLI             // in the order their sections first appear across the layers
	Models   []Model           // catalog order: the order their sections first appear across the layers
	Efforts  map[string]Effort // keyed "<cli>.<level>"
}

// Agrouter is the [agrouter] section.
type Agrouter struct {
	APIKey       string
	APIKeySource string // layer that set api_key, one of the Layer* names
	JevModel     string
	Timeout      time.Duration // the whole routing budget: capture, file reading, requests and retries
	// RoutingPolicy is the cost/quality preference sent with every routing question, whole or chunk.
	RoutingPolicy string
	// ComplexityPolicy is how the complexity stage judges the codebase the --doc text describes.
	ComplexityPolicy string
}

// CLI is a [cli.<name>] section: the command and the fixed invocation descriptors agrouter builds the
// child's argv from. A missing template key is nil; an empty JSON array is an empty, non-nil slice.
type CLI struct {
	Name        string // section name without the "cli." prefix
	Command     string
	Description string
	ExecArgs    []string // the noninteractive invocation, always emitted first
	ModelArgs   []string // emitted with {model} substituted when a model is selected
	EffortArgs  []string // emitted with {effort} substituted when an effort is selected
	PromptArgs  []string // emitted last, only for a command-line prompt; {prompt} is its last token
	// SelectionFlags are the native spellings that select a model or effort, rejected after "--".
	SelectionFlags []string
	// ConfigFlags are the native config flag spellings (key=value); SelectionConfigKeys are the keys
	// that select a model or effort through them, rejected after "--" like SelectionFlags.
	ConfigFlags         []string
	SelectionConfigKeys []string
	// ContextFlags are native spellings that change the child's provider, profile or settings context.
	ContextFlags []string
}

// Model is a [model.<section>] section.
type Model struct {
	Section     string // section name without the "model." prefix; the option id is built from it
	CLI         string
	Name        string
	Aliases     []string
	Efforts     []string
	Description string
	Source      string
}

// Effort is an [effort.<cli>.<level>] section.
type Effort struct {
	CLI         string
	Level       string
	Description string
	Source      string
}

// CLIByName returns the enabled CLI with the given name.
func (c *Config) CLIByName(name string) (CLI, bool) {
	for _, cli := range c.CLIs {
		if cli.Name == name {
			return cli, true
		}
	}
	return CLI{}, false
}
