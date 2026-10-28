package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SvetlovA/agrouter/pkg/args"
	"github.com/SvetlovA/agrouter/pkg/catalog"
	"github.com/SvetlovA/agrouter/pkg/config"
	"github.com/SvetlovA/agrouter/pkg/config/defaults"
	"github.com/SvetlovA/agrouter/pkg/jev"
	"github.com/SvetlovA/agrouter/pkg/prompt"
	"github.com/SvetlovA/agrouter/pkg/readiness"
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
	// childStderr is the child's stderr: the console as given, never the mirror log's copy. run sets
	// it, and swaps stderr for the copy, for its duration.
	childStderr io.Writer
	getenv      func(string) string
	workDir     string
	embedded    []byte
	newJev      func(key string, longContext bool) router.JevClient
	runner      CommandRunner
	checker     ReadinessChecker // exec only
	now         func() time.Time // nil means time.Now
	debug       *debugLog        // nil unless AGROUTER_DEBUG=1
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
		newJev:   func(key string, longContext bool) router.JevClient { return newJevClient(key, longContext) },
		runner:   runner.Runner{},
		checker:  readiness.Checker{Dir: wd},
	}
}

// newJevClient is the Jev client for key, sending the long-context header when longContext is set.
func newJevClient(key string, longContext bool) *jev.Client {
	c := jev.New(key)
	c.LongContext = longContext
	return c
}

func (a *app) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

// stdinReader returns the prompt's stdin: nil for a terminal, which agrouter does not read.
func stdinReader(r io.Reader) io.Reader {
	if f, ok := r.(*os.File); ok {
		return prompt.StdinOf(f)
	}
	return r
}

// selectionJSON records the selection, why routing failed or a value is null, project complexity
// and available Jev confidence.
type selectionJSON struct {
	CLI               *string          `json:"cli"`
	Model             *string          `json:"model"`
	Effort            *string          `json:"effort"`
	Reason            *reasonJSON      `json:"reason,omitempty"`
	NullReasons       *nullReasonsJSON `json:"null_reasons,omitempty"`
	ProjectComplexity string           `json:"project_complexity,omitempty"`
	Confidence        *confidenceJSON  `json:"confidence,omitempty"`
	Options           []optionJSON     `json:"options,omitempty"`
	// exec only
	Fallback  *fallbackJSON  `json:"fallback,omitempty"`
	Effective *effectiveJSON `json:"effective,omitempty"`
	Readiness *readinessJSON `json:"readiness,omitempty"`
}

// optionJSON is one eligible option, in catalog order.
type optionJSON struct {
	ID     string `json:"id"`
	CLI    string `json:"cli"`
	Model  string `json:"model"`
	Effort string `json:"effort"`
}

// decisionJSON adds the argv for decision mode.
type decisionJSON struct {
	selectionJSON
	Argv          []string `json:"argv"`
	StdinRequired bool     `json:"stdin_required"`
	Command       string   `json:"command"`
}

// run executes the pipeline: parse, config, catalog, then under the routing deadline capture, route
// and build the argv; then print the decision or run the child. It returns the exit code.
func (a *app) run(argv []string) int {
	// agrouter's own lines pass through the log copy, which waits for log_dir; the child keeps the console
	console := a.stderr
	mirror := newConsoleLog(console, a.clock)
	a.stderr, a.childStderr = mirror, console
	defer func() {
		mirror.close()
		a.stderr, a.childStderr = console, nil
	}()

	cmd, err := parseArgs(argv)
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
	if a.getenv(legacyCLIEnv) != "" {
		fmt.Fprintf(a.stderr, "agrouter: warning: %s is ignored: use exec CLI to run a CLI, or --cli=NAME to restrict a decision\n",
			legacyCLIEnv)
	}

	cfg, cat, rt, err := a.setup(req)
	if err != nil {
		return a.fail(err)
	}
	a.startLog(mirror, cfg.Agrouter.LogDir)
	if req.Mode == args.ModeExec {
		// a CLI without catalog options would fall back to routing across every CLI
		if len(catalog.ByCLI(cat.Options, req.CLI)) == 0 {
			return a.fail(fmt.Errorf("exec %s: not a configured, enabled CLI", req.CLI))
		}
	}

	d, err := a.route(cfg, cat, rt, req)
	if err != nil {
		// the stages completed before Jev failed, if any
		a.debug.decision(d.Decision)
		a.debug.failed(err)
		if blocked, ok := errors.AsType[*preflightError](err); ok {
			// the block came before any routing request, so say why routing never ran
			fmt.Fprintln(a.stderr, blocked.routingSkipped())
			fmt.Fprintln(a.stderr, blocked.line)
			return blocked.code
		}
		if req.Mode == args.ModeDecision && errors.Is(err, router.ErrCannotDecide) {
			// stdout still gets the decision, all null with the reasons, beside the error line
			if code := a.writeDecision(selectionOutput(d, cmd.verbose, err), cmd.verbose); code != exitOK {
				return code
			}
		}
		return a.fail(err)
	}
	a.debug.decision(d.Decision)

	cli, _ := cfg.CLIByName(d.CLI)
	res := args.Build(cli, req, d.Choice())
	a.debug.command(res)

	if req.Mode == args.ModeDecision {
		return a.printDecision(d, res, cmd.verbose)
	}
	// --verbose is agrouter's decision detail; the exec selection line stays concise
	s, blocked := a.execSelection(cli, req, d)
	if blocked != nil {
		// routing chose before the block, so its selection is logged; the skipped line does not claim a run
		a.logSelection(s)
		fmt.Fprintln(a.stderr, blocked.runSkipped())
		fmt.Fprintln(a.stderr, blocked.line)
		return blocked.code
	}
	a.logSelection(s)
	return a.exec(res, d.captured)
}

