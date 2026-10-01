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
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
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

// This re-executes the test binary under production memory limits. Race
// tracing can exhaust those limits while parsing the SQLite fixture.
func TestSandboxProviderJSONAndSQLite(t *testing.T) {
	p, err := NewSubprocessParser(20 * time.Second)
	require.NoError(t, err)
	if err = p.Preflight(t.Context()); err != nil {
		if os.Getenv("RAW_SANDBOX_REQUIRED") == "1" {
			require.NoError(t, err)
		}
		t.Skip("kernel isolation unavailable")
	}
	fixtures := []struct {
		name      string
		provider  parser.AgentType
		path, key string
		body      []byte
		ids       []string
	}{
		{"json", parser.AgentClaude, "project/session-a.jsonl", "/canonical/project/session-a.jsonl", []byte(`{"type":"user","timestamp":"2026-08-13T12:00:00Z","uuid":"u1","sessionId":"session-a","message":{"content":"hello"},"cwd":"/work/project"}` + "\n" + `{"type":"assistant","timestamp":"2026-08-13T12:00:01Z","uuid":"a1","parentUuid":"u1","sessionId":"session-a","message":{"content":"hi"}}` + "\n"), []string{"session-a"}},
		{"sqlite", parser.AgentForge, parser.ForgeDBFilename, parser.ForgeDBFilename, forgeSnapshotFixture(t, map[string]string{"conv-001": "2026-05-02 09:58:15", "conv-002": "2026-05-03 09:58:15"}), []string{"forge:conv-001", "forge:conv-002"}},
	}
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			ref := objectRefForBytes(t, f.body)
			m := parserTestManifest(t, f.provider, f.key, []rawsync.Entry{{Path: f.path, Type: "file", Length: int64(len(f.body)), Objects: []rawsync.ObjectRef{ref}}})
			tree, err := (Materializer{Store: &materializerStore{objects: map[rawsync.ObjectRef][]byte{ref: f.body}}, BaseDir: t.TempDir(), MaxTotalBytes: 1 << 20}).Materialize(t.Context(), m)
			require.NoError(t, err)
			defer func() { require.NoError(t, tree.Cleanup()) }()
			got, err := p.Parse(t.Context(), m, tree)
			require.NoError(t, err)
			require.Len(t, got.Outcome.Results, len(f.ids))
			assert.True(t, got.Outcome.ResultSetComplete)
			assert.Empty(t, got.Outcome.SourceErrors)
			for i, id := range f.ids {
				assert.Equal(t, id, got.Outcome.Results[i].Result.Session.ID)
				require.Len(t, got.Outcome.Results[i].Result.Messages, 2)
			}
			plain, err := NewProviderParser(parser.ProviderFactories(), "hosted")
			require.NoError(t, err)
			expected, err := plain.Parse(t.Context(), m, tree)
			require.NoError(t, err)
			assert.Equal(t, expected, got, "kernel boundary preserves complete normalized provider semantics")
		})
	}
}
