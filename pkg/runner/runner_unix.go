//go:build !windows

package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
)

// newCmd returns the command for path, in a new session and so its own process group, which is what
// signals are forwarded to and what is killed.
func newCmd(ctx context.Context, path string, args []string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd, nil
}

// tree is the child's process group. SIGINT, SIGTERM, SIGHUP and SIGQUIT to agrouter are forwarded to
// it while the child runs; in its own session the child would not get a Ctrl+C or a terminal hangup
// otherwise, and an uncaught SIGHUP would kill agrouter and leave the child running.
type tree struct {
	cmd  *exec.Cmd
	sigs chan os.Signal
	done chan struct{}
	once sync.Once
}

// newTree starts catching the forwarded signals, so one arriving while the child starts is not lost.
func newTree(cmd *exec.Cmd) *tree {
	t := &tree{cmd: cmd, sigs: make(chan os.Signal, 4), done: make(chan struct{})}
	signal.Notify(t.sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	return t
}

// started forwards the caught signals to the group until release.
func (t *tree) started() {
	go func() {
		for {
			select {
			case s := <-t.sigs:
				if sig, ok := s.(syscall.Signal); ok {
					_ = t.signal(sig)
				}
			case <-t.done:
				return
			}
		}
	}()
}

// cancel asks the group to stop on cancellation; exec.Cmd kills the direct child after waitDelay and
// release kills whatever is left.
func (t *tree) cancel() error { return t.signal(syscall.SIGTERM) }

// release stops forwarding and kills what remains of the group (descendants that outlived the child).
func (t *tree) release() {
	t.once.Do(func() {
		signal.Stop(t.sigs)
		close(t.done)
		if t.cmd.Process != nil {
			_ = t.signal(syscall.SIGKILL)
		}
	})
}

func (t *tree) signal(sig syscall.Signal) error {
	if t.cmd.Process == nil || t.cmd.Process.Pid <= 0 {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-t.cmd.Process.Pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err //nolint:wrapcheck // returned to exec.Cmd.Cancel
}

// exitCode is the child's exit status, or 128+signal when a signal killed it.
func exitCode(state *os.ProcessState) int {
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return state.ExitCode()
}
