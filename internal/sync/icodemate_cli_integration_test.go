package sync_test

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestIcodemateCLIForkSessionsReconcileAcrossSourceReparse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	if testing.Short() {
		t.Skip("skipping integration test")
	}

	root := t.TempDir()
	projectDir := filepath.Join(root, "project")
	require.NoError(os.MkdirAll(projectDir, 0o755))
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
	require.NoError(os.WriteFile(path, []byte(content), 0o644))

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentIcodemate: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	first := engine.SyncAll(t.Context(), nil)
	require.Zero(first.Failed)
	require.Equal(2, first.Synced)

	mainSession, err := database.GetSession(
		t.Context(), "icodemate:fork-session",
	)
	require.NoError(err)
	require.NotNil(mainSession)
	forkSession, err := database.GetSession(
		t.Context(), "icodemate:fork-session-fork",
	)
	require.NoError(err)
	require.NotNil(forkSession)
	require.NotNil(forkSession.ParentSessionID)
	assert.Equal("icodemate:fork-session", *forkSession.ParentSessionID)

	updated := content + `{"type":"ai-title","aiTitle":"Updated title"}` + "\n"
	require.NoError(os.WriteFile(path, []byte(updated), 0o644))
	second := engine.SyncAll(t.Context(), nil)
	require.Zero(second.Failed)
	require.Equal(2, second.Synced)

	mainSession, err = database.GetSession(
		t.Context(), "icodemate:fork-session",
	)
	require.NoError(err)
	require.NotNil(mainSession)
	require.NotNil(mainSession.DisplayName)
	assert.Equal("Updated title", *mainSession.DisplayName)
	forkSession, err = database.GetSession(
		t.Context(), "icodemate:fork-session-fork",
	)
	require.NoError(err)
	require.NotNil(forkSession)
	require.NotNil(forkSession.ParentSessionID)
	assert.Equal("icodemate:fork-session", *forkSession.ParentSessionID)

	truncated := mainLines[0] + "\n" + `{"type":"assistant","uuid":"partial`
	require.NoError(os.WriteFile(path, []byte(truncated), 0o644))
	incomplete := engine.SyncAll(t.Context(), nil)
	require.Equal(1, incomplete.Failed)
	assert.Zero(incomplete.Synced)
	forkSession, err = database.GetSession(
		t.Context(), "icodemate:fork-session-fork",
	)
	require.NoError(err)
	assert.NotNil(forkSession)
	messages, err := database.GetMessages(
		t.Context(), "icodemate:fork-session", 0, 20, true,
	)
	require.NoError(err)
	assert.Len(messages, len(mainLines))

	mainOnly := strings.Join(mainLines, "\n") + "\n" +
		`{"type":"ai-title","aiTitle":"Main only"}` + "\n"
	require.NoError(os.WriteFile(path, []byte(mainOnly), 0o644))
	third := engine.SyncAll(t.Context(), nil)
	require.Zero(third.Failed)
	require.Equal(1, third.Synced)

	mainSession, err = database.GetSession(
		t.Context(), "icodemate:fork-session",
	)
	require.NoError(err)
	require.NotNil(mainSession)
	require.NotNil(mainSession.DisplayName)
	assert.Equal("Main only", *mainSession.DisplayName)
	forkSession, err = database.GetSession(
		t.Context(), "icodemate:fork-session-fork",
	)
	require.NoError(err)
	assert.NotNil(forkSession)
	archivedFork, err := database.GetSessionFull(
		t.Context(), "icodemate:fork-session-fork",
	)
	require.NoError(err)
	assertSourceMissingState(t, archivedFork)
}

func TestIcodemateCLIShortenedTranscriptReplacesArchivedMessages(t *testing.T) {
	require := require.New(t)

	root := t.TempDir()
	path := filepath.Join(root, "project", "shortened.jsonl")
	initial := testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T00:00:00Z", "first prompt").
		AddClaudeAssistant("2024-01-01T00:00:05Z", "obsolete reply").
		String()
	dbtest.WriteTestFile(t, path, []byte(initial))

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentIcodemate: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(1, engine.SyncAll(t.Context(), nil).Synced)

	shortened := testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T00:00:00Z", "replacement prompt").
		String()
	dbtest.WriteTestFile(t, path, []byte(shortened))
	require.Equal(1, engine.SyncAll(t.Context(), nil).Synced)

	messages, err := database.GetMessages(
		t.Context(), "icodemate:shortened", 0, 10, true,
	)
	require.NoError(err)
	require.Len(messages, 1)
	assert.Equal(t, "replacement prompt", messages[0].Content)
}

