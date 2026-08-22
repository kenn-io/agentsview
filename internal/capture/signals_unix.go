//go:build !windows

package capture

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
)

func configureChildProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func registerChildSignals() chan os.Signal {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	return ch
}

func unregisterChildSignals(ch chan os.Signal) {
	signal.Stop(ch)
}

func forwardSignals(
	process *os.Process, ch chan os.Signal,
) func() (string, int) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	var receipt struct {
		sync.Mutex
		signal syscall.Signal
		count  int
	}
	handle := func(sig os.Signal, forward bool) {
		unixSignal, ok := sig.(syscall.Signal)
		if !ok {
			return
		}
		receipt.Lock()
		if receipt.count == 0 {
			receipt.signal = unixSignal
		}
		receipt.count++
		count := receipt.count
		receipt.Unlock()
		if !forward {
			return
		}
		if count == 1 {
			_ = syscall.Kill(-process.Pid, unixSignal)
			return
		}
		_ = syscall.Kill(-process.Pid, syscall.SIGKILL)
	}
	wg.Go(func() {
		for {
			select {
			case sig := <-ch:
				handle(sig, true)
			case <-done:
				for {
					select {
					case sig := <-ch:
						handle(sig, false)
					default:
						return
					}
				}
			}
		}
	})
	return func() (string, int) {
		unregisterChildSignals(ch)
		close(done)
		wg.Wait()
		receipt.Lock()
		defer receipt.Unlock()
		return signalNameAndCode(receipt.signal)
	}
}

func processSignal(state *os.ProcessState) (string, int) {
	status, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return "", 0
	}
	return signalNameAndCode(status.Signal())
}

func signalNameAndCode(sig syscall.Signal) (string, int) {
	if sig == 0 {
		return "", 0
	}
	name := fmt.Sprintf("SIG%d", sig)
	switch sig {
	case syscall.SIGINT:
		name = "SIGINT"
	case syscall.SIGTERM:
		name = "SIGTERM"
	}
	return name, 128 + int(sig)
}
