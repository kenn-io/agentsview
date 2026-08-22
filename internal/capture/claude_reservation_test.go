package capture

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaudeSessionReservationCrossesProcesses(t *testing.T) {
	if os.Getenv("AGENTSVIEW_CAPTURE_RESERVATION_PROBE") == "1" {
		reservation, err := reserveClaudeSession(
			context.Background(),
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
		cmd := exec.Command(os.Args[0],
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
