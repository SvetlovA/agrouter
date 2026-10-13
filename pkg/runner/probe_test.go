package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SvetlovA/agrouter/pkg/config"
)

// probeCommand returns a ProbeCommand running the helper in mode, recording into a temp dir.
func probeCommand(t *testing.T, mode string, extraEnv ...string) (ProbeCommand, string) {
	t.Helper()
	c, dir := helperCommand(t, mode)
	return ProbeCommand{Argv: c.Argv, Env: append(c.Env, extraEnv...)}, dir
}

func TestRunProbe_StdinIsolated(t *testing.T) {
	c, _ := probeCommand(t, "cat")
	res, err := RunProbe(context.Background(), c, []byte("initialize\nread\n"))
	require.NoError(t, err)
	assert.Equal(t, 0, res.ExitCode)
	assert.Equal(t, "initialize\nread\n", string(res.Stdout), "the probe reads only what it is given")

	res, err = RunProbe(context.Background(), c, nil)
	require.NoError(t, err)
	assert.Empty(t, res.Stdout, "no input is empty stdin, never the caller's")
}

func TestRunProbe_EnvWithoutKey(t *testing.T) {
	c, _ := probeCommand(t, "env-out", config.EnvAPIKey+"=sk-secret", "PROBE_KEEP=1")
	res, err := RunProbe(context.Background(), c, nil)
	require.NoError(t, err)
	var env []string
	require.NoError(t, json.Unmarshal(res.Stdout, &env))
	assert.Contains(t, env, "PROBE_KEEP=1")
	assert.NotContains(t, string(res.Stdout), "sk-secret")
}

func TestRunProbe_WorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	c, _ := probeCommand(t, "pwd")
	c.Dir = dir
	res, err := RunProbe(context.Background(), c, nil)
	require.NoError(t, err)
	want, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	got, err := filepath.EvalSymlinks(string(res.Stdout))
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestRunProbe_ExitCode(t *testing.T) {
	c, _ := probeCommand(t, "exit", helperExit+"=3")
	res, err := RunProbe(context.Background(), c, nil)
	require.NoError(t, err, "a nonzero exit is the probe's answer, not a run error")
	assert.Equal(t, 3, res.ExitCode)
}

func TestRunProbe_ResponseLimit(t *testing.T) {
	tests := []struct {
		name    string
		size    int
		wantErr bool
	}{
		{name: "at the limit", size: 64},
		{name: "past the limit", size: 65, wantErr: true},
		{name: "far past the limit", size: 1 << 20, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := probeCommand(t, "flood", helperExit+"="+strconv.Itoa(tt.size))
			c.MaxResponse = 64
			res, err := RunProbe(context.Background(), c, nil)
			if tt.wantErr {
				require.ErrorIs(t, err, ErrProbeResponseTooLarge)
				assert.Empty(t, res.Stdout)
				return
			}
			require.NoError(t, err)
			assert.Len(t, res.Stdout, tt.size)
		})
	}
}

func TestRunProbe_Timeout(t *testing.T) {
	c, dir := probeCommand(t, "sleep")
	c.Timeout = 200 * time.Millisecond
	start := time.Now()
	_, err := RunProbe(context.Background(), c, nil)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 10*time.Second)
	waitPid(t, filepath.Join(dir, "pid"))
}

func TestRunProbe_ContextCanceled(t *testing.T) {
	c, dir := probeCommand(t, "sleep")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := RunProbe(ctx, c, nil)
		done <- err
	}()
	waitPid(t, filepath.Join(dir, "pid"))
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(15 * time.Second):
		t.Fatal("RunProbe did not return after cancellation")
	}
}

func TestRunProbe_StartErrors(t *testing.T) {
	notExec := filepath.Join(t.TempDir(), "probe.txt")
	require.NoError(t, os.WriteFile(notExec, []byte("not a program"), 0o600))
	tests := []struct {
		name string
		argv []string
	}{
		{name: "empty", argv: nil},
		{name: "missing", argv: []string{filepath.Join(t.TempDir(), "no-such-probe")}},
		{name: "not executable", argv: []string{notExec}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := RunProbe(context.Background(), ProbeCommand{Argv: tt.argv}, nil)
			var se *StartError
			require.ErrorAs(t, err, &se)
		})
	}
}

func TestLookPath(t *testing.T) {
	path, err := LookPath(os.Args[0])
	require.NoError(t, err)
	assert.NotEmpty(t, path)

	_, err = LookPath(filepath.Join(t.TempDir(), "no-such-cli"))
	var se *StartError
	require.ErrorAs(t, err, &se)
}

// a streaming probe answers each request line in turn, as a control protocol does
func TestStartProbe_Streaming(t *testing.T) {
	c, _ := probeCommand(t, "cat")
	p, err := StartProbe(context.Background(), c)
	require.NoError(t, err)
	lines := bufio.NewScanner(p.Stdout)
	for _, msg := range []string{"initialize", "read"} {
		_, err = p.Stdin.Write([]byte(msg + "\n"))
		require.NoError(t, err)
		require.True(t, lines.Scan())
		assert.Equal(t, msg, lines.Text())
	}
	require.NoError(t, p.Stdin.Close())
	require.NoError(t, p.Stdin.Close(), "closing twice is harmless")
	assert.False(t, lines.Scan(), "stdout ends when the probe exits")
	code, err := p.Wait()
	require.NoError(t, err)
	assert.Equal(t, 0, code)
}

func TestStartProbe_Kill(t *testing.T) {
	c, dir := probeCommand(t, "sleep")
	p, err := StartProbe(context.Background(), c)
	require.NoError(t, err)
	waitPid(t, filepath.Join(dir, "pid"))
	p.Kill()
	_, err = p.Wait()
	require.ErrorIs(t, err, context.Canceled)
}

func TestLimitReader_PassesEOF(t *testing.T) {
	over := false
	r := &limitReader{r: eofReader{}, left: 0, over: func() { over = true }}
	_, err := r.Read(make([]byte, 4))
	require.ErrorIs(t, err, errEOF)
	assert.False(t, over)
}

var errEOF = errors.New("eof")

type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, errEOF }
