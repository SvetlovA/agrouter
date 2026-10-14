package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const baseConfig = `
[agrouter]
api_key   =
jev_model = jev-latest
timeout   = 10s
routing_policy    = Prefer the cheapest option; compare ` + "`models`" + ` "well".
complexity_policy = Judge the whole codebase.

[cli.alpha]
command               = alpha
description           = Alpha agent.
exec_args             = ["-p"]
model_args            = ["--model", "{model}"]
effort_args           = ["-c", "effort=\"{effort}\""]
prompt_args           = ["--", "{prompt}"]

[cli.alpha.availability]
checks = usage

[cli.alpha.availability.usage]
transport = json-helper

[cli.beta]
command     = beta
exec_args   = []
model_args  = ["-m", "{model}"]
prompt_args = ["{prompt}"]

[model.alpha-big]
cli         = alpha
name        = alpha-big-1
aliases     = big, large
efforts     = low, high
description = Strong model.
source      = https://example.com/alpha-big

[model.alpha-small]
cli     = alpha
name    = alpha-small-1
efforts =

[model.beta-one]
cli  = beta
name = beta-one

[effort.alpha.low]
description = Fast.
source      = https://example.com/effort
`

// writeFile writes content to dir/name, creating directories, and returns the path.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func load(t *testing.T, global, local string) (*Config, error) {
	t.Helper()
	dir := t.TempDir()
	src := Sources{Embedded: []byte(baseConfig)}
	if global != "" {
		src.GlobalPath = writeFile(t, dir, "global/config", global)
	}
	if local != "" {
		src.LocalPath = writeFile(t, dir, "local/.agrouter/config", local)
	}
	return Load(src)
}

func modelSections(cfg *Config) []string {
	out := make([]string, 0, len(cfg.Models))
	for _, m := range cfg.Models {
		out = append(out, m.Section)
	}
	return out
}

func TestLoad_Embedded(t *testing.T) {
	cfg, err := load(t, "", "")
	require.NoError(t, err)

	assert.Equal(t, Agrouter{
		APIKeySource:     LayerEmbedded,
		JevModel:         "jev-latest",
		Timeout:          10 * time.Second,
		RoutingPolicy:    "Prefer the cheapest option; compare `models` \"well\".",
		ComplexityPolicy: "Judge the whole codebase.",
	}, cfg.Agrouter)

	require.Len(t, cfg.CLIs, 2)
	alpha := cfg.CLIs[0]
	assert.Equal(t, "alpha", alpha.Name)
	assert.Equal(t, "alpha", alpha.Command)
	assert.Equal(t, "Alpha agent.", alpha.Description)
	assert.Equal(t, CLI{
		Name: "alpha", Command: "alpha", Description: "Alpha agent.",
		ExecArgs:   []string{"-p"},
		ModelArgs:  []string{"--model", "{model}"},
		EffortArgs: []string{"-c", `effort="{effort}"`},
		PromptArgs: []string{"--", "{prompt}"},
		Availability: &Availability{
			Enabled: true, Timeout: DefaultAvailabilityTimeout, Unknown: UnknownAllow, QuotaErrorPrefix: DefaultQuotaErrorPrefix,
			Checks: []Check{{Name: "usage", Transport: TransportJSONHelper}},
		},
	}, alpha)
	assert.Nil(t, cfg.CLIs[1].Availability, "a CLI without the section has no availability config")

	assert.Equal(t, []string{"alpha-big", "alpha-small", "beta-one"}, modelSections(cfg))
	assert.Equal(t, Model{
		Section: "alpha-big", CLI: "alpha", Name: "alpha-big-1",
		Aliases: []string{"big", "large"}, Efforts: []string{"low", "high"},
		Description: "Strong model.", Source: "https://example.com/alpha-big",
	}, cfg.Models[0])
	assert.Empty(t, cfg.Models[1].Efforts)

	assert.Equal(t, map[string]Effort{
		"alpha.low": {CLI: "alpha", Level: "low", Description: "Fast.", Source: "https://example.com/effort"},
	}, cfg.Efforts)

	beta, ok := cfg.CLIByName("beta")
	require.True(t, ok)
	assert.Equal(t, "beta", beta.Command)
	assert.NotNil(t, beta.ExecArgs, "[] must stay distinguishable from a missing key")
	assert.Empty(t, beta.ExecArgs)
	assert.Nil(t, beta.EffortArgs)
	_, ok = cfg.CLIByName("gamma")
	assert.False(t, ok)
}

