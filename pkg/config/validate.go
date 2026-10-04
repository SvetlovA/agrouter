package config

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Mapping keys with special meaning in [cli.*.args].
const (
	KeyPrint  = "print"
	KeyPrompt = "prompt"
	KeyModel  = "model"
	KeyEffort = "effort"
)

// ConfigKeyPrefix prefixes the mapping key of a -c key=value argument: "config.<key>".
const ConfigKeyPrefix = "config."

// Placeholders substituted inside template tokens. {prompt} is a whole token only.
const (
	PlaceholderPrompt = "{prompt}"
	PlaceholderValue  = "{value}"
	PlaceholderModel  = "{model}"
	PlaceholderEffort = "{effort}"
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
	if a.Timeout <= 0 {
		errs = append(errs, fmt.Errorf("[agrouter] timeout = %s: must be positive", a.Timeout))
	}
	for _, q := range []struct{ key, text string }{
		{"complexity_question", a.ComplexityQuestion},
		{"complexity_evidence", a.ComplexityEvidence},
	} {
		if strings.TrimSpace(q.text) == "" {
			errs = append(errs, fmt.Errorf("[agrouter] %s: must not be empty", q.key))
		}
	}
	return errs
}

// validate checks the CLI's command and its [cli.<name>.args] mappings against the rules for
// required keys, {prompt} placement and allowed placeholders.
func (cli CLI) validate(models []Model) []error {
	section := "cli." + cli.Name + ".args"
	var errs []error
	if strings.TrimSpace(cli.Command) == "" {
		errs = append(errs, fmt.Errorf("[cli.%s] command: must not be empty", cli.Name))
	}
	for _, key := range []string{KeyPrint, KeyModel} {
		if _, ok := cli.Args[key]; !ok {
			errs = append(errs, fmt.Errorf("[%s] %s: required mapping is missing", section, key))
		}
	}
	if _, ok := cli.Args[KeyEffort]; !ok {
		if i := slices.IndexFunc(models, func(m Model) bool { return m.CLI == cli.Name && len(m.Efforts) > 0 }); i >= 0 {
			errs = append(errs, fmt.Errorf("[%s] %s: required mapping is missing, [model.%s] has efforts",
				section, KeyEffort, models[i].Section))
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
			if tok == PlaceholderPrompt && (key == KeyPrint || key == KeyPrompt) {
				prompts++
			}
		}
	}
	if prompts != 1 {
		errs = append(errs, fmt.Errorf("[%s] %s/%s: %s must appear exactly once across print and prompt, found %d",
			section, KeyPrint, KeyPrompt, PlaceholderPrompt, prompts))
	}
	return errs
}

// checkPlaceholder reports whether placeholder ph is allowed in token tok of mapping key.
func checkPlaceholder(key, tok, ph string) error {
	switch ph {
	case PlaceholderModel, PlaceholderEffort:
		return nil
	case PlaceholderValue:
		if !strings.HasPrefix(key, ConfigKeyPrefix) {
			return fmt.Errorf("%s is only allowed in config.* mappings", PlaceholderValue)
		}
		return nil
	case PlaceholderPrompt:
		if key != KeyPrint && key != KeyPrompt {
			return fmt.Errorf("%s is only allowed in print or prompt", PlaceholderPrompt)
		}
		if tok != PlaceholderPrompt {
			return fmt.Errorf("%s must be a whole token, found in %q", PlaceholderPrompt, tok)
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
