package config

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Special mapping keys and template placeholders.
const (
	keyPrint  = "print"
	keyPrompt = "prompt"
	keyModel  = "model"
	keyEffort = "effort"

	configKeyPrefix = "config."

	phPrompt = "{prompt}"
	phValue  = "{value}"
)

// placeholderRe finds anything shaped like a placeholder, so a typo such as {mdl} is caught.
var placeholderRe = regexp.MustCompile(`\{[A-Za-z_][A-Za-z0-9_]*\}`)

// Validate checks the merged config and returns every violation, each naming its section and key.
func (c *Config) Validate() error {
	errs := c.Agrouter.validate()
	for _, cli := range c.CLIs {
		errs = append(errs, cli.validate(c.Models)...)
	}
	errs = append(errs, c.validateModels()...)
	return errors.Join(errs...)
}

func (a Agrouter) validate() []error {
	var errs []error
	if a.MaxChunks < 1 {
		errs = append(errs, fmt.Errorf("[agrouter] max_chunks = %d: must be at least 1", a.MaxChunks))
	}
	if a.ChunkParallel < 1 {
		errs = append(errs, fmt.Errorf("[agrouter] chunk_parallel = %d: must be at least 1", a.ChunkParallel))
	}
	// written as a negation so NaN fails too
	if !(a.RelevanceFloor > 0 && a.RelevanceFloor <= 1) {
		errs = append(errs, fmt.Errorf("[agrouter] relevance_floor = %v: must be in (0, 1]", a.RelevanceFloor))
	}
	return errs
}

// validate checks the CLI's [cli.<name>.args] mappings against the rules for required keys,
// {prompt} placement and allowed placeholders.
func (cli CLI) validate(models []Model) []error {
	section := "cli." + cli.Name + ".args"
	var errs []error
	for _, key := range []string{keyPrint, keyModel} {
		if _, ok := cli.Args[key]; !ok {
			errs = append(errs, fmt.Errorf("[%s] %s: required mapping is missing", section, key))
		}
	}
	if _, ok := cli.Args[keyEffort]; !ok {
		if i := slices.IndexFunc(models, func(m Model) bool { return m.CLI == cli.Name && len(m.Efforts) > 0 }); i >= 0 {
			errs = append(errs, fmt.Errorf("[%s] %s: required mapping is missing, [model.%s] has efforts",
				section, keyEffort, models[i].Section))
		}
	}

	keys := make([]string, 0, len(cli.Args))
	for k := range cli.Args {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	prompts := 0 // whole {prompt} tokens across print and prompt
	for _, key := range keys {
		for _, tok := range cli.Args[key] {
			for _, ph := range placeholderRe.FindAllString(tok, -1) {
				if err := checkPlaceholder(key, tok, ph); err != nil {
					errs = append(errs, fmt.Errorf("[%s] %s: %w", section, key, err))
				}
			}
			if tok == phPrompt && (key == keyPrint || key == keyPrompt) {
				prompts++
			}
		}
	}
	if prompts != 1 {
		errs = append(errs, fmt.Errorf("[%s] %s/%s: %s must appear exactly once across print and prompt, found %d",
			section, keyPrint, keyPrompt, phPrompt, prompts))
	}
	return errs
}

// checkPlaceholder reports whether placeholder ph is allowed in token tok of mapping key.
func checkPlaceholder(key, tok, ph string) error {
	switch ph {
	case "{model}", "{effort}":
		return nil
	case phValue:
		if !strings.HasPrefix(key, configKeyPrefix) {
			return fmt.Errorf("%s is only allowed in config.* mappings", phValue)
		}
		return nil
	case phPrompt:
		if key != keyPrint && key != keyPrompt {
			return fmt.Errorf("%s is only allowed in print or prompt", phPrompt)
		}
		if tok != phPrompt {
			return fmt.Errorf("%s must be a whole token, found in %q", phPrompt, tok)
		}
		return nil
	default:
		return fmt.Errorf("unknown placeholder %s in %q", ph, tok)
	}
}

// validateModels checks section names, required names, known CLIs and that every name and alias
// resolves to exactly one model.
func (c *Config) validateModels() []error {
	var errs []error
	owner := map[string]string{} // name or alias -> model section that claimed it first
	claim := func(section, key, id string) {
		if prev, ok := owner[id]; ok {
			errs = append(errs, fmt.Errorf("[model.%s] %s: %q is already used by [model.%s]", section, key, id, prev))
			return
		}
		owner[id] = section
	}
	for _, m := range c.Models {
		if strings.Contains(m.Section, "@") {
			errs = append(errs, fmt.Errorf("[model.%s]: '@' is reserved for option ids and not allowed in model section names", m.Section))
		}
		if _, ok := c.CLIByName(m.CLI); !ok {
			errs = append(errs, fmt.Errorf("[model.%s] cli = %q: no enabled [cli.%s] section", m.Section, m.CLI, m.CLI))
		}
		if m.Name == "" {
			errs = append(errs, fmt.Errorf("[model.%s] name: required", m.Section))
		} else {
			claim(m.Section, "name", m.Name)
		}
		for _, alias := range m.Aliases {
			claim(m.Section, "aliases", alias)
		}
	}
	return errs
}
