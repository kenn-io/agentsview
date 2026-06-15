//go:build windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

const (
	detachedProcess       = 0x00000008
	createNewProcessGroup = 0x00000200
)

func configureServeBackgroundCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: detachedProcess | createNewProcessGroup,
	}
}

// terminateProcess stops the server. Windows does not deliver POSIX signals to
// a detached process, so there is no graceful equivalent of SIGTERM here; the
// process is killed and serve stop removes its runtime record afterward.
func terminateProcess(proc *os.Process) error {
	return proc.Kill()
}
