package rawwatch

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawcapture"
	"go.kenn.io/agentsview/internal/rawcheckpoint"
	syncpkg "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestWorkerCodexRawCapturePrefersPlainRollout(t *testing.T) {
	for _, tc := range []struct {
		name     string
		audit    bool
		hasPlain bool
	}{
		{name: "audit siblings", audit: true, hasPlain: true},
		{name: "watch compressed sibling", hasPlain: true},
		{name: "audit compressed only", audit: true},
		{name: "watch compressed only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			day := filepath.Join(root, "2026", "09", "01")
			require.NoError(t, os.MkdirAll(day, 0o755))
			const uuid = "019eb791-cf7d-75c1-8439-9ed74c122b05"
			const name = "rollout-2026-09-01T10-00-00-" + uuid + ".jsonl"
			plain := filepath.Join(day, name)
			compressed := plain + ".zst"
			older := testjsonl.JoinJSONL(
				testjsonl.CodexSessionMetaJSON(uuid, "/work/project", "codex_cli_rs", "2026-09-01T10:00:00Z"),
				testjsonl.CodexMsgJSON("user", "one", "2026-09-01T10:00:01Z"),
			)
			newer := older + testjsonl.CodexMsgJSON("assistant", "new answer", "2026-09-01T10:00:02Z") + "\n"
			encoder, err := zstd.NewWriter(nil)
			require.NoError(t, err)
			compressedBytes := encoder.EncodeAll([]byte(older), nil)
			require.NoError(t, encoder.Close())
			require.NoError(t, os.WriteFile(compressed, compressedBytes, 0o600))
			wantPath := "2026/09/01/" + name + ".zst"
			wantContent := compressedBytes
			if tc.hasPlain {
				require.NoError(t, os.WriteFile(plain, []byte(newer), 0o600))
				wantPath = "2026/09/01/" + name
				wantContent = []byte(newer)
			}

			base := t.TempDir()
			store, err := rawcheckpoint.OpenWithOptions(t.Context(), filepath.Join(base, "checkpoint.db"), rawcheckpoint.Options{
				SpoolDir: filepath.Join(base, "spool"), MaxOutboxBytes: 1 << 20,
			})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			provider, ok := parser.NewProvider(parser.AgentCodex, parser.ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			capturer := rawcapture.New(store)
			worker := NewWorker([]parser.Provider{provider}, capturer, NewAuditor(store, capturer, 16), nil)
			batch := syncpkg.WatchBatch{FullSync: tc.audit, Paths: []string{compressed}}
			require.NoError(t, worker.HandleBatch(t.Context(), batch))
			configured, err := store.ResolveConfiguredRoot(t.Context(), parser.AgentCodex, root)
			require.NoError(t, err)
			identity := rawcheckpoint.SourceIdentity{
				Provider: parser.AgentCodex, ConfiguredRootID: configured.ID, SourceKey: "codex:" + uuid,
			}
			captured, ok, err := store.CaptureBase(t.Context(), identity)
			require.NoError(t, err)
			require.True(t, ok)
			require.Len(t, captured.Entries, 1)
			assert.Equal(t, wantPath, captured.Entries[0].Path)
			require.Len(t, captured.Entries[0].Objects, 1)
			content, err := os.ReadFile(store.ObjectPath(captured.Entries[0].Objects[0]))
			require.NoError(t, err)
			assert.Equal(t, wantContent, content)

			require.NoError(t, worker.HandleBatch(t.Context(), batch))
			repeated, ok, err := store.CaptureBase(t.Context(), identity)
			require.NoError(t, err)
			require.True(t, ok)
			assert.Equal(t, captured.CaptureID, repeated.CaptureID, "unchanged sources must not alternate captures")
		})
	}
}