func TestLoad_Layering(t *testing.T) {
	global := `
[agrouter]
api_key = global-key
timeout = 20s
routing_policy = Prefer the strongest option.

[cli.alpha]
exec_args     = ["--print"]

[model.alpha-big]
description = Global description.
`
	local := `
[agrouter]
timeout = 30s
complexity_policy = Judge only the architecture.

[cli.alpha]
model_args = ["--use-model", "{model}"]

[model.alpha-big]
description = Local description.

[model.local-new]
cli  = beta
name = local-new
`
	cfg, err := load(t, global, local)
	require.NoError(t, err)

	assert.Equal(t, 30*time.Second, cfg.Agrouter.Timeout, "local overrides global")
	assert.Equal(t, "global-key", cfg.Agrouter.APIKey)
	assert.Equal(t, LayerGlobal, cfg.Agrouter.APIKeySource)
	assert.Equal(t, "jev-latest", cfg.Agrouter.JevModel, "untouched keys keep the embedded value")
	assert.Equal(t, "Prefer the strongest option.", cfg.Agrouter.RoutingPolicy, "global overrides a policy")
	assert.Equal(t, "Judge only the architecture.", cfg.Agrouter.ComplexityPolicy, "local overrides a policy")

	alpha, _ := cfg.CLIByName("alpha")
	assert.Equal(t, []string{"--print"}, alpha.ExecArgs, "global overrides one descriptor key")
	assert.Equal(t, []string{"--use-model", "{model}"}, alpha.ModelArgs, "local overrides one descriptor key")
	assert.Equal(t, []string{"-c", `effort="{effort}"`}, alpha.EffortArgs, "other descriptor keys stay embedded")
	assert.Equal(t, "alpha", alpha.Command)

	assert.Equal(t, []string{"alpha-big", "alpha-small", "beta-one", "local-new"}, modelSections(cfg))
	assert.Equal(t, "Local description.", cfg.Models[0].Description)
	assert.Equal(t, "alpha-big-1", cfg.Models[0].Name, "other model keys stay embedded")
}

func TestLoad_Disabled(t *testing.T) {
	tests := []struct {
		name        string
		local       string
		wantCLIs    []string
		wantModels  []string
		wantEfforts int
	}{
		{name: "model", local: "[model.alpha-small]\nenabled = false\n",
			wantCLIs: []string{"alpha", "beta"}, wantModels: []string{"alpha-big", "beta-one"}, wantEfforts: 1},
		{name: "effort", local: "[effort.alpha.low]\nenabled = false\n",
			wantCLIs: []string{"alpha", "beta"}, wantModels: []string{"alpha-big", "alpha-small", "beta-one"}},
		{name: "cli takes its models and efforts", local: "[cli.alpha]\nenabled = false\n",
			wantCLIs: []string{"beta"}, wantModels: []string{"beta-one"}},
		{name: "disabled cli skips its subsections", local: "[cli.alpha]\nenabled = false\n[cli.alpha.availability]\nchecks = x\n",
			wantCLIs: []string{"beta"}, wantModels: []string{"beta-one"}},
		{name: "availability subsections of an enabled cli", local: "[cli.beta.availability]\nunknown = allow\n" +
			"[cli.beta.availability.status]\ntransport = json-helper\n",
			wantCLIs: []string{"alpha", "beta"}, wantModels: []string{"alpha-big", "alpha-small", "beta-one"}, wantEfforts: 1},
		{name: "re-enabled explicitly", local: "[model.alpha-small]\nenabled = true\n",
			wantCLIs: []string{"alpha", "beta"}, wantModels: []string{"alpha-big", "alpha-small", "beta-one"}, wantEfforts: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := load(t, "", tc.local)
			require.NoError(t, err)
			var clis []string
			for _, c := range cfg.CLIs {
				clis = append(clis, c.Name)
			}
			assert.Equal(t, tc.wantCLIs, clis)
			assert.Equal(t, tc.wantModels, modelSections(cfg))
			assert.Len(t, cfg.Efforts, tc.wantEfforts)
		})
	}
}

