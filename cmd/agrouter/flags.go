package main

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/jessevdk/go-flags"

	"github.com/SvetlovA/agrouter/pkg/args"
	"github.com/SvetlovA/agrouter/pkg/config"
)

// execToken selects exec mode, but only as the first token.
const execToken = "exec"

// cliEnv restricts routing to one CLI when --cli is not given.
const cliEnv = "AGROUTER_CLI"

// -c keys that are model and effort constraints unless --model / --effort is also given.
const (
	configModelKey  = "model"
	configEffortKey = "model_reasoning_effort"
)

// slashEscape is prepended to every token starting with "/" before go-flags sees it, and removed
// from every value it hands back. go-flags on Windows takes "/x" as an option, which would make a
// prompt such as "/review" or a value such as "/tmp/doc.md" an unknown flag.
const slashEscape = "\x00"

// helpText is the long description in --help, including the API key cautions from the design.
const helpText = `agrouter asks TypeSafe's Jev which (cli, model, effort) should run a prompt.
Without "exec" it prints that decision as one JSON line; with "exec" as the
first token it runs the chosen CLI with the arguments translated through its
config.

The prompt is the positional argument and/or stdin. Tokens after "--" are
passed to the chosen CLI unchanged, only with --cli.

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
}

// parseArgs parses agrouter's command line. getenv supplies AGROUTER_CLI. The error is a usage
// error, for one "agrouter:" line and exit 2.
func parseArgs(argv []string, getenv func(string) string) (command, error) {
	req := &args.Request{}
	if len(argv) > 0 && argv[0] == execToken {
		req.Mode = args.ModeExec
		argv = argv[1:]
	}

	flagArgv := argv
	if i := slices.Index(argv, "--"); i >= 0 {
		flagArgv, req.Raw = argv[:i], append([]string{}, argv[i+1:]...)
	}

	cliSet := false
	opts := newFlagSet(req, &cliSet)
	parser := flags.NewParser(opts, flags.HelpFlag)
	parser.Name = "agrouter"
	parser.Usage = "[exec] [OPTIONS] [PROMPT] [-- RAW ARGS...]"
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

	switch len(rest) {
	case 0:
	case 1:
		req.Prompt = args.Optional{Value: unescape(rest[0]), Set: true}
	default:
		return command{}, errors.New("more than one positional argument: pass the prompt as one quoted argument")
	}

	if !cliSet {
		req.CLI = getenv(cliEnv)
	}
	applyConfigConstraints(req)
	return command{req: req}, nil
}

// flagSet is agrouter's options. The func fields append to the request as go-flags parses them,
// which keeps the caller's order across different flags.
type flagSet struct {
	CLI       func(string) `long:"cli" value-name:"NAME" unquote:"false" description:"restrict routing to one CLI (env AGROUTER_CLI)"`
	JevAPIKey func(string) `long:"jev-api-key" value-name:"KEY" unquote:"false" description:"TypeSafe API key for Jev; empty clears it (env TYPESAFE_API_KEY)"`
	Print     bool         `short:"p" long:"print" description:"accepted for compatibility; the CLI's print mapping is always emitted"`
	Version   bool         `long:"version" description:"print agrouter's version and exit"`

	Model  func(string) `long:"model" value-name:"MODEL" unquote:"false" description:"use this model; Jev chooses only its effort"`
	Effort func(string) `long:"effort" value-name:"EFFORT" unquote:"false" description:"use this effort; Jev chooses among options with it"`

	PermissionMode  func(string) `long:"permission-mode" value-name:"MODE" unquote:"false" description:"permission mode, Claude names (mapped)"`
	SkipPermissions func()       `long:"dangerously-skip-permissions" description:"alias for --permission-mode bypassPermissions"`
	BypassApprovals func()       `long:"dangerously-bypass-approvals-and-sandbox" description:"alias for --permission-mode bypassPermissions"`
	OutputFormat    func(string) `long:"output-format" value-name:"FORMAT" unquote:"false" description:"text, json or stream-json (mapped)"`
	Verbose         func()       `long:"verbose" description:"verbose output (mapped)"`
	Sandbox         func(string) `long:"sandbox" value-name:"MODE" unquote:"false" description:"read-only, workspace-write or danger-full-access (mapped)"`
	Config          func(string) `short:"c" long:"config" value-name:"KEY=VALUE" unquote:"false" description:"config override, repeatable (mapped by key)"`
}

// newFlagSet binds the options to req. cliSet records whether --cli was given, even empty.
func newFlagSet(req *args.Request, cliSet *bool) *flagSet {
	valueKeyed := func(flag string) func(string) {
		return func(v string) {
			v = unescape(v)
			req.Args = append(req.Args, args.Arg{Spelling: "--" + flag + " " + v, Key: flag + "." + v})
		}
	}
	plain := func(flag, key string) func() {
		return func() { req.Args = append(req.Args, args.Arg{Spelling: "--" + flag, Key: key}) }
	}
	const bypassKey = "permission-mode.bypassPermissions"

	return &flagSet{
		CLI: func(v string) { req.CLI, *cliSet = unescape(v), true },
		JevAPIKey: func(v string) {
			req.APIKey = args.Optional{Value: unescape(v), Set: true}
		},
		Model: func(v string) { req.Model, req.ModelSource = unescape(v), args.SourceFlag },
		Effort: func(v string) {
			v = unescape(v)
			req.Effort, req.EffortSource, req.EffortSpell = v, args.SourceFlag, "--effort "+v
		},
		PermissionMode:  valueKeyed("permission-mode"),
		SkipPermissions: plain("dangerously-skip-permissions", bypassKey),
		BypassApprovals: plain("dangerously-bypass-approvals-and-sandbox", bypassKey),
		OutputFormat:    valueKeyed("output-format"),
		Verbose:         plain("verbose", "verbose"),
		Sandbox:         valueKeyed("sandbox"),
		Config: func(v string) {
			v = unescape(v)
			key, _, _ := strings.Cut(v, "=")
			req.Args = append(req.Args, args.Arg{Spelling: "-c " + v, Key: config.ConfigKeyPrefix + key, Value: v})
		},
	}
}

// applyConfigConstraints turns -c model=... and -c model_reasoning_effort=... into constraints when
// --model / --effort is not given; otherwise they stay ordinary config.* arguments. The last one wins.
func applyConfigConstraints(req *args.Request) {
	kept := req.Args[:0]
	for _, a := range req.Args {
		_, value, _ := strings.Cut(a.Value, "=")
		switch {
		case a.Key == config.ConfigKeyPrefix+configModelKey && req.ModelSource != args.SourceFlag:
			req.Model, req.ModelSource = unquoteTOML(value), args.SourceConfig
		case a.Key == config.ConfigKeyPrefix+configEffortKey && req.EffortSource != args.SourceFlag:
			req.Effort, req.EffortSource, req.EffortSpell = unquoteTOML(value), args.SourceConfig, a.Spelling
		default:
			kept = append(kept, a)
		}
	}
	req.Args = kept
}

// unquoteTOML removes TOML string quotes from a -c value: "x" (with escapes) or 'x' (literal).
func unquoteTOML(v string) string {
	if len(v) < 2 {
		return v
	}
	switch {
	case v[0] == '"' && v[len(v)-1] == '"':
		if s, err := strconv.Unquote(v); err == nil {
			return s
		}
		return v[1 : len(v)-1]
	case v[0] == '\'' && v[len(v)-1] == '\'':
		return v[1 : len(v)-1]
	}
	return v
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

// windowsOption matches one option name as go-flags renders it in Windows help: "/p", "/print",
// "/cli:NAME", with the spaces that pad it to the description column.
var windowsOption = regexp.MustCompile(`/([^\s,:]+)(:\S+)?( *)`)

// posixHelp rewrites go-flags' Windows help, which lists "/cli:NAME" and "/p, /print", to the
// spellings agrouter accepts, "--cli=NAME" and "-p, --print", keeping the description column. The
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
