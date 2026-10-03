//go:build !windows

package runner

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
)

// newCmd returns the command for path. The child stays in agrouter's process group, so a signal or
// kill sent to that group (a terminal's Ctrl+C, a caller such as ralphex killing the group it started)
// reaches the child and its descendants too, even when it also kills agrouter outright.
func newCmd(ctx context.Context, path string, args []string) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, path, args...), nil
}

// tree is the child. While it runs, agrouter catches SIGINT, SIGTERM, SIGHUP and SIGQUIT so it outlives
// the child and returns its exit code. SIGTERM and SIGHUP, often sent to agrouter's pid alone, are
// forwarded to the child; SIGINT and SIGQUIT come from the terminal to the whole group, so the child
// already has them and forwarding would deliver them twice.
type tree struct {
	cmd  *exec.Cmd
	sigs chan os.Signal
	done chan struct{}
	once sync.Once
}

// newTree starts catching the signals, so one arriving while the child starts is not lost.
func newTree(cmd *exec.Cmd) *tree {
	t := &tree{cmd: cmd, sigs: make(chan os.Signal, 4), done: make(chan struct{})}
	signal.Notify(t.sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	return t
}

// started forwards SIGTERM and SIGHUP to the child until release.
func (t *tree) started() {
	go func() {
		for {
			select {
			case s := <-t.sigs:
				if s == syscall.SIGTERM || s == syscall.SIGHUP {
					_ = t.signal(s)
				}
			case <-t.done:
				return
			}
		}
	}()
}

// cancel asks the child to stop on cancellation; exec.Cmd kills it after waitDelay.
func (t *tree) cancel() error { return t.signal(syscall.SIGTERM) }

// release stops catching the signals.
func (t *tree) release() {
	t.once.Do(func() {
		signal.Stop(t.sigs)
		close(t.done)
	})
}

func (t *tree) signal(sig os.Signal) error {
	if t.cmd.Process == nil {
		return os.ErrProcessDone
	}
	return t.cmd.Process.Signal(sig) //nolint:wrapcheck // returned to exec.Cmd.Cancel
}

// exitCode is the child's exit status, or 128+signal when a signal killed it.
func exitCode(state *os.ProcessState) int {
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return state.ExitCode()
}
