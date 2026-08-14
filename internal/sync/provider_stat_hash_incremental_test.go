package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestClaudeIncrementalWritePersistsCompleteSourceStatHash(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	projectDir := filepath.Join(root, "project-a")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	path := filepath.Join(projectDir, "incremental-hash.jsonl")
	builder := testjsonl.NewSessionBuilder().
		AddClaudeUserWithUUID(
			"2024-01-01T10:00:00Z", "start", "a", "",
		).
		AddClaudeAssistantWithUUID(
			"2024-01-01T10:00:01Z", "ok", "b", "a",
		)
	require.NoError(t, os.WriteFile(path, []byte(builder.String()), 0o644))

	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	initial := engine.SyncAll(context.Background(), nil)
	require.Equal(t, 1, initial.Synced)
	require.Zero(t, initial.Failed)
	hasher := engine.providerStatHashers[parser.AgentClaude]
	require.NotNil(t, hasher)
	initialDigest, ok, err := database.GetProviderStatHash(
		t.Context(), parser.AgentClaude, path,
	)
	require.NoError(t, err)
	require.True(t, ok)

	// Consume one complete record while leaving an unfinished record at EOF.
	// The cursor may advance, but the digest must continue to describe the last
	// completely consumed source snapshot.
	builder.AddClaudeUserWithUUID(
		"2024-01-01T10:00:02Z", "next", "c", "b",
	)
	require.NoError(t, os.WriteFile(
		path, []byte(builder.String()+`{"type":"assistant"`), 0o644,
	))
	partial := engine.SyncAll(context.Background(), nil)
	require.Equal(t, 1, partial.Synced)
	require.Zero(t, partial.Failed)
	partialDigest, ok, err := database.GetProviderStatHash(
		t.Context(), parser.AgentClaude, path,
	)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, initialDigest, partialDigest)
	assert.NotEqual(t, hasher.ComputeMultiFileStatHash(path), partialDigest)

	// Replacing the unfinished tail with a complete record lets the
	// incremental parser consume the full fingerprinted source. Its successful
	// write must advance the digest in the same pass.
	builder.AddClaudeAssistantWithUUID(
		"2024-01-01T10:00:03Z", "done", "d", "c",
	)
	require.NoError(t, os.WriteFile(path, []byte(builder.String()), 0o644))
	complete := engine.SyncAll(context.Background(), nil)
	require.Equal(t, 1, complete.Synced)
	require.Zero(t, complete.Failed)
	completeDigest, ok, err := database.GetProviderStatHash(
		t.Context(), parser.AgentClaude, path,
	)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, hasher.ComputeMultiFileStatHash(path), completeDigest)
}
