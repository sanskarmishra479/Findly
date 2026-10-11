//go:build windows

// Package sidecar runs helper programs (llama-server, later ocr-helper and ffmpeg) as
// child processes. Every child:
//   - gets only the environment variables it is given (nothing inherited),
//   - runs without a console window, optionally at below-normal priority,
//   - is placed in a Windows job object that kills it when Findly exits or crashes,
//     so no helper is ever left running in the background.
package sidecar

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Options for Start.
type Options struct {
	Env         []string  // the complete environment; SystemRoot is added automatically
	LowPriority bool      // BELOW_NORMAL_PRIORITY_CLASS (plan 5: during indexing)
	Output      io.Writer // stdout+stderr of the child; nil discards
}

// Proc is a running child process.
type Proc struct {
	cmd  *exec.Cmd
	job  windows.Handle
	done chan struct{}
	err  error // exit error, valid after done is closed

	stopOnce sync.Once
}

// Start launches exe with args. exe must be an absolute path.
func Start(exe string, args []string, opts Options) (*Proc, error) {
	if !filepath.IsAbs(exe) {
		return nil, fmt.Errorf("sidecar: %q must be an absolute path", exe)
	}
	job, err := newKillOnCloseJob()
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(exe, args...)
	cmd.Dir = filepath.Dir(exe)
	cmd.Env = append([]string{"SystemRoot=" + os.Getenv("SystemRoot")}, opts.Env...)
	out := opts.Output
	if out == nil {
		out = io.Discard
	}
	cmd.Stdout, cmd.Stderr = out, out
	flags := uint32(windows.CREATE_NO_WINDOW)
	if opts.LowPriority {
		flags |= windows.BELOW_NORMAL_PRIORITY_CLASS
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: flags}
	// If a grandchild keeps the output pipe open after the child exits, stop waiting for
	// it after this delay instead of blocking Wait (and Stop) indefinitely.
	cmd.WaitDelay = 2 * time.Second

	if err := cmd.Start(); err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("sidecar: start %s: %w", filepath.Base(exe), err)
	}
	if err := assign(job, cmd.Process.Pid); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		windows.CloseHandle(job)
		return nil, err
	}

	p := &Proc{cmd: cmd, job: job, done: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

// Done is closed when the process exits.
func (p *Proc) Done() <-chan struct{} { return p.done }

// Exited reports whether the process has exited, and its exit error.
func (p *Proc) Exited() (bool, error) {
	select {
	case <-p.done:
		return true, p.err
	default:
		return false, nil
	}
}

// Pid returns the process id.
func (p *Proc) Pid() int { return p.cmd.Process.Pid }

// Stop kills the process and waits for it to exit. Safe to call more than once.
func (p *Proc) Stop() {
	p.stopOnce.Do(func() {
		// Terminating the job kills the child and anything it started.
		windows.TerminateJobObject(p.job, 1)
		p.cmd.Process.Kill()
		<-p.done
		windows.CloseHandle(p.job)
	})
}

// newKillOnCloseJob creates a job object whose processes die when its last handle closes.
// Findly holds that handle, so if Findly exits or crashes, Windows kills the children.
func newKillOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("sidecar: create job: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return 0, fmt.Errorf("sidecar: configure job: %w", err)
	}
	return job, nil
}

func assign(job windows.Handle, pid int) error {
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("sidecar: open process: %w", err)
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		return fmt.Errorf("sidecar: assign job: %w", err)
	}
	return nil
}

// Tail keeps the last Size bytes written to it, for error messages from a helper's
// output without storing everything. Safe for concurrent use.
type Tail struct {
	Size int
	mu   sync.Mutex
	buf  []byte
}

func (t *Tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(p) >= t.Size {
		// Keep only the end, in a fresh buffer, so one huge write does not keep a huge
		// backing array alive.
		t.buf = append(make([]byte, 0, t.Size), p[len(p)-t.Size:]...)
		return len(p), nil
	}
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.Size; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

func (t *Tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// ErrExited is returned when an operation needs a process that has exited.
var ErrExited = errors.New("sidecar: process exited")

// Note: a child could start its own children before AssignProcessToJobObject runs; those
// would escape the job. llama-server starts none. If a future helper does, start it
// suspended and assign it before resuming.
