package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/ini.v1"
)

// Environment variables read by the config package.
const (
	EnvConfigDir = "AGROUTER_CONFIG_DIR"
	EnvAPIKey    = "TYPESAFE_API_KEY" //nolint:gosec // variable name, not a credential
)

// Sources are the config layers to load, lowest precedence first. An empty path or a missing file
// is no layer; any other read error is a config error.
type Sources struct {
	Embedded   []byte
	GlobalPath string
	LocalPath  string
}

// DefaultPaths returns the global config path (AGROUTER_CONFIG_DIR or ~/.config/agrouter) and the local
// .agrouter/config path under workDir.
func DefaultPaths(workDir string) (global, local string, err error) {
	dir := os.Getenv(EnvConfigDir)
	if dir == "" {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return "", "", fmt.Errorf("find global config directory: %w", homeErr)
		}
		dir = filepath.Join(home, ".config", "agrouter")
	}
	return filepath.Join(dir, "config"), filepath.Join(workDir, ".agrouter", "config"), nil
}

// ResolveAPIKey applies the key precedence and returns the key with the layer that decided it:
// --jev-api-key when given (an explicit empty value clears the key), then a non-empty TYPESAFE_API_KEY,
// then api_key from the merged config (local > global > embedded).
func ResolveAPIKey(flagValue string, flagSet bool, envValue string, cfg Agrouter) (key, source string) {
	switch {
	case flagSet:
		return flagValue, LayerFlag
	case envValue != "":
		return envValue, LayerEnv
	default:
		return cfg.APIKey, cfg.APIKeySource
	}
}

