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
			name:  "empty policies",
			local: "[agrouter]\nrouting_policy =\ncomplexity_policy = \" \"\n",
			want: []string{"[agrouter] routing_policy: must not be empty",
				"[agrouter] complexity_policy: must not be empty"},
		},
		{
			name:  "empty command",
			local: "[cli.beta]\ncommand =\n",
			want:  []string{"[cli.beta] command: must not be empty"},
		},
		{
			name:  "missing exec_args",
			local: "[cli.gamma]\ncommand = gamma\nmodel_args = [\"-m\", \"{model}\"]\nprompt_args = [\"{prompt}\"]\n",
			want:  []string{"[cli.gamma] exec_args: required"},
		},
		{
			name:  "missing model_args",
			local: "[cli.gamma]\ncommand = gamma\nexec_args = []\nprompt_args = [\"{prompt}\"]\n",
			want:  []string{"[cli.gamma] model_args: required"},
		},
		{
			name:  "missing prompt_args",
			local: "[cli.gamma]\ncommand = gamma\nexec_args = []\nmodel_args = [\"-m\", \"{model}\"]\n",
			want:  []string{"[cli.gamma] prompt_args: required"},
		},
		{
			name: "missing effort_args with efforts on a model",
			local: "[cli.gamma]\ncommand = gamma\nexec_args = []\nmodel_args = [\"-m\", \"{model}\"]\nprompt_args = [\"{prompt}\"]\n" +
				"[model.gamma-one]\ncli = gamma\nname = gamma-one\nefforts = low\n",
			want: []string{"[cli.gamma] effort_args: required"},
		},
		{
			name:  "model_args without its placeholder",
			local: "[cli.beta]\nmodel_args = [\"-m\"]\n",
			want:  []string{"[cli.beta] model_args: must contain {model}"},
		},
		{
			name:  "effort_args without its placeholder",
			local: "[cli.alpha]\neffort_args = [\"--effort\"]\n",
			want:  []string{"[cli.alpha] effort_args: must contain {effort}"},
		},
		{
			name:  "prompt in exec_args",
			local: "[cli.beta]\nexec_args = [\"run\", \"{prompt}\"]\n",
			want:  []string{"[cli.beta] exec_args: {prompt} is not allowed in exec_args"},
		},
		{
			name:  "effort in model_args",
			local: "[cli.beta]\nmodel_args = [\"-m\", \"{model}\", \"{effort}\"]\n",
			want:  []string{"[cli.beta] model_args: {effort} is not allowed in model_args"},
		},
		{
			name:  "model in effort_args",
			local: "[cli.alpha]\neffort_args = [\"--effort\", \"{effort}\", \"--model={model}\"]\n",
			want:  []string{"[cli.alpha] effort_args: {model} is not allowed in effort_args"},
		},
		{
			name:  "model in prompt_args",
			local: "[cli.beta]\nprompt_args = [\"{model}\", \"{prompt}\"]\n",
			want:  []string{"[cli.beta] prompt_args: {model} is not allowed in prompt_args"},
		},
		{
			name:  "prompt not last",
			local: "[cli.beta]\nprompt_args = [\"--\", \"{prompt}\", \"--verbose\"]\n",
			want:  []string{"[cli.beta] prompt_args: must hold {prompt} once, as its last element"},
		},
		{
			name:  "prompt twice",
			local: "[cli.beta]\nprompt_args = [\"{prompt}\", \"--\", \"{prompt}\"]\n",
			want:  []string{"must hold {prompt} once"},
		},
		{
			name:  "prompt_args without the prompt",
			local: "[cli.beta]\nprompt_args = [\"--task\"]\n",
			want:  []string{"[cli.beta] prompt_args: must contain {prompt}", "must hold {prompt} once"},
		},
		{
			name:  "empty prompt_args",
			local: "[cli.beta]\nprompt_args = []\n",
			want:  []string{"[cli.beta] prompt_args: must contain {prompt}"},
		},
		{
			name:  "prompt inside a longer token",
			local: "[cli.beta]\nprompt_args = [\"--\", \"--p={prompt}\"]\n",
			want:  []string{`[cli.beta] prompt_args: {prompt} must be a whole token, found in "--p={prompt}"`, "must hold {prompt} once"},
		},
		{
			name:  "removed value placeholder",
			local: "[cli.beta]\nexec_args = [\"{value}\"]\n",
			want:  []string{`[cli.beta] exec_args: unknown placeholder {value} in "{value}"`},
		},
		{
			name:  "unknown placeholder",
			local: "[cli.beta]\nmodel_args = [\"-m\", \"{model}\", \"{mdl}\"]\n",
			want:  []string{`[cli.beta] model_args: unknown placeholder {mdl} in "{mdl}"`},
		},
		{
			name:  "selection flag not a flag",
			local: "[cli.beta]\nselection_flags = model, -, ---m, --m=x\n",
			want: []string{`[cli.beta] selection_flags: "model" is not a flag spelling`, `"-" is not a flag spelling`,
				`"---m" is not a flag spelling`, `"--m=x" is not a flag spelling`},
		},
		{
			name:  "context flag not a flag",
			local: "[cli.beta]\ncontext_flags = profile\n",
			want:  []string{`[cli.beta] context_flags: "profile" is not a flag spelling`},
		},
		{
			name:  "config flag not a flag",
			local: "[cli.beta]\nconfig_flags = c\nselection_config_keys = model\n",
			want:  []string{`[cli.beta] config_flags: "c" is not a flag spelling`},
		},
		{
			name:  "selection config key with a value",
			local: "[cli.alpha]\nselection_config_keys = model=x\n",
			want:  []string{`[cli.alpha] selection_config_keys: "model=x" is not a config key`},
		},
		{
			name:  "context config key with a value",
			local: "[cli.alpha]\ncontext_config_keys = profile=x\n",
			want:  []string{`[cli.alpha] context_config_keys: "profile=x" is not a config key`},
		},
		{
			name:  "context config keys without config flags",
			local: "[cli.beta]\ncontext_config_keys = profile\n",
			want:  []string{"[cli.beta] context_config_keys: needs config_flags"},
		},
		{
			name:  "selection config keys without config flags",
			local: "[cli.beta]\nselection_config_keys = model\n",
			want:  []string{"[cli.beta] selection_config_keys: set together with config_flags"},
		},
		{
			name:  "config flags without selection config keys",
			local: "[cli.beta]\nconfig_flags = -c\n",
			want:  []string{"[cli.beta] selection_config_keys: set together with config_flags"},
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
		{name: "options before the end of options", local: "[cli.beta]\nprompt_args = [\"--task\", \"--\", \"{prompt}\"]\n"},
		{name: "prompt without the end of options", local: "[cli.beta]\nprompt_args = [\"{prompt}\"]\n"},
		{name: "model and effort inside tokens", local: "[cli.alpha]\neffort_args = [\"-c\", \"model_reasoning_effort=\\\"{effort}\\\"\"]\nmodel_args = [\"--m={model}\"]\n"},
		{name: "braces that are not placeholders", local: "[cli.beta]\nexec_args = [\"--json={\\\"a\\\": 1}\", \"{}\"]\n"},
		{name: "empty exec_args", local: "[cli.alpha]\nexec_args = []\n"},
		{name: "a third cli configured in a local layer", local: "[cli.gamma]\ncommand = gamma\nexec_args = [\"run\"]\n" +
			"model_args = [\"--model\", \"{model}\"]\neffort_args = [\"--effort\", \"{effort}\"]\nprompt_args = [\"--\", \"{prompt}\"]\n" +
			"selection_flags = --model, -m\n[model.gamma-one]\ncli = gamma\nname = gamma-one\nefforts = low, high\n"},
		{name: "no effort_args when no model has efforts", local: ""},
		{name: "model of a disabled cli is dropped, not unknown", local: "[cli.beta]\nenabled = false\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := load(t, "", tc.local)
			require.NoError(t, err)
		})
	}
}
