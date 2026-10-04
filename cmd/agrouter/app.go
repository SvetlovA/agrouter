package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/SvetlovA/agrouter/pkg/args"
	"github.com/SvetlovA/agrouter/pkg/catalog"
	"github.com/SvetlovA/agrouter/pkg/config"
	"github.com/SvetlovA/agrouter/pkg/config/defaults"
	"github.com/SvetlovA/agrouter/pkg/jev"
	"github.com/SvetlovA/agrouter/pkg/prompt"
	"github.com/SvetlovA/agrouter/pkg/router"
	"github.com/SvetlovA/agrouter/pkg/runner"
)

// Exit codes of agrouter's own outcomes; a child's code is passed through as is.
const (
	exitOK    = 0
	exitUsage = 2 // arguments, prompt, config, or no decision with the CLI unknown
)

// app is one agrouter invocation with its dependencies, so tests can replace the Jev client and runner.
type app struct {
	stdin          io.Reader // nil: no stdin
	stdout, stderr io.Writer
	getenv         func(string) string
	workDir        string
	embedded       []byte
	newJev         func(key string) router.JevClient
	runner         CommandRunner
	debug          *debugLog // nil unless AGROUTER_DEBUG=1
}

// newApp returns an app wired to the real process environment.
func newApp(stdin io.Reader, stdout, stderr io.Writer) *app {
	wd, _ := os.Getwd() // an unresolvable directory fails in config.DefaultPaths or ReadMentions
	return &app{
		stdin:    stdinReader(stdin),
		stdout:   stdout,
		stderr:   stderr,
		getenv:   os.Getenv,
		workDir:  wd,
		embedded: defaults.Config,
		newJev:   func(key string) router.JevClient { return jev.New(key) },
		runner:   runner.Runner{},
	}
}

// stdinReader returns the prompt's stdin: nil for a terminal, which agrouter does not read.
func stdinReader(r io.Reader) io.Reader {
	if f, ok := r.(*os.File); ok {
		return prompt.StdinOf(f)
	}
	return r
}

// selectionJSON is the selected CLI, model and effort; unset values are null.
type selectionJSON struct {
	CLI    string  `json:"cli"`
	Model  *string `json:"model"`
	Effort *string `json:"effort"`
}

// decisionJSON adds the argv and skipped arguments for decision mode.
type decisionJSON struct {
	selectionJSON
	Argv    []string `json:"argv"`
	Skipped []string `json:"skipped"`
}

// run executes the pipeline: parse, config, catalog, then under the routing deadline capture, route
// and build the argv; then print the decision or run the child. It returns the exit code.
func (a *app) run(argv []string) int {
	cmd, err := parseArgs(argv, a.getenv)
	if err != nil {
		return a.fail(err)
	}
	if cmd.help != "" {
		fmt.Fprintln(a.stdout, cmd.help)
		return exitOK
	}
	if cmd.version {
		fmt.Fprintln(a.stdout, version())
		return exitOK
	}
	req := cmd.req
	a.debug = newDebugLog(a.getenv(envDebug), a.stderr)

	cfg, cat, rt, err := a.setup(req)
	if err != nil {
		return a.fail(err)
	}

	d, err := a.route(cfg, cat, rt, req)
	if err != nil {
		a.debug.failed(err)
		return a.fail(err)
	}
	a.debug.decision(d.Decision)

	cli, _ := cfg.CLIByName(d.CLI)
	res := args.Build(cli, req, d.Choice())
	for _, s := range res.Skipped {
		fmt.Fprintln(a.stderr, s.Warning)
	}
	a.debug.command(res)

	if req.Mode == args.ModeDecision {
		return a.printDecision(d, res)
	}
	// selection logging is best effort, like warnings; it never includes the prompt or raw argv
	enc := json.NewEncoder(a.stderr)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(selection(d.Decision))
	return a.exec(res, d.captured)
}

