//go:build windows

package main

import (
	"fmt"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// jobCaddyGuard holds a job-object handle. While the server process keeps it
// open, the job lives; when the server exits and the OS closes its handles, the
// job's KILL_ON_JOB_CLOSE limit terminates every process in it, including Caddy.
type jobCaddyGuard struct {
	job windows.Handle
}

func (g jobCaddyGuard) Close() error {
	return windows.CloseHandle(g.job)
}

// newCaddyGuard assigns the started Caddy process to a job object configured to
// kill its processes when the last handle to the job closes. The server holds
// that handle for its lifetime, so when the server is terminated -- including
// the uncatchable TerminateProcess that `serve stop` issues on Windows -- the
// OS tears down the job and the managed Caddy child with it, instead of leaving
// Caddy holding the public port. On any failure it returns a no-op guard so the
// caller can keep running with the prior (leak-prone) behavior.
func newCaddyGuard(cmd *exec.Cmd) (caddyGuard, error) {
	if cmd == nil || cmd.Process == nil {
		return noopCaddyGuard{}, fmt.Errorf("caddy process not started")
	}

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return noopCaddyGuard{}, fmt.Errorf("creating job object: %w", err)
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return noopCaddyGuard{}, fmt.Errorf("configuring job object: %w", err)
	}

	proc, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(cmd.Process.Pid),
	)
	if err != nil {
		_ = windows.CloseHandle(job)
		return noopCaddyGuard{}, fmt.Errorf("opening caddy process: %w", err)
	}
	defer func() { _ = windows.CloseHandle(proc) }()

	if err := windows.AssignProcessToJobObject(job, proc); err != nil {
		_ = windows.CloseHandle(job)
		return noopCaddyGuard{}, fmt.Errorf("assigning caddy to job: %w", err)
	}

	return jobCaddyGuard{job: job}, nil
}