func TestIcodemateCLISourceMtimeIncludesPersistedToolResults(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	path := filepath.Join(root, "project", "mtime.jsonl")
	sidecar := filepath.Join(
		root, "project", "mtime", "tool-results", "output.txt",
	)
	olderSidecar := filepath.Join(filepath.Dir(sidecar), "older.txt")
	dbtest.WriteTestFile(t, path, []byte(testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T00:00:00Z", "prompt").String()))
	dbtest.WriteTestFile(t, sidecar, []byte("persisted output\n"))
	dbtest.WriteTestFile(t, olderSidecar, []byte("older output\n"))
	transcriptTime := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	olderSidecarTime := transcriptTime.Add(30 * time.Second)
	sidecarTime := transcriptTime.Add(time.Minute)
	require.NoError(os.Chtimes(path, transcriptTime, transcriptTime))
	require.NoError(os.Chtimes(
		olderSidecar, olderSidecarTime, olderSidecarTime,
	))
	require.NoError(os.Chtimes(sidecar, sidecarTime, sidecarTime))
	require.NoError(os.Chtimes(
		filepath.Dir(sidecar), transcriptTime, transcriptTime,
	))
	require.NoError(os.Chtimes(
		filepath.Dir(filepath.Dir(sidecar)), transcriptTime, transcriptTime,
	))

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentIcodemate: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	assert.Equal(sidecarTime.UnixNano(),
		engine.SourceMtime(t.Context(), "icodemate:mtime"))

	deletedAt := sidecarTime.Add(time.Minute)
	require.NoError(os.Remove(olderSidecar))
	require.NoError(os.Chtimes(
		filepath.Dir(sidecar), deletedAt, deletedAt,
	))
	assert.Equal(deletedAt.UnixNano(),
		engine.SourceMtime(t.Context(), "icodemate:mtime"))
}

func TestIcodemateCLISyncAllSinceDetectsDeletedToolResult(t *testing.T) {
	require := require.New(t)

	root := t.TempDir()
	projectDir := filepath.Join(root, "project")
	sessionPath := filepath.Join(projectDir, "sidecar.jsonl")
	sidecarPath := filepath.Join(
		projectDir, "sidecar", "tool-results", "output.txt",
	)
	dbtest.WriteTestFile(t, sidecarPath, []byte("full persisted output\n"))

	pathJSON, err := json.Marshal(sidecarPath)
	require.NoError(err)
	placeholder := "<persisted-output>\nOutput too large. Full output saved to: " +
		sidecarPath + "\n</persisted-output>"
	placeholderJSON, err := json.Marshal(placeholder)
	require.NoError(err)
	transcript := strings.Join([]string{
		`{"type":"user","timestamp":"2024-01-01T00:00:00Z","uuid":"u1","message":{"content":"run it"}}`,
		`{"type":"assistant","timestamp":"2024-01-01T00:00:01Z","uuid":"a1","parentUuid":"u1","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"make logs"}}]}}`,
		`{"type":"user","timestamp":"2024-01-01T00:00:02Z","uuid":"u2","parentUuid":"a1","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":` + string(placeholderJSON) + `}]},"toolUseResult":{"persistedOutputPath":` + string(pathJSON) + `}}`,
	}, "\n") + "\n"
	dbtest.WriteTestFile(t, sessionPath, []byte(transcript))

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentIcodemate: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(1, engine.SyncAll(t.Context(), nil).Synced)
	initialMessages, err := database.GetMessages(
		t.Context(), "icodemate:sidecar", 0, 10, true,
	)
	require.NoError(err)
	require.Len(initialMessages, 2)
	require.Len(initialMessages[1].ToolCalls, 1)
	require.Equal("full persisted output\n",
		initialMessages[1].ToolCalls[0].ResultContent)

	cutoff := time.Now()
	toolResultsDir := filepath.Dir(sidecarPath)
	require.NoError(os.RemoveAll(filepath.Dir(toolResultsDir)))
	changedAt := cutoff.Add(time.Second)
	require.NoError(os.Chtimes(projectDir, changedAt, changedAt))

	stats := engine.SyncAllSince(t.Context(), cutoff, nil)
	require.Equal(1, stats.Synced)
	messages, err := database.GetMessages(
		t.Context(), "icodemate:sidecar", 0, 10, true,
	)
	require.NoError(err)
	require.Len(messages, 2)
	require.Len(messages[1].ToolCalls, 1)
	assert.Equal(t, placeholder, messages[1].ToolCalls[0].ResultContent)
}

func TestIcodemateCLIResyncAllAbortsOnEmptyDiscovery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	path := filepath.Join(root, "project", "resync.jsonl")
	dbtest.WriteTestFile(t, path, []byte(testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T00:00:00Z", "keep me").
		AddClaudeAssistant("2024-01-01T00:00:05Z", "ok").
		String()))

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentIcodemate: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(1, engine.SyncAll(t.Context(), nil).Synced)
	require.NoError(os.Remove(path))

	stats := engine.ResyncAll(t.Context(), nil)
	assert.True(stats.Aborted)
	messages, err := database.GetMessages(
		t.Context(), "icodemate:resync", 0, 10, true,
	)
	require.NoError(err)
	require.Len(messages, 2)
	assert.Equal("keep me", messages[0].Content)
}

func TestIcodemateCLIPartialForkWriteRetriesWholeSource(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	if testing.Short() {
		t.Skip("skipping integration test")
	}

	root := t.TempDir()
	projectDir := filepath.Join(root, "project")
	require.NoError(os.MkdirAll(projectDir, 0o755))
	path := filepath.Join(projectDir, "forked.jsonl")
	mainLines := []string{
		`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"root","message":{"content":"start"}}`,
		`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"a1","parentUuid":"root","message":{"content":[{"type":"text","text":"main reply 1"}]}}`,
		`{"type":"user","timestamp":"2024-01-01T10:00:02Z","uuid":"u2","parentUuid":"a1","message":{"content":"main prompt 2"}}`,
		`{"type":"user","timestamp":"2024-01-01T10:00:03Z","uuid":"u3","parentUuid":"u2","message":{"content":"main prompt 3"}}`,
		`{"type":"user","timestamp":"2024-01-01T10:00:04Z","uuid":"u4","parentUuid":"u3","message":{"content":"main prompt 4"}}`,
		`{"type":"user","timestamp":"2024-01-01T10:00:05Z","uuid":"u5","parentUuid":"u4","message":{"content":"main prompt 5"}}`,
	}
	require.NoError(os.WriteFile(
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
	require.Zero(initial.Failed)
	require.Equal(1, initial.Synced)
	require.Equal(db.CurrentDataVersion(),
		database.GetSessionDataVersion("icodemate:forked"))

	forkLine := `{"type":"assistant","timestamp":"2024-01-01T10:00:06Z","uuid":"fork","parentUuid":"root","message":{"content":[{"type":"text","text":"fork reply"}]}}`
	require.NoError(os.WriteFile(path, []byte(
		strings.Join(append(mainLines, forkLine), "\n")+"\n",
	), 0o644))

	raw, err := sql.Open("sqlite3", database.Path())
	require.NoError(err)
	t.Cleanup(func() { require.NoError(raw.Close()) })
	_, err = raw.ExecContext(t.Context(), `
		CREATE TRIGGER fail_icodemate_fork_insert
		BEFORE INSERT ON sessions
		WHEN NEW.id = 'icodemate:forked-fork'
		BEGIN
			SELECT RAISE(FAIL, 'injected ICodeMate fork write failure');
		END;
	`)
	require.NoError(err)

	partialEngine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentIcodemate: {root},
		},
		Machine: "local",
	})
	t.Cleanup(partialEngine.Close)
	failed := partialEngine.SyncAll(t.Context(), nil)
	require.Equal(1, failed.Synced)
	require.Equal(1, failed.Failed)
	assert.Less(database.GetSessionDataVersion("icodemate:forked"),
		db.CurrentDataVersion(),
		"the committed main branch must remain retryable",
	)
	fork, err := database.GetSession(t.Context(), "icodemate:forked-fork")
	require.NoError(err)
	assert.Nil(fork)

	_, err = raw.ExecContext(t.Context(), `DROP TRIGGER fail_icodemate_fork_insert`)
	require.NoError(err)
	restarted := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentIcodemate: {root},
		},
		Machine: "local",
	})
	t.Cleanup(restarted.Close)
	retry := restarted.SyncAll(t.Context(), nil)
	require.Zero(retry.Failed)
	require.Equal(2, retry.Synced,
		"the unchanged transcript must retry every branch")
	fork, err = database.GetSession(t.Context(), "icodemate:forked-fork")
	require.NoError(err)
	require.NotNil(fork)
	assert.Equal(db.CurrentDataVersion(),
		database.GetSessionDataVersion("icodemate:forked"))
	assert.Equal(db.CurrentDataVersion(),
		database.GetSessionDataVersion("icodemate:forked-fork"))
}

