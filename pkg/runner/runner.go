// Package runner runs the chosen CLI as a child process: no shell, the caller's stdin replayed then
// relayed, the parent's stdout and stderr, the child's exit code, and the whole process tree killed on
// cancellation (a process group on Unix, a Job Object on Windows).
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/SvetlovA/agrouter/pkg/config"
)

// ExitStartFailure is the exit code when the command is missing or cannot be started.
const ExitStartFailure = 127

// waitDelay bounds how long Wait waits for the I/O relay after the child exits (or after cancellation
// before the child is killed), so a caller keeping stdin open cannot hang agrouter.
const waitDelay = time.Second

// goos is the platform the environment filter and batch handling decide on; tests override it.
var goos = runtime.GOOS

// Command is one child to run.
type Command struct {
	Argv   []string  // the command, then its arguments; never passed through a shell
	Stdin  io.Reader // nil for empty stdin
	Stdout io.Writer // an *os.File is handed to the child as is
	Stderr io.Writer
	Env    []string // the environment before filtering; nil means os.Environ()
}

// StartError is a command that is missing, not executable, or failed to start.
type StartError struct {
	Command string
	Err     error
}

func (e *StartError) Error() string { return fmt.Sprintf("cannot start %q: %v", e.Command, e.Err) }

func (e *StartError) Unwrap() error { return e.Err }

// Runner runs commands with os/exec.
type Runner struct{}

// Run runs c to completion and returns the child's exit code (128+signal for a child killed by a
// signal on Unix). A command that cannot be started returns ExitStartFailure and a *StartError; no
// other error is returned. Canceling ctx kills the child's process tree.
func (Runner) Run(ctx context.Context, c Command) (int, error) {
	if len(c.Argv) == 0 || c.Argv[0] == "" {
		return ExitStartFailure, &StartError{Err: errors.New("empty command")}
	}
	fail := func(err error) (int, error) { return ExitStartFailure, &StartError{Command: c.Argv[0], Err: err} }

	path, err := exec.LookPath(c.Argv[0])
	if err != nil {
		return fail(err)
	}
	cmd, err := newCmd(ctx, path, c.Argv[1:])
	if err != nil {
		return fail(err)
	}
	env := c.Env
	if env == nil {
		env = os.Environ()
	}
	cmd.Env = childEnv(env)
	cmd.Stdout, cmd.Stderr = c.Stdout, c.Stderr
	cmd.WaitDelay = waitDelay
	relay, err := stdinRelay(cmd, c.Stdin)
	if err != nil {
		return fail(err)
	}

	t := newTree(cmd)
	cmd.Cancel = t.cancel
	if err := cmd.Start(); err != nil {
		t.release()
		return fail(err)
	}
	t.started()
	go relay()
	// the exit code comes from the process state: a stdin relay error, ErrWaitDelay or the context
	// error after cancellation do not change what the child exited with
	_ = cmd.Wait()
	t.release()
	return exitCode(cmd.ProcessState), nil
}

// stdinRelay connects stdin to cmd and returns the copy to run once the child has started. An
// *os.File (or nil, for empty stdin) is handed to the child as is. Any other reader is copied by a
// goroutine of our own rather than by os/exec, whose Wait awaits its copy even past WaitDelay: a
// caller keeping stdin open would block the copy in Read forever. Ours is abandoned when the child
// exits; Wait closes the pipe, so a copy blocked in Write fails and returns.
func stdinRelay(cmd *exec.Cmd, stdin io.Reader) (func(), error) {
	if f, ok := stdin.(*os.File); ok || stdin == nil {
		if ok {
			cmd.Stdin = f
		}
		return func() {}, nil
	}
	w, err := cmd.StdinPipe()
	if err != nil {
		return nil, err //nolint:wrapcheck // wrapped by StartError
	}
	return func() {
		_, _ = io.Copy(w, stdin) // a child that stops reading ends the copy with EPIPE
		_ = w.Close()
	}, nil
}

// childEnv returns env without the Jev API key (names are case-insensitive on Windows).
func childEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if name == config.EnvAPIKey || (goos == "windows" && strings.EqualFold(name, config.EnvAPIKey)) {
			continue
		}
		out = append(out, kv)
	}
	return out
}