func TestLoad_MissingFilesIgnored(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(Sources{
		Embedded:   []byte(baseConfig),
		GlobalPath: filepath.Join(dir, "nope", "config"),
		LocalPath:  filepath.Join(dir, ".agrouter", "config"),
	})
	require.NoError(t, err)
	assert.Len(t, cfg.Models, 3)
}

func TestLoad_Errors(t *testing.T) {
	tests := []struct {
		name  string
		local string
		want  string
	}{
		{name: "malformed JSON template", local: "[cli.alpha]\nexec_args = [\"-p\", \n",
			want: `[cli.alpha] exec_args = "[\"-p\"," (local `},
		{name: "template not an array", local: "[cli.alpha]\nprompt_args = -p {prompt}\n",
			want: "template must be a JSON array of strings"},
		{name: "template null", local: "[cli.alpha]\nmodel_args = null\n",
			want: "template must be a JSON array of strings"},
		{name: "template of numbers", local: "[cli.alpha]\neffort_args = [1]\n",
			want: "[cli.alpha] effort_args"},
		{name: "bad duration", local: "[agrouter]\ntimeout = 10\n", want: `[agrouter] timeout = "10" (local `},
		{name: "removed max_chunks", local: "[agrouter]\nmax_chunks = 64\n", want: "[agrouter] max_chunks (local "},
		{name: "removed chunk_parallel", local: "[agrouter]\nchunk_parallel = 4\n", want: "[agrouter] chunk_parallel (local "},
		{name: "removed relevance_floor", local: "[agrouter]\nrelevance_floor = 0.05\n", want: "[agrouter] relevance_floor (local "},
		{name: "bad enabled", local: "[model.alpha-big]\nenabled = nope\n", want: "[model.alpha-big] enabled"},
		{name: "bad cli enabled", local: "[cli.beta]\nenabled = maybe\n", want: "[cli.beta] enabled"},
		{name: "unknown agrouter key", local: "[agrouter]\ntimeuot = 1s\n", want: "[agrouter] timeuot"},
		{name: "unknown cli key", local: "[cli.alpha]\ncmd = x\n", want: "[cli.alpha] cmd"},
		{name: "unknown model key", local: "[model.alpha-big]\neffort = low\n", want: "[model.alpha-big] effort"},
		{name: "unknown effort key", local: "[effort.alpha.low]\nname = x\n", want: "[effort.alpha.low] name"},
		{name: "unknown section", local: "[typo.x]\ncli = alpha\n", want: "[typo.x]: unknown section"},
		{name: "removed args section", local: "[cli.alpha.args]\nprint = []\n",
			want: "[cli.alpha.args]: argument mappings were removed; set exec_args, model_args, effort_args and prompt_args in [cli.alpha]"},
		{name: "removed args section without cli", local: "[cli.gamma.args]\nprint = []\n", want: "[cli.gamma.args]: argument mappings were removed"},
		{name: "removed args section of a disabled cli", local: "[cli.alpha]\nenabled = false\n[cli.alpha.args]\nprint = []\n",
			want: "[cli.alpha.args]: argument mappings were removed"},
		{name: "availability without cli", local: "[cli.gamma.availability]\nchecks = x\n",
			want: "[cli.gamma.availability]: no [cli.gamma] section"},
		{name: "availability check without cli", local: "[cli.gamma.availability.usage]\ntransport = x\n",
			want: "[cli.gamma.availability.usage]: no [cli.gamma] section"},
		{name: "dotted cli name", local: "[cli.tool.v2]\ncommand = tool\n",
			want: `[cli.tool.v2]: unknown section (CLI names cannot contain ".")`},
		{name: "effort section name", local: "[effort.alpha]\ndescription = x\n", want: "[effort.<cli>.<level>]"},
		{name: "key outside section", local: "api_key = x\n", want: `key "api_key" outside any section`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := load(t, "", tc.local)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestLoad_RemovedQuestionKeys(t *testing.T) {
	tests := []struct {
		key  string
		hint string
	}{
		{"question", "removed; set routing_policy instead"},
		{"chunk_question", "removed; set routing_policy instead"},
		{"relevance", "no longer configurable; tune routing_policy instead"},
		{"complexity_question", "removed; set complexity_policy instead"},
		{"complexity_evidence", "no longer configurable; tune complexity_policy instead"},
	}
	for _, tc := range tests {
		for _, layer := range []string{LayerGlobal, LayerLocal} {
			t.Run(tc.key+"/"+layer, func(t *testing.T) {
				dir := t.TempDir()
				path := writeFile(t, dir, layer+"/config", "[agrouter]\n"+tc.key+" = Which option? sk-secret\n")
				src := Sources{Embedded: []byte(baseConfig)}
				if layer == LayerGlobal {
					src.GlobalPath = path
				} else {
					src.LocalPath = path
				}
				_, err := Load(src)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "[agrouter] "+tc.key+" ("+layer+" "+path+"): "+tc.hint)
				assert.NotContains(t, err.Error(), "sk-secret", "the old value is left out")
			})
		}
	}
}

func TestLoad_RemovedNativeMetadata(t *testing.T) {
	for _, key := range []string{"selection_flags", "config_flags", "selection_config_keys", "context_flags", "context_config_keys"} {
		t.Run(key, func(t *testing.T) {
			_, err := load(t, "", "[cli.alpha]\n"+key+" = obsolete\n")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "[cli.alpha] "+key)
		})
	}
}

func TestLoad_UnknownKeyHidesValue(t *testing.T) {
	_, err := load(t, "", "[agrouter]\napikey = sk-secret\n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "[agrouter] apikey (local ")
	assert.Contains(t, err.Error(), "unknown key")
	assert.NotContains(t, err.Error(), "sk-secret")
}

func TestLoad_ParseErrorHidesLine(t *testing.T) {
	tests := []struct {
		name  string
		local string
		want  string
	}{
		{name: "wrong delimiter", local: "[agrouter]\n\napi_key: sk-secret\n", want: "line 3: key-value delimiter not found"},
		{name: "empty key name", local: "[agrouter]\n= sk-secret\n", want: "line 2: empty key name"},
		{name: "unclosed section", local: "[agrouter sk-secret\n", want: "line 1: unclosed section"},
		{name: "unclosed key quote", local: "[agrouter]\n`sk-secret = x\n", want: "line 2: missing closing key quote"},
		{name: "unclosed value quote", local: "[agrouter]\napi_key = \"\"\"sk-secret\n", want: "malformed INI"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := load(t, "", tc.local)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "parse local ")
			assert.Contains(t, err.Error(), tc.want)
			assert.NotContains(t, err.Error(), "sk-secret")
		})
	}
}

func TestParseError_Unrecognized(t *testing.T) {
	require.EqualError(t, parseError([]byte("x\n"), errors.New("BOM: x")), "malformed INI")
	require.EqualError(t, parseError([]byte("a\n"), errors.New("unclosed section: b")), "unclosed section")
}

func TestLoad_UnreadableFile(t *testing.T) {
	dir := t.TempDir()
	// a directory where the file should be: exists, but cannot be read as a file on every OS
	global := filepath.Join(dir, "config")
	require.NoError(t, os.Mkdir(global, 0o750))
	_, err := Load(Sources{Embedded: []byte(baseConfig), GlobalPath: global})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read global config")
}

func TestLoad_ErrorNamesEmbeddedLayer(t *testing.T) {
	_, err := Load(Sources{Embedded: []byte("[agrouter]\ntimeout = x\n")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `[agrouter] timeout = "x" (embedded)`)
}

func TestLoad_InlineCommentKeptInValue(t *testing.T) {
	cfg, err := load(t, "", "[agrouter]\njev_model = jev-1 ; pinned\n")
	require.NoError(t, err)
	assert.Equal(t, "jev-1 ; pinned", cfg.Agrouter.JevModel, "IgnoreInlineComment: comments must be on their own lines")
}

func TestDefaultPaths(t *testing.T) {
	work := filepath.Join("some", "work")

	t.Run("config dir env", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(EnvConfigDir, dir)
		global, local, err := DefaultPaths(work)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(dir, "config"), global)
		assert.Equal(t, filepath.Join(work, ".agrouter", "config"), local)
	})

	t.Run("home", func(t *testing.T) {
		t.Setenv(EnvConfigDir, "")
		home, err := os.UserHomeDir()
		require.NoError(t, err)
		global, _, err := DefaultPaths(work)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(home, ".config", "agrouter", "config"), global)
	})

	t.Run("loads from config dir", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(EnvConfigDir, dir)
		writeFile(t, dir, "config", "[agrouter]\napi_key = from-dir\n")
		global, local, err := DefaultPaths(t.TempDir())
		require.NoError(t, err)
		cfg, err := Load(Sources{Embedded: []byte(baseConfig), GlobalPath: global, LocalPath: local})
		require.NoError(t, err)
		assert.Equal(t, "from-dir", cfg.Agrouter.APIKey)
		assert.Equal(t, LayerGlobal, cfg.Agrouter.APIKeySource)
	})
}

