//go:build windows

package capture

import (
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"

	"golang.org/x/sys/windows"
)

func configureChildProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP,
	}
}

func registerChildSignals() chan os.Signal {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt)
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
	var first os.Signal
	wg.Go(func() {
		interrupts := 0
		first = relayWindowsSignals(ch, done, func(sig os.Signal) {
			if sig != os.Interrupt {
				return
			}
			interrupts++
			if interrupts > 1 {
				_ = process.Kill()
				return
			}
			if err := windows.GenerateConsoleCtrlEvent(
				windows.CTRL_BREAK_EVENT, uint32(process.Pid),
			); err != nil {
				_ = process.Kill()
			}
		})
	})
	return func() (string, int) {
		unregisterChildSignals(ch)
		close(done)
		wg.Wait()
		if first == os.Interrupt {
			return "SIGINT", 130
		}
		return "", 0
	}
}

func relayWindowsSignals(
	signals <-chan os.Signal,
	done <-chan struct{},
	forward func(os.Signal),
) os.Signal {
	var first os.Signal
	handle := func(sig os.Signal) {
		if first == nil {
			first = sig
		}
		forward(sig)
	}
	for {
		select {
		case sig := <-signals:
			handle(sig)
		case <-done:
			for {
				select {
				case sig := <-signals:
					handle(sig)
				default:
					return first
				}
			}
		}
	}
}

func processSignal(*os.ProcessState) (string, int) { return "", 0 }
