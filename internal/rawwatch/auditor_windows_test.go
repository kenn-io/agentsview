package rawwatch

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawcapture"
	"go.kenn.io/agentsview/internal/rawcheckpoint"
)

func TestAuditorPausedDirectoryAllowsRenameAndRemoval(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	require.NoError(t, os.Mkdir(root, 0o700))
	project := filepath.Join(root, "project")
	require.NoError(t, os.Mkdir(project, 0o700))
	for _, name := range []string{"a.jsonl", "b.jsonl"} {
		require.NoError(t, os.WriteFile(filepath.Join(project, name), []byte(name), 0o600))
	}
	base := t.TempDir()
	store, err := rawcheckpoint.OpenWithOptions(
		t.Context(), filepath.Join(base, "checkpoint.db"),
		rawcheckpoint.Options{
			SpoolDir: filepath.Join(base, "spool"), MaxOutboxBytes: 1 << 20,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	provider, ok := parser.NewProvider(parser.AgentClaude, parser.ProviderConfig{
		Roots: []string{root},
	})
	require.True(t, ok)
	auditor := NewAuditor(store, rawcapture.New(store), 3)
	ctx, cancel := context.WithCancel(t.Context())

	result, err := auditor.AuditProvider(ctx, provider)

	require.NoError(t, err)
	assert.False(t, result.Complete)
	assert.Zero(t, result.Visited)
	moved := root + "-moved"
	require.NoError(t, os.Rename(root, moved))
	require.NoError(t, os.RemoveAll(moved))
	cancel()
}