func TestResolveAPIKey(t *testing.T) {
	tests := []struct {
		name       string
		global     string
		local      string
		flag       string
		flagSet    bool
		env        string
		wantKey    string
		wantSource string
	}{
		{name: "embedded placeholder", wantKey: "", wantSource: LayerEmbedded},
		{name: "global", global: "g", wantKey: "g", wantSource: LayerGlobal},
		{name: "local over global", global: "g", local: "l", wantKey: "l", wantSource: LayerLocal},
		{name: "env over config", global: "g", local: "l", env: "e", wantKey: "e", wantSource: LayerEnv},
		{name: "empty env ignored", local: "l", env: "", wantKey: "l", wantSource: LayerLocal},
		{name: "flag over env", local: "l", env: "e", flag: "f", flagSet: true, wantKey: "f", wantSource: LayerFlag},
		{name: "explicit empty flag clears", local: "l", env: "e", flag: "", flagSet: true, wantKey: "", wantSource: LayerFlag},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var global, local string
			if tc.global != "" {
				global = "[agrouter]\napi_key = " + tc.global + "\n"
			}
			if tc.local != "" {
				local = "[agrouter]\napi_key = " + tc.local + "\n"
			}
			cfg, err := load(t, global, local)
			require.NoError(t, err)
			key, source := ResolveAPIKey(tc.flag, tc.flagSet, tc.env, cfg.Agrouter)
			assert.Equal(t, tc.wantKey, key)
			assert.Equal(t, tc.wantSource, source)
		})
	}
}

