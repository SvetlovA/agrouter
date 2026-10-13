//go:build !windows

package runner

import (
	"os"
	"os/exec"
	"syscall"
)

// probeGroup is the probe's own process group. Unlike the real child, which stays in agrouter's group,
// a probe and its descendants are killed together without touching agrouter.
type probeGroup struct{ cmd *exec.Cmd }

func newProbeGroup(cmd *exec.Cmd) *probeGroup {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return &probeGroup{cmd: cmd}
}

func (g *probeGroup) started() {}

// kill kills the whole group; the probe is its leader, so the group id is its pid.
func (g *probeGroup) kill() error {
	if g.cmd.Process == nil {
		return os.ErrProcessDone
	}
	return syscall.Kill(-g.cmd.Process.Pid, syscall.SIGKILL) //nolint:wrapcheck // returned to exec.Cmd.Cancel
}

// release kills descendants that outlived the probe.
func (g *probeGroup) release() { _ = g.kill() }