// setup loads and validates the config, derives the catalog and builds the router with the resolved
// Jev key. Every error is a config error.
func (a *app) setup(req *args.Request) (*config.Config, *catalog.Catalog, *router.Router, error) {
	global, local, err := config.DefaultPaths(a.workDir)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("config: %w", err)
	}
	cfg, err := config.Load(config.Sources{Embedded: a.embedded, GlobalPath: global, LocalPath: local})
	if err != nil {
		return nil, nil, nil, configError{err}
	}
	cat, err := catalog.Build(cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	key, source := config.ResolveAPIKey(req.APIKey.Value, req.APIKey.Set, a.getenv(config.EnvAPIKey), cfg.Agrouter)
	a.debug.apiKey(key, source)
	rt, err := router.New(cfg, cat, a.newJev(key), router.EncodingCompact)
	if err != nil {
		return nil, nil, nil, err
	}
	return cfg, cat, rt, nil
}

// routed is the decision together with the captured prompt, whose stdin the child replays, and the
// routing arguments eligibility skipped.
type routed struct {
	router.Decision
	captured *prompt.Result
	skipped  []args.Skip
}

// route captures the prompt and decides, all within the routing deadline. Eligibility warnings go to
// stderr once the prompt is known to be there.
func (a *app) route(cfg *config.Config, cat *catalog.Catalog, rt *router.Router, req *args.Request) (routed, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Agrouter.Timeout)
	defer cancel()

	captured, err := prompt.Capture(ctx, req.Prompt.Value, a.stdin)
	if err != nil {
		return routed{}, err
	}

	el := router.Eligible(cfg, cat, req)
	for _, s := range el.Skipped {
		fmt.Fprintln(a.stderr, s.Warning)
	}
	a.debug.eligibility(el)
	// one option runs without Jev, so the files the prompt mentions are not read for it
	if len(el.Options) > 1 {
		if err = captured.ReadMentions(ctx, a.workDir); err != nil {
			return routed{}, fmt.Errorf("read mentioned files: %w", err)
		}
	}

	d, err := rt.Route(ctx, el, req, captured)
	if err != nil {
		return routed{}, err
	}
	return routed{Decision: d, captured: captured, skipped: el.Skipped}, nil
}

// printDecision writes the decision-mode JSON line to stdout; skipped lists what eligibility skipped,
// then what the argv left out.
func (a *app) printDecision(d routed, res args.Result) int {
	out := decisionJSON{selectionJSON: selection(d.Decision), Argv: res.Argv, Skipped: []string{}}
	for _, s := range slices.Concat(d.skipped, res.Skipped) {
		out.Skipped = append(out.Skipped, s.Spelling)
	}
	enc := json.NewEncoder(a.stdout)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return a.fail(fmt.Errorf("write decision: %w", err))
	}
	return exitOK
}

// selection shares the selected values between decision output and exec logging.
func selection(d router.Decision) selectionJSON {
	out := selectionJSON{CLI: d.CLI}
	if d.Model != "" {
		out.Model = &d.Model
	}
	if d.Effort != "" {
		out.Effort = &d.Effort
	}
	return out
}

// exec runs the child with the caller's stdin replayed, and returns its exit code.
func (a *app) exec(res args.Result, captured *prompt.Result) int {
	var stdin io.Reader
	if captured.Stdin != nil {
		stdin = captured.Stdin.Reader()
	}
	code, err := a.runner.Run(context.Background(), runner.Command{
		Argv: res.Argv, Stdin: stdin, Stdout: a.stdout, Stderr: a.stderr,
	})
	if err != nil {
		a.printErr(err)
	}
	return code
}

// fail prints err and returns exitUsage.
func (a *app) fail(err error) int {
	a.printErr(err)
	return exitUsage
}

// printErr prints err as "agrouter:" lines, one per line of its message.
func (a *app) printErr(err error) {
	prefix := "agrouter: "
	if _, ok := errors.AsType[configError](err); ok {
		prefix += "config: "
	}
	for line := range strings.SplitSeq(err.Error(), "\n") {
		fmt.Fprintln(a.stderr, prefix+line)
	}
}

// configError marks an error from loading the config files; each violation is its own line.
type configError struct{ err error }

func (e configError) Error() string { return e.err.Error() }

func (e configError) Unwrap() error { return e.err }
