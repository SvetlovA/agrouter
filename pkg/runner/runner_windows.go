//go:build windows

package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// jobExitCode is what the Job Object reports for every process it kills.
const jobExitCode = 1

// newCmd returns the command for path. A batch file (an npm .cmd shim) runs through cmd.exe with a
// command line quoted so the arguments reach the program behind it intact (see batchCommandLine).
func newCmd(ctx context.Context, path string, args []string) (*exec.Cmd, error) {
	if !isBatchFile(path) {
		return exec.CommandContext(ctx, path, args...), nil
	}
	line, err := batchCommandLine(path, args)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, comspec())
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: line}
	return cmd, nil
}

func comspec() string {
	if c := os.Getenv("ComSpec"); c != "" {
		return c
	}
	return filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
}

// tree is the child's Job Object with kill-on-close: the child and every descendant it spawns, killed
// together on cancellation and when the job handle is closed after the child exits. Without a job the
// tree degrades to the direct child. The child stays in agrouter's console group so it gets Ctrl+C
// itself; agrouter only ignores it meanwhile and returns the child's exit code.
type tree struct {
	cmd    *exec.Cmd
	mu     sync.Mutex
	job    windows.Handle
	killed bool
	sigs   chan os.Signal
	done   chan struct{}
	once   sync.Once
}

func newTree(cmd *exec.Cmd) *tree {
	t := &tree{cmd: cmd, job: windows.InvalidHandle, sigs: make(chan os.Signal, 4), done: make(chan struct{})}
	signal.Notify(t.sigs, os.Interrupt)
	return t
}

// started assigns the child to a new job. A descendant spawned between Start and the assignment
// escapes it: os/exec cannot start the child suspended, and the window is far shorter than a CLI's
// startup.
func (t *tree) started() {
	go func() {
		for {
			select {
			case <-t.sigs:
			case <-t.done:
				return
			}
		}
	}()
	job, err := assignToNewJob(t.cmd.Process.Pid)
	if err != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.job = job
	if t.killed {
		_ = windows.TerminateJobObject(job, jobExitCode)
	}
}

// cancel kills the tree on cancellation.
func (t *tree) cancel() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.killed = true
	if t.job != windows.InvalidHandle {
		if err := windows.TerminateJobObject(t.job, jobExitCode); err != nil {
			return fmt.Errorf("terminate job: %w", err)
		}
		return nil
	}
	return t.cmd.Process.Kill() //nolint:wrapcheck // returned to exec.Cmd.Cancel
}

// release stops ignoring Ctrl+C and closes the job, killing descendants that outlived the child.
func (t *tree) release() {
	t.once.Do(func() {
		signal.Stop(t.sigs)
		close(t.done)
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.job != windows.InvalidHandle {
			_ = windows.CloseHandle(t.job)
			t.job = windows.InvalidHandle
		}
	})
}

// assignToNewJob creates a kill-on-close job and assigns the process pid to it.
func assignToNewJob(pid int) (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return windows.InvalidHandle, fmt.Errorf("create job object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	//nolint:gosec // G103: unsafe.Pointer is how SetInformationJobObject takes its struct
	if _, setErr := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); setErr != nil {
		_ = windows.CloseHandle(job)
		return windows.InvalidHandle, fmt.Errorf("set job limits: %w", setErr)
	}
	// os.Process keeps its handle unexported; the live handle pins the pid, so reopening cannot race
	// onto a recycled process
	proc, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		_ = windows.CloseHandle(job)
		return windows.InvalidHandle, fmt.Errorf("open process %d: %w", pid, err)
	}
	defer windows.CloseHandle(proc)
	if err = windows.AssignProcessToJobObject(job, proc); err != nil {
		_ = windows.CloseHandle(job)
		return windows.InvalidHandle, fmt.Errorf("assign process to job: %w", err)
	}
	return job, nil
}

// exitCode is the child's exit code.
func exitCode(state *os.ProcessState) int { return state.ExitCode() }