func TestIcodemateCLIDeduplicatesSameSessionAcrossRoots(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	liveRoot := t.TempDir()
	archiveRoot := t.TempDir()
	livePath := filepath.Join(liveRoot, "live-project", "duplicate.jsonl")
	archivePath := filepath.Join(archiveRoot, "archive-project", "duplicate.jsonl")
	liveContent := testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T00:00:00Z", "complete session").
		AddClaudeAssistant("2024-01-01T00:00:05Z", "complete reply").
		String()
	archiveContent := testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T00:00:00Z", "stale session").
		String()
	dbtest.WriteTestFile(t, livePath, []byte(liveContent))
	dbtest.WriteTestFile(t, archivePath, []byte(archiveContent))
	liveSidecar := filepath.Join(
		liveRoot, "live-project", "duplicate", "tool-results", "output.txt",
	)
	archiveSidecar := filepath.Join(
		archiveRoot, "archive-project", "duplicate", "tool-results", "output.txt",
	)
	dbtest.WriteTestFile(t, liveSidecar, []byte("persisted output\n"))
	dbtest.WriteTestFile(t, archiveSidecar, []byte(strings.Repeat("stale output\n", 100)))
	older := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := older.Add(time.Second)
	require.NoError(os.Chtimes(archivePath, older, older))
	require.NoError(os.Chtimes(archiveSidecar, older, older))
	require.NoError(os.Chtimes(livePath, newer, newer))
	require.NoError(os.Chtimes(liveSidecar, newer, newer))

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentIcodemate: {liveRoot, archiveRoot},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	first := engine.SyncAll(t.Context(), nil)
	require.Zero(first.Failed)
	assert.Equal(1, first.Synced)
	assert.Equal(livePath,
		database.GetSessionFilePath("icodemate:duplicate"))
	messages, err := database.GetMessages(
		t.Context(), "icodemate:duplicate", 0, 10, true,
	)
	require.NoError(err)
	assert.Len(messages, 2)

	touchedArchive := newer.Add(time.Second)
	require.NoError(os.Chtimes(archivePath, touchedArchive, touchedArchive))
	engine.SyncPaths([]string{archivePath})
	assert.Zero(engine.LastSyncStats().Synced,
		"a stale duplicate watcher event must not replace the committed source")
	assert.Equal(livePath,
		database.GetSessionFilePath("icodemate:duplicate"))

	staleLarge := testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T00:00:00Z", strings.Repeat("stale ", 50)).
		AddClaudeAssistant("2024-01-01T00:00:05Z", strings.Repeat("reply ", 50)).
		String()
	shortened := testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T00:00:00Z", "replacement").
		String()
	dbtest.WriteTestFile(t, archivePath, []byte(staleLarge))
	dbtest.WriteTestFile(t, livePath, []byte(shortened))
	require.NoError(os.Chtimes(
		archivePath, touchedArchive, touchedArchive,
	))
	shortenedAt := touchedArchive.Add(time.Second)
	require.NoError(os.Chtimes(livePath, shortenedAt, shortenedAt))

	stats := engine.SyncAll(t.Context(), nil)
	require.Zero(stats.Failed)
	require.Equal(1, stats.Synced)
	assert.Equal(livePath,
		database.GetSessionFilePath("icodemate:duplicate"))
	messages, err = database.GetMessages(
		t.Context(), "icodemate:duplicate", 0, 10, true,
	)
	require.NoError(err)
	require.Len(messages, 1)
	assert.Equal("replacement", messages[0].Content)

	require.NoError(engine.ReconcileWatchRoots(
		t.Context(), []string{liveRoot, archiveRoot}, false,
	))
	assert.Equal(livePath,
		database.GetSessionFilePath("icodemate:duplicate"))
	messages, err = database.GetMessages(
		t.Context(), "icodemate:duplicate", 0, 10, true,
	)
	require.NoError(err)
	require.Len(messages, 1)
	assert.Equal("replacement", messages[0].Content)

	staleSameSize := testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T00:00:00Z", "stale value").
		String()
	require.Len(staleSameSize, len(shortened))
	dbtest.WriteTestFile(t, archivePath, []byte(staleSameSize))
	touchedArchive = shortenedAt.Add(time.Second)
	require.NoError(os.Chtimes(
		archivePath, touchedArchive, touchedArchive,
	))

	require.NoError(engine.ReconcileWatchRoots(
		t.Context(), []string{liveRoot, archiveRoot}, false,
	))
	assert.Equal(livePath,
		database.GetSessionFilePath("icodemate:duplicate"))
	messages, err = database.GetMessages(
		t.Context(), "icodemate:duplicate", 0, 10, true,
	)
	require.NoError(err)
	require.Len(messages, 1)
	assert.Equal("replacement", messages[0].Content)

	continued := testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T00:00:00Z", "replacement").
		AddClaudeAssistant("2024-01-01T00:00:05Z", "continued elsewhere").
		String()
	dbtest.WriteTestFile(t, archivePath, []byte(continued))
	continuedAt := touchedArchive.Add(time.Second)
	require.NoError(os.Chtimes(archivePath, continuedAt, continuedAt))

	require.NoError(engine.ReconcileWatchRoots(
		t.Context(), []string{liveRoot, archiveRoot}, false,
	))
	assert.Equal(archivePath,
		database.GetSessionFilePath("icodemate:duplicate"))
	messages, err = database.GetMessages(
		t.Context(), "icodemate:duplicate", 0, 10, true,
	)
	require.NoError(err)
	require.Len(messages, 2)
	assert.Equal("continued elsewhere", messages[1].Content)

	dbtest.WriteTestFile(t, archiveSidecar, []byte("updated persisted output\n"))
	dbtest.WriteTestFile(t, livePath, []byte(shortened))
	staleAt := continuedAt.Add(time.Second)
	require.NoError(os.Chtimes(livePath, staleAt, staleAt))

	stats = engine.SyncAll(t.Context(), nil)
	require.Zero(stats.Failed)
	assert.Equal(archivePath,
		database.GetSessionFilePath("icodemate:duplicate"))
	messages, err = database.GetMessages(
		t.Context(), "icodemate:duplicate", 0, 10, true,
	)
	require.NoError(err)
	require.Len(messages, 2)
	assert.Equal("continued elsewhere", messages[1].Content)
}

