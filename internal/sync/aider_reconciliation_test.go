package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReconcileWatchRootsAiderOverlappingRootsReuseStableRunIdentity(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	historyPath := filepath.Join(repo, parser.AiderHistoryFileName())
	require.NoError(t, os.WriteFile(
		historyPath,
		[]byte("# aider chat started at 2026-06-09 14:01:00\n#### prompt\nanswer\n"),
		0o644,
	))
	expectedRawID, ok := parser.AiderRawIDAt(historyPath, 0)
	require.True(t, ok)
	expectedID := "aider:" + expectedRawID

	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentAider: {root, repo},
		},
		Machine: "test-machine",
	})
	t.Cleanup(engine.Close)

	require.NoError(t, engine.ReconcileWatchRoots(t.Context(), []string{root}, false))

	active, err := database.GetSession(t.Context(), expectedID)
	require.NoError(t, err)
	assert.NotNil(t, active, "the physical run keeps its parser-derived identity")
	page, err := database.ListSessions(t.Context(), db.SessionFilter{
		Agent: string(parser.AgentAider), Limit: 10,
	})
	require.NoError(t, err)
	activeIDs := make([]string, 0, len(page.Sessions))
	for _, session := range page.Sessions {
		activeIDs = append(activeIDs, session.ID)
	}
	assert.Equal(t, []string{expectedID}, activeIDs,
		"overlapping roots must produce exactly one active Aider session")
	assert.Equal(t, 1, engine.LastReconciliationResult().Metrics.SharedContainerScans)
}

func TestReconcileWatchRootsAiderScansOneLargeContainerOnce(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	var history strings.Builder
	for i := range 600 {
		history.WriteString("# aider chat started at 2026-06-09 14:01:00\n")
		history.WriteString("#### prompt ")
		history.WriteString(strings.Repeat("x", i%17+1))
		history.WriteString("\nanswer\n")
	}
	require.NoError(t, os.WriteFile(
		filepath.Join(repo, parser.AiderHistoryFileName()),
		[]byte(history.String()), 0o644,
	))
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentAider: {root}},
		Machine:   "test-machine",
	})
	defer engine.Close()

	require.NoError(t, engine.ReconcileWatchRoots(t.Context(), []string{root}, false))

	result := engine.LastReconciliationResult()
	assert.True(t, result.Complete)
	assert.Equal(t, 1, result.Metrics.SharedContainerScans)
	assert.Positive(t, result.Metrics.MaxProviderRetainedBytes)
	assert.Less(t, result.Metrics.MaxProviderRetainedBytes, int64(1<<20))
}

func TestReconcileWatchRootsAiderTombstonesDeletedVirtualRun(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	historyPath := filepath.Join(repo, parser.AiderHistoryFileName())
	firstRun := "# aider chat started at 2026-06-09 14:01:00\n" +
		"#### keep this run\nkept answer\n"
	secondRun := "# aider chat started at 2026-06-09 15:30:00\n" +
		"#### delete this run\ndeleted answer\n"
	require.NoError(t, os.WriteFile(historyPath, []byte(firstRun+secondRun), 0o644))
	survivingRawID, ok := parser.AiderRawIDAt(historyPath, 0)
	require.True(t, ok)
	deletedRawID, ok := parser.AiderRawIDAt(historyPath, 1)
	require.True(t, ok)
	survivingID := "aider:" + survivingRawID
	deletedID := "aider:" + deletedRawID

	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentAider: {root}},
		Machine:   "test-machine",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 2, engine.SyncAll(t.Context(), nil).Synced)
	require.NoError(t, os.WriteFile(historyPath, []byte(firstRun), 0o644))

	require.NoError(t, engine.ReconcileWatchRoots(t.Context(), []string{root}, false))

	active, err := database.GetSession(t.Context(), deletedID)
	require.NoError(t, err)
	assert.Nil(t, active)
	archived, err := database.GetSessionFull(t.Context(), deletedID)
	require.NoError(t, err)
	require.NotNil(t, archived)
	require.NotNil(t, archived.DeletionCause)
	assert.Equal(t, "source_missing", *archived.DeletionCause)
	surviving, err := database.GetSession(t.Context(), survivingID)
	require.NoError(t, err)
	assert.NotNil(t, surviving)
}

func TestReconcileWatchRootsAiderTombstonesDeletedRunWhenPositionIsReused(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	historyPath := filepath.Join(repo, parser.AiderHistoryFileName())
	runs := []string{
		"# aider chat started at 2026-06-09 14:01:00\n#### first run\nfirst answer\n",
		"# aider chat started at 2026-06-09 15:30:00\n#### deleted run\ndeleted answer\n",
		"# aider chat started at 2026-06-09 16:45:00\n#### shifted run\nshifted answer\n",
	}
	require.NoError(t, os.WriteFile(historyPath, []byte(strings.Join(runs, "")), 0o644))
	deletedRawID, ok := parser.AiderRawIDAt(historyPath, 1)
	require.True(t, ok)
	shiftedRawID, ok := parser.AiderRawIDAt(historyPath, 2)
	require.True(t, ok)
	deletedID := "aider:" + deletedRawID
	shiftedID := "aider:" + shiftedRawID

	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentAider: {root}},
		Machine:   "test-machine",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 3, engine.SyncAll(t.Context(), nil).Synced)
	require.NoError(t, os.WriteFile(historyPath, []byte(runs[0]+runs[2]), 0o644))

	require.NoError(t, engine.ReconcileWatchRoots(t.Context(), []string{root}, false))

	deleted, err := database.GetSession(t.Context(), deletedID)
	require.NoError(t, err)
	assert.Nil(t, deleted)
	archived, err := database.GetSessionFull(t.Context(), deletedID)
	require.NoError(t, err)
	require.NotNil(t, archived)
	require.NotNil(t, archived.DeletionCause)
	assert.Equal(t, "source_missing", *archived.DeletionCause)
	shifted, err := database.GetSessionFull(t.Context(), shiftedID)
	require.NoError(t, err)
	require.NotNil(t, shifted)
	assert.Nil(t, shifted.DeletedAt)
	require.NotNil(t, shifted.FilePath)
	assert.Equal(t, parser.AiderVirtualPath(historyPath, 1), *shifted.FilePath)
}
