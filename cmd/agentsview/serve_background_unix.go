//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

func configureServeBackgroundCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// terminateProcess asks the server to shut down gracefully. The server traps
// SIGTERM and exits cleanly, removing its runtime record.
func terminateProcess(proc *os.Process) error {
	return proc.Signal(syscall.SIGTERM)
}
