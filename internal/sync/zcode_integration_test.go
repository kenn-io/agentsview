package sync

import (
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
)

func TestSyncZCodeTranscript(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	dbPath := writeProcessProviderZCodeDB(t, filepath.Join(root, ".zcode", "cli"))
	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentZCode: {filepath.Join(root, ".zcode", "cli")},
		},
		Machine: "devbox",
	})

	runSyncAndAssert(t, engine, SyncStats{TotalSessions: 1, Synced: 1, Skipped: 0})

	sess, err := database.GetSession(t.Context(), "zcode:session-001")
	require.NoError(err)
	require.NotNil(sess)
	assert.Equal(2, sess.MessageCount)
	assert.Equal("acme_app", sess.Project)
	assert.Equal(dbPath+"#session-001", database.GetSessionFilePath("zcode:session-001"))
	_, storedMtime, ok := database.GetSessionFileInfo("zcode:session-001")
	require.True(ok)
	assert.Equal(engine.SourceMtime(t.Context(), "zcode:session-001"), storedMtime)

	msgs, err := database.GetMessages(t.Context(), "zcode:session-001", 0, 100, true)
	require.NoError(err)
	require.Len(msgs, 2)
	assert.Equal("Inspect the auth flow.", msgs[0].Content)
	assert.Equal("assistant", msgs[1].Role)
	require.Len(msgs[1].ToolCalls, 1)
	assert.Equal("Read", msgs[1].ToolCalls[0].ToolName)
	assert.Equal("package auth", msgs[1].ToolCalls[0].ResultContent)

	events, err := database.GetUsageEvents(t.Context(), "zcode:session-001")
	require.NoError(err)
	require.Len(events, 1)
	assert.Equal("claude-sonnet-4-6", events[0].Model)
	assert.Equal(1, events[0].InputTokens)
	assert.Equal(2, events[0].OutputTokens)
	assert.Contains(events[0].DedupKey, "session:zcode:session-001")
	assert.Contains(events[0].DedupKey, "turn=1")
	assert.Contains(events[0].DedupKey, "model=claude-sonnet-4-6")

	runSyncAndAssert(t, engine, SyncStats{TotalSessions: 0, Synced: 0, Skipped: 0})
}

func runSyncAndAssert(t *testing.T, engine *Engine, want SyncStats) SyncStats {
	t.Helper()
	stats := engine.SyncAll(t.Context(), nil)
	diff := cmp.Diff(want, stats, cmpopts.IgnoreUnexported(SyncStats{}))
	require.Empty(t, diff, "SyncAll() mismatch (-want +got):\n%s", diff)
	return stats
}
