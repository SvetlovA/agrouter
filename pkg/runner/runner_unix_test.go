//go:build !windows

package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gone reports whether pid has exited within 10 seconds.
func gone(pid int) bool {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func TestRun_Signals(t *testing.T) {
	tests := []struct {
		sig   syscall.Signal
		group bool // sent to the whole group (a terminal's Ctrl+C) rather than to agrouter's pid alone
	}{
		{syscall.SIGTERM, false},
		{syscall.SIGHUP, false},
		{syscall.SIGINT, true},
		{syscall.SIGQUIT, true},
	}
	for _, tt := range tests {
		t.Run(tt.sig.String(), func(t *testing.T) {
			c, dir := helperCommand(t, "trap")
			done := make(chan int, 1)
			go func() {
				code, _ := Runner{}.Run(context.Background(), c)
				done <- code
			}()
			pid := waitPid(t, filepath.Join(dir, "pid"))

			// agrouter catches the signal and outlives the child; SIGTERM and SIGHUP are forwarded, while
			// SIGINT and SIGQUIT reach the child through the group it shares with agrouter (simulated
			// here, since the test's group includes go test itself)
			require.NoError(t, syscall.Kill(os.Getpid(), tt.sig))
			if tt.group {
				require.NoError(t, syscall.Kill(pid, tt.sig))
			}
			select {
			case code := <-done:
				assert.Equal(t, 42, code)
			case <-time.After(15 * time.Second):
				t.Fatal("the child did not get the signal")
			}
			assert.Equal(t, tt.sig.String(), string(readFile(t, filepath.Join(dir, "signal"))))
		})
	}
}

func TestRun_SharesProcessGroup(t *testing.T) {
	c, dir := helperCommand(t, "sleep")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan int, 1)
	go func() {
		code, _ := Runner{}.Run(ctx, c)
		done <- code
	}()
	pid := waitPid(t, filepath.Join(dir, "pid"))
	// a caller killing agrouter's group (ralphex on cancellation) kills the child too
	pgid, err := syscall.Getpgid(pid)
	require.NoError(t, err)
	assert.Equal(t, syscall.Getpgrp(), pgid)

	cancel()
	select {
	case code := <-done:
		assert.Equal(t, 128+int(syscall.SIGTERM), code)
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	assert.True(t, gone(pid), "the child survived cancellation")
}

func TestRun_SignalExitCode(t *testing.T) {
	c, dir := helperCommand(t, "sleep")
	done := make(chan int, 1)
	go func() {
		code, _ := Runner{}.Run(context.Background(), c)
		done <- code
	}()
	pid := waitPid(t, filepath.Join(dir, "pid"))
	require.NoError(t, syscall.Kill(pid, syscall.SIGKILL))
	select {
	case code := <-done:
		assert.Equal(t, 128+int(syscall.SIGKILL), code)
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return")
	}
}

func TestRunProbe_OwnProcessGroup(t *testing.T) {
	c, dir := probeCommand(t, "sleep")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p, err := StartProbe(ctx, c)
	require.NoError(t, err)
	pid := waitPid(t, filepath.Join(dir, "pid"))
	// unlike the real child, a probe leads its own group, so killing it never reaches agrouter
	pgid, err := syscall.Getpgid(pid)
	require.NoError(t, err)
	assert.Equal(t, pid, pgid)
	assert.NotEqual(t, syscall.Getpgrp(), pgid)
	cancel()
	_, err = p.Wait()
	require.ErrorIs(t, err, context.Canceled)
	assert.True(t, gone(pid), "the probe survived cancellation")
}

func TestRunProbe_KillsDescendants(t *testing.T) {
	tests := []struct {
		mode    string
		timeout time.Duration
		wantErr error
	}{
		{mode: "spawn-exit", timeout: time.Minute},                                               // the probe exits, its grandchild is left behind
		{mode: "spawn-wait", timeout: 500 * time.Millisecond, wantErr: context.DeadlineExceeded}, // the probe times out with its grandchild
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			c, dir := probeCommand(t, tt.mode)
			ctx, cancel := context.WithTimeout(context.Background(), tt.timeout)
			defer cancel()
			res, err := RunProbe(ctx, c, nil)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
				assert.Equal(t, 0, res.ExitCode)
			}
			gc := waitPid(t, filepath.Join(dir, "grandchild"))
			t.Cleanup(func() { _ = syscall.Kill(gc, syscall.SIGKILL) })
			assert.True(t, gone(gc), "the grandchild survived the probe")
		})
	}
}

