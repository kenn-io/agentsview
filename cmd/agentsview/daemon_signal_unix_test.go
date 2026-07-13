//go:build !windows

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/kit/daemon"
)

func TestDaemonRestartSIGTERMStillLaunchesReplacement(t *testing.T) {
	cmd := exec.Command(
		os.Args[0], "-test.v",
		"-test.run=^TestDaemonRestartSIGTERMHelperProcess$",
	)
	cmd.Env = append(os.Environ(), "AGENTSVIEW_RESTART_SIGTERM_HELPER=1")
	out, err := cmd.CombinedOutput()

	require.NoError(t, err, string(out))
	assert.Contains(t, string(out), "replacement launch observed canceled context")
}

func TestDaemonRestartSIGTERMHelperProcess(t *testing.T) {
	if os.Getenv("AGENTSVIEW_RESTART_SIGTERM_HELPER") != "1" {
		t.Skip("subprocess helper")
	}

	deps, out := daemonCommandTestDeps(t)
	deps.writableRecords = func(string, string) ([]daemon.RuntimeRecord, error) {
		return []daemon.RuntimeRecord{testWritableRecord(91, "/runtime/91.json")}, nil
	}
	deps.stopProcess = func(daemon.RuntimeRecord, time.Duration) error {
		return syscall.Kill(os.Getpid(), syscall.SIGTERM)
	}
	deps.startBackground = func(
		_ config.Config, _ []string, _ serveReplacementOptions,
		policy backgroundLaunchPolicy,
	) (backgroundLaunchResult, error) {
		require.NotNil(t, policy.Context)
		require.Eventually(t, func() bool {
			return errors.Is(policy.Context.Err(), context.Canceled)
		}, time.Second, time.Millisecond)
		t.Log("replacement launch observed canceled context")
		return backgroundLaunchResult{
			Started: true, childPID: 92, LogPath: "/data/serve.log",
		}, context.Canceled
	}

	err := executeDaemonCommand(t, *deps, out, "restart")
	require.Error(t, err)
	assert.ErrorContains(t, err, "child continues running")
}
