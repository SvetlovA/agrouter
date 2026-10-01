package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SvetlovA/agrouter/pkg/config"
	"github.com/SvetlovA/agrouter/pkg/prompt"
)

// the test binary doubles as the child: with GO_WANT_HELPER_PROCESS=1 it runs helper instead of the
// tests, so its argv is exactly what Run passed.
const (
	helperEnv  = "GO_WANT_HELPER_PROCESS"
	helperMode = "RUNNER_HELPER_MODE"
	helperOut  = "RUNNER_HELPER_OUT"
	helperExit = "RUNNER_HELPER_EXIT"
)

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		os.Exit(helper())
	}
	os.Exit(m.Run())
}

// helper is the fake CLI. Modes:
//   - record: writes argv.json, stdin.bin and env.json to $RUNNER_HELPER_OUT, then exits $RUNNER_HELPER_EXIT;
//   - exit: exits $RUNNER_HELPER_EXIT without reading stdin;
//   - echo: writes "out" to stdout and "err" to stderr;
//   - sleep: writes its pid to $RUNNER_HELPER_OUT/pid, then sleeps;
//   - spawn-wait, spawn-exit: starts a sleeping grandchild (pid in $RUNNER_HELPER_OUT/grandchild), then
//     sleeps or exits 0;
//   - trap: catches SIGINT/SIGTERM/SIGHUP/SIGQUIT, writes $RUNNER_HELPER_OUT/ready, and on a signal
//     writes its name to $RUNNER_HELPER_OUT/signal and exits 42.
func helper() int {
	out := os.Getenv(helperOut)
	code, _ := strconv.Atoi(os.Getenv(helperExit))
	switch os.Getenv(helperMode) {
	case "record":
		argv, _ := json.Marshal(os.Args[1:])
		stdin, _ := io.ReadAll(os.Stdin)
		env, _ := json.Marshal(os.Environ())
		_ = os.WriteFile(filepath.Join(out, "argv.json"), argv, 0o600)
		_ = os.WriteFile(filepath.Join(out, "stdin.bin"), stdin, 0o600)
		_ = os.WriteFile(filepath.Join(out, "env.json"), env, 0o600)
		return code
	case "exit":
		return code
	case "echo":
		_, _ = os.Stdout.WriteString("out")
		_, _ = os.Stderr.WriteString("err")
		return 0
	case "sleep":
		writePid(filepath.Join(out, "pid"), os.Getpid())
		time.Sleep(time.Minute)
		return 0
	case "spawn-wait", "spawn-exit":
		gc := exec.Command(os.Args[0])
		gc.Env = append(os.Environ(), helperMode+"=sleep", helperOut+"="+filepath.Join(out, "gc"))
		_ = os.MkdirAll(filepath.Join(out, "gc"), 0o700)
		if err := gc.Start(); err != nil {
			return 99
		}
		writePid(filepath.Join(out, "grandchild"), gc.Process.Pid)
		if os.Getenv(helperMode) == "spawn-exit" {
			return 0
		}
		time.Sleep(time.Minute)
		return 0
	case "trap":
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
		_ = os.WriteFile(filepath.Join(out, "ready"), nil, 0o600)
		select {
		case s := <-sigs:
			_ = os.WriteFile(filepath.Join(out, "signal"), []byte(s.String()), 0o600)
			return 42
		case <-time.After(time.Minute):
			return 0
		}
	}
	return 98
}

// writePid writes pid via a rename, so a reader never sees a partial file.
func writePid(path string, pid int) {
	_ = os.WriteFile(path+".tmp", []byte(strconv.Itoa(pid)), 0o600)
	_ = os.Rename(path+".tmp", path)
}

// waitPid waits for the pid a helper writes to path.
func waitPid(t *testing.T, path string) int {
	t.Helper()
	var pid int
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(path) //nolint:gosec // test file
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(string(data))
		return err == nil
	}, 10*time.Second, 10*time.Millisecond)
	return pid
}

// helperCommand returns a Command running the helper in mode with args, recording into a temp dir.
func helperCommand(t *testing.T, mode string, args ...string) (Command, string) {
	t.Helper()
	dir := t.TempDir()
	env := append(os.Environ(), helperEnv+"=1", helperMode+"="+mode, helperOut+"="+dir)
	return Command{Argv: append([]string{os.Args[0]}, args...), Env: env}, dir
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test file
	require.NoError(t, err)
	return data
}

