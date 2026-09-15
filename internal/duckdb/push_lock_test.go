//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestPushSerializesMirrorWriters(t *testing.T) {
	if path := os.Getenv("AGENTSVIEW_TEST_PUSH_MIRROR"); path != "" {
		local, err := db.OpenReadOnly(os.Getenv("AGENTSVIEW_TEST_PUSH_ARCHIVE"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, local.Close()) })
		blocked := os.Getenv("AGENTSVIEW_TEST_PUSH_BLOCKED") == "1"
		timeout := 30 * time.Second
		if blocked {
			timeout = 2 * time.Second
		}
		ctx, cancel := context.WithTimeout(t.Context(), timeout)
		defer cancel()
		_, err = Push(ctx, path, local, "m", SyncOptions{}, false, nil)
		if blocked {
			require.ErrorIs(t, err, context.DeadlineExceeded,
				"a competing push must wait before opening or replacing the mirror")
		} else {
			require.NoError(t, err, "the completed push must release the mirror")
		}
		return
	}

	executable, err := os.Executable()
	require.NoError(t, err)
	for _, full := range []bool{true, false} {
		name := "incremental"
		if full {
			name = "rebuild"
		}
		t.Run(name, func(t *testing.T) {
			local, path := newPushFixture(t, 1)
			_, err := Push(t.Context(), path, local, "m", SyncOptions{}, false, nil)
			require.NoError(t, err)
			appendMessage(t, local, "sess-1")
			runCompetitor := func(blocked string) {
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, executable, "-test.run=^TestPushSerializesMirrorWriters$")
				cmd.Env = append(os.Environ(),
					"AGENTSVIEW_TEST_PUSH_MIRROR="+path,
					"AGENTSVIEW_TEST_PUSH_ARCHIVE="+local.Path(),
					"AGENTSVIEW_TEST_PUSH_BLOCKED="+blocked,
				)
				output, err := cmd.CombinedOutput()
				require.NoError(t, err, "%s", output)
			}
			progressCalled := false
			_, err = Push(t.Context(), path, local, "m", SyncOptions{}, full, func(PushProgress) {
				if !progressCalled {
					progressCalled = true
					runCompetitor("1")
				}
			})
			require.NoError(t, err)
			assert.True(t, progressCalled)
			runCompetitor("0")
			assertMirrorMessageCount(t, path, "sess-1", 3)
		})
	}
}
