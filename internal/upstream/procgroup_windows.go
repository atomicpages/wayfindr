//go:build windows

package upstream

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// startNewGroup creates cmd's child suspended, so it runs no code (and
// starts no process) until newProcGroup has put it in a job object and
// resumed it. A caller that starts cmd must pass the process to
// newProcGroup, or the child stays suspended.
func startNewGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}

	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
}

// procGroup is a started stdio child and the job object holding it and
// every process it starts. The job is created with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, so closing its last handle (release,
// or the daemon exiting for any reason) kills everything in it.
//
// Untested: CI and development run on unix, so none of this file has run.
// TestStoppingStdioChildStopsItsGrandchildren in procgroup_windows_test.go
// covers it once Windows CI exists, but even there it can't force a
// grandchild into the gap that starting suspended closes; it only shows
// that grandchildren end up in the job. Known gaps:
//   - Windows has no group-wide terminate signal, so stopping skips
//     straight from the closed stdin to TerminateJobObject.
//   - If the job can't be set up (say the daemon runs in a job that forbids
//     nesting), job is zero and only the child itself is killed.
type procGroup struct {
	proc *os.Process
	job  windows.Handle
}

// newProcGroup puts p, started suspended by startNewGroup, in a new
// kill-on-close job object and then resumes it. If the job can't be set up
// it still resumes p and returns a usable procGroup that stops p alone,
// with the error. If p can't be resumed it kills p rather than leave it
// suspended, and returns the error.
func newProcGroup(p *os.Process) (*procGroup, error) {
	g := &procGroup{proc: p}
	pid := uint32(p.Pid) //nolint:gosec // G115: a PID fits in a DWORD.
	job, jobErr := newJob(pid)

	if jobErr == nil {
		g.job = job
	}

	if err := resumeProcess(pid); err != nil {
		// A child left suspended would never answer, so its connect would
		// hang. Killing it fails the connect instead.
		_ = g.kill()
		_ = p.Kill()
		g.release()

		return g, errors.Join(jobErr, fmt.Errorf("resume stdio child: %w", err))
	}

	return g, jobErr
}

// newJob creates a kill-on-close job object and assigns process pid to it.
func newJob(pid uint32) (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)

	if err != nil {
		return 0, fmt.Errorf("create job object: %w", err)
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}

	//nolint:gosec // G103: the Win32 API takes the struct by pointer.
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)

		return 0, fmt.Errorf("set job object limits: %w", err)
	}

	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)

	if err != nil {
		_ = windows.CloseHandle(job)

		return 0, fmt.Errorf("open stdio child: %w", err)
	}

	defer func() { _ = windows.CloseHandle(h) }()

	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		_ = windows.CloseHandle(job)

		return 0, fmt.Errorf("assign stdio child to job object: %w", err)
	}

	return job, nil
}

// resumeProcess resumes every thread of process pid. os/exec closes the
// primary thread's handle once the process starts, so the threads are
// found through a Toolhelp32 snapshot instead. A process created suspended
// has just its primary thread, and its PID can't be reused while the
// os.Process holds a handle to it, so the snapshot finds exactly that
// thread.
//
// This uses documented Win32 calls that golang.org/x/sys/windows wraps. The
// alternative, NtResumeProcess, is a single call but is undocumented and
// would have to be loaded from ntdll by hand.
func resumeProcess(pid uint32) error {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)

	if err != nil {
		return fmt.Errorf("snapshot threads: %w", err)
	}

	defer func() { _ = windows.CloseHandle(snap) }()

	var entry windows.ThreadEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	resumed := 0

	for err = windows.Thread32First(snap, &entry); err == nil; err = windows.Thread32Next(snap, &entry) {
		if entry.OwnerProcessID != pid {
			continue
		}

		if rerr := resumeThread(entry.ThreadID); rerr != nil {
			return rerr
		}

		resumed++
	}

	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return fmt.Errorf("list threads: %w", err)
	}

	if resumed == 0 {
		return errors.New("no threads found")
	}

	return nil
}

// resumeThread decrements thread tid's suspend count once.
func resumeThread(tid uint32) error {
	h, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, tid)

	if err != nil {
		return fmt.Errorf("open thread %d: %w", tid, err)
	}

	defer func() { _ = windows.CloseHandle(h) }()

	if _, err := windows.ResumeThread(h); err != nil {
		return fmt.Errorf("resume thread %d: %w", tid, err)
	}

	return nil
}

// terminate is unsupported: Windows has no signal asking a process tree to
// exit.
func (g *procGroup) terminate() error { return errors.ErrUnsupported }

// kill terminates every process in the job, or the child alone if it has
// no job.
func (g *procGroup) kill() error {
	if g.job == 0 {
		return g.proc.Kill()
	}

	return windows.TerminateJobObject(g.job, 1)
}

// alive reports whether the job may still hold processes. Telling for sure
// would take a job query; since stopping goes straight to kill, which is
// harmless on an empty job, a job is taken to be alive until released.
func (g *procGroup) alive() bool { return g.job != 0 }

// release closes the job, which kills anything still in it.
func (g *procGroup) release() {
	if g.job != 0 {
		_ = windows.CloseHandle(g.job)
		g.job = 0
	}
}