func TestLoad_Availability(t *testing.T) {
	tests := []struct {
		name   string
		global string
		local  string
		cli    string
		want   *Availability
	}{
		{name: "defaults", cli: "alpha", want: &Availability{
			Enabled: true, Timeout: DefaultAvailabilityTimeout, Unknown: UnknownAllow, QuotaErrorPrefix: DefaultQuotaErrorPrefix,
			Checks: []Check{{Name: "usage", Transport: TransportJSONHelper}},
		}},
		{name: "every key, whitespace in quoted values preserved", cli: "alpha",
			local: "[cli.alpha.availability]\nenabled = true\ntimeout = 2s\nunknown = block\n" +
				"quota_error_prefix = \"ERROR: agrouter: \"\nquota_error_message = \"  Limit reached\"\n" +
				"[cli.alpha.availability.usage]\ntransport = sdk-control-usage-v1\ncommand = alpha-status\n" +
				"args = [\"--json\", \"\"]\nmin_version = 2.1.0\n",
			want: &Availability{
				Enabled: true, Timeout: 2 * time.Second, Unknown: UnknownBlock,
				QuotaErrorPrefix: "ERROR: agrouter: ", QuotaErrorMessage: "  Limit reached",
				Checks: []Check{{Name: "usage", Transport: TransportSDKControlUsage, Command: "alpha-status",
					Args: []string{"--json", ""}, MinVersion: "2.1.0"}},
			}},
		{name: "a layer replaces the checks list, unlisted sections are ignored", cli: "alpha",
			global: "[cli.alpha.availability.limits]\ntransport = jsonrpc-ratelimits-v1\n",
			local:  "[cli.alpha.availability]\nchecks = limits, usage\n",
			want: &Availability{
				Enabled: true, Timeout: DefaultAvailabilityTimeout, Unknown: UnknownAllow, QuotaErrorPrefix: DefaultQuotaErrorPrefix,
				Checks: []Check{{Name: "limits", Transport: TransportJSONRPCRateLimit}, {Name: "usage", Transport: TransportJSONHelper}},
			}},
		{name: "disabled keeps its checks", cli: "alpha", local: "[cli.alpha.availability]\nenabled = false\n",
			want: &Availability{
				Timeout: DefaultAvailabilityTimeout, Unknown: UnknownAllow, QuotaErrorPrefix: DefaultQuotaErrorPrefix,
				Checks: []Check{{Name: "usage", Transport: TransportJSONHelper}},
			}},
		{name: "section without checks", cli: "beta", local: "[cli.beta.availability]\n",
			want: &Availability{Enabled: true, Timeout: DefaultAvailabilityTimeout, Unknown: UnknownAllow, QuotaErrorPrefix: DefaultQuotaErrorPrefix}},
		{name: "a third CLI entirely in the local layer", cli: "gamma",
			local: "[cli.gamma]\ncommand = gamma\nexec_args = []\nmodel_args = [\"-m\", \"{model}\"]\nprompt_args = [\"{prompt}\"]\n" +
				"[cli.gamma.availability]\nchecks = status\n[cli.gamma.availability.status]\ntransport = json-helper\n" +
				"command = gamma-status\nargs = [\"--json\"]\n",
			want: &Availability{
				Enabled: true, Timeout: DefaultAvailabilityTimeout, Unknown: UnknownAllow, QuotaErrorPrefix: DefaultQuotaErrorPrefix,
				Checks: []Check{{Name: "status", Transport: TransportJSONHelper, Command: "gamma-status", Args: []string{"--json"}}},
			}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := load(t, tc.global, tc.local)
			require.NoError(t, err)
			cli, ok := cfg.CLIByName(tc.cli)
			require.True(t, ok)
			assert.Equal(t, tc.want, cli.Availability)
		})
	}
}

