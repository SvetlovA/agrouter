package args

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/SvetlovA/agrouter/pkg/config"
)

// CheckNative rejects native tokens that would select the model or effort behind routing's back: a
// configured selection flag (bare, --flag=value, or a short flag with its value attached) or a config
// flag whose key is a selection key (-c key=..., -ckey=..., --config=key=...). Only tokens before the
// child's own "--" are scanned. The error names the spelling, never its value. There is no option
// arity table, so a value that is literally a reserved spelling is rejected too.
func CheckNative(cli config.CLI, raw []string) error {
	for i := 0; i < len(raw); i++ {
		tok := raw[i]
		if tok == config.EndOfOptions {
			return nil
		}
		for _, f := range cli.SelectionFlags {
			if _, ok := flagValue(tok, f); ok {
				return selectionError(cli.Name, f)
			}
		}
		for _, f := range cli.ConfigFlags {
			v, ok := flagValue(tok, f)
			if !ok {
				continue
			}
			if v == nil {
				if i+1 >= len(raw) {
					break
				}
				i++
				v = &raw[i]
			}
			if key := configKey(*v); slices.Contains(cli.SelectionConfigKeys, key) {
				return selectionError(cli.Name, f+" "+key)
			}
		}
	}
	return nil
}

// ContextFlag returns the first configured context spelling among the native tokens before the
// child's own "--": a flag that switches the child's provider, profile or settings, so a probe of the
// default context says nothing about the run. Like CheckNative it has no arity table.
func ContextFlag(cli config.CLI, raw []string) (string, bool) {
	for _, tok := range raw {
		if tok == config.EndOfOptions {
			break
		}
		for _, f := range cli.ContextFlags {
			if _, ok := flagValue(tok, f); ok {
				return f, true
			}
		}
	}
	return "", false
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

// configKey returns the key of a key=value config argument, unquoted and without spaces.
func configKey(v string) string {
	key, _, ok := strings.Cut(v, "=")
	if !ok {
		return ""
	}
	key = strings.TrimSpace(key)
	if len(key) >= 2 && (key[0] == '"' || key[0] == '\'') && key[len(key)-1] == key[0] {
		key = key[1 : len(key)-1]
	}
	return key
}

func selectionError(cli, spelling string) error {
	return fmt.Errorf("native argument %s after \"--\" selects the %s model or effort: use agrouter's --model/--effort instead",
		spelling, cli)
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