// Load reads and merges the layers per section and per key, then decodes and validates the result.
func Load(src Sources) (*Config, error) {
	m := &merged{sections: map[string]*section{}}
	if err := m.add(LayerEmbedded, "", src.Embedded); err != nil {
		return nil, err
	}
	for _, l := range []struct{ layer, path string }{{LayerGlobal, src.GlobalPath}, {LayerLocal, src.LocalPath}} {
		if l.path == "" {
			continue
		}
		data, err := os.ReadFile(l.path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s config: %w", l.layer, err)
		}
		if err := m.add(l.layer, l.path, data); err != nil {
			return nil, err
		}
	}
	cfg, err := m.decode()
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// entry is one key's winning value and where it came from.
type entry struct {
	value string
	layer string
	path  string
}

// section keeps its keys in first-seen order.
type section struct {
	name   string
	keys   []string
	values map[string]entry
}

// merged is the per-section, per-key union of the layers. ini.v1's own lookups are avoided because
// they inherit keys from parent sections ([cli.x.args] would see [cli.x]'s keys).
type merged struct {
	order    []string
	sections map[string]*section
}

func (m *merged) add(layer, path string, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	f, err := ini.LoadSources(ini.LoadOptions{IgnoreInlineComment: true, KeyValueDelimiters: "="}, data)
	if err != nil {
		return fmt.Errorf("parse %s: %w", origin(layer, path), err)
	}
	for _, s := range f.Sections() {
		keys := s.Keys()
		if s.Name() == ini.DefaultSection {
			if len(keys) > 0 {
				return fmt.Errorf("%s: key %q outside any section", origin(layer, path), keys[0].Name())
			}
			continue
		}
		dst, ok := m.sections[s.Name()]
		if !ok {
			dst = &section{name: s.Name(), values: map[string]entry{}}
			m.sections[s.Name()] = dst
			m.order = append(m.order, s.Name())
		}
		for _, k := range keys {
			if _, seen := dst.values[k.Name()]; !seen {
				dst.keys = append(dst.keys, k.Name())
			}
			dst.values[k.Name()] = entry{value: k.Value(), layer: layer, path: path}
		}
	}
	return nil
}

func (m *merged) decode() (*Config, error) {
	cfg := &Config{Efforts: map[string]Effort{}}
	disabled := map[string]bool{} // CLI names with enabled = false
	for _, name := range m.order {
		if cliName, ok := strings.CutPrefix(name, "cli."); ok && !strings.HasSuffix(name, ".args") {
			on, err := m.sections[name].enabled()
			if err != nil {
				return nil, err
			}
			disabled[cliName] = !on
		}
	}

	for _, name := range m.order {
		if err := m.decodeSection(cfg, m.sections[name], disabled); err != nil {
			return nil, err
		}
	}

	// a disabled CLI takes its models and effort descriptions with it
	models := cfg.Models[:0]
	for _, mdl := range cfg.Models {
		if !disabled[mdl.CLI] {
			models = append(models, mdl)
		}
	}
	cfg.Models = models
	for id, eff := range cfg.Efforts {
		if disabled[eff.CLI] {
			delete(cfg.Efforts, id)
		}
	}
	return cfg, nil
}

// decodeSection decodes one section into cfg by its name prefix, skipping disabled sections.
func (m *merged) decodeSection(cfg *Config, s *section, disabled map[string]bool) error {
	switch name := s.name; {
	case name == "agrouter":
		return s.decodeAgrouter(&cfg.Agrouter)
	case strings.HasPrefix(name, "cli.") && strings.HasSuffix(name, ".args"):
		// decoded with its [cli.*] section
		cliName := strings.TrimSuffix(strings.TrimPrefix(name, "cli."), ".args")
		if _, known := disabled[cliName]; !known {
			return fmt.Errorf("[%s]: no [cli.%s] section", name, cliName)
		}
	case strings.HasPrefix(name, "cli."):
		if disabled[strings.TrimPrefix(name, "cli.")] {
			return nil
		}
		cli, err := m.decodeCLI(s)
		if err != nil {
			return err
		}
		cfg.CLIs = append(cfg.CLIs, cli)
	case strings.HasPrefix(name, "model."):
		mdl, on, err := s.decodeModel()
		if err != nil {
			return err
		}
		if on {
			cfg.Models = append(cfg.Models, mdl)
		}
	case strings.HasPrefix(name, "effort."):
		eff, on, err := s.decodeEffort()
		if err != nil {
			return err
		}
		if on {
			cfg.Efforts[eff.CLI+"."+eff.Level] = eff
		}
	default:
		return fmt.Errorf("[%s]: unknown section", name)
	}
	return nil
}

func (s *section) decodeAgrouter(a *Agrouter) error {
	for _, k := range s.keys {
		e := s.values[k]
		var err error
		switch k {
		case "api_key":
			a.APIKey, a.APIKeySource = e.value, e.layer
		case "jev_model":
			a.JevModel = e.value
		case "timeout":
			a.Timeout, err = time.ParseDuration(e.value)
		case "max_chunks":
			a.MaxChunks, err = strconv.Atoi(e.value)
		case "chunk_parallel":
			a.ChunkParallel, err = strconv.Atoi(e.value)
		case "relevance_floor":
			a.RelevanceFloor, err = strconv.ParseFloat(e.value, 64)
		case "question":
			a.Question = e.value
		case "chunk_question":
			a.ChunkQuestion = e.value
		case "relevance":
			a.Relevance = e.value
		default:
			return s.errorf(k, "unknown key")
		}
		if err != nil {
			return s.errorf(k, "%w", err)
		}
	}
	return nil
}

func (m *merged) decodeCLI(s *section) (CLI, error) {
	cli := CLI{Name: strings.TrimPrefix(s.name, "cli."), Args: map[string][]string{}}
	for _, k := range s.keys {
		switch k {
		case "command":
			cli.Command = s.values[k].value
		case "description":
			cli.Description = s.values[k].value
		case "enabled":
		default:
			return CLI{}, s.errorf(k, "unknown key")
		}
	}
	args, ok := m.sections[s.name+".args"]
	if !ok {
		return cli, nil
	}
	for _, k := range args.keys {
		tmpl, err := parseTemplate(args.values[k].value)
		if err != nil {
			return CLI{}, args.errorf(k, "%w", err)
		}
		cli.Args[k] = tmpl
	}
	return cli, nil
}

func (s *section) decodeModel() (Model, bool, error) {
	mdl := Model{Section: strings.TrimPrefix(s.name, "model.")}
	for _, k := range s.keys {
		v := s.values[k].value
		switch k {
		case "cli":
			mdl.CLI = v
		case "name":
			mdl.Name = v
		case "aliases":
			mdl.Aliases = splitList(v)
		case "efforts":
			mdl.Efforts = splitList(v)
		case "description":
			mdl.Description = v
		case "source":
			mdl.Source = v
		case "enabled":
		default:
			return Model{}, false, s.errorf(k, "unknown key")
		}
	}
	on, err := s.enabled()
	return mdl, on, err
}

func (s *section) decodeEffort() (Effort, bool, error) {
	cliName, level, ok := strings.Cut(strings.TrimPrefix(s.name, "effort."), ".")
	if !ok || cliName == "" || level == "" {
		return Effort{}, false, fmt.Errorf("[%s]: effort sections are named [effort.<cli>.<level>]", s.name)
	}
	eff := Effort{CLI: cliName, Level: level}
	for _, k := range s.keys {
		switch k {
		case "description":
			eff.Description = s.values[k].value
		case "source":
			eff.Source = s.values[k].value
		case "enabled":
		default:
			return Effort{}, false, s.errorf(k, "unknown key")
		}
	}
	on, err := s.enabled()
	return eff, on, err
}

// enabled reports the section's enabled key, true when absent.
func (s *section) enabled() (bool, error) {
	e, ok := s.values["enabled"]
	if !ok {
		return true, nil
	}
	on, err := strconv.ParseBool(e.value)
	if err != nil {
		return false, s.errorf("enabled", "%w", err)
	}
	return on, nil
}

// errorf formats an error naming the section, the key, its value and the layer that set it.
func (s *section) errorf(key, format string, args ...any) error {
	e := s.values[key]
	return fmt.Errorf("[%s] %s = %q (%s): %w", s.name, key, e.value, origin(e.layer, e.path), fmt.Errorf(format, args...))
}

// parseTemplate decodes a [cli.*.args] value: a JSON array of strings, possibly empty.
func parseTemplate(v string) ([]string, error) {
	if !strings.HasPrefix(strings.TrimSpace(v), "[") {
		return nil, errors.New("template must be a JSON array of strings")
	}
	tmpl := []string{}
	if err := json.Unmarshal([]byte(v), &tmpl); err != nil {
		return nil, fmt.Errorf("template must be a JSON array of strings: %w", err)
	}
	return tmpl, nil
}

// splitList splits a comma-separated value, trimming spaces and dropping empty items.
func splitList(v string) []string {
	var out []string
	for item := range strings.SplitSeq(v, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func origin(layer, path string) string {
	if path == "" {
		return layer
	}
	return layer + " " + path
}
