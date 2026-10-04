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
question  = Which option? Look up ` + "`models`" + `; answer "well".

[cli.alpha]
command     = alpha
description = Alpha agent.

[cli.alpha.args]
print                  = ["-p", "{prompt}"]
model                  = ["--model", "{model}"]
effort                 = ["-c", "effort=\"{effort}\""]
output-format.text     = []
permission-mode.plan   = ["--mode", "plan"]

[cli.beta]
command = beta

[cli.beta.args]
print = ["run", "{prompt}"]
model = ["-m", "{model}"]

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
		APIKeySource: LayerEmbedded,
		JevModel:     "jev-latest",
		Timeout:      10 * time.Second,
		Question:     "Which option? Look up `models`; answer \"well\".",
	}, cfg.Agrouter)

	require.Len(t, cfg.CLIs, 2)
	alpha := cfg.CLIs[0]
	assert.Equal(t, "alpha", alpha.Name)
	assert.Equal(t, "alpha", alpha.Command)
	assert.Equal(t, "Alpha agent.", alpha.Description)
	assert.Equal(t, map[string][]string{
		"print":                {"-p", "{prompt}"},
		"model":                {"--model", "{model}"},
		"effort":               {"-c", `effort="{effort}"`},
		"output-format.text":   {},
		"permission-mode.plan": {"--mode", "plan"},
	}, alpha.Args)
	assert.NotNil(t, alpha.Args["output-format.text"], "[] must stay distinguishable from a missing key")
	_, has := alpha.Args["verbose"]
	assert.False(t, has)

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
	_, ok = cfg.CLIByName("gamma")
	assert.False(t, ok)
}

func TestLoad_Layering(t *testing.T) {
	global := `
[agrouter]
api_key = global-key
timeout = 20s

[cli.alpha.args]
print = ["--print", "{prompt}"]
verbose = ["-v"]

[model.alpha-big]
description = Global description.
`
	local := `
[agrouter]
timeout = 30s

[cli.alpha.args]
model = ["--use-model", "{model}"]

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

	alpha, _ := cfg.CLIByName("alpha")
	assert.Equal(t, []string{"--print", "{prompt}"}, alpha.Args["print"], "global overrides one args key")
	assert.Equal(t, []string{"--use-model", "{model}"}, alpha.Args["model"], "local overrides one args key")
	assert.Equal(t, []string{"-c", `effort="{effort}"`}, alpha.Args["effort"], "other args keys stay embedded")
	assert.Equal(t, []string{"-v"}, alpha.Args["verbose"], "a layer can add an args key")
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
		{name: "malformed JSON template", local: "[cli.alpha.args]\nprint = [\"-p\", \n",
			want: `[cli.alpha.args] print = "[\"-p\"," (local `},
		{name: "template not an array", local: "[cli.alpha.args]\nprint = -p {prompt}\n",
			want: "template must be a JSON array of strings"},
		{name: "template null", local: "[cli.alpha.args]\nprint = null\n",
			want: "template must be a JSON array of strings"},
		{name: "template of numbers", local: "[cli.alpha.args]\nprint = [1]\n",
			want: "[cli.alpha.args] print"},
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
		{name: "args without cli", local: "[cli.gamma.args]\nprint = []\n", want: "[cli.gamma.args]: no [cli.gamma] section"},
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
