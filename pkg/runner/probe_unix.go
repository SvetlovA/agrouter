//go:build !windows

package runner

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
)

// probeGroup is the probe's own process group. Unlike the real child, which stays in agrouter's group,
// a probe and its descendants are killed together without touching agrouter.
type probeGroup struct {
	cmd       *exec.Cmd
	canceled bool // set by a signal before the group was spawned; guarded by liveProbes.mu
}

// errProbeCanceled is returned by start when a signal that ends agrouter arrived before the spawn.
var errProbeCanceled = errors.New("probe canceled by a signal")

func newProbeGroup(cmd *exec.Cmd) *probeGroup {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return &probeGroup{cmd: cmd}
}

// start registers the group, then runs start (the probe's exec.Cmd.Start), so a signal that ends
// agrouter kills it first even while it is being spawned, and a group still waiting to be spawned
// when the signal arrives is never spawned.
func (g *probeGroup) start(start func() error) error {
	liveProbes.add(g)
	// a signal waits for spawns in progress, so it sees their process
	liveProbes.spawning.RLock()
	err := errProbeCanceled
	if !liveProbes.isCanceled(g) {
		err = start()
	}
	liveProbes.spawning.RUnlock()
	if err != nil {
		liveProbes.remove(g)
	}
	return err
}

// kill kills the whole group; the probe is its leader, so the group id is its pid.
func (g *probeGroup) kill() error {
	if g.cmd.Process == nil {
		return os.ErrProcessDone
	}
	return syscall.Kill(-g.cmd.Process.Pid, syscall.SIGKILL) //nolint:wrapcheck // returned to exec.Cmd.Cancel
}

// release kills descendants that outlived the probe.
func (g *probeGroup) release() {
	_ = g.kill()
	liveProbes.remove(g)
}

// probeSignals are the signals that end agrouter. A probe's own group does not receive a terminal's
// Ctrl+C, so agrouter kills its probes before the signal ends it.
var probeSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT}

// liveProbes are the running probe groups; while there are any, probeSignals are caught.
var liveProbes probeSet

type probeSet struct {
	mu       sync.Mutex
	spawning sync.RWMutex // read-held while a probe is spawned; forward write-holds it
	groups   map[*probeGroup]struct{}
	sigs     chan os.Signal
}

func (s *probeSet) add(g *probeGroup) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.groups == nil {
		s.groups = map[*probeGroup]struct{}{}
	}
	s.groups[g] = struct{}{}
	if s.sigs == nil {
		s.sigs = make(chan os.Signal, 1)
		signal.Notify(s.sigs, probeSignals...)
		go s.forward(s.sigs)
	}
}

func (s *probeSet) remove(g *probeGroup) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.groups, g)
	if len(s.groups) == 0 && s.sigs != nil {
		signal.Stop(s.sigs)
		close(s.sigs)
		s.sigs = nil
	}
}

func (s *probeSet) isCanceled(g *probeGroup) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return g.canceled
}

// forward kills every live probe group on a signal, then stops catching the signals and raises it
// again, so it ends agrouter as it would have without probes.
func (s *probeSet) forward(sigs chan os.Signal) {
	sig, ok := <-sigs
	if !ok {
		return
	}
	// no probe is spawned while the lock is held, so every registered group has its process or is
	// canceled before its spawn
	s.spawning.Lock()
	defer s.spawning.Unlock()
	s.cancel(sigs)
	if sig, ok := sig.(syscall.Signal); ok {
		_ = syscall.Kill(os.Getpid(), sig)
	}
}

// cancel kills every registered group that has its process, cancels the ones still waiting to be
// spawned, and stops catching the signals on sigs. Groups registered later are not affected.
func (s *probeSet) cancel(sigs chan os.Signal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for g := range s.groups {
		g.canceled = true
		_ = g.kill()
	}
	signal.Stop(sigs)
	if s.sigs == sigs {
		s.sigs = nil
	}
}
