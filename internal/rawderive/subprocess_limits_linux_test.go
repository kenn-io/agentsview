//go:build linux && (amd64 || arm64) && !race

package rawderive

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Race tracing allocates outside Go's heap and exceeds the production
// address/data limits. Run these unchanged limits with the ordinary Linux
// binary; descriptor isolation and seccomp thread checks also run under race.
func TestSandboxKernelControls(t *testing.T) {
	p, err := NewSubprocessParser(5 * time.Second)
	require.NoError(t, err)
	if err = p.Preflight(t.Context()); err != nil {
		require.ErrorIs(t, err, ErrSandboxUnavailable)
		if os.Getenv("RAW_SANDBOX_REQUIRED") == "1" {
			require.FailNow(t, "mandatory kernel isolation unavailable")
		}
		t.Skip("kernel isolation unavailable; positive controls require Linux namespace support")
	}
	t.Setenv("RAW_SANDBOX_SECRET", "must-not-inherit")
	for _, mode := range []string{"denials", "memory", "cpu"} {
		t.Run(mode, func(t *testing.T) {
			source := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(source, "input"), []byte("fixture"), 0o400))
			jail := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(jail, "source"), 0o700))
			outside := filepath.Join(t.TempDir(), "sentinel")
			require.NoError(t, os.WriteFile(outside, []byte("outside"), 0o400))
			ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "--sandbox-kernel-test", mode, source, jail, outside)
			cmd.Env = []string{"GOMAXPROCS=2", "GOMEMLIMIT=384MiB"}
			require.NoError(t, configureParserNamespace(cmd))
			start := time.Now()
			out, err := runParserProcess(ctx, cancel, cmd, nil, parserOutputLimit, parserErrorLimit)
			require.NotNil(t, cmd.ProcessState)
			if mode == "denials" {
				require.NoError(t, err)
				var checks map[string]bool
				require.NoError(t, json.Unmarshal(out, &checks))
				require.GreaterOrEqual(t, len(checks), 14)
				for key, value := range checks {
					assert.True(t, value, key)
				}
			} else {
				require.Error(t, err)
				require.NoError(t, ctx.Err())
				assert.Less(t, time.Since(start), 38*time.Second)
				if mode == "cpu" {
					status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
					require.True(t, ok)
					assert.Equal(t, syscall.SIGKILL, status.Signal())
					assert.Greater(t, time.Since(start), 20*time.Second)
				} else {
					assert.Equal(t, 2, cmd.ProcessState.ExitCode(), "Go hard allocation failure")
				}
			}
			body, err := os.ReadFile(filepath.Join(source, "input"))
			require.NoError(t, err)
			assert.Equal(t, "fixture", string(body))
			require.NoError(t, p.Preflight(t.Context()), "subsequent jobs remain usable")
		})
	}
}
