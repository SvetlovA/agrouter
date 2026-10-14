package args

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNativeOptions(t *testing.T) {
	tests := []struct {
		name      string
		raw, want []string
	}{
		{name: "empty", want: []string{}},
		{name: "model and effort", raw: []string{"--model", "native", "--effort", "native", "--json"}, want: []string{"--json"}},
		{name: "equals values", raw: []string{"--model=native", "--effort=native", "--json"}, want: []string{"--json"}},
		{name: "short forms", raw: []string{"-m", "a", "-mb", "-m=c", "-m", ""}, want: []string{}},
		{name: "prefixes are unrelated", raw: []string{"--models", "--model-provider", "x", "--efforts"}, want: []string{"--models", "--model-provider", "x", "--efforts"}},
		{name: "settings and config unchanged", raw: []string{"--profile", "work", "-c", `model="native"`, "--config=reasoning=native"}, want: []string{"--profile", "work", "-c", `model="native"`, "--config=reasoning=native"}},
		{name: "empty values and repeats", raw: []string{"", "two words", "-v", "-v"}, want: []string{"", "two words", "-v", "-v"}},
		{name: "child separator stops scanning", raw: []string{"-m", "native", "--", "-m", "text", "--effort=x"}, want: []string{"--", "-m", "text", "--effort=x"}},
		{name: "dangling selection", raw: []string{"--json", "-m"}, want: []string{"--json"}},
		{name: "dangling model before another option", raw: []string{"-m", "--json"}, want: []string{"--json"}},
		{name: "selection before separator", raw: []string{"--effort", "--", "--model", "text"}, want: []string{"--", "--model", "text"}},
		{name: "selection before another option", raw: []string{"--model", "--effort", "native", "--json"}, want: []string{"--json"}},
		{name: "unrelated attached flag value", raw: []string{"--add-dir=-m"}, want: []string{"--add-dir=-m"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			original := append([]string(nil), tc.raw...)
			assert.Equal(t, tc.want, nativeOptions(tc.raw))
			assert.Equal(t, original, tc.raw, "the caller's native slice stays unchanged")
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
