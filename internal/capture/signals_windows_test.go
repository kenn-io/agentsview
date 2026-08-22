//go:build windows

package capture

import (
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func captureHelperProcessGroupID() int { return 0 }

func captureHelperWaitForSignal(mode, marker string) {
	var ch chan os.Signal
	if mode == "claude-trap-signal" {
		ch = make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt)
	}
	if mode == "claude-ignore-signal" {
		signal.Ignore(os.Interrupt)
	}
	_ = os.WriteFile(marker, []byte("0"), 0o600)
	if ch != nil {
		<-ch
		_ = os.WriteFile(
			os.Getenv("AGENTSVIEW_CAPTURE_TEST_SIGNAL_HANDLED_MARKER"),
			[]byte("handled"), 0o600,
		)
		os.Exit(0)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func TestRelayWindowsSignalsForwardsRepeatedInterrupts(t *testing.T) {
	signals := make(chan os.Signal, 2)
	done := make(chan struct{})
	forwarded := make(chan os.Signal, 2)
	exited := make(chan struct{})
	received := make(chan os.Signal, 1)
	signals <- os.Interrupt
	signals <- os.Interrupt
	go func() {
		received <- relayWindowsSignals(signals, done, func(sig os.Signal) {
			forwarded <- sig
		})
		close(exited)
	}()

	for range 2 {
		select {
		case got := <-forwarded:
			assert.Equal(t, os.Interrupt, got)
		case <-time.After(time.Second):
			require.FailNow(t, "interrupt was not forwarded")
		}
	}
	close(done)
	select {
	case <-exited:
	case <-time.After(time.Second):
		require.FailNow(t, "signal relay did not stop")
	}
	assert.Equal(t, os.Interrupt, <-received)
}

func TestForwardSignalsRecordsInterruptAfterSuccessfulChild(t *testing.T) {
	if os.Getenv("AGENTSVIEW_CAPTURE_WINDOWS_EXIT_ZERO") == "1" {
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0],
		"-test.run=^TestForwardSignalsRecordsInterruptAfterSuccessfulChild$")
	cmd.Env = append(os.Environ(), "AGENTSVIEW_CAPTURE_WINDOWS_EXIT_ZERO=1")
	require.NoError(t, cmd.Start())
	require.NoError(t, cmd.Wait())
	signals := make(chan os.Signal, 2)
	stopForwarding := forwardSignals(cmd.Process, signals)
	signals <- os.Interrupt

	signalName, exitCode := stopForwarding()
	assert.Equal(t, "SIGINT", signalName)
	assert.Equal(t, 130, exitCode)
}

func TestForwardSignalsStopsBlockingChild(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(t.TempDir(), "child-started")
	producer := copyCaptureHelper(t, "claude")
	cmd := exec.Command(producer, "-p", "prompt")
	cmd.Env = append(
		helperEnvironment(root, "claude-ignore-signal", 0),
		"AGENTSVIEW_CAPTURE_TEST_SIGNAL_MARKER="+marker,
	)
	configureChildProcess(cmd)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
		}
	})
	require.Eventually(t, func() bool {
		_, err := os.Stat(marker)
		return err == nil
	}, time.Second, 10*time.Millisecond)

	signals := make(chan os.Signal, 2)
	stopForwarding := forwardSignals(cmd.Process, signals)
	signals <- os.Interrupt
	signals <- os.Interrupt
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err := <-waited:
		require.Error(t, err)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "blocking child survived repeated interrupts")
	}

	signalName, exitCode := stopForwarding()
	assert.Equal(t, "SIGINT", signalName)
	assert.Equal(t, 130, exitCode)
}