func TestRunProbe_DescendantKeepsStdout(t *testing.T) {
	c, dir := probeCommand(t, "spawn-stdout")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	start := time.Now()
	// the probe's exit, not its grandchild's copy of stdout, ends the read
	res, err := RunProbe(ctx, c, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, res.ExitCode)
	assert.Equal(t, "done", string(res.Stdout))
	assert.Less(t, time.Since(start), 30*time.Second)
	gc := waitPid(t, filepath.Join(dir, "grandchild"))
	t.Cleanup(func() { _ = syscall.Kill(gc, syscall.SIGKILL) })
	assert.True(t, gone(gc), "the grandchild survived the probe")
}

func TestStartProbe_SignalKillsProbes(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			hc, dir := helperCommand(t, "probe-parent")
			parent := exec.Command(hc.Argv[0])
			parent.Env = hc.Env
			require.NoError(t, parent.Start())
			probe := waitPid(t, filepath.Join(dir, "probe", "pid"))
			t.Cleanup(func() { _ = syscall.Kill(probe, syscall.SIGKILL) })

			// the probe leads its own group, so only agrouter gets the signal; it kills the probe and
			// still ends by the signal
			require.NoError(t, parent.Process.Signal(sig))
			err := parent.Wait()
			var exitErr *exec.ExitError
			require.ErrorAs(t, err, &exitErr, "agrouter outlived the signal")
			status, ok := exitErr.Sys().(syscall.WaitStatus)
			require.True(t, ok)
			assert.Equal(t, sig, status.Signal())
			assert.True(t, gone(probe), "the probe survived the signal")
		})
	}
}

func TestProbeGroup_StartRegistersBeforeSpawn(t *testing.T) {
	live := func(g *probeGroup) bool {
		liveProbes.mu.Lock()
		defer liveProbes.mu.Unlock()
		_, ok := liveProbes.groups[g]
		return ok
	}
	tests := []struct {
		name string
		err  error
	}{
		{name: "started"},
		{name: "start failed", err: os.ErrNotExist},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := newProbeGroup(exec.Command("unused"))
			// a signal during the spawn finds the group registered and waits for its process
			err := g.start(func() error {
				assert.True(t, live(g), "the group is not registered while it is spawned")
				assert.False(t, liveProbes.spawning.TryLock(), "a signal does not wait for the spawn")
				return tt.err
			})
			require.ErrorIs(t, err, tt.err)
			assert.Equal(t, tt.err == nil, live(g))
			liveProbes.remove(g)
			assert.False(t, live(g))
		})
	}
}

func TestProbeGroup_StartAfterSignalCanceled(t *testing.T) {
	queued := newProbeGroup(exec.Command("unused"))
	liveProbes.add(queued)
	liveProbes.mu.Lock()
	sigs := liveProbes.sigs
	liveProbes.mu.Unlock()
	// the signal is handled after the group registered but before it took the spawn lock
	liveProbes.cancel(sigs)
	close(sigs) // ends the forward goroutine the signal would have run in

	spawned := false
	err := queued.start(func() error { spawned = true; return nil })
	require.ErrorIs(t, err, errProbeCanceled)
	assert.False(t, spawned, "a probe queued before the signal was spawned")
	liveProbes.mu.Lock()
	_, ok := liveProbes.groups[queued]
	liveProbes.mu.Unlock()
	assert.False(t, ok, "the canceled group stays registered")

	// a probe registered after the signal is spawned as usual
	later := newProbeGroup(exec.Command("unused"))
	require.NoError(t, later.start(func() error { spawned = true; return nil }))
	assert.True(t, spawned)
	liveProbes.remove(later)
}
