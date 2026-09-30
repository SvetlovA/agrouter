// Package main is the agrouter CLI: it asks Jev which (cli, model, effort) should run a prompt,
// then prints that decision or runs the chosen CLI.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"

	"github.com/jessevdk/go-flags"
)

// options are agrouter's command-line options.
type options struct {
	Version bool `long:"version" description:"print agrouter's version and exit"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run executes agrouter with the given arguments and streams, returning the process exit code.
func run(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	var opts options
	parser := flags.NewParser(&opts, flags.HelpFlag|flags.PassDoubleDash)
	parser.Name = "agrouter"
	parser.Usage = "[OPTIONS] [exec] [PROMPT]"

	if _, err := parser.ParseArgs(args); err != nil {
		var flagsErr *flags.Error
		if errors.As(err, &flagsErr) && flagsErr.Type == flags.ErrHelp {
			fmt.Fprintln(stdout, flagsErr.Message)
			return 0
		}
		fmt.Fprintf(stderr, "agrouter: %v\n", err)
		return 2
	}

	if opts.Version {
		fmt.Fprintln(stdout, version())
		return 0
	}

	fmt.Fprintln(stderr, "agrouter: not implemented yet")
	return 2
}

// version reports the module version recorded by `go install ...@<tag>`, or "unknown".
func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" {
		return "unknown"
	}
	return info.Main.Version
}
