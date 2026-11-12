//go:build unix

package main

import (
	"os"
	"os/exec"
	"syscall"
)

func configureRestartTestProcess(*exec.Cmd)                 {}
func interruptRestartTestProcess(process *os.Process) error { return process.Signal(syscall.SIGTERM) }
