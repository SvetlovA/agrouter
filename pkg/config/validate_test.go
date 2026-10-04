package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidate_BaseConfigPasses(t *testing.T) {
	cfg, err := load(t, "", "")
	require.NoError(t, err)
	require.NoError(t, cfg.Validate())
}

func TestValidate_Violations(t *testing.T) {
	tests := []struct {
		name  string
		local string
		want  []string // substrings of the error
	}{
		{
			name:  "empty command",
			local: "[cli.beta]\ncommand =\n",
			want:  []string{"[cli.beta] command: must not be empty"},
		},
		{
			name:  "missing print",
			local: "[cli.gamma]\ncommand = gamma\n[cli.gamma.args]\nmodel = [\"-m\", \"{model}\"]\nprompt = [\"{prompt}\"]\n",
			want:  []string{"[cli.gamma.args] print: required mapping is missing"},
		},
		{
			name:  "missing model",
			local: "[cli.gamma]\ncommand = gamma\n[cli.gamma.args]\nprint = [\"{prompt}\"]\n",
			want:  []string{"[cli.gamma.args] model: required mapping is missing"},
		},
		{
			name: "missing effort with efforts on a model",
			local: "[cli.gamma]\ncommand = gamma\n[cli.gamma.args]\nprint = [\"{prompt}\"]\nmodel = [\"-m\", \"{model}\"]\n" +
				"[model.gamma-one]\ncli = gamma\nname = gamma-one\nefforts = low\n",
			want: []string{"[cli.gamma.args] effort: required mapping is missing, [model.gamma-one] has efforts"},
		},
		{
			name:  "prompt in both print and prompt",
			local: "[cli.beta.args]\nprompt = [\"{prompt}\"]\n",
			want:  []string{"[cli.beta.args] print/prompt: {prompt} must appear exactly once across print and prompt, found 2"},
		},
		{
			name:  "prompt in neither",
			local: "[cli.beta.args]\nprint = [\"run\"]\n",
			want:  []string{"found 0"},
		},
		{
			name:  "prompt twice in print",
			local: "[cli.beta.args]\nprint = [\"{prompt}\", \"{prompt}\"]\n",
			want:  []string{"found 2"},
		},
		{
			name:  "prompt elsewhere",
			local: "[cli.beta.args]\nverbose = [\"{prompt}\"]\n",
			want:  []string{"[cli.beta.args] verbose: {prompt} is only allowed in print or prompt"},
		},
		{
			name:  "prompt inside a longer token",
			local: "[cli.beta.args]\nprint = [\"run\", \"--p={prompt}\"]\n",
			want:  []string{`[cli.beta.args] print: {prompt} must be a whole token, found in "--p={prompt}"`, "found 0"},
		},
		{
			name:  "value outside config.*",
			local: "[cli.beta.args]\nverbose = [\"{value}\"]\n",
			want:  []string{"[cli.beta.args] verbose: {value} is only allowed in config.* mappings"},
		},
		{
			name:  "unknown placeholder",
			local: "[cli.beta.args]\nmodel = [\"-m\", \"{mdl}\"]\n",
			want:  []string{`[cli.beta.args] model: unknown placeholder {mdl} in "{mdl}"`},
		},
		{
			name:  "at sign in model section",
			local: "[model.beta@two]\ncli = beta\nname = beta-two\n",
			want:  []string{"[model.beta@two]: '@' is reserved"},
		},
		{
			name:  "duplicate name",
			local: "[model.beta-two]\ncli = beta\nname = beta-one\n",
			want:  []string{`[model.beta-two] name: "beta-one" is already used by [model.beta-one]`},
		},
		{
			name:  "alias duplicates another model's name",
			local: "[model.beta-two]\ncli = beta\nname = beta-two\naliases = alpha-small-1\n",
			want:  []string{`[model.beta-two] aliases: "alpha-small-1" is already used by [model.alpha-small]`},
		},
		{
			name:  "duplicate alias",
			local: "[model.beta-two]\ncli = beta\nname = beta-two\naliases = large\n",
			want:  []string{`[model.beta-two] aliases: "large" is already used by [model.alpha-big]`},
		},
		{
			name:  "unknown cli",
			local: "[model.delta-one]\ncli = delta\nname = delta-one\n",
			want:  []string{`[model.delta-one] cli = "delta": no enabled [cli.delta] section`},
		},
		{
			name:  "missing name",
			local: "[model.beta-two]\ncli = beta\n",
			want:  []string{"[model.beta-two] name: required"},
		},
		{
			name:  "timeout not positive",
			local: "[agrouter]\ntimeout = 0s\n",
			want:  []string{"[agrouter] timeout = 0s: must be positive"},
		},
		{
			name:  "several violations reported together",
			local: "[agrouter]\ntimeout = 0s\n[model.beta-two]\ncli = beta\n",
			want:  []string{"timeout = 0s", "[model.beta-two] name: required"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := load(t, "", tc.local)
			require.Error(t, err)
			for _, w := range tc.want {
				assert.Contains(t, err.Error(), w)
			}
		})
	}
}

func TestValidate_Allowed(t *testing.T) {
	tests := []struct {
		name  string
		local string
	}{
		{name: "prompt in the prompt key", local: "[cli.beta.args]\nprint = [\"run\"]\nprompt = [\"{prompt}\"]\n"},
		{name: "value in config.*", local: "[cli.beta.args]\nconfig.project_doc = [\"-c\", \"{value}\"]\n"},
		{name: "model and effort inside tokens", local: "[cli.alpha.args]\neffort = [\"-c\", \"model_reasoning_effort=\\\"{effort}\\\"\", \"--m={model}\"]\n"},
		{name: "braces that are not placeholders", local: "[cli.beta.args]\nverbose = [\"--json={\\\"a\\\": 1}\", \"{}\"]\n"},
		{name: "no effort mapping when no model has efforts", local: ""},
		{name: "model of a disabled cli is dropped, not unknown", local: "[cli.beta]\nenabled = false\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := load(t, "", tc.local)
			require.NoError(t, err)
		})
	}
}
