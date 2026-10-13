package config

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// EndOfOptions ends option parsing in the child. When prompt_args puts it before {prompt}, prompt text
// starting with "-" stays a positional argument instead of becoming a flag.
const EndOfOptions = "--"

// Placeholders substituted in the invocation descriptors, each only in its own key. {prompt} is a whole
// token only.
const (
	PlaceholderPrompt = "{prompt}"
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
		{"routing_policy", a.RoutingPolicy},
		{"complexity_policy", a.ComplexityPolicy},
	} {
		if strings.TrimSpace(q.text) == "" {
			errs = append(errs, fmt.Errorf("[agrouter] %s: must not be empty", q.key))
		}
	}
	return errs
}

// validate checks the CLI's command and invocation descriptors: required keys, placeholders only in
// their own key, {prompt} as the last prompt_args token, and the native flag lists.
func (cli CLI) validate(models []Model) []error {
	section := "cli." + cli.Name
	var errs []error
	add := func(key, format string, a ...any) {
		errs = append(errs, fmt.Errorf("[%s] %s: %s", section, key, fmt.Sprintf(format, a...)))
	}
	if strings.TrimSpace(cli.Command) == "" {
		add("command", "must not be empty")
	}
	hasEfforts := slices.ContainsFunc(models, func(m Model) bool { return m.CLI == cli.Name && len(m.Efforts) > 0 })
	cli.validateTemplates(hasEfforts, add)
	cli.validateFlags(add)
	if cli.Availability != nil {
		errs = append(errs, cli.Availability.validate(section+".availability")...)
	}
	return errs
}

// versionRe is a dotted numeric version such as 2.1 or 0.162.1.
var versionRe = regexp.MustCompile(`^\d+(\.\d+)*$`)

// validate checks the availability policy, the quota line parts and every listed check.
func (a *Availability) validate(section string) []error {
	var errs []error
	add := func(section, key, format string, args ...any) {
		errs = append(errs, fmt.Errorf("[%s] %s: %s", section, key, fmt.Sprintf(format, args...)))
	}
	if a.Timeout <= 0 {
		add(section, "timeout", "must be positive")
	}
	if a.Unknown != UnknownAllow && a.Unknown != UnknownBlock {
		add(section, "unknown", "must be %s or %s", UnknownAllow, UnknownBlock)
	}
	for _, f := range []struct{ key, value string }{
		{"quota_error_prefix", a.QuotaErrorPrefix},
		{"quota_error_message", a.QuotaErrorMessage},
	} {
		if strings.ContainsFunc(f.value, unicode.IsControl) {
			add(section, f.key, "must not contain a newline or control character")
		}
	}
	for _, c := range a.Checks {
		cs := section + "." + c.Name
		switch c.Transport {
		case TransportJSONHelper, TransportSDKControlUsage, TransportJSONRPCRateLimit:
		case "":
			add(cs, "transport", "required")
		default:
			add(cs, "transport", "unknown transport %q (want %s, %s or %s)", c.Transport,
				TransportJSONHelper, TransportSDKControlUsage, TransportJSONRPCRateLimit)
		}
		if c.MinVersion != "" && !versionRe.MatchString(c.MinVersion) {
			add(cs, "min_version", "%q is not a dotted numeric version", c.MinVersion)
		}
	}
	return errs
}

// validateTemplates checks the *_args keys: presence, each key's own placeholder, and prompt_args'
// last token.
func (cli CLI) validateTemplates(hasEfforts bool, add func(key, format string, a ...any)) {
	templates := []struct {
		key      string
		tmpl     []string
		own      string // the one placeholder the key holds, if any
		required bool
	}{
		{"exec_args", cli.ExecArgs, "", true},
		{"model_args", cli.ModelArgs, PlaceholderModel, true},
		{"effort_args", cli.EffortArgs, PlaceholderEffort, hasEfforts},
		{"prompt_args", cli.PromptArgs, PlaceholderPrompt, true},
	}
	for _, t := range templates {
		if t.tmpl == nil {
			if t.required {
				add(t.key, "required")
			}
			continue
		}
		if t.own != "" && !slices.ContainsFunc(t.tmpl, func(tok string) bool { return strings.Contains(tok, t.own) }) {
			add(t.key, "must contain %s", t.own)
		}
		for _, tok := range t.tmpl {
			for _, ph := range placeholderRe.FindAllString(tok, -1) {
				if err := checkPlaceholder(t.key, t.own, tok, ph); err != nil {
					add(t.key, "%v", err)
				}
			}
		}
	}
	if n := len(cli.PromptArgs); n > 0 && (cli.PromptArgs[n-1] != PlaceholderPrompt ||
		slices.Index(cli.PromptArgs, PlaceholderPrompt) != n-1) {
		add("prompt_args", "must hold %s once, as its last element", PlaceholderPrompt)
	}
}

// validateFlags checks the native flag spellings and selection config keys.
func (cli CLI) validateFlags(add func(key, format string, a ...any)) {
	for _, l := range []struct {
		key   string
		items []string
	}{
		{"selection_flags", cli.SelectionFlags},
		{"config_flags", cli.ConfigFlags},
		{"context_flags", cli.ContextFlags},
	} {
		for _, f := range l.items {
			if !isFlagSpelling(f) {
				add(l.key, "%q is not a flag spelling (-x or --name)", f)
			}
		}
	}
	for _, k := range cli.SelectionConfigKeys {
		if strings.ContainsAny(k, "= \t") {
			add("selection_config_keys", "%q is not a config key", k)
		}
	}
	if (len(cli.ConfigFlags) == 0) != (len(cli.SelectionConfigKeys) == 0) {
		add("selection_config_keys", "set together with config_flags")
	}
}

// isFlagSpelling reports whether f is a single option spelling such as -m or --model.
func isFlagSpelling(f string) bool {
	name := strings.TrimPrefix(strings.TrimPrefix(f, "-"), "-")
	return strings.HasPrefix(f, "-") && name != "" && !strings.HasPrefix(name, "-") && !strings.ContainsAny(name, "= \t")
}

// checkPlaceholder reports whether placeholder ph is allowed in token tok of key, whose own
// placeholder is own.
func checkPlaceholder(key, own, tok, ph string) error {
	switch ph {
	case PlaceholderModel, PlaceholderEffort, PlaceholderPrompt:
	default:
		return fmt.Errorf("unknown placeholder %s in %q", ph, tok)
	}
	if ph != own {
		return fmt.Errorf("%s is not allowed in %s", ph, key)
	}
	if ph == PlaceholderPrompt && tok != PlaceholderPrompt {
		return fmt.Errorf("%s must be a whole token, found in %q", PlaceholderPrompt, tok)
	}
	return nil
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
