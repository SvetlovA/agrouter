//go:build windows

package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

// exited reports whether pid has exited within timeout.
func exited(pid int, timeout time.Duration) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return true // no such process
	}
	defer windows.CloseHandle(h)
	ev, err := windows.WaitForSingleObject(h, uint32(timeout.Milliseconds()))
	return err == nil && ev == windows.WAIT_OBJECT_0
}

func killPid(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

func TestRun_JobClosedKillsTree(t *testing.T) {
	c, dir := helperCommand(t, "spawn-exit")
	code, err := Runner{}.Run(context.Background(), c)
	require.NoError(t, err)
	assert.Equal(t, 0, code)

	gc := waitPid(t, filepath.Join(dir, "grandchild"))
	t.Cleanup(func() { killPid(gc) })
	assert.True(t, exited(gc, 10*time.Second), "grandchild survived the job being closed")
}

func TestRun_CancelKillsTree(t *testing.T) {
	c, dir := helperCommand(t, "spawn-wait")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan int, 1)
	go func() {
		code, _ := Runner{}.Run(ctx, c)
		done <- code
	}()
	gc := waitPid(t, filepath.Join(dir, "grandchild"))
	t.Cleanup(func() { killPid(gc) })
	cancel()
	select {
	case code := <-done:
		assert.Equal(t, jobExitCode, code)
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	assert.True(t, exited(gc, 10*time.Second), "grandchild survived cancellation")
}

// writeShim writes an npm-style .cmd shim forwarding %* to the test binary into a directory on PATH.
func writeShim(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	shim := "@echo off\r\n\"" + os.Args[0] + "\" %*\r\nexit /b %errorlevel%\r\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "fakecli.cmd"), []byte(shim), 0o600))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestRun_BatchShimReceivesSpecialCharacters(t *testing.T) {
	writeShim(t)
	t.Setenv("AGROUTER_TEST_VAR", "EXPANDED")
	args := []string{
		"-p", `fix "main.go" & echo pwned | more > out.txt < in ^ caret`,
		"100% %AGROUTER_TEST_VAR% %% !AGROUTER_TEST_VAR!", "", "a b", `C:\dir\`, `back\"slash`, `a\\b`, "unicodé ✓",
		`x "a & b" y`, `"(paren) | pipe"`, `say "hi there" > out.txt`, `\"`, `"`, `""`, "tab\there",
	}
	c, dir := helperCommand(t, "record", args...)
	c.Argv[0] = "fakecli" // resolved through PATH and PATHEXT to fakecli.cmd
	code, err := Runner{}.Run(context.Background(), c)
	require.NoError(t, err)
	assert.Equal(t, 0, code)

	var got []string
	require.NoError(t, json.Unmarshal(readFile(t, filepath.Join(dir, "argv.json")), &got))
	assert.Equal(t, args, got)
	assert.NoFileExists(t, "out.txt")
}

func TestRun_BatchShimExitCode(t *testing.T) {
	writeShim(t)
	c, _ := helperCommand(t, "exit")
	c.Argv[0] = "fakecli"
	c.Env = append(c.Env, helperExit+"=7")
	code, err := Runner{}.Run(context.Background(), c)
	require.NoError(t, err)
	assert.Equal(t, 7, code)
}

func TestRun_BatchShimRejectsLineBreak(t *testing.T) {
	writeShim(t)
	c, dir := helperCommand(t, "record", "-p", "line one\nline two")
	c.Argv[0] = "fakecli"
	code, err := Runner{}.Run(context.Background(), c)
	assert.Equal(t, ExitStartFailure, code)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "line break")
	assert.NoFileExists(t, filepath.Join(dir, "argv.json"), "the shim must not run")
}
