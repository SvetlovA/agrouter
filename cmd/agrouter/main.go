// Package main is the agrouter CLI: it asks Jev which (cli, model, effort) should run a prompt,
// then prints that decision or runs the chosen CLI.
package main

import (
	"io"
	"os"
	"runtime/debug"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run executes agrouter with the given arguments and streams, returning the process exit code.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return newApp(stdin, stdout, stderr).run(args)
}

// version reports the module version recorded by `go install ...@<tag>`, or "unknown".
func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" {
		return "unknown"
	}
	return info.Main.Version
}
