//go:build !windows

package capture

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInterruptedChildStillSealsRecoverableUsage(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(t.TempDir(), "child-started")
	captureDir := filepath.Join(t.TempDir(), "capture")
	resultPath := filepath.Join(t.TempDir(), "result.json")
	workDir := t.TempDir()
	producer := copyCaptureHelper(t, "claude")
	env := append(
		helperEnvironment(root, "claude-wait-signal", 0),
		"AGENTSVIEW_CAPTURE_TEST_SIGNAL_MARKER="+marker,
	)
	type response struct {
		outcome RunOutcome
		err     error
	}
	done := make(chan response, 1)
	limits := testLimits()
	limits.FinalizationWait = 3 * time.Second
	go func() {
		outcome, err := Run(context.Background(), RunOptions{
			Provider: ProviderClaude, OccurrenceID: "interrupted",
			CaptureDir: captureDir, ResultPath: resultPath,
			ProviderRoot: root, WorkDir: workDir,
			Command: []string{producer, "-p", "prompt"}, Environment: env,
			Streams: Streams{Stdout: io.Discard, Stderr: io.Discard},
			Limits:  limits, CustomPricing: testPricing(),
		})
		done <- response{outcome: outcome, err: err}
	}()
	childProcessGroup := waitForCaptureSignalMarker(t, marker)
	assert.NotEqual(t, syscall.Getpgrp(), childProcessGroup)
	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGTERM))

	var got response
	select {
	case got = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("capture did not finish after forwarding SIGTERM")
	}
	require.NoError(t, got.err)
	assert.Equal(t, 128+int(syscall.SIGTERM), got.outcome.ExitCode)
	data, err := os.ReadFile(resultPath)
	require.NoError(t, err)
	result, err := DecodeResult(bytes.NewReader(data))
	require.NoError(t, err)
	assert.Equal(t, "SIGTERM", result.Execution.Signal)
	require.NotNil(t, result.Usage)

	var replay bytes.Buffer
	_, err = Report(context.Background(), ReportOptions{
		CaptureDir: captureDir, ResultPath: "-", Stdout: &replay,
		CustomPricing: testPricing(),
	})
	require.NoError(t, err)
	assert.Equal(t, data, replay.Bytes())
}

func TestWrapperSignalOverridesSuccessfulChildExit(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(t.TempDir(), "child-started")
	handledMarker := filepath.Join(t.TempDir(), "signal-handled")
	producer := copyCaptureHelper(t, "claude")
	workDir := t.TempDir()
	env := append(
		helperEnvironment(root, "claude-trap-signal", 0),
		"AGENTSVIEW_CAPTURE_TEST_SIGNAL_MARKER="+marker,
		"AGENTSVIEW_CAPTURE_TEST_SIGNAL_HANDLED_MARKER="+handledMarker,
	)
	type response struct {
		outcome ExecutionOutcome
		code    int
		err     error
	}
	done := make(chan response, 1)
	go func() {
		outcome, code, _, err := runChild(
			[]string{
				producer, "-p", "prompt", "--session-id",
				"11111111-1111-4111-8111-111111111111",
			},
			env, workDir, Streams{Stdout: io.Discard, Stderr: io.Discard}, nil,
		)
		done <- response{outcome: outcome, code: code, err: err}
	}()
	childProcessGroup := waitForCaptureSignalMarker(t, marker)
	assert.NotEqual(t, syscall.Getpgrp(), childProcessGroup)
	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGTERM))

	select {
	case got := <-done:
		require.NoError(t, got.err)
		assert.Equal(t, "SIGTERM", got.outcome.Signal)
		assert.Equal(t, 128+int(syscall.SIGTERM), got.code)
		assert.FileExists(t, handledMarker)
	case <-time.After(2 * time.Second):
		t.Fatal("capture did not retain the wrapper signal after the child exited")
	}
}

func TestRepeatedWrapperSignalEscalatesIgnoredChild(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(t.TempDir(), "child-started")
	producer := copyCaptureHelper(t, "claude")
	workDir := t.TempDir()
	env := append(
		helperEnvironment(root, "claude-ignore-signal", 0),
		"AGENTSVIEW_CAPTURE_TEST_SIGNAL_MARKER="+marker,
	)
	type response struct {
		outcome ExecutionOutcome
		code    int
		err     error
	}
	done := make(chan response, 1)
	go func() {
		outcome, code, _, err := runChild(
			[]string{
				producer, "-p", "prompt", "--session-id",
				"22222222-2222-4222-8222-222222222222",
			},
			env, workDir, Streams{Stdout: io.Discard, Stderr: io.Discard}, nil,
		)
		done <- response{outcome: outcome, code: code, err: err}
	}()
	childProcessGroup := waitForCaptureSignalMarker(t, marker)
	require.NotEqual(t, syscall.Getpgrp(), childProcessGroup)
	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGTERM))
	select {
	case early := <-done:
		t.Fatalf("signal-ignoring child exited before escalation: %+v", early)
	case <-time.After(50 * time.Millisecond):
	}
	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGTERM))

	select {
	case got := <-done:
		require.NoError(t, got.err)
		assert.Equal(t, "SIGTERM", got.outcome.Signal)
		assert.Equal(t, 128+int(syscall.SIGTERM), got.code)
	case <-time.After(2 * time.Second):
		require.Positive(t, childProcessGroup)
		_ = syscall.Kill(-childProcessGroup, syscall.SIGKILL)
		t.Fatal("repeated signal did not terminate the child process group")
	}
}

func TestForwardSignalsDeliversSignalBufferedBeforeChildAvailable(t *testing.T) {
	producer := copyCaptureHelper(t, "claude")
	cmd := exec.Command(producer, "-p", "prompt")
	cmd.Env = helperEnvironment(t.TempDir(), "claude-wait-signal", 0)
	configureChildProcess(cmd)
	signals := make(chan os.Signal, 2)
	signals <- syscall.SIGTERM
	require.NoError(t, cmd.Start())
	stopForwarding := forwardSignals(cmd.Process, signals)

	err := cmd.Wait()
	wrapperSignal, wrapperCode := stopForwarding()

	require.Error(t, err)
	assert.Equal(t, "SIGTERM", wrapperSignal)
	assert.Equal(t, 128+int(syscall.SIGTERM), wrapperCode)
	childSignal, childCode := processSignal(cmd.ProcessState)
	assert.Equal(t, "SIGTERM", childSignal)
	assert.Equal(t, 128+int(syscall.SIGTERM), childCode)
}

func waitForCaptureSignalMarker(t *testing.T, marker string) int {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if data, err := os.ReadFile(marker); err == nil {
			group, parseErr := strconv.Atoi(string(data))
			if parseErr == nil {
				return group
			}
		}
		select {
		case <-deadline:
			t.Fatal("child did not write signal marker")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func captureHelperProcessGroupID() int {
	return syscall.Getpgrp()
}

func captureHelperWaitForSignal(mode, marker string) {
	var ch chan os.Signal
	switch mode {
	case "claude-trap-signal":
		ch = make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	case "claude-ignore-signal":
		signal.Ignore(os.Interrupt, syscall.SIGTERM)
	}
	_ = os.WriteFile(
		marker, fmt.Appendf(nil, "%d", captureHelperProcessGroupID()), 0o600,
	)
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
