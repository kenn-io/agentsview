package sync_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

func TestIcodemateCLIForkSessionsReconcileAcrossSourceReparse(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	root := t.TempDir()
	projectDir := filepath.Join(root, "project")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	path := filepath.Join(projectDir, "fork-session.jsonl")
	mainLines := []string{
		`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"root","message":{"content":"start"}}`,
		`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"a1","parentUuid":"root","message":{"content":[{"type":"text","text":"main reply 1"}]}}`,
		`{"type":"user","timestamp":"2024-01-01T10:00:02Z","uuid":"u2","parentUuid":"a1","message":{"content":"main prompt 2"}}`,
		`{"type":"user","timestamp":"2024-01-01T10:00:03Z","uuid":"u3","parentUuid":"u2","message":{"content":"main prompt 3"}}`,
		`{"type":"user","timestamp":"2024-01-01T10:00:04Z","uuid":"u4","parentUuid":"u3","message":{"content":"main prompt 4"}}`,
		`{"type":"user","timestamp":"2024-01-01T10:00:05Z","uuid":"u5","parentUuid":"u4","message":{"content":"main prompt 5"}}`,
	}
	forkLine := `{"type":"assistant","timestamp":"2024-01-01T10:00:06Z","uuid":"fork","parentUuid":"root","message":{"content":[{"type":"text","text":"fork reply"}]}}`
	content := strings.Join(append(mainLines, forkLine), "\n") + "\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentIcodemate: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	first := engine.SyncAll(context.Background(), nil)
	require.Zero(t, first.Failed)
	require.Equal(t, 2, first.Synced)

	mainSession, err := database.GetSession(
		context.Background(), "icodemate:fork-session",
	)
	require.NoError(t, err)
	require.NotNil(t, mainSession)
	forkSession, err := database.GetSession(
		context.Background(), "icodemate:fork-session-fork",
	)
	require.NoError(t, err)
	require.NotNil(t, forkSession)
	require.NotNil(t, forkSession.ParentSessionID)
	assert.Equal(t, "icodemate:fork-session", *forkSession.ParentSessionID)

	updated := content + `{"type":"ai-title","aiTitle":"Updated title"}` + "\n"
	require.NoError(t, os.WriteFile(path, []byte(updated), 0o644))
	second := engine.SyncAll(context.Background(), nil)
	require.Zero(t, second.Failed)
	require.Equal(t, 2, second.Synced)

	mainSession, err = database.GetSession(
		context.Background(), "icodemate:fork-session",
	)
	require.NoError(t, err)
	require.NotNil(t, mainSession)
	require.NotNil(t, mainSession.DisplayName)
	assert.Equal(t, "Updated title", *mainSession.DisplayName)
	forkSession, err = database.GetSession(
		context.Background(), "icodemate:fork-session-fork",
	)
	require.NoError(t, err)
	require.NotNil(t, forkSession)
	require.NotNil(t, forkSession.ParentSessionID)
	assert.Equal(t, "icodemate:fork-session", *forkSession.ParentSessionID)

	mainOnly := strings.Join(mainLines, "\n") + "\n" +
		`{"type":"ai-title","aiTitle":"Main only"}` + "\n"
	require.NoError(t, os.WriteFile(path, []byte(mainOnly), 0o644))
	third := engine.SyncAll(context.Background(), nil)
	require.Zero(t, third.Failed)
	require.Equal(t, 1, third.Synced)

	mainSession, err = database.GetSession(
		context.Background(), "icodemate:fork-session",
	)
	require.NoError(t, err)
	require.NotNil(t, mainSession)
	require.NotNil(t, mainSession.DisplayName)
	assert.Equal(t, "Main only", *mainSession.DisplayName)
	forkSession, err = database.GetSession(
		context.Background(), "icodemate:fork-session-fork",
	)
	require.NoError(t, err)
	assert.Nil(t, forkSession)
	archivedFork, err := database.GetSessionFull(
		context.Background(), "icodemate:fork-session-fork",
	)
	require.NoError(t, err)
	require.NotNil(t, archivedFork)
	require.NotNil(t, archivedFork.DeletionCause)
	assert.Equal(t, "source_missing", *archivedFork.DeletionCause)
}

