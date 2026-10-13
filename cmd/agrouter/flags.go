package main

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/jessevdk/go-flags"

	"github.com/SvetlovA/agrouter/pkg/args"
)

// execToken selects exec mode, but only as the first token; the CLI to run follows it.
const execToken = "exec"

// legacyCLIEnv once restricted routing to one CLI; it is ignored now, with a warning when set.
const legacyCLIEnv = "AGROUTER_CLI"

// slashEscape is prepended to every token starting with "/" before go-flags sees it, and removed
// from every value it hands back. go-flags on Windows takes "/x" as an option, which would make a
// prompt such as "/review" or a value such as "/tmp/doc.md" an unknown flag.
const slashEscape = "\x00"

// helpText is the long description in --help, including the API key cautions.
const helpText = `agrouter asks TypeSafe's Jev which (cli, model, effort) should run a prompt.

  agrouter [OPTIONS] [PROMPT]
  agrouter exec CLI [OPTIONS] [PROMPT] [-- NATIVE ARGS...]

Without "exec" it prints that decision as JSON. With "exec" as the first token,
followed by the CLI to run, it routes within that CLI and runs it; the CLI's own
options go after "--" and are passed to it unchanged, in order. "--" is only
accepted with exec.
The default decision includes the selection, project complexity and Jev's
confidence at each routing stage (cli, model, effort). --verbose prints indented
JSON with every routing stage, chunk, candidate probability, complexity score,
argv, skipped argument and a copyable command. --verbose before "--" controls
agrouter's output only; after "--" it is the chosen CLI's own option. Exec always
logs a concise selection line to stderr.

The prompt is -p TEXT (also --prompt or --print), the positional argument, the
--prompt-file text and stdin, in that order, joined by blank lines; at least one
must be non-empty. Nothing after "--" is read as a prompt. The chosen CLI gets
the first three as its prompt argument and stdin replayed.
--doc files (the project's agent instructions and docs) describe the project to
Jev; they are read on every call but never passed to the chosen CLI.

The Jev API key comes from --jev-api-key, then TYPESAFE_API_KEY, then api_key
in the config. A key on the command line is visible to other local users in
process listings; prefer the environment variable or the global config with
user-only permissions, and never put it in a local .agrouter/config that may
be committed.`

// command is the outcome of parsing: a request to route, or --help / --version.
type command struct {
	req     *args.Request
	help    string // the help text when --help was given
	version bool
	verbose bool
}

// parseArgs parses agrouter's command line. The error is a usage error, for one "agrouter:" line
// and exit 2. Whether the exec CLI is configured and enabled is checked once the config is loaded.
func parseArgs(argv []string) (command, error) {
	req, argv, err := execOperand(argv)
	if err != nil {
		return command{}, err
	}

	flagArgv := argv
	separator := false
	if i := slices.Index(argv, "--"); i >= 0 {
		flagArgv, req.Raw, separator = argv[:i], append([]string{}, argv[i+1:]...), true
	}

	var seen flagCounts
	opts := newFlagSet(req, &seen)
	parser := flags.NewParser(opts, flags.HelpFlag)
	parser.Name = "agrouter"
	parser.Usage = "[exec CLI] [OPTIONS] [-p TEXT] [--prompt-file PATH] [--doc PATH]... [PROMPT] [-- NATIVE ARGS...]"
	parser.LongDescription = helpText

	rest, err := parser.ParseArgs(escapeSlashes(flagArgv))
	if err != nil {
		var flagsErr *flags.Error
		if errors.As(err, &flagsErr) && flagsErr.Type == flags.ErrHelp {
			return command{help: posixHelp(flagsErr.Message)}, nil
		}
		return command{}, fmt.Errorf("%s", unescape(err.Error()))
	}
	if opts.Version {
		return command{version: true}, nil
	}
	if err := seen.check(req, separator); err != nil {
		return command{}, err
	}

	switch len(rest) {
	case 0:
	case 1:
		req.Prompt = args.Optional{Value: unescape(rest[0]), Set: true}
	default:
		return command{}, errors.New("more than one positional argument: pass the prompt as one quoted argument")
	}

	if seen.cli && req.Mode != args.ModeExec {
		req.CLI = seen.cliValue
	}
	return command{req: req, verbose: opts.Verbose}, nil
}

// flagSet is agrouter's options. The func fields set the request as go-flags parses them.
type flagSet struct {
	CLI       func(string) `long:"cli" value-name:"NAME" unquote:"false" description:"restrict routing to one CLI; with exec, only the CLI after exec"`
	JevAPIKey func(string) `long:"jev-api-key" value-name:"KEY" unquote:"false" description:"TypeSafe API key for Jev; empty clears it (env TYPESAFE_API_KEY)"`
	Version   bool         `long:"version" description:"print agrouter's version and exit"`

	Prompt     func(string) `short:"p" long:"prompt" value-name:"TEXT" unquote:"false" description:"the prompt, before the positional prompt, --prompt-file and stdin"`
	Print      func(string) `long:"print" value-name:"TEXT" unquote:"false" description:"the same as -p TEXT"`
	PromptFile func(string) `long:"prompt-file" value-name:"PATH" unquote:"false" description:"read prompt text from a file, after -p and the positional prompt"`
	Doc        func(string) `long:"doc" value-name:"PATH" unquote:"false" description:"project doc for routing only, repeatable; never passed to the chosen CLI"`

	Model  func(string) `long:"model" value-name:"MODEL" unquote:"false" description:"use this model; Jev chooses only its effort"`
	Effort func(string) `long:"effort" value-name:"EFFORT" unquote:"false" description:"use this effort; Jev chooses among options with it"`

	Verbose bool `long:"verbose" description:"include full decision details (agrouter only; after -- it goes to the chosen CLI)"`
}

