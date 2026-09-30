// Package main is the agrouter CLI: it asks Jev which (cli, model, effort) should run a prompt,
// then prints that decision or runs the chosen CLI.
package main

import (
	"fmt"
	"io"
	"os"
	"runtime/debug"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run executes agrouter with the given arguments and streams, returning the process exit code.
func run(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	cmd, err := parseArgs(args, os.Getenv)
	if err != nil {
		fmt.Fprintf(stderr, "agrouter: %v\n", err)
		return 2
	}
	if cmd.help != "" {
		fmt.Fprintln(stdout, cmd.help)
		return 0
	}
	if cmd.version {
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