// selectionPrefix starts the exec selection line. A bare JSON line would look like one of the
// child's stream-json events to a caller that merges stderr into stdout, such as ralphex, which
// drops JSON without an event type; a prefixed line is shown as text, like warnings and debug lines.
const selectionPrefix = "agrouter: selection: "

// logSelection writes the selection as one prefixed JSON line to stderr. It is best effort, like
// warnings, and never includes the prompt or argv.
func (a *app) logSelection(s selectionJSON) {
	var line bytes.Buffer
	line.WriteString(selectionPrefix)
	enc := json.NewEncoder(&line)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return
	}
	_, _ = a.stderr.Write(line.Bytes())
}

// startLog starts copying agrouter's console lines to agrouter.log in logDir: a relative path is
// resolved against the work directory. The default directory is kept out of git first; any other is
// the user's to manage. A failure ends the copy with one warning and changes nothing else.
func (a *app) startLog(mirror *consoleLog, logDir string) {
	dir := logDir
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(a.workDir, dir)
	}
	dir = filepath.Clean(dir)
	if local := config.LocalDir(a.workDir); dir == filepath.Join(local, defaultLogSubdir) {
		if err := ensureLogIgnored(local); err != nil {
			mirror.abandon(err)
			return
		}
		if err := notSymlink(dir); err != nil {
			mirror.abandon(err)
			return
		}
	}
	mirror.start(filepath.Join(dir, logFileName))
}

// setup loads and validates the config, derives the catalog and builds the router with the resolved
// Jev key. Every error is a config error.
func (a *app) setup(req *args.Request) (*config.Config, *catalog.Catalog, *router.Router, error) {
	global, local, err := config.DefaultPaths(a.workDir)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("config: %w", err)
	}
	if err = config.InstallDefaults(global, a.embedded); err != nil {
		return nil, nil, nil, configError{err}
	}
	cfg, err := config.Load(config.Sources{Embedded: a.embedded, GlobalPath: global, LocalPath: local})
	if err != nil {
		return nil, nil, nil, configError{err}
	}
	cat, err := catalog.Build(cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	key, source := config.ResolveAPIKey(req.APIKey.Value, req.APIKey.Set, a.getenv(config.EnvAPIKey), cfg.Classifier)
	a.debug.apiKey(key, source)
	rt, err := router.New(cfg, cat, a.newJev(key, cfg.Classifier.LongContext))
	if err != nil {
		return nil, nil, nil, err
	}
	return cfg, cat, rt, nil
}

// routed is the decision together with the captured prompt, whose stdin the child replays.
type routed struct {
	router.Decision
	captured *prompt.Result
	options  []optionJSON
	checked  *checked // exec's preflight; nil in decision mode
	// effortDropped is true when the caller's --effort was dropped: the chosen CLI has no effort_args
	// to pass it.
	effortDropped bool
}

