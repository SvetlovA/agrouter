//go:build windows

package runner

import (
	"fmt"
	"os/exec"
	"sync"

	"golang.org/x/sys/windows"
)

// probeGroup is the probe's own kill-on-close Job Object, separate from the real child's, so a probe
// and its descendants are killed together. Without a job it degrades to the probe itself.
type probeGroup struct {
	cmd    *exec.Cmd
	mu     sync.Mutex
	job    windows.Handle
	killed bool
}

func newProbeGroup(cmd *exec.Cmd) *probeGroup {
	return &probeGroup{cmd: cmd, job: windows.InvalidHandle}
}

// started assigns the probe to a new job; see tree.started for the window before the assignment.
func (g *probeGroup) started() {
	job, err := assignToNewJob(g.cmd.Process.Pid)
	if err != nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.job = job
	if g.killed {
		_ = windows.TerminateJobObject(job, jobExitCode)
	}
}

func (g *probeGroup) kill() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.killed = true
	if g.job != windows.InvalidHandle {
		if err := windows.TerminateJobObject(g.job, jobExitCode); err != nil {
			return fmt.Errorf("terminate job: %w", err)
		}
		return nil
	}
	if g.cmd.Process == nil {
		return nil
	}
	return g.cmd.Process.Kill() //nolint:wrapcheck // returned to exec.Cmd.Cancel
}

// release closes the job, killing descendants that outlived the probe.
func (g *probeGroup) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.job != windows.InvalidHandle {
		_ = windows.CloseHandle(g.job)
		g.job = windows.InvalidHandle
	}
}
