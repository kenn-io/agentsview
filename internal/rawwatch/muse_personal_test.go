package rawwatch

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawcapture"
	"go.kenn.io/agentsview/internal/rawcheckpoint"
)

func TestMusePersonalAuditPreservesTemporarilyUnavailableTranscript(t *testing.T) {
	for _, state := range []string{"unfinished header", "unreadable"} {
		t.Run(state, func(t *testing.T) {
			root := t.TempDir()
			const id = "11111111-2222-4333-8444-555555555555"
			path := filepath.Join(root, "agent-66666666-7777-4888-8999-000000000000", "sessions", id+".jsonl")
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			const original = `{"type":"session_header","version":1,"session_id":"11111111-2222-4333-8444-555555555555","agent_id":"66666666-7777-4888-8999-000000000000","created_at":"2026-01-01T00:00:00Z"}` + "\n"
			require.NoError(t, os.WriteFile(path, []byte(original), 0o600))
			provider, ok := parser.NewProvider(parser.AgentMusePersonal, parser.ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			base := t.TempDir()
			store, err := rawcheckpoint.OpenWithOptions(t.Context(), filepath.Join(base, "checkpoint.db"), rawcheckpoint.Options{SpoolDir: filepath.Join(base, "spool"), MaxOutboxBytes: 1 << 20})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			auditor := NewAuditor(store, rawcapture.New(store), 8)
			first, err := auditor.AuditProviderFull(t.Context(), provider)
			require.NoError(t, err)
			require.True(t, first.Complete)
			require.Equal(t, 1, first.Captured)
			if state == "unreadable" {
				require.NoError(t, os.Chmod(path, 0))
				t.Cleanup(func() {
					err := os.Chmod(path, 0o600)
					if !os.IsNotExist(err) {
						require.NoError(t, err)
					}
				})
				if f, err := os.Open(path); err == nil {
					require.NoError(t, f.Close())
					t.Skip("platform permits reading despite denied file permissions")
				}
			} else {
				require.NoError(t, os.WriteFile(path, []byte(`{"type":"session_header"`), 0o600))
			}
			next, err := auditor.AuditProviderFull(t.Context(), provider)
			assert.True(t, err != nil || !next.Complete, "uncertain discovery cannot authorize removals")
			assert.Zero(t, next.Tombstoned, "a temporary read failure or unfinished rewrite must preserve custody")
			require.NoError(t, os.Chmod(path, 0o600))
			require.NoError(t, os.WriteFile(path, []byte(original), 0o600))
			recovered, err := auditor.AuditProviderFull(t.Context(), provider)
			require.NoError(t, err)
			assert.True(t, recovered.Complete)
			assert.Zero(t, recovered.Tombstoned)
			require.NoError(t, os.Remove(path))
			removed, err := auditor.AuditProviderFull(t.Context(), provider)
			require.NoError(t, err)
			assert.True(t, removed.Complete)
			assert.Equal(t, 1, removed.Tombstoned, "a real deletion still reconciles")
		})
	}
}
