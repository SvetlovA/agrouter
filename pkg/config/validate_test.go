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
			name:  "empty url",
			local: "[classifier]\nurl =\n",
			want:  []string{"[classifier] url: must not be empty"},
		},
		{
			name:  "blank url",
			local: "[classifier]\nurl = \" \"\n",
			want:  []string{"[classifier] url: must not be empty"},
		},
		{
			name:  "empty model",
			local: "[classifier]\nmodel =\n",
			want:  []string{"[classifier] model: must not be empty"},
		},
		{
			name:  "timeout not positive",
			local: "[agrouter]\ntimeout = 0s\n",
			want:  []string{"[agrouter] timeout = 0s: must be positive"},
		},
		{
			name:  "request_tokens zero",
			local: "[classifier]\nrequest_tokens = 0\n",
			want:  []string{"[classifier] request_tokens = 0: must be positive"},
		},
		{
			name:  "request_tokens negative",
			local: "[classifier]\nrequest_tokens = -5\n",
			want:  []string{"[classifier] request_tokens = -5: must be positive"},
		},
		{
			name:  "bytes_per_token zero",
			local: "[classifier]\nbytes_per_token = 0\n",
			want:  []string{"[classifier] bytes_per_token = 0: must be a finite number greater than zero"},
		},
		{
			name:  "bytes_per_token negative",
			local: "[classifier]\nbytes_per_token = -2.5\n",
			want:  []string{"[classifier] bytes_per_token = -2.5: must be a finite number greater than zero"},
		},
		{
			name:  "bytes_per_token NaN",
			local: "[classifier]\nbytes_per_token = NaN\n",
			want:  []string{"[classifier] bytes_per_token = NaN: must be a finite number greater than zero"},
		},
		{
			name:  "bytes_per_token positive infinity",
			local: "[classifier]\nbytes_per_token = +Inf\n",
			want:  []string{"[classifier] bytes_per_token = +Inf: must be a finite number greater than zero"},
		},
		{
			name:  "bytes_per_token negative infinity",
			local: "[classifier]\nbytes_per_token = -Inf\n",
			want:  []string{"[classifier] bytes_per_token = -Inf: must be a finite number greater than zero"},
		},
		{
			name:  "blank log_dir",
			local: "[agrouter]\nlog_dir =\n",
			want:  []string{"[agrouter] log_dir: must not be empty"},
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
			"[model.gamma-one]\ncli = gamma\nname = gamma-one\nefforts = low, high\n"},
		{name: "no effort_args when no model has efforts", local: ""},
		{name: "any url value", local: "[classifier]\nurl = not a url\n"},
		{name: "api_key left empty", local: "[classifier]\napi_key =\n"},
		{name: "fractional ratio", local: "[classifier]\nbytes_per_token = 2.5\n"},
		{name: "very small finite ratio", local: "[classifier]\nbytes_per_token = 1e-300\n"},
		{name: "very large finite ratio", local: "[classifier]\nbytes_per_token = 1e300\n"},
		{name: "huge request_tokens", local: "[classifier]\nrequest_tokens = 9000000000\n"},
		{name: "model of a disabled cli is dropped, not unknown", local: "[cli.beta]\nenabled = false\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := load(t, "", tc.local)
			require.NoError(t, err)
		})
	}
}

func TestValidate_ClassifierSectionMissing(t *testing.T) {
	_, err := Load(Sources{Embedded: []byte("[agrouter]\ntimeout = 10s\nrouting_policy = cheap\ncomplexity_policy = whole\nlog_dir = logs\n")})
	require.Error(t, err)
	for _, want := range []string{
		"[classifier] url: must not be empty",
		"[classifier] model: must not be empty",
		"[classifier] request_tokens = 0: must be positive",
		"[classifier] bytes_per_token = 0: must be a finite number greater than zero",
	} {
		assert.Contains(t, err.Error(), want)
	}
}