// flagCounts records options whose repetition or presence matters after parsing.
type flagCounts struct {
	cli        bool   // --cli was given, even empty
	cliValue   string // the last --cli value
	prompt     int    // -p, --prompt and --print together
	promptFile int
}

// newFlagSet binds the options to req, recording in seen what parseArgs checks afterwards.
func newFlagSet(req *args.Request, seen *flagCounts) *flagSet {
	promptFlag := func(v string) {
		req.PromptFlag = args.Optional{Value: unescape(v), Set: true}
		seen.prompt++
	}
	return &flagSet{
		CLI:    func(v string) { seen.cliValue, seen.cli = unescape(v), true },
		Prompt: promptFlag,
		Print:  promptFlag,
		PromptFile: func(v string) {
			req.PromptFile = args.Optional{Value: unescape(v), Set: true}
			seen.promptFile++
		},
		Doc: func(v string) { req.Docs = append(req.Docs, unescape(v)) },
		JevAPIKey: func(v string) {
			req.APIKey = args.Optional{Value: unescape(v), Set: true}
		},
		Model: func(v string) { req.Model, req.ModelSource = unescape(v), args.SourceFlag },
		Effort: func(v string) {
			v = unescape(v)
			req.Effort, req.EffortSource, req.EffortSpell = v, args.SourceFlag, "--effort "+v
		},
	}
}

// execOperand starts the request: exec mode with its CLI when argv starts with "exec CLI", decision
// mode otherwise. It returns the arguments after them.
func execOperand(argv []string) (*args.Request, []string, error) {
	req := &args.Request{}
	if len(argv) == 0 || argv[0] != execToken {
		return req, argv, nil
	}
	if len(argv) < 2 || argv[1] == "" || strings.HasPrefix(argv[1], "-") {
		return nil, nil, errors.New("exec needs the CLI to run: agrouter exec CLI [OPTIONS] [PROMPT] [-- NATIVE ARGS...]")
	}
	req.Mode, req.CLI = args.ModeExec, argv[1]
	return req, argv[2:], nil
}

// check reports the usage errors go-flags cannot see: repeated prompt options, "--" outside exec and
// a --cli other than the exec CLI.
func (c *flagCounts) check(req *args.Request, separator bool) error {
	switch {
	case c.prompt > 1:
		return errors.New("-p/--prompt/--print given more than once: pass the prompt as one argument")
	case c.promptFile > 1:
		return errors.New("--prompt-file given more than once")
	case separator && req.Mode != args.ModeExec:
		return errors.New(`"--" is only accepted with exec: agrouter exec CLI [OPTIONS] [PROMPT] [-- NATIVE ARGS...]`)
	case c.cli && req.Mode == args.ModeExec && c.cliValue != req.CLI:
		return fmt.Errorf("--cli %s conflicts with exec %s: drop --cli", c.cliValue, req.CLI)
	}
	return nil
}

// escapeSlashes returns argv with slashEscape prepended to every token starting with "/".
func escapeSlashes(argv []string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		if strings.HasPrefix(a, "/") {
			a = slashEscape + a
		}
		out[i] = a
	}
	return out
}

// windowsOption matches one option name as go-flags renders it in Windows help: "/p", "/prompt",
// "/cli:NAME", with the spaces that pad it to the description column.
var windowsOption = regexp.MustCompile(`/([^\s,:]+)(:\S+)?( *)`)

// posixHelp rewrites go-flags' Windows help, which lists "/cli:NAME" and "/p, /prompt:TEXT", to the
// spellings agrouter accepts, "--cli=NAME" and "-p, --prompt=TEXT", keeping the description column. The
// "/?" line is dropped: escapeSlashes makes "/?" a prompt. Help on other platforms is unchanged.
func posixHelp(help string) string {
	lines := strings.Split(help, "\n")
	out := lines[:0]
	for _, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		if !strings.HasPrefix(trimmed, "/") {
			out = append(out, line)
			continue
		}
		if strings.HasPrefix(trimmed, "/? ") {
			continue
		}
		indent := line[:len(line)-len(trimmed)]
		name, desc := trimmed, ""
		if i := strings.Index(trimmed, "  "); i >= 0 {
			name, desc = trimmed[:i], trimmed[i:]
		}
		renamed := windowsOption.ReplaceAllStringFunc(name, func(m string) string {
			sub := windowsOption.FindStringSubmatch(m)
			dash := "--"
			if len(sub[1]) == 1 {
				dash = "-"
			}
			return dash + sub[1] + strings.Replace(sub[2], ":", "=", 1) + sub[3]
		})
		// take the added width out of the padding so descriptions stay aligned
		pad := len(desc) - len(strings.TrimLeft(desc, " "))
		desc = desc[min(max(len(renamed)-len(name), 0), max(pad-1, 0)):]
		out = append(out, indent+renamed+desc)
	}
	return strings.Join(out, "\n")
}

// unescape reverses escapeSlashes for one value, or inside an error message.
func unescape(s string) string {
	return strings.ReplaceAll(s, slashEscape+"/", "/")
}
