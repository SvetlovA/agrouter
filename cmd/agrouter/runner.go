package main

import (
	"context"

	"github.com/SvetlovA/agrouter/pkg/runner"
)

//go:generate go tool moq -out mocks/command_runner.go -pkg mocks -skip-ensure . CommandRunner

// CommandRunner runs the chosen CLI in exec mode; runner.Runner implements it.
type CommandRunner interface {
	// Run returns the child's exit code, or runner.ExitStartFailure and a *runner.StartError.
	Run(ctx context.Context, c runner.Command) (int, error)
}

var _ CommandRunner = runner.Runner{}
