package sync_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

func TestAntigravityBrainReadFailurePreservesArchive(t *testing.T) {
	for _, withDatabase := range []bool{false, true} {
		name := "standalone"
		if withDatabase {
			name = "database companion"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			const id = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
			path := filepath.Join(root, "brain", id, ".system_generated", "logs", "transcript.jsonl")
			dbtest.WriteTestFile(t, path, []byte(`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"keep this archived message"}`+"\n"))
			if withDatabase {
				dbPath := filepath.Join(root, "conversations", id+".db")
				require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o755))
				copyAntigravityCLITestSchemaTemplate(t, dbPath)
			}
			provider, ok := parser.NewProvider(parser.AgentAntigravity, parser.ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			sources, err := provider.Discover(t.Context())
			require.NoError(t, err)
			require.Len(t, sources, 1)
			parsed, err := provider.Parse(t.Context(), parser.ParseRequest{Source: sources[0]})
			require.NoError(t, err)
			require.Len(t, parsed.Results, 1)
			sessionID := parsed.Results[0].Result.Session.ID
			archive := dbtest.OpenTestDB(t)
			engine := sync.NewEngine(t.Context(), archive, sync.EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentAntigravity: {root}}, Machine: "host-a",
			})
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))
			before, err := archive.GetMessages(t.Context(), sessionID, 0, 10, true)
			require.NoError(t, err)
			require.Len(t, before, 1)
			require.Equal(t, "keep this archived message", before[0].Content)

			require.NoError(t, os.Truncate(path, (64<<20)+1))
			_, err = provider.Parse(t.Context(), parser.ParseRequest{Source: sources[0]})
			require.Error(t, err, "an oversized transcript must fail the parse")
			_ = engine.SyncPathsContext(t.Context(), []string{path})
			after, err := archive.GetMessages(t.Context(), sessionID, 0, 10, true)
			require.NoError(t, err)
			require.Len(t, after, 1, "failed reads must retain the archived message")
			assert.Equal(t, "keep this archived message", after[0].Content)
		})
	}
}

func TestAntigravityBrainDatabaseTransitionsKeepOneSession(t *testing.T) {
	root := t.TempDir()
	const id = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	const sessionID = "antigravity:" + id
	path := filepath.Join(root, "brain", id, ".system_generated", "logs", "transcript.jsonl")
	dbtest.WriteTestFile(t, path, []byte(`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"one conversation"}`+"\n"))
	archive := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(t.Context(), archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentAntigravity: {root}}, Machine: "host-a",
	})
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))
	before, err := archive.GetSession(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, before, "transcripts use the existing conversation ID")

	dbPath := filepath.Join(root, "conversations", id+".db")
	require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o755))
	copyAntigravityCLITestSchemaTemplate(t, dbPath)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{dbPath}))
	oldIDs, err := archive.ListSessionIDsByFilePath(t.Context(), path, "antigravity")
	require.NoError(t, err)
	assert.Empty(t, oldIDs, "database arrival must move the existing session")
	dbIDs, err := archive.ListSessionIDsByFilePath(t.Context(), dbPath, "antigravity")
	require.NoError(t, err)
	assert.Equal(t, []string{sessionID}, dbIDs)

	require.NoError(t, os.Remove(dbPath))
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{dbPath}))
	after, err := archive.GetSessionFull(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, after, "database removal must retain the surviving transcript")
	assert.Nil(t, after.SourceMissingAt)
	require.NotNil(t, after.FilePath)
	assert.Equal(t, path, *after.FilePath)
	assert.Equal(t, 1, after.MessageCount)
}