// route reads --prompt-file and every --doc, captures the prompt and decides, all within the routing
// deadline. Exec checks its CLI's readiness meanwhile and routes only over what it can run now; when
// nothing is left it returns a *preflightError without asking Jev. Eligibility warnings go to stderr
// once the prompt is known to be there. When Jev cannot decide, the error comes with the partial
// decision: the stages and complexity completed.
func (a *app) route(cfg *config.Config, cat *catalog.Catalog, rt *router.Router, req *args.Request) (routed, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Agrouter.Timeout)
	defer cancel()

	if req.PromptFile.Set {
		text, err := prompt.ReadTextFile(ctx, a.workDir, req.PromptFile.Value)
		if err != nil {
			return routed{}, fmt.Errorf("--prompt-file: %w", err)
		}
		req.FileText = text
	}
	var (
		execCLI config.CLI
		pf      *preflight
	)
	if req.Mode == args.ModeExec {
		execCLI, _ = cfg.CLIByName(req.CLI)
		if err := args.CheckSeparator(execCLI, req); err != nil {
			return routed{}, err
		}
		pf = a.startPreflight(ctx, execCLI)
		// an early return stops the probes before leaving
		defer func() { cancel(); pf.wait() }()
	}
	// docs are read even when one option will be left, so a bad --doc fails every call alike; the
	// deadline passing mid-read is not a bad doc, so Jev cannot decide
	var (
		docs        []string
		undecidable error
	)
	for _, path := range req.Docs {
		text, err := prompt.ReadTextFile(ctx, a.workDir, path)
		if err != nil && ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			undecidable = fmt.Errorf("--doc: %w", err)
			continue // the later docs are still checked for being missing or not regular files
		}
		if err != nil {
			return routed{}, fmt.Errorf("--doc: %w", err)
		}
		docs = append(docs, text)
	}
	captured, err := prompt.Capture(ctx, req.ArgvPrompt(), a.stdin)
	if err != nil {
		return routed{}, err
	}
	captured.Docs = docs
	if undecidable != nil && captured.Undecidable == nil {
		captured.Undecidable = undecidable
	}

	el := router.Eligible(cat, req)
	var pre *checked
	if pf != nil {
		if pre, err = a.applyPreflight(execCLI, cfg.Models, pf, el); err != nil {
			return routed{}, err
		}
	}
	for _, w := range el.Warnings {
		fmt.Fprintln(a.stderr, w)
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
		return routed{Decision: d}, err
	}
	options := make([]optionJSON, len(el.Options))
	for i, o := range el.Options {
		options[i] = optionJSON{ID: o.ID, CLI: o.CLI, Model: o.Name, Effort: el.EffortFor(o)}
	}
	dropped := a.dropUnpassableEffort(cfg, &d)
	return routed{Decision: d, captured: captured, options: options, checked: pre, effortDropped: dropped}, nil
}

// dropUnpassableEffort clears d's effort when its CLI has no effort_args to pass it, with one warning,
// so the selection never reports an effort the child does not receive. Only a caller's --effort can
// get here: a CLI whose models have efforts must have effort_args.
func (a *app) dropUnpassableEffort(cfg *config.Config, d *router.Decision) bool {
	cli, ok := cfg.CLIByName(d.CLI)
	if d.Effort == "" || !ok || len(cli.EffortArgs) > 0 {
		return false
	}
	fmt.Fprintf(a.stderr, "agrouter: warning: dropped --effort %s: %s has no effort_args to pass it\n", d.Effort, cli.Name)
	d.Effort, d.NoEffort = "", true
	return true
}

// printDecision writes concise or indented verbose JSON to stdout.
func (a *app) printDecision(d routed, res args.Result, verbose bool) int {
	summary := selectionOutput(d, verbose, nil)
	var out any = summary
	if verbose {
		details := decisionJSON{selectionJSON: summary, Argv: res.Argv}
		details.Command = commandText(res.Argv)
		details.StdinRequired = d.captured.Stdin != nil && (len(d.captured.Stdin.Buffered) > 0 || d.captured.Stdin.Rest != nil)
		out = details
	}
	return a.writeDecision(out, verbose)
}

// writeDecision encodes out to stdout, indented when verbose.
func (a *app) writeDecision(out any, verbose bool) int {
	enc := json.NewEncoder(a.stdout)
	enc.SetEscapeHTML(false)
	if verbose {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(out); err != nil {
		return a.fail(fmt.Errorf("write decision: %w", err))
	}
	return exitOK
}

// selectionOutput is the selection of d; failure is the routing error when no CLI was chosen.
func selectionOutput(d routed, verbose bool, failure error) selectionJSON {
	out := selection(d.Decision, verbose, failure)
	if d.effortDropped && out.NullReasons != nil {
		out.NullReasons.Effort = &nullReasonJSON{nullEffortDropped, "The CLI has no effort_args to pass the requested effort"}
	}
	if verbose {
		out.Options = d.options
	}
	return out
}

// selection shares the selected values between decision output and exec logging. The reason comes
// from failure, or from d.Undecided when the CLI was known anyway.
func selection(d router.Decision, verbose bool, failure error) selectionJSON {
	out := selectionJSON{Confidence: recordedConfidence(d, verbose)}
	if d.CLI != "" {
		out.CLI = &d.CLI
	}
	if d.Complexity != nil {
		out.ProjectComplexity = formatComplexity(d.Complexity.Complexity)
	}
	if d.Model != "" {
		out.Model = &d.Model
	}
	if d.Effort != "" {
		out.Effort = &d.Effort
	}
	if failure == nil {
		failure = d.Undecided
	}
	out.Reason = failureReason(failure)
	out.NullReasons = nullReasons(out, failure != nil, d.NoEffort)
	return out
}

func formatComplexity(score float64) string {
	return fmt.Sprintf("%.1f/10", score)
}

// exec runs the child with the caller's stdin replayed, and returns its exit code.
func (a *app) exec(res args.Result, captured *prompt.Result) int {
	var stdin io.Reader
	if captured.Stdin != nil {
		stdin = captured.Stdin.Reader()
	}
	code, err := a.runner.Run(context.Background(), runner.Command{
		Argv: res.Argv, Stdin: stdin, Stdout: a.stdout, Stderr: a.childStderr,
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
