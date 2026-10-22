package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// logFileName is the mirror log inside log_dir.
const logFileName = "agrouter.log"

// consoleLog is the writer behind agrouter's own stderr lines. Every write goes to the console
// first and unchanged. The complete lines are also copied to a log file, so a caller that hides
// stderr still leaves a record. The copy waits in memory until start gives the file, which lets
// the first lines (usage warnings, debug) be mirrored once config has resolved log_dir; close
// drops it when start never came. The child's output does not pass through here.
//
// A mirror failure never reaches the console stream or the return values: mirroring stops and
// one console-only warning says why.
type consoleLog struct {
	console io.Writer
	now     func() time.Time
	pid     int

	mu      sync.Mutex
	partial []byte   // the unfinished last line
	held    [][]byte // stamped lines waiting for the file
	file    *os.File
	off     bool // mirroring ended: stopped, failed or closed
	warned  bool
}

// newConsoleLog returns a tee over console. It stamps each line with now and this process's id.
func newConsoleLog(console io.Writer, now func() time.Time) *consoleLog {
	return &consoleLog{console: console, now: now, pid: os.Getpid()}
}

// Write passes p to the console and returns its result, then copies the complete lines of p to the
// log. The console write stays first and holds the lock, so the file keeps the console's order.
func (c *consoleLog) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, err := c.console.Write(p)
	c.collect(p)
	return n, err //nolint:wrapcheck // the console's own result, passed through
}

// collect splits p into lines and records each complete one.
func (c *consoleLog) collect(p []byte) {
	if c.off {
		return
	}
	c.partial = append(c.partial, p...)
	for {
		i := bytes.IndexByte(c.partial, '\n')
		if i < 0 {
			return
		}
		line := c.partial[:i+1]
		c.partial = c.partial[i+1:]
		c.record(line)
		if c.off {
			return
		}
	}
}

// record stamps one line, ending in a newline, and appends it to the file or holds it for start.
func (c *consoleLog) record(line []byte) {
	stamped := fmt.Appendf(nil, "%s pid=%d %s", c.now().UTC().Format(time.RFC3339), c.pid, line)
	if c.file == nil {
		c.held = append(c.held, stamped)
		return
	}
	// one write per line: with O_APPEND a concurrent invocation's lines stay whole
	if _, err := c.file.Write(stamped); err != nil {
		c.stop(err)
	}
}

// start opens the log file, creating its directory, and replays the held lines to it only. A failure
// stops mirroring with the warning. It has no effect once mirroring began or ended.
func (c *consoleLog) start(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.off || c.file != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		c.stop(err)
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // the configured log_dir
	if err != nil {
		c.stop(err)
		return
	}
	c.file = f
	held := c.held
	c.held = nil
	for _, line := range held {
		if c.off {
			return
		}
		if _, err = c.file.Write(line); err != nil {
			c.stop(err)
		}
	}
}

// abandon ends mirroring before it began, because the log cannot be set up, with the warning.
func (c *consoleLog) abandon(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stop(err)
}

// close flushes an unfinished last line and closes the file. Lines held for a file that never
// came are dropped. Later writes reach the console only.
func (c *consoleLog) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.off {
		return
	}
	if len(c.partial) > 0 {
		line := slices.Concat(c.partial, []byte{'\n'})
		c.partial = nil
		c.record(line)
		if c.off {
			return
		}
	}
	var err error
	if c.file != nil {
		err = c.file.Close()
		c.file = nil
	}
	c.stop(err)
}

// stop ends mirroring. A non-nil err is reported once, to the console only so it never recurses
// into the copy. The caller holds the lock.
func (c *consoleLog) stop(err error) {
	c.off = true
	c.partial, c.held = nil, nil
	if c.file != nil {
		_ = c.file.Close()
		c.file = nil
	}
	if err != nil && !c.warned {
		c.warned = true
		fmt.Fprintf(c.console, "agrouter: warning: log: %v\n", err)
	}
}

// logIgnoreRules keep the default log directory out of git, ralphex-style: the file ignores itself
// and the logs, while the sibling config stays trackable.
var logIgnoreRules = []string{".gitignore", "logs/"}

// ensureLogIgnored makes dir/.gitignore end with logIgnoreRules, creating the directory and the
// file when missing and never truncating. The rules are appended unless they already are the file's
// last lines, so a later negation (!logs/) cannot undo them.
func ensureLogIgnored(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	path := filepath.Join(dir, ".gitignore")
	data, err := os.ReadFile(path) //nolint:gosec // the ignore file next to the local config
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if endsWithLines(data, logIgnoreRules) {
		return nil
	}
	var add bytes.Buffer
	if len(data) > 0 && data[len(data)-1] != '\n' {
		add.WriteByte('\n')
	}
	for _, rule := range logIgnoreRules {
		add.WriteString(rule + "\n")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // the ignore file next to the local config
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	if _, err = f.Write(add.Bytes()); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}

// endsWithLines reports whether the last lines of data are exactly lines, in order.
func endsWithLines(data []byte, lines []string) bool {
	have := bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
	if len(have) < len(lines) {
		return false
	}
	tail := have[len(have)-len(lines):]
	for i, line := range lines {
		if string(tail[i]) != line {
			return false
		}
	}
	return true
}
