package sync

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
)

func TestSyncZCode(t *testing.T) {
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

	sess, err := database.GetSession(context.Background(), "zcode:session-001")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, 0, sess.MessageCount)
	assert.Equal(t, "acme_app", sess.Project)
	assert.Equal(t, dbPath+"#session-001", database.GetSessionFilePath("zcode:session-001"))
	_, storedMtime, ok := database.GetSessionFileInfo("zcode:session-001")
	require.True(t, ok)
	assert.Equal(t, engine.SourceMtime("zcode:session-001"), storedMtime)

	events, err := database.GetUsageEvents(context.Background(), "zcode:session-001")
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, "claude-sonnet-4-6", events[0].Model)
	assert.Equal(t, 1, events[0].InputTokens)
	assert.Equal(t, 2, events[0].OutputTokens)
	assert.Contains(t, events[0].DedupKey, "session:zcode:session-001")
	assert.Contains(t, events[0].DedupKey, "turn=1")
	assert.Contains(t, events[0].DedupKey, "model=claude-sonnet-4-6")

	runSyncAndAssert(t, engine, SyncStats{TotalSessions: 0, Synced: 0, Skipped: 0})
}

func runSyncAndAssert(t *testing.T, engine *Engine, want SyncStats) SyncStats {
	t.Helper()
	stats := engine.SyncAll(context.Background(), nil)
	diff := cmp.Diff(want, stats, cmpopts.IgnoreUnexported(SyncStats{}))
	require.Empty(t, diff, "SyncAll() mismatch (-want +got):\n%s", diff)
	return stats
}
