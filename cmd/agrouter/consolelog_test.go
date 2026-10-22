package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testPID is the pid the tee stamps, which is this test process.
var testPID = os.Getpid()

var logEpoch = time.Date(2026, 10, 22, 15, 17, 3, 0, time.UTC)

// stepClock returns a clock that advances one second per call, so each stamped line differs.
func stepClock() func() time.Time {
	var n int
	return func() time.Time {
		t := logEpoch.Add(time.Duration(n) * time.Second)
		n++
		return t
	}
}

// stamp is the line prefix the tee writes for the n-th stamped line (from 0).
func stamp(n int) string {
	return fmt.Sprintf("%s pid=%d ", logEpoch.Add(time.Duration(n)*time.Second).Format(time.RFC3339), testPID)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // a path the test built under t.TempDir()
	require.NoError(t, err)
	return string(data)
}

// failingWriter fails every write after reporting how many bytes it took.
type failingWriter struct{ err error }

func (w failingWriter) Write(p []byte) (int, error) { return 0, w.err }

func TestConsoleLog_WholeLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", logFileName)
	var console bytes.Buffer
	c := newConsoleLog(&console, stepClock())
	c.start(path)

	for _, part := range []string{"agrouter: a", "lpha\nagro", "uter: beta\nagrouter: gamma\n"} {
		n, err := c.Write([]byte(part))
		require.NoError(t, err)
		assert.Len(t, part, n)
	}
	c.close()

	assert.Equal(t, "agrouter: alpha\nagrouter: beta\nagrouter: gamma\n", console.String())
	assert.Equal(t,
		stamp(0)+"agrouter: alpha\n"+stamp(1)+"agrouter: beta\n"+stamp(2)+"agrouter: gamma\n",
		readFile(t, path))
}

func TestConsoleLog_HeldLinesReplayToFileOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agrouter.log")
	var console bytes.Buffer
	c := newConsoleLog(&console, stepClock())

	_, err := c.Write([]byte("agrouter: early one\nagrouter: early two\n"))
	require.NoError(t, err)
	_, err = c.Write([]byte("agrouter: unfinished"))
	require.NoError(t, err)
	_, statErr := os.Stat(path)
	require.ErrorIs(t, statErr, os.ErrNotExist, "no file before start")

	c.start(path)
	assert.Equal(t, stamp(0)+"agrouter: early one\n"+stamp(1)+"agrouter: early two\n", readFile(t, path),
		"complete lines are replayed with their original stamps; the unfinished one waits")

	_, err = c.Write([]byte(" line\nagrouter: live\n"))
	require.NoError(t, err)
	c.close()

	assert.Equal(t, "agrouter: early one\nagrouter: early two\nagrouter: unfinished line\nagrouter: live\n", console.String(),
		"the console got every byte once, in order")
	assert.Equal(t,
		stamp(0)+"agrouter: early one\n"+stamp(1)+"agrouter: early two\n"+
			stamp(2)+"agrouter: unfinished line\n"+stamp(3)+"agrouter: live\n",
		readFile(t, path))
}

func TestConsoleLog_ConsoleBytesIdentical(t *testing.T) {
	writes := [][]byte{
		[]byte("agrouter: one\n"),
		[]byte("two\nthree\n"),
		[]byte("\n"),
		[]byte("tail without newline"),
		nil,
	}
	var plain bytes.Buffer
	for _, w := range writes {
		_, err := plain.Write(w)
		require.NoError(t, err)
	}

	for _, tc := range []struct {
		name  string
		start bool
	}{
		{name: "mirror never started"},
		{name: "mirror started", start: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var console bytes.Buffer
			c := newConsoleLog(&console, stepClock())
			if tc.start {
				c.start(filepath.Join(t.TempDir(), logFileName))
			}
			for _, w := range writes {
				n, err := c.Write(w)
				require.NoError(t, err)
				assert.Len(t, w, n)
			}
			c.close()
			assert.Equal(t, plain.String(), console.String())
		})
	}
}

func TestConsoleLog_PartialLineFlushedOnClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), logFileName)
	var console bytes.Buffer
	c := newConsoleLog(&console, stepClock())
	c.start(path)
	_, err := c.Write([]byte("agrouter: done\nagrouter: cut off"))
	require.NoError(t, err)
	assert.Equal(t, stamp(0)+"agrouter: done\n", readFile(t, path), "the unfinished line is not written yet")

	c.close()
	c.close() // idempotent
	assert.Equal(t, stamp(0)+"agrouter: done\n"+stamp(1)+"agrouter: cut off\n", readFile(t, path))
	assert.Equal(t, "agrouter: done\nagrouter: cut off", console.String(), "the console never gets the added newline")
}