func TestRun_Argv(t *testing.T) {
	args := []string{"-p", "fix the bug in \"main.go\"", "", "a b", `C:\dir\`, `back\"slash`, "unicodé ✓", "--model", "x"}
	c, dir := helperCommand(t, "record", args...)
	code, err := Runner{}.Run(context.Background(), c)
	require.NoError(t, err)
	assert.Equal(t, 0, code)

	var got []string
	require.NoError(t, json.Unmarshal(readFile(t, filepath.Join(dir, "argv.json")), &got))
	assert.Equal(t, args, got)
}

func TestRun_Stdin(t *testing.T) {
	binary := []byte{0x89, 'P', 'N', 'G', 0, 0xff, 0xfe, '\r', '\n', 0}
	tests := []struct {
		name  string
		stdin func() io.Reader
		want  []byte
	}{
		{name: "none", stdin: func() io.Reader { return nil }, want: []byte{}},
		{name: "text", stdin: func() io.Reader { return strings.NewReader("hello\r\nworld\n") }, want: []byte("hello\r\nworld\n")},
		{name: "binary", stdin: func() io.Reader { return bytes.NewReader(binary) }, want: binary},
		{name: "buffered then relayed", stdin: func() io.Reader {
			s := &prompt.Stdin{Buffered: []byte("captured prefix|"), Rest: bytes.NewReader(append([]byte("rest|"), binary...))}
			return s.Reader()
		}, want: append([]byte("captured prefix|rest|"), binary...)},
		{name: "read to EOF", stdin: func() io.Reader {
			s := &prompt.Stdin{Buffered: []byte("all of it")}
			return s.Reader()
		}, want: []byte("all of it")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, dir := helperCommand(t, "record")
			c.Stdin = tc.stdin()
			code, err := Runner{}.Run(context.Background(), c)
			require.NoError(t, err)
			assert.Equal(t, 0, code)
			assert.Equal(t, tc.want, readFile(t, filepath.Join(dir, "stdin.bin")))
		})
	}
}

func TestRun_EnvWithoutAPIKey(t *testing.T) {
	t.Setenv(config.EnvAPIKey, "secret-key")
	t.Setenv("AGROUTER_TEST_KEEP", "kept")

	c, dir := helperCommand(t, "record")
	code, err := Runner{}.Run(context.Background(), c)
	require.NoError(t, err)
	assert.Equal(t, 0, code)

	var env []string
	require.NoError(t, json.Unmarshal(readFile(t, filepath.Join(dir, "env.json")), &env))
	assert.Contains(t, env, "AGROUTER_TEST_KEEP=kept")
	for _, kv := range env {
		assert.NotContains(t, kv, "secret-key")
	}
}

func TestRun_NilEnvInheritsWithoutAPIKey(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.EnvAPIKey, "secret-key")
	t.Setenv(helperEnv, "1")
	t.Setenv(helperMode, "record")
	t.Setenv(helperOut, dir)

	code, err := Runner{}.Run(context.Background(), Command{Argv: []string{os.Args[0]}})
	require.NoError(t, err)
	assert.Equal(t, 0, code)
	assert.NotContains(t, string(readFile(t, filepath.Join(dir, "env.json"))), "secret-key")
}

func TestChildEnv(t *testing.T) {
	env := []string{"A=1", config.EnvAPIKey + "=k", "typesafe_api_key=k2", "TYPESAFE_API_KEY_OTHER=3", "=C:=C:\\"}
	tests := []struct {
		goos string
		want []string
	}{
		{goos: "linux", want: []string{"A=1", "typesafe_api_key=k2", "TYPESAFE_API_KEY_OTHER=3", "=C:=C:\\"}},
		{goos: "windows", want: []string{"A=1", "TYPESAFE_API_KEY_OTHER=3", "=C:=C:\\"}},
	}
	for _, tc := range tests {
		t.Run(tc.goos, func(t *testing.T) {
			old := goos
			goos = tc.goos
			t.Cleanup(func() { goos = old })
			assert.Equal(t, tc.want, childEnv(env))
		})
	}
}

func TestRun_ExitCode(t *testing.T) {
	for _, want := range []int{0, 1, 3, 42} {
		t.Run(strconv.Itoa(want), func(t *testing.T) {
			c, _ := helperCommand(t, "exit")
			c.Env = append(c.Env, helperExit+"="+strconv.Itoa(want))
			code, err := Runner{}.Run(context.Background(), c)
			require.NoError(t, err)
			assert.Equal(t, want, code)
		})
	}
}