func TestIcodemateCLIReconcilePreservesMovedSessionAcrossRoots(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	oldRoot := t.TempDir()
	newRoot := t.TempDir()
	oldPath := filepath.Join(oldRoot, "old-project", "moved.jsonl")
	mainLines := []string{
		`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"root","message":{"content":"start"}}`,
		`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"a1","parentUuid":"root","message":{"content":[{"type":"text","text":"main reply 1"}]}}`,
		`{"type":"user","timestamp":"2024-01-01T10:00:02Z","uuid":"u2","parentUuid":"a1","message":{"content":"main prompt 2"}}`,
		`{"type":"user","timestamp":"2024-01-01T10:00:03Z","uuid":"u3","parentUuid":"u2","message":{"content":"main prompt 3"}}`,
		`{"type":"user","timestamp":"2024-01-01T10:00:04Z","uuid":"u4","parentUuid":"u3","message":{"content":"main prompt 4"}}`,
		`{"type":"user","timestamp":"2024-01-01T10:00:05Z","uuid":"u5","parentUuid":"u4","message":{"content":"main prompt 5"}}`,
	}
	forkLine := `{"type":"assistant","timestamp":"2024-01-01T10:00:06Z","uuid":"fork","parentUuid":"root","message":{"content":[{"type":"text","text":"fork reply"}]}}`
	initial := strings.Join(append(mainLines, forkLine), "\n") + "\n"
	dbtest.WriteTestFile(t, oldPath, []byte(initial))

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentIcodemate: {oldRoot, newRoot},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(2, engine.SyncAll(t.Context(), nil).Synced)

	require.NoError(os.Remove(oldPath))
	newPath := filepath.Join(newRoot, "new-project", "moved.jsonl")
	replacement := testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T00:00:00Z", "replacement").
		AddClaudeAssistant("2024-01-01T00:00:05Z", "still active").
		String()
	dbtest.WriteTestFile(t, newPath, []byte(replacement))
	require.NoError(engine.ReconcileWatchRoots(
		t.Context(), []string{oldRoot, newRoot}, false,
	))

	active, err := database.GetSession(t.Context(), "icodemate:moved")
	require.NoError(err)
	require.NotNil(active)
	assert.Equal(newPath,
		database.GetSessionFilePath("icodemate:moved"))
	messages, err := database.GetMessages(
		t.Context(), "icodemate:moved", 0, 10, true,
	)
	require.NoError(err)
	assert.Len(messages, 2)

	fork, err := database.GetSession(t.Context(), "icodemate:moved-fork")
	require.NoError(err)
	assert.NotNil(fork)
	archivedFork, err := database.GetSessionFull(
		t.Context(), "icodemate:moved-fork",
	)
	require.NoError(err)
	assertSourceMissingState(t, archivedFork)
}

