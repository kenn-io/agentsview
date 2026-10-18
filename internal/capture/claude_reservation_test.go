package capture

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaudeReservationGuardWaitsForRelease(t *testing.T) {
	dir := t.TempDir()
	guard, err := lockReservationGuard(t.Context(), dir)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, guard.Unlock()) })

	done := make(chan error, 1)
	go func() {
		next, err := lockReservationGuard(t.Context(), dir)
		if err == nil {
			err = next.Unlock()
		}
		done <- err
	}()

	// A valid reservation can hold the registry beyond the former one-second cap.
	select {
	case err := <-done:
		require.FailNow(t, "registry waiter stopped before release", "%v", err)
	case <-time.After(2 * time.Second):
	}
	require.NoError(t, guard.Unlock())
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(20 * time.Second):
		require.FailNow(t, "registry waiter did not acquire the released lock")
	}
}

func TestClaudeReservationGuardHonorsCancellation(t *testing.T) {
	dir := t.TempDir()
	guard, err := lockReservationGuard(t.Context(), dir)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, guard.Unlock()) })

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		next, err := lockReservationGuard(ctx, dir)
		if err == nil {
			_ = next.Unlock()
		}
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(20 * time.Second):
		require.FailNow(t, "registry waiter ignored cancellation")
	}
}

func TestClaudeSessionReservationCrossesProcesses(t *testing.T) {
	if os.Getenv("AGENTSVIEW_CAPTURE_RESERVATION_PROBE") == "1" {
		reservation, err := reserveClaudeSession(
			t.Context(),
			os.Getenv("AGENTSVIEW_CAPTURE_RESERVATION_ROOT"),
			os.Getenv("AGENTSVIEW_CAPTURE_RESERVATION_SESSION"),
		)
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(23)
		}
		reservation.close()
		return
	}

	root := t.TempDir()
	secureTestProviderRoot(root)
	sessionID := "55555555-5555-4555-8555-555555555555"
	reservation, err := reserveClaudeSession(
		t.Context(), root, sessionID,
	)
	require.NoError(t, err)
	t.Cleanup(reservation.close)

	command := func() *exec.Cmd {
		cmd := exec.CommandContext(t.Context(), os.Args[0],
			"-test.run=^TestClaudeSessionReservationCrossesProcesses$")
		cmd.Env = append(os.Environ(),
			"AGENTSVIEW_CAPTURE_RESERVATION_PROBE=1",
			"AGENTSVIEW_CAPTURE_RESERVATION_ROOT="+root,
			"AGENTSVIEW_CAPTURE_RESERVATION_SESSION="+sessionID,
		)
		return cmd
	}
	output, err := command().CombinedOutput()
	require.Error(t, err)
	assert.Contains(t, string(output), "already reserved")

	reservation.close()
	output, err = command().CombinedOutput()
	require.NoError(t, err, string(output))
}
