package args

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckNative(t *testing.T) {
	const secret = "secret-model-value"
	tests := []struct {
		name string
		cli  string
		raw  []string
		hit  string // the spelling the error names, empty for no error
	}{
		{name: "no native tokens", cli: "beta"},
		{name: "unrelated options", cli: "beta", raw: []string{"--sandbox", "read-only", "-c", "features.x=true", "--json"}},
		{name: "bare long flag", cli: "beta", raw: []string{"--model", secret}, hit: "--model"},
		{name: "long flag with =", cli: "beta", raw: []string{"--model=" + secret}, hit: "--model"},
		{name: "bare short flag", cli: "beta", raw: []string{"-m", secret}, hit: "-m"},
		{name: "attached short flag", cli: "beta", raw: []string{"-m" + secret}, hit: "-m"},
		{name: "short flag with =", cli: "beta", raw: []string{"-m=" + secret}, hit: "-m"},
		{name: "effort flag", cli: "alpha", raw: []string{"--json", "--effort", secret}, hit: "--effort"},
		{name: "longer flag sharing a prefix", cli: "beta", raw: []string{"--model-provider", "x", "--models"}},
		{name: "config flag with a selection key", cli: "beta", raw: []string{"-c", "model=" + secret}, hit: "-c model"},
		{name: "attached config flag", cli: "beta", raw: []string{"-cmodel_reasoning_effort=" + secret}, hit: "-c model_reasoning_effort"},
		{name: "short config flag with =", cli: "beta", raw: []string{"-c=model=" + secret}, hit: "-c model"},
		{name: "long config flag with =", cli: "beta", raw: []string{"--config=model=" + secret}, hit: "--config model"},
		{name: "long config flag, separate value", cli: "beta", raw: []string{"--config", "model=" + secret}, hit: "--config model"},
		{name: "TOML-quoted value", cli: "beta", raw: []string{"-c", `model_reasoning_effort="` + secret + `"`},
			hit: "-c model_reasoning_effort"},
		{name: "quoted key and spaces", cli: "beta", raw: []string{"-c", ` "model" = "` + secret + `"`}, hit: "-c model"},
		{name: "dotted key is not a selection key", cli: "beta", raw: []string{"-c", "profiles.x.model=" + secret}},
		{name: "config value without =", cli: "beta", raw: []string{"-c", "model"}},
		{name: "config flag at the end", cli: "beta", raw: []string{"-c"}},
		{name: "config flags not configured", cli: "alpha", raw: []string{"-c", "model=" + secret}},
		{name: "after the child's --", cli: "beta", raw: []string{"--json", "--", "--model", secret, "-c", "model=x"}},
		{name: "a value that is a reserved spelling", cli: "beta", raw: []string{"--add-dir", "-m"}, hit: "-m"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckNative(fixtureCLI(t, tc.cli), tc.raw)
			if tc.hit == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), "native argument "+tc.hit+" after \"--\" selects the "+tc.cli+" model or effort")
			assert.Contains(t, err.Error(), "--model/--effort")
			assert.NotContains(t, err.Error(), secret, "the value is never echoed")
		})
	}
}

func TestCheckSeparator(t *testing.T) {
	alpha := fixtureCLI(t, "alpha")
	noSeparator := alpha
	noSeparator.PromptArgs = []string{"{prompt}"}
	withPrompt := Request{Prompt: Optional{Value: "task", Set: true}}
	tests := []struct {
		name    string
		req     Request
		raw     []string
		wantErr bool
	}{
		{name: "prompt and a native --", req: withPrompt, raw: []string{"-x", "--", "y"}, wantErr: true},
		{name: "prompt from the file", req: Request{FileText: "task"}, raw: []string{"--"}, wantErr: true},
		{name: "prompt without native --", req: withPrompt, raw: []string{"-x"}},
		{name: "no command-line prompt: stdin only", raw: []string{"--"}},
		{name: "empty prompt sources", req: Request{Prompt: Optional{Set: true}}, raw: []string{"--"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.req.Raw = tc.raw
			err := CheckSeparator(alpha, &tc.req)
			if !tc.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), "pass the prompt on stdin, or drop the extra \"--\"")
			assert.NotContains(t, err.Error(), "task")
		})
	}
	t.Run("prompt_args without --", func(t *testing.T) {
		req := withPrompt
		req.Raw = []string{"--"}
		require.NoError(t, CheckSeparator(noSeparator, &req))
	})
}
