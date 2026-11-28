//go:build !windows

package main

import (
	"context"
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

func TestDaemonRestartSIGTERMLeavesLaunchedReplacementRunning(t *testing.T) {
	cmd := exec.CommandContext(t.Context(),
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
	deps.writableRecords = func(context.Context, string, string) ([]daemon.RuntimeRecord, error) {
		return []daemon.RuntimeRecord{testWritableRecord(91, "/runtime/91.json")}, nil
	}
	deps.startBackground = func(ctx context.Context,
		_ config.Config, _ []string, _ serveReplacementOptions,
		policy backgroundLaunchPolicy,
	) (backgroundLaunchResult, error) {
		policy.OnLaunch(92, "/data/serve.log")
		// Signal only after launch. During the database-lock wait, cancellation
		// intentionally prevents a replacement from starting.
		require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGTERM))
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Second):
			require.FailNow(t, "SIGTERM did not cancel the startup wait")
		}
		require.ErrorIs(t, ctx.Err(), context.Canceled)
		t.Log("replacement launch observed canceled context")
		return backgroundLaunchResult{
			Started: true, childPID: 92, LogPath: "/data/serve.log",
		}, context.Canceled
	}

	err := executeDaemonCommand(t, *deps, out, "restart")
	require.Error(t, err)
	assert.ErrorContains(t, err, "child continues running")
}
