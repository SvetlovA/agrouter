//go:build !windows

package runner

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gone reports whether pid has exited within timeout.
func gone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
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
	assert.True(t, gone(pid, 10*time.Second), "the child survived cancellation")
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
	assert.True(t, gone(pid, 10*time.Second), "the probe survived cancellation")
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
			assert.True(t, gone(gc, 10*time.Second), "the grandchild survived the probe")
		})
	}
}