func TestRun_StdoutStderr(t *testing.T) {
	c, _ := helperCommand(t, "echo")
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	code, err := Runner{}.Run(context.Background(), c)
	require.NoError(t, err)
	assert.Equal(t, 0, code)
	assert.Equal(t, "out", stdout.String())
	assert.Equal(t, "err", stderr.String())
}

func TestRun_StartFailure(t *testing.T) {
	tests := []struct {
		name string
		argv []string
	}{
		{name: "missing from PATH", argv: []string{"agrouter-no-such-command-xyz", "arg"}},
		{name: "missing path", argv: []string{filepath.Join(t.TempDir(), "nope")}},
		{name: "empty argv"},
		{name: "empty command", argv: []string{""}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, err := Runner{}.Run(context.Background(), Command{Argv: tc.argv})
			assert.Equal(t, ExitStartFailure, code)
			var se *StartError
			require.ErrorAs(t, err, &se)
			assert.Contains(t, err.Error(), "cannot start")
			if len(tc.argv) > 0 && tc.argv[0] != "" {
				assert.Equal(t, tc.argv[0], se.Command)
				assert.Error(t, errors.Unwrap(err))
			}
		})
	}
}

func TestRun_ChildExitsWhileStdinStaysOpen(t *testing.T) {
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })

	c, _ := helperCommand(t, "exit")
	c.Env = append(c.Env, helperExit+"=5")
	c.Stdin = io.MultiReader(strings.NewReader("prefix"), pr)

	done := make(chan int, 1)
	go func() {
		code, _ := Runner{}.Run(context.Background(), c)
		done <- code
	}()
	select {
	case code := <-done:
		assert.Equal(t, 5, code)
	case <-time.After(15 * time.Second):
		t.Fatal("Run hung on stdin the caller kept open")
	}
}

func TestRun_Cancel(t *testing.T) {
	c, dir := helperCommand(t, "sleep")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan int, 1)
	go func() {
		code, _ := Runner{}.Run(ctx, c)
		done <- code
	}()
	waitPid(t, filepath.Join(dir, "pid"))
	cancel()
	select {
	case code := <-done:
		assert.NotEqual(t, 0, code)
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

func TestIsBatchFile(t *testing.T) {
	for path, want := range map[string]bool{
		`C:\npm\claude.cmd`: true, `C:\npm\CODEX.CMD`: true, `x.bat`: true, `x.Bat`: true,
		`C:\bin\claude.exe`: false, `claude`: false, `x.cmd.exe`: false, `/usr/bin/cmd`: false,
	} {
		assert.Equal(t, want, isBatchFile(path), path)
	}
}

func TestBatchCommandLine(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "no args", want: `cmd.exe /d /v:off /s /c ""C:\npm\claude.cmd""`},
		{name: "plain and empty", args: []string{"-p", ""}, want: `cmd.exe /d /v:off /s /c ""C:\npm\claude.cmd" "-p" """`},
		{name: "quote escaped", args: []string{`say "hi"`}, want: `cmd.exe /d /v:off /s /c ""C:\npm\claude.cmd" "say \"hi\"""`},
		{name: "metacharacters between quotes reopen cmd quoting", args: []string{`x "a & b" y`},
			want: `cmd.exe /d /v:off /s /c ""C:\npm\claude.cmd" "x \"a "&" b\" y""`},
		{name: "metacharacters stay quoted", args: []string{"a & b | c < d > e ^ f !g!"},
			want: `cmd.exe /d /v:off /s /c ""C:\npm\claude.cmd" "a & b | c < d > e ^ f !g!""`},
		{name: "percent", args: []string{"100% %PATH%"},
			want: `cmd.exe /d /v:off /s /c ""C:\npm\claude.cmd" "100%%cd:~,% %%cd:~,%PATH%%cd:~,%""`},
		{name: "backslashes", args: []string{`C:\dir\`, `a\"b`, `a\\b`, `a\%`},
			want: `cmd.exe /d /v:off /s /c ""C:\npm\claude.cmd" "C:\dir\\" "a\\\"b" "a\\b" "a\%%cd:~,%""`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := batchCommandLine(`C:\npm\claude.cmd`, tc.args)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	for _, bad := range []string{"line\nbreak", "cr\rhere", "nul\x00"} {
		_, err := batchCommandLine(`C:\npm\claude.cmd`, []string{"ok", bad})
		require.Error(t, err, "%q", bad)
		assert.Contains(t, err.Error(), "argument 2")
	}
}