func TestConsoleLog_NothingWrittenWithoutStart(t *testing.T) {
	dir := t.TempDir()
	var console bytes.Buffer
	c := newConsoleLog(&console, stepClock())
	_, err := c.Write([]byte("agrouter: usage error\nand a partial"))
	require.NoError(t, err)
	c.close()

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries)
	assert.Equal(t, "agrouter: usage error\nand a partial", console.String())
}

func TestConsoleLog_WritesAfterCloseReachConsoleOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), logFileName)
	var console bytes.Buffer
	c := newConsoleLog(&console, stepClock())
	c.start(path)
	c.close()

	_, err := c.Write([]byte("agrouter: late\n"))
	require.NoError(t, err)
	c.start(path) // no effect once closed

	assert.Equal(t, "agrouter: late\n", console.String())
	assert.Empty(t, readFile(t, path))
}

func TestConsoleLog_ConsoleErrorPassesThrough(t *testing.T) {
	path := filepath.Join(t.TempDir(), logFileName)
	boom := errors.New("console closed")
	c := newConsoleLog(failingWriter{err: boom}, stepClock())
	c.start(path)

	n, err := c.Write([]byte("agrouter: kept\n"))
	require.ErrorIs(t, err, boom)
	assert.Zero(t, n)
	c.close()
	assert.Equal(t, stamp(0)+"agrouter: kept\n", readFile(t, path), "the log still records what agrouter said")
}

func TestConsoleLog_MirrorFailureWarnsOnce(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("not a directory"), 0o600))

	tests := []struct {
		name  string
		setup func(t *testing.T, c *consoleLog)
	}{
		{
			name: "directory cannot be created",
			setup: func(_ *testing.T, c *consoleLog) {
				c.start(filepath.Join(blocker, "logs", logFileName))
			},
		},
		{
			name: "file cannot be opened",
			setup: func(t *testing.T, c *consoleLog) {
				dirAsFile := filepath.Join(t.TempDir(), logFileName)
				require.NoError(t, os.Mkdir(dirAsFile, 0o750))
				c.start(dirAsFile)
			},
		},
		{
			name: "append fails after the start",
			setup: func(t *testing.T, c *consoleLog) {
				c.start(filepath.Join(t.TempDir(), logFileName))
				require.NoError(t, c.file.Close()) // the next write to the closed file fails
			},
		},
		{
			name: "setup abandoned by the caller",
			setup: func(_ *testing.T, c *consoleLog) {
				c.abandon(errors.New("ignore file is read-only"))
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var console bytes.Buffer
			c := newConsoleLog(&console, stepClock())
			_, err := c.Write([]byte("agrouter: before\n"))
			require.NoError(t, err)
			tc.setup(t, c)
			for range 3 {
				n, err := c.Write([]byte("agrouter: after\n"))
				require.NoError(t, err)
				assert.Len(t, "agrouter: after\n", n)
			}
			c.close()

			out := console.String()
			assert.Equal(t, 1, strings.Count(out, "agrouter: warning: log: "), out)
			assert.Equal(t, 3, strings.Count(out, "agrouter: after\n"), "console output unchanged")
			assert.Equal(t, 1, strings.Count(out, "agrouter: before\n"))
			assert.NotContains(t, out, "pid=", "the warning is not stamped, so it was not mirrored")
		})
	}
}

func TestConsoleLog_ConcurrentWritesStayWhole(t *testing.T) {
	path := filepath.Join(t.TempDir(), logFileName)
	var console bytes.Buffer
	c := newConsoleLog(&console, stepClock())
	c.start(path)

	const writers, perWriter = 8, 50
	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			for i := range perWriter {
				_, err := fmt.Fprintf(c, "agrouter: writer %d line %d\n", w, i)
				assert.NoError(t, err)
			}
		})
	}
	wg.Wait()
	c.close()

	lines := strings.Split(strings.TrimSuffix(readFile(t, path), "\n"), "\n")
	require.Len(t, lines, writers*perWriter)
	for _, line := range lines {
		assert.Regexp(t, `^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ pid=\d+ agrouter: writer \d line \d+$`, line)
	}
	assert.Equal(t, writers*perWriter, strings.Count(console.String(), "\n"))
}