func TestIcodemateCLIPartialForkWriteRetriesWholeSource(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	root := t.TempDir()
	projectDir := filepath.Join(root, "project")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	path := filepath.Join(projectDir, "forked.jsonl")
	mainLines := []string{
		`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"root","message":{"content":"start"}}`,
		`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"a1","parentUuid":"root","message":{"content":[{"type":"text","text":"main reply 1"}]}}`,
		`{"type":"user","timestamp":"2024-01-01T10:00:02Z","uuid":"u2","parentUuid":"a1","message":{"content":"main prompt 2"}}`,
		`{"type":"user","timestamp":"2024-01-01T10:00:03Z","uuid":"u3","parentUuid":"u2","message":{"content":"main prompt 3"}}`,
		`{"type":"user","timestamp":"2024-01-01T10:00:04Z","uuid":"u4","parentUuid":"u3","message":{"content":"main prompt 4"}}`,
		`{"type":"user","timestamp":"2024-01-01T10:00:05Z","uuid":"u5","parentUuid":"u4","message":{"content":"main prompt 5"}}`,
	}
	require.NoError(t, os.WriteFile(
		path, []byte(strings.Join(mainLines, "\n")+"\n"), 0o644,
	))

	database := dbtest.OpenTestDB(t)
	initialEngine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentIcodemate: {root},
		},
		Machine: "local",
	})
	t.Cleanup(initialEngine.Close)
	initial := initialEngine.SyncAll(t.Context(), nil)
	require.Zero(t, initial.Failed)
	require.Equal(t, 1, initial.Synced)
	require.Equal(t, db.CurrentDataVersion(),
		database.GetSessionDataVersion("icodemate:forked"))

	forkLine := `{"type":"assistant","timestamp":"2024-01-01T10:00:06Z","uuid":"fork","parentUuid":"root","message":{"content":[{"type":"text","text":"fork reply"}]}}`
	require.NoError(t, os.WriteFile(path, []byte(
		strings.Join(append(mainLines, forkLine), "\n")+"\n",
	), 0o644))

	raw, err := sql.Open("sqlite3", database.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	_, err = raw.Exec(`
		CREATE TRIGGER fail_icodemate_fork_insert
		BEFORE INSERT ON sessions
		WHEN NEW.id = 'icodemate:forked-fork'
		BEGIN
			SELECT RAISE(FAIL, 'injected ICodeMate fork write failure');
		END;
	`)
	require.NoError(t, err)

	partialEngine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentIcodemate: {root},
		},
		Machine: "local",
	})
	t.Cleanup(partialEngine.Close)
	failed := partialEngine.SyncAll(t.Context(), nil)
	require.Equal(t, 1, failed.Synced)
	require.Equal(t, 1, failed.Failed)
	assert.Less(t,
		database.GetSessionDataVersion("icodemate:forked"),
		db.CurrentDataVersion(),
		"the committed main branch must remain retryable",
	)
	fork, err := database.GetSession(t.Context(), "icodemate:forked-fork")
	require.NoError(t, err)
	assert.Nil(t, fork)

	_, err = raw.Exec(`DROP TRIGGER fail_icodemate_fork_insert`)
	require.NoError(t, err)
	restarted := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentIcodemate: {root},
		},
		Machine: "local",
	})
	t.Cleanup(restarted.Close)
	retry := restarted.SyncAll(t.Context(), nil)
	require.Zero(t, retry.Failed)
	require.Equal(t, 2, retry.Synced,
		"the unchanged transcript must retry every branch")
	fork, err = database.GetSession(t.Context(), "icodemate:forked-fork")
	require.NoError(t, err)
	require.NotNil(t, fork)
	assert.Equal(t, db.CurrentDataVersion(),
		database.GetSessionDataVersion("icodemate:forked"))
	assert.Equal(t, db.CurrentDataVersion(),
		database.GetSessionDataVersion("icodemate:forked-fork"))
}
