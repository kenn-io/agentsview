package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestProviderSourceFreshBeforeFingerprintRejectsUnverifiedStatDigest(
	t *testing.T,
) {
	require := require.New(t)

	database := openTestDB(t)
	path := filepath.Join(t.TempDir(), "session.jsonl")
	require.NoError(os.WriteFile(path, []byte("unchanged\n"), 0o644))
	info, err := os.Stat(path)
	require.NoError(err)
	require.NoError(database.UpsertSession(db.Session{
		ID:        "session",
		Agent:     string(parser.AgentClaude),
		Project:   "project-a",
		Machine:   "local",
		FilePath:  strPtr(path),
		FileSize:  int64Ptr(info.Size()),
		FileMtime: int64Ptr(info.ModTime().UnixNano()),
	}))
	require.NoError(database.SetSessionDataVersion(
		"session", db.CurrentDataVersion(),
	))
	require.NoError(database.UpsertProviderStatHash(
		t.Context(), parser.AgentClaude, path, 0,
	))

	engine := &Engine{db: database}
	_, fresh := engine.providerSourceFreshBeforeFingerprint(
		t.Context(),
		parser.SourceRef{Provider: parser.AgentClaude, DisplayPath: path},
		parser.DiscoveredFile{Agent: parser.AgentClaude, Path: path},
		&pendingProviderStatHash{
			agent:        parser.AgentClaude,
			physicalPath: path,
			targetKey:    path,
			digest:       0,
		},
	)

	assert.False(t, fresh,
		"an unverified stat digest must not accept persisted freshness")
}

func TestClaudeIncrementalWritePersistsCompleteSourceStatHash(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := openTestDB(t)
	root := t.TempDir()
	projectDir := filepath.Join(root, "project-a")
	require.NoError(os.MkdirAll(projectDir, 0o755))
	path := filepath.Join(projectDir, "incremental-hash.jsonl")
	builder := testjsonl.NewSessionBuilder().
		AddClaudeUserWithUUID(
			"2024-01-01T10:00:00Z", "start", "a", "",
		).
		AddClaudeAssistantWithUUID(
			"2024-01-01T10:00:01Z", "ok", "b", "a",
		)
	require.NoError(os.WriteFile(path, []byte(builder.String()), 0o644))

	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	initial := engine.SyncAll(t.Context(), nil)
	require.Equal(1, initial.Synced)
	require.Zero(initial.Failed)
	hasher := engine.providerStatHashers[parser.AgentClaude]
	require.NotNil(hasher)
	initialDigest, ok, err := database.GetProviderStatHash(
		t.Context(), parser.AgentClaude, path,
	)
	require.NoError(err)
	require.True(ok)

	// Consume one complete record while leaving an unfinished record at EOF.
	// The cursor may advance, but the digest must continue to describe the last
	// completely consumed source snapshot.
	builder.AddClaudeUserWithUUID(
		"2024-01-01T10:00:02Z", "next", "c", "b",
	)
	require.NoError(os.WriteFile(
		path, []byte(builder.String()+`{"type":"assistant"`), 0o644,
	))
	partial := engine.SyncAll(t.Context(), nil)
	require.Equal(1, partial.Synced)
	require.Zero(partial.Failed)
	partialDigest, ok, err := database.GetProviderStatHash(
		t.Context(), parser.AgentClaude, path,
	)
	require.NoError(err)
	require.True(ok)
	assert.Equal(initialDigest, partialDigest)
	assert.NotEqual(hasher.ComputeMultiFileStatHash(path), partialDigest)

	// Replacing the unfinished tail with a complete record lets the
	// incremental parser consume the full fingerprinted source. Its successful
	// write must advance the digest in the same pass.
	builder.AddClaudeAssistantWithUUID(
		"2024-01-01T10:00:03Z", "done", "d", "c",
	)
	require.NoError(os.WriteFile(path, []byte(builder.String()), 0o644))
	complete := engine.SyncAll(t.Context(), nil)
	require.Equal(1, complete.Synced)
	require.Zero(complete.Failed)
	completeDigest, ok, err := database.GetProviderStatHash(
		t.Context(), parser.AgentClaude, path,
	)
	require.NoError(err)
	require.True(ok)
	assert.Equal(hasher.ComputeMultiFileStatHash(path), completeDigest)
}
