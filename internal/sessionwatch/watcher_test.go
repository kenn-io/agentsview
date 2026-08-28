package sessionwatch

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

// testWatcher creates a Watcher backed by a fresh SQLite database
// and a minimal sync engine for tests that need checkDBForChanges
// access.
func testWatcher(t *testing.T) *Watcher {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	database, err := db.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {dir},
		},
		Machine: "test",
	})
	return New(database, engine)
}

func TestStatMtime_NonexistentFile(t *testing.T) {
	t.Parallel()
	got := StatMtime(
		filepath.Join(t.TempDir(), "no-such-file"),
	)
	assert.Equal(t, int64(0), got)
}

func TestStatMtime_ExistingFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "file.txt")
	require.NoError(t, os.WriteFile(path, []byte("data"), 0o644))
	got := StatMtime(path)
	assert.NotZero(t, got)
}

func TestCheckDBForChanges_FileDisappears(t *testing.T) {
	t.Parallel()
	w := testWatcher(t)

	path := filepath.Join(t.TempDir(), "gone.jsonl")
	var lastMtime int64 = 12345
	var mchanged time.Time
	var lastCount int
	var lastDBMtime int64

	changed := w.checkDBForChanges(
		"test-session",
		&lastCount,
		&lastDBMtime,
		&path,
		&lastMtime,
		&mchanged,
	)
	assert.False(t, changed, "expected no change signal")
	assert.Empty(t, path)
	assert.Equal(t, int64(0), lastMtime)
}

func TestCheckDBForChanges_FileHashChange(t *testing.T) {
	t.Parallel()
	w := testWatcher(t)
	database, ok := w.db.(*db.DB)
	require.True(t, ok, "test watcher should use SQLite DB")

	const sessionID = "hash-change"
	var mtime int64 = 12345
	hash1 := "shelley-fingerprint-1"
	dbtest.SeedSession(t, database, sessionID, "proj", func(s *db.Session) {
		s.MessageCount = 2
		s.FileMtime = &mtime
		s.FileHash = &hash1
	})

	lastCount, lastDBMtime, ok := w.db.GetSessionVersion(sessionID)
	require.True(t, ok, "initial session version")

	hash2 := "shelley-fingerprint-2"
	dbtest.SeedSession(t, database, sessionID, "proj", func(s *db.Session) {
		s.MessageCount = 2
		s.FileMtime = &mtime
		s.FileHash = &hash2
	})

	sourcePath := ""
	var lastFileMtime int64
	var mchanged time.Time
	changed := w.checkDBForChanges(
		sessionID,
		&lastCount,
		&lastDBMtime,
		&sourcePath,
		&lastFileMtime,
		&mchanged,
	)

	assert.True(t, changed,
		"file_hash-only rewrites must refresh session watchers")
}

func TestCheckDBForChangesPositAssistantSidecarAppendFallsBackToSync(
	t *testing.T,
) {
	root := t.TempDir()
	const conversationID = "11111111-1111-4111-8111-111111111111"
	conversationDir := filepath.Join(root, "workspace-1", conversationID)
	workspacePath := filepath.Join(root, "workspace-1", "workspace.json")
	conversationPath := filepath.Join(conversationDir, "conversation.json")
	lmMessagesPath := filepath.Join(conversationDir, "lm-messages.jsonl")
	usageEventsPath := filepath.Join(conversationDir, "usage-events.jsonl")

	dbtest.WriteTestFile(t, workspacePath, []byte(`{"path":"/work/example"}`))
	dbtest.WriteTestFile(t, conversationPath, []byte(`{
		"schemaVersion":"3",
		"root":{"id":"`+conversationID+`","timestamp":1735689600000,"metadata":{"kind":"main"}},
		"messages":[{"id":"node-1","parentId":"`+conversationID+`","isActive":true,"lmMessageIds":[0,1],"timestamp":1735689600000}],
		"files":[]
	}`))
	dbtest.WriteTestFile(t, lmMessagesPath, []byte(
		`{"id":0,"message":{"role":"user","content":"Inspect the project."}}`+"\n"+
			`{"id":1,"message":{"role":"assistant","content":[{"type":"text","text":"Looking."}],"providerOptions":{"providerMetadata":{"positai":{"timestamp":1735689601000,"modelId":"claude-sonnet-4-6","providerId":"anthropic","usage":{"inputTokens":10,"outputTokens":5,"cacheReadTokens":0,"cacheWriteTokens":100}}}}}}`+"\n",
	))
	baseTime := time.Date(2026, time.August, 28, 10, 0, 0, 0, time.UTC)
	for _, path := range []string{workspacePath, conversationPath, lmMessagesPath} {
		require.NoError(t, os.Chtimes(path, baseTime, baseTime))
	}

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentPositAssistant: {root},
		},
		Machine: "test",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)

	const sessionID = "posit-assistant:" + conversationID
	lastCount, lastDBVersion, ok := database.GetSessionVersion(sessionID)
	require.True(t, ok)
	sourcePath := engine.FindSourceFile(sessionID)
	require.Equal(t, conversationPath, sourcePath)
	lastFileMtime := engine.SourceMtime(sessionID)
	require.Equal(t, baseTime.UnixNano(), lastFileMtime)

	dbtest.WriteTestFile(t, usageEventsPath, []byte(
		`{"type":"usage","kind":"keepalive","timestamp":1735693200000,"anchorMessageId":"node-1","providerId":"anthropic","modelId":"claude-sonnet-4-6","inputTokens":2,"outputTokens":1,"totalTokens":24642,"cacheReadTokens":24639,"cacheWriteTokens":0}`+"\n",
	))
	sidecarTime := baseTime.Add(time.Minute)
	require.NoError(t, os.Chtimes(usageEventsPath, sidecarTime, sidecarTime))

	watcher := New(database, engine)
	var fileMtimeChangedAt time.Time
	changed := watcher.checkDBForChanges(
		sessionID,
		&lastCount,
		&lastDBVersion,
		&sourcePath,
		&lastFileMtime,
		&fileMtimeChangedAt,
	)
	assert.False(t, changed, "the fallback delay must elapse before direct sync")
	require.False(t, fileMtimeChangedAt.IsZero(),
		"the sidecar mtime must start the fallback timer")
	assert.Equal(t, sidecarTime.UnixNano(), lastFileMtime)

	fileMtimeChangedAt = time.Now().Add(-SyncFallbackDelay)
	changed = watcher.checkDBForChanges(
		sessionID,
		&lastCount,
		&lastDBVersion,
		&sourcePath,
		&lastFileMtime,
		&fileMtimeChangedAt,
	)
	require.True(t, changed, "the elapsed fallback must sync the sidecar")

	usageEvents, err := database.GetUsageEvents(t.Context(), sessionID)
	require.NoError(t, err)
	require.Len(t, usageEvents, 1)
	assert.Equal(t, "posit-assistant-keepalive", usageEvents[0].Source)
	assert.Equal(t, 24639, usageEvents[0].CacheReadInputTokens)
}