func TestEnsureLogIgnored(t *testing.T) {
	const rules = ".gitignore\nlogs/\n"
	tests := []struct {
		name     string
		existing *string
		want     string
	}{
		{name: "new file", want: rules},
		{name: "empty file", existing: new(""), want: rules},
		{name: "content kept", existing: new("# local\nnotes/\n"), want: "# local\nnotes/\n" + rules},
		{name: "missing final newline", existing: new("notes/"), want: "notes/\n" + rules},
		{name: "already the last lines", existing: new("notes/\n" + rules), want: "notes/\n" + rules},
		{name: "already the only lines, no final newline", existing: new(".gitignore\nlogs/"), want: ".gitignore\nlogs/"},
		{name: "negation after the rules", existing: new(rules + "!logs/\n"), want: rules + "!logs/\n" + rules},
		{name: "only one of the rules", existing: new("logs/\n"), want: "logs/\n" + rules},
		{name: "rules in the wrong order", existing: new("logs/\n.gitignore\n"), want: "logs/\n.gitignore\n" + rules},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), ".agrouter")
			path := filepath.Join(dir, ".gitignore")
			if tc.existing != nil {
				require.NoError(t, os.MkdirAll(dir, 0o750))
				require.NoError(t, os.WriteFile(path, []byte(*tc.existing), 0o600))
			}

			require.NoError(t, ensureLogIgnored(dir))
			assert.Equal(t, tc.want, readFile(t, path))

			require.NoError(t, ensureLogIgnored(dir))
			assert.Equal(t, tc.want, readFile(t, path), "a repeat changes nothing")
		})
	}
}

func TestEnsureLogIgnored_Errors(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("file"), 0o600))

	t.Run("directory under a file", func(t *testing.T) {
		require.Error(t, ensureLogIgnored(filepath.Join(blocker, ".agrouter")))
	})
	t.Run("ignore file is a directory", func(t *testing.T) {
		dir := filepath.Join(root, ".agrouter")
		require.NoError(t, os.MkdirAll(filepath.Join(dir, ".gitignore"), 0o750))
		require.Error(t, ensureLogIgnored(dir))
	})
}

func TestSymlinksRefused(t *testing.T) {
	victim := filepath.Join(t.TempDir(), "victim")
	require.NoError(t, os.WriteFile(victim, []byte("keep\n"), 0o600))

	t.Run("ignore file", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), ".agrouter")
		linkTo(t, victim, filepath.Join(dir, ".gitignore"))
		require.ErrorContains(t, ensureLogIgnored(dir), "symlink")
		assert.Equal(t, "keep\n", readFile(t, victim))
	})
	t.Run("ignore directory", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "target")
		require.NoError(t, os.Mkdir(target, 0o750))
		dir := filepath.Join(root, ".agrouter")
		linkTo(t, target, dir)
		require.ErrorContains(t, ensureLogIgnored(dir), "symlink")
		assert.NoFileExists(t, filepath.Join(target, ".gitignore"))
	})
	t.Run("log file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "logs", logFileName)
		linkTo(t, victim, path)
		var console bytes.Buffer
		c := newConsoleLog(&console, stepClock())
		_, err := c.Write([]byte("agrouter: line\n"))
		require.NoError(t, err)
		c.start(path)
		c.close()
		assert.Contains(t, console.String(), "symlink")
		assert.Equal(t, "keep\n", readFile(t, victim))
	})
}

func TestEnsureLogIgnored_Git(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	repo := t.TempDir()
	git := func(args ...string) error {
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
		return cmd.Run()
	}
	require.NoError(t, git("init", "--quiet"))

	dir := filepath.Join(repo, ".agrouter")
	require.NoError(t, ensureLogIgnored(dir))
	logPath := filepath.Join(dir, "logs", logFileName)
	require.NoError(t, os.MkdirAll(filepath.Dir(logPath), 0o750))
	require.NoError(t, os.WriteFile(logPath, []byte("line\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config"), []byte("[agrouter]\n"), 0o600))

	require.NoError(t, git("check-ignore", "--quiet", filepath.Join(".agrouter", "logs", logFileName)),
		"the log is ignored")
	require.NoError(t, git("check-ignore", "--quiet", filepath.Join(".agrouter", ".gitignore")),
		"the ignore file ignores itself")
	var exitErr *exec.ExitError
	require.ErrorAs(t, git("check-ignore", "--quiet", filepath.Join(".agrouter", "config")), &exitErr,
		"the local config stays trackable")
	assert.Equal(t, 1, exitErr.ExitCode(), "exit 1 means not ignored")
}
