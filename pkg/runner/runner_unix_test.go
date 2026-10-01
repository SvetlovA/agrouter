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

func TestRun_ForwardsSignals(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT} {
		t.Run(sig.String(), func(t *testing.T) {
			c, dir := helperCommand(t, "trap")
			done := make(chan int, 1)
			go func() {
				code, _ := Runner{}.Run(context.Background(), c)
				done <- code
			}()
			require.Eventually(t, func() bool {
				_, err := os.Stat(filepath.Join(dir, "ready"))
				return err == nil
			}, 10*time.Second, 10*time.Millisecond)

			// agrouter itself gets the signal (a terminal's Ctrl+C, a caller's SIGTERM); Run catches and
			// forwards it to the child's group
			require.NoError(t, syscall.Kill(os.Getpid(), sig))
			select {
			case code := <-done:
				assert.Equal(t, 42, code)
			case <-time.After(15 * time.Second):
				t.Fatal("the child did not get the forwarded signal")
			}
			assert.Equal(t, sig.String(), string(readFile(t, filepath.Join(dir, "signal"))))
		})
	}
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

func TestRun_CancelKillsGroup(t *testing.T) {
	c, dir := helperCommand(t, "spawn-wait")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan int, 1)
	go func() {
		code, _ := Runner{}.Run(ctx, c)
		done <- code
	}()
	gc := waitPid(t, filepath.Join(dir, "grandchild"))
	t.Cleanup(func() { _ = syscall.Kill(gc, syscall.SIGKILL) })
	cancel()
	select {
	case code := <-done:
		assert.Equal(t, 128+int(syscall.SIGTERM), code)
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	assert.True(t, gone(gc, 10*time.Second), "grandchild survived cancellation")
}

func TestRun_ReapsGroupAfterExit(t *testing.T) {
	c, dir := helperCommand(t, "spawn-exit")
	code, err := Runner{}.Run(context.Background(), c)
	require.NoError(t, err)
	assert.Equal(t, 0, code)

	gc := waitPid(t, filepath.Join(dir, "grandchild"))
	t.Cleanup(func() { _ = syscall.Kill(gc, syscall.SIGKILL) })
	assert.True(t, gone(gc, 10*time.Second), "grandchild outlived the child")
}