func TestLoad_AvailabilityErrors(t *testing.T) {
	tests := []struct {
		name  string
		local string
		want  string
	}{
		{name: "unknown availability key", local: "[cli.alpha.availability]\nwait = 1m\n", want: "[cli.alpha.availability] wait"},
		{name: "unknown check key", local: "[cli.alpha.availability.usage]\nfield_path = a.b\n",
			want: "[cli.alpha.availability.usage] field_path"},
		{name: "bad timeout", local: "[cli.alpha.availability]\ntimeout = 5\n", want: `[cli.alpha.availability] timeout = "5"`},
		{name: "zero timeout", local: "[cli.alpha.availability]\ntimeout = 0s\n", want: "[cli.alpha.availability] timeout: must be positive"},
		{name: "bad enabled", local: "[cli.alpha.availability]\nenabled = sometimes\n", want: "[cli.alpha.availability] enabled"},
		{name: "bad unknown policy", local: "[cli.alpha.availability]\nunknown = warn\n",
			want: "[cli.alpha.availability] unknown: must be allow or block"},
		{name: "control character in prefix", local: "[cli.alpha.availability]\nquota_error_prefix = \"a\tb\"\n",
			want: "[cli.alpha.availability] quota_error_prefix: must not contain a newline or control character"},
		{name: "escape in message", local: "[cli.alpha.availability]\nquota_error_message = \"x\x1b[31m\"\n",
			want: "[cli.alpha.availability] quota_error_message: must not contain"},
		{name: "listed check without section", local: "[cli.alpha.availability]\nchecks = usage, limits\n",
			want: "[cli.alpha.availability] checks: no [cli.alpha.availability.limits] section"},
		{name: "missing transport", local: "[cli.alpha.availability.usage]\ntransport =\n",
			want: "[cli.alpha.availability.usage] transport: required"},
		{name: "unknown transport", local: "[cli.alpha.availability.usage]\ntransport = auth-status\n",
			want: `[cli.alpha.availability.usage] transport: unknown transport "auth-status"`},
		{name: "bad args", local: "[cli.alpha.availability.usage]\nargs = --json\n",
			want: "[cli.alpha.availability.usage] args"},
		{name: "bad min_version", local: "[cli.alpha.availability.usage]\nmin_version = v2.1\n",
			want: `[cli.alpha.availability.usage] min_version: "v2.1" is not a dotted numeric version`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := load(t, "", tc.local)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}
