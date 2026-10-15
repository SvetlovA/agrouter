package args

import (
	"errors"
	"slices"
	"strings"

	"github.com/SvetlovA/agrouter/pkg/config"
)

// nativeOptions removes agrouter's own selection spellings from the child's options. Other tokens
// stay unchanged; the child's "--" ends the scan. Selections are removed even when routing delegates
// a value, so a native flag cannot silently replace the reported child default.
func nativeOptions(raw []string) []string {
	out := make([]string, 0, len(raw))
	for i := 0; i < len(raw); i++ {
		if raw[i] == config.EndOfOptions {
			return append(out, raw[i:]...)
		}
		matched := false
		for _, f := range []string{"--model", "-m", "--effort"} {
			if value, ok := flagValue(raw[i], f); ok {
				if value == nil && i+1 < len(raw) && !strings.HasPrefix(raw[i+1], "-") {
					i++
				}
				matched = true
				break
			}
		}
		if !matched {
			out = append(out, raw[i])
		}
	}
	return out
}

// flagValue reports whether tok is flag f, and returns its attached value: nil for a bare flag,
// the text after "=" for --flag=value, or the text after a short flag such as -mvalue or -m=value.
func flagValue(tok, f string) (*string, bool) {
	if tok == f {
		return nil, true
	}
	rest, ok := strings.CutPrefix(tok, f)
	if !ok {
		return nil, false
	}
	if short := len(f) == 2 && f[1] != '-'; short {
		rest = strings.TrimPrefix(rest, "=")
		return &rest, true
	}
	if v, ok := strings.CutPrefix(rest, "="); ok {
		return &v, true
	}
	return nil, false
}

// CheckSeparator rejects a command-line prompt that prompt_args puts after "--" when the native tokens
// already hold "--": everything after the first one is positional for the child, so the prompt's "--"
// would reach it as prompt text.
func CheckSeparator(cli config.CLI, req *Request) error {
	if req.ArgvPrompt() == "" || !slices.Contains(cli.PromptArgs, config.EndOfOptions) ||
		!slices.Contains(req.Raw, config.EndOfOptions) {
		return nil
	}
	return errors.New(`the native arguments after "--" hold another "--", and the prompt would follow a second one: ` +
		`pass the prompt on stdin, or drop the extra "--"`)
}