func TestIcodemateCLIResyncAllTombstonesRemovedFork(t *testing.T) {
	require := require.New(t)

	root := t.TempDir()
	path := filepath.Join(root, "project", "resync-fork.jsonl")
	mainLines := []string{
		`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"root","message":{"content":"start"}}`,
		`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"a1","parentUuid":"root","message":{"content":[{"type":"text","text":"main reply 1"}]}}`,
		`{"type":"user","timestamp":"2024-01-01T10:00:02Z","uuid":"u2","parentUuid":"a1","message":{"content":"main prompt 2"}}`,
		`{"type":"user","timestamp":"2024-01-01T10:00:03Z","uuid":"u3","parentUuid":"u2","message":{"content":"main prompt 3"}}`,
		`{"type":"user","timestamp":"2024-01-01T10:00:04Z","uuid":"u4","parentUuid":"u3","message":{"content":"main prompt 4"}}`,
		`{"type":"user","timestamp":"2024-01-01T10:00:05Z","uuid":"u5","parentUuid":"u4","message":{"content":"main prompt 5"}}`,
	}
	forkLine := `{"type":"assistant","timestamp":"2024-01-01T10:00:06Z","uuid":"fork","parentUuid":"root","message":{"content":[{"type":"text","text":"fork reply"}]}}`
	dbtest.WriteTestFile(t, path, []byte(
		strings.Join(append(mainLines, forkLine), "\n")+"\n",
	))

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentIcodemate: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(2, engine.SyncAll(t.Context(), nil).Synced)

	dbtest.WriteTestFile(t, path, []byte(strings.Join(mainLines, "\n")+"\n"))
	rebuilt := engine.ResyncAll(t.Context(), nil)
	require.False(rebuilt.Aborted)
	require.Zero(rebuilt.Failed)

	fork, err := database.GetSession(t.Context(), "icodemate:resync-fork-fork")
	require.NoError(err)
	assert.NotNil(t, fork)
	archivedFork, err := database.GetSessionFull(
		t.Context(), "icodemate:resync-fork-fork",
	)
	require.NoError(err)
	assertSourceMissingState(t, archivedFork)
}
