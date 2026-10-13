package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// DefaultProbeResponseLimit bounds a probe's stdout when ProbeCommand.MaxResponse is unset.
const DefaultProbeResponseLimit = 1 << 20

// ErrProbeResponseTooLarge is a probe whose stdout went past its byte limit.
var ErrProbeResponseTooLarge = errors.New("probe response exceeds its byte limit")

// ProbeCommand is one prompt-free status probe. Unlike Command, it never sees the caller's stdin or
// output streams: its stdin carries only what the caller writes to Probe.Stdin, its stdout is read
// back with a byte limit, and its stderr is discarded.
type ProbeCommand struct {
	Argv        []string      // the command, then its arguments; never passed through a shell
	Env         []string      // the environment before filtering; nil means os.Environ()
	Dir         string        // the working directory; empty for the current one
	Timeout     time.Duration // bounds the probe on top of the context; zero for none
	MaxResponse int64         // stdout byte limit; zero for DefaultProbeResponseLimit
}

// LookPath resolves name the way Run and StartProbe do, including Windows extensions and batch shims.
func LookPath(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", &StartError{Command: name, Err: err}
	}
	return path, nil
}

// Probe is a running probe. Its whole process tree (a process group on Unix, a Job Object on Windows)
// is killed when the context ends, when the response limit is hit, and once the probe exits.
type Probe struct {
	Stdin  io.WriteCloser // the probe's own stdin; Wait closes it
	Stdout io.Reader      // the probe's stdout; fails with ErrProbeResponseTooLarge past the limit

	ctx       context.Context
	cancel    context.CancelFunc
	cmd       *exec.Cmd
	group     *probeGroup
	stdinW    *os.File
	stdoutR   *os.File
	stopAfter func() bool
	closeIn   sync.Once
	closeOut  sync.Once
}

// StartProbe starts c. A command that is missing, not executable or fails to start returns a
// *StartError.
func StartProbe(ctx context.Context, c ProbeCommand) (*Probe, error) {
	if len(c.Argv) == 0 || c.Argv[0] == "" {
		return nil, &StartError{Err: errors.New("empty command")}
	}
	fail := func(err error) (*Probe, error) { return nil, &StartError{Command: c.Argv[0], Err: err} }
	path, err := exec.LookPath(c.Argv[0])
	if err != nil {
		return fail(err)
	}
	var cancel context.CancelFunc
	if c.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	p := &Probe{ctx: ctx, cancel: cancel}
	if p.cmd, err = newCmd(ctx, path, c.Argv[1:]); err != nil {
		cancel()
		return fail(err)
	}
	env := c.Env
	if env == nil {
		env = os.Environ()
	}
	p.cmd.Env = childEnv(env)
	p.cmd.Dir = c.Dir
	p.cmd.WaitDelay = waitDelay
	p.group = newProbeGroup(p.cmd)
	p.cmd.Cancel = p.group.kill

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		cancel()
		return fail(err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		cancel()
		closeAll(stdinR, stdinW)
		return fail(err)
	}
	p.cmd.Stdin, p.cmd.Stdout = stdinR, stdoutW
	if err := p.cmd.Start(); err != nil {
		cancel()
		closeAll(stdinR, stdinW, stdoutR, stdoutW)
		return fail(err)
	}
	// the probe holds its own ends now
	closeAll(stdinR, stdoutW)
	p.group.started()

	limit := c.MaxResponse
	if limit <= 0 {
		limit = DefaultProbeResponseLimit
	}
	p.stdinW, p.stdoutR = stdinW, stdoutR
	p.Stdin = probeStdin{p}
	p.Stdout = &limitReader{r: stdoutR, left: limit, over: p.Kill}
	// a descendant outside the tree may keep stdout open; closing our end unblocks the reader
	p.stopAfter = context.AfterFunc(ctx, func() {
		_ = p.group.kill()
		p.closeStdout()
	})
	return p, nil
}

// Kill stops the probe's tree; Wait still has to be called.
func (p *Probe) Kill() {
	p.cancel()
}

// Wait closes the probe's stdin, waits for it to exit and kills what is left of its tree. It returns
// the exit code, and the context's error when the probe was stopped by it (a timeout or cancellation).
func (p *Probe) Wait() (int, error) {
	p.closeStdin()
	_ = p.cmd.Wait()
	p.group.release()
	p.stopAfter()
	p.closeStdout()
	err := p.ctx.Err()
	p.cancel()
	if err != nil {
		return exitCode(p.cmd.ProcessState), fmt.Errorf("probe %q stopped: %w", p.cmd.Args[0], err)
	}
	return exitCode(p.cmd.ProcessState), nil
}

func (p *Probe) closeStdin()  { p.closeIn.Do(func() { _ = p.stdinW.Close() }) }
func (p *Probe) closeStdout() { p.closeOut.Do(func() { _ = p.stdoutR.Close() }) }

// ProbeResult is a finished probe.
type ProbeResult struct {
	ExitCode int
	Stdout   []byte
}

// RunProbe runs c to completion with stdin as its whole input and returns its stdout. The error is a
// *StartError, ErrProbeResponseTooLarge, or the context's error when the probe timed out or was
// canceled; a probe that exits nonzero is not an error.
func RunProbe(ctx context.Context, c ProbeCommand, stdin []byte) (ProbeResult, error) {
	p, err := StartProbe(ctx, c)
	if err != nil {
		return ProbeResult{}, err
	}
	go func() {
		_, _ = p.Stdin.Write(stdin) // a probe that stops reading ends the write with EPIPE
		_ = p.Stdin.Close()
	}()
	out, readErr := io.ReadAll(p.Stdout)
	if readErr != nil {
		p.Kill()
	}
	code, waitErr := p.Wait()
	switch {
	case errors.Is(readErr, ErrProbeResponseTooLarge):
		return ProbeResult{ExitCode: code}, ErrProbeResponseTooLarge
	case waitErr != nil:
		return ProbeResult{ExitCode: code}, waitErr
	case readErr != nil:
		return ProbeResult{ExitCode: code}, fmt.Errorf("read probe response: %w", readErr)
	}
	return ProbeResult{ExitCode: code, Stdout: out}, nil
}

// probeStdin is the probe's stdin; closing it more than once is harmless.
type probeStdin struct{ p *Probe }

func (s probeStdin) Write(b []byte) (int, error) { return s.p.stdinW.Write(b) } //nolint:wrapcheck // a pipe write

func (s probeStdin) Close() error {
	s.p.closeStdin()
	return nil
}

// limitReader reads up to left bytes and fails with ErrProbeResponseTooLarge, calling over, when
// there is more.
type limitReader struct {
	r    io.Reader
	left int64
	over func()
}

func (l *limitReader) Read(b []byte) (int, error) {
	if l.left <= 0 {
		var one [1]byte
		if n, err := l.r.Read(one[:]); n == 0 {
			return 0, err //nolint:wrapcheck // io.EOF must stay unwrapped
		}
		l.over()
		return 0, ErrProbeResponseTooLarge
	}
	if int64(len(b)) > l.left {
		b = b[:l.left]
	}
	n, err := l.r.Read(b)
	l.left -= int64(n)
	return n, err //nolint:wrapcheck // io.EOF must stay unwrapped
}

func closeAll(files ...*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}
