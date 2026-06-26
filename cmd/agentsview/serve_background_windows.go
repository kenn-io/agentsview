//go:build windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

const (
	createNewProcessGroup = 0x00000200
	createNoWindow        = 0x08000000
)

func configureServeBackgroundCommand(cmd *exec.Cmd) {
	// Use CREATE_NO_WINDOW rather than DETACHED_PROCESS. A detached
	// process has no console at all, so every console child it spawns
	// (notably git, invoked while resolving a session's repo during
	// sync) forces Windows to allocate a brand-new console window that
	// flashes on screen. CREATE_NO_WINDOW instead gives the daemon a
	// hidden console that those children inherit, so they run without
	// popping a window. CREATE_NEW_PROCESS_GROUP still isolates it from
	// the launching terminal's Ctrl-C.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createNoWindow | createNewProcessGroup,
	}
}

// terminateProcess stops the server. Windows does not deliver POSIX signals to
// a background process, so there is no graceful equivalent of SIGTERM here; the
// process is killed and serve stop removes its runtime record afterward.
func terminateProcess(proc *os.Process) error {
	return proc.Kill()
}
