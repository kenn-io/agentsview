package sessionwatch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

type baselineStore struct {
	db.Store
	count atomic.Int64
}

func (s *baselineStore) GetSessionWatchState(id string) (db.SessionWatchState, error) {
	return db.SessionWatchState{State: db.SessionWatchResolved, PublicID: id}, nil
}

func (s *baselineStore) GetSessionVersion(context.Context, string) (int, int64, bool) {
	return int(s.count.Load()), 1, true
}

func TestEventsBaselineTakenBeforeReturn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		store := &baselineStore{}
		store.count.Store(1)
		updates := New(store, nil).Events(ctx, "session-a")
		store.count.Store(2)
		time.Sleep(PollInterval)
		synctest.Wait()
		select {
		case _, ok := <-updates:
			assert.True(t, ok)
		default:
			require.FailNow(t, "reply after watch creation was lost")
		}
	})
}

type transientIdentityStore struct {
	baselineStore
	reads atomic.Int32
	state string
}

func (s *transientIdentityStore) GetSessionWatchState(id string) (db.SessionWatchState, error) {
	if s.reads.Add(1) == 1 {
		return db.SessionWatchState{}, errors.New("watch state unavailable")
	}
	return db.SessionWatchState{State: s.state, PublicID: id}, nil
}

func TestEventsRecoversFromInitialWatchStateError(t *testing.T) {
	for _, state := range []string{db.SessionWatchResolved, "ambiguous"} {
		t.Run(state, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				store := &transientIdentityStore{state: state}
				updates := New(store, nil).Events(ctx, "session-a")
				synctest.Wait()
				time.Sleep(PollInterval)
				synctest.Wait()
				select {
				case _, ok := <-updates:
					require.True(t, ok, "recovery must emit an update")
				default:
					require.FailNow(t, "recovery did not emit an update")
				}
				synctest.Wait()
				if state != db.SessionWatchResolved {
					_, ok := <-updates
					assert.False(t, ok)
				} else {
					select {
					case _, ok := <-updates:
						require.True(t, ok, "resolved watch must remain open")
						require.FailNow(t, "unexpected second update")
					default:
					}
				}
			})
		})
	}
}

// testWatcher creates a Watcher backed by a fresh SQLite database
// and a minimal sync engine for tests that need checkDBForChanges
// access.
func testWatcher(t *testing.T) *Watcher {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	database, err := db.Open(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

	engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{
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

	changed := w.checkDBForChanges(t.Context(),
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

	lastCount, lastDBMtime, ok := w.db.GetSessionVersion(t.Context(), sessionID)
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
	changed := w.checkDBForChanges(t.Context(),
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
	engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentPositAssistant: {root},
		},
		Machine: "test",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)

	const sessionID = "posit-assistant:" + conversationID
	lastCount, lastDBVersion, ok := database.GetSessionVersion(t.Context(), sessionID)
	require.True(t, ok)
	sourcePath := engine.FindSourceFile(t.Context(), sessionID)
	require.Equal(t, conversationPath, sourcePath)
	lastFileMtime := engine.SourceMtime(t.Context(), sessionID)
	require.Equal(t, baseTime.UnixNano(), lastFileMtime)

	dbtest.WriteTestFile(t, usageEventsPath, []byte(
		`{"type":"usage","kind":"keepalive","timestamp":1735693200000,"anchorMessageId":"node-1","providerId":"anthropic","modelId":"claude-sonnet-4-6","inputTokens":2,"outputTokens":1,"totalTokens":24642,"cacheReadTokens":24639,"cacheWriteTokens":0}`+"\n",
	))
	sidecarTime := baseTime.Add(time.Minute)
	require.NoError(t, os.Chtimes(usageEventsPath, sidecarTime, sidecarTime))

	watcher := New(database, engine)
	var fileMtimeChangedAt time.Time
	changed := watcher.checkDBForChanges(t.Context(),
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
	changed = watcher.checkDBForChanges(t.Context(),
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

// kiro-cli appends to a subagent's own transcript, which is the subagent
// session's source. When the filesystem event for that append is missed, the
// fallback must still notice it and sync.
func TestCheckDBForChangesKiroSubagentAppendFallsBackToSync(t *testing.T) {
	const childID = "kiro:11111111-2222-3333-4444-555555555555"
	root := t.TempDir()
	sessDir := filepath.Join(root, "workspace", "sess_0123456789abcdef")
	childPath := filepath.Join(sessDir, "sub-executions", "11111111-2222-3333-4444-555555555555.jsonl")
	childLine := testjsonl.KiroAssistantJSON("2026-06-01T10:00:02Z", "working")
	dbtest.WriteTestFile(t, filepath.Join(sessDir, "messages.jsonl"), []byte(testjsonl.JoinJSONL(
		testjsonl.KiroUserJSON("2026-06-01T10:00:00Z", "parent prompt"),
	)))
	dbtest.WriteTestFile(t, childPath, []byte(testjsonl.JoinJSONL(childLine)))
	baseTime := time.Date(2026, time.June, 1, 10, 0, 5, 0, time.UTC)
	require.NoError(t, os.Chtimes(childPath, baseTime, baseTime))

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentKiro: {root}},
		Machine:   "test",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 2, engine.SyncAll(t.Context(), nil).Synced)
	watcher := New(database, engine)
	lastCount, lastDBVersion, ok := database.GetSessionVersion(t.Context(), childID)
	require.True(t, ok)
	sourcePath := engine.FindSourceFile(t.Context(), childID)
	require.Equal(t, childPath, sourcePath)
	lastFileMtime := engine.SourceMtime(t.Context(), childID)
	require.NotZero(t, lastFileMtime)
	check := func(fileMtimeChangedAt *time.Time) bool {
		t.Helper()
		return watcher.checkDBForChanges(t.Context(), childID,
			&lastCount, &lastDBVersion, &sourcePath, &lastFileMtime, fileMtimeChangedAt)
	}

	dbtest.WriteTestFile(t, childPath, []byte(testjsonl.JoinJSONL(
		childLine, testjsonl.KiroAssistantJSON("2026-06-01T10:00:04Z", "subtask done"),
	)))
	childTime := baseTime.Add(time.Minute)
	require.NoError(t, os.Chtimes(childPath, childTime, childTime))

	var fileMtimeChangedAt time.Time
	assert.False(t, check(&fileMtimeChangedAt), "the fallback delay must elapse before direct sync")
	require.False(t, fileMtimeChangedAt.IsZero(), "a subagent append must start the fallback timer")

	fileMtimeChangedAt = time.Now().Add(-SyncFallbackDelay)
	require.True(t, check(&fileMtimeChangedAt), "the elapsed fallback must sync the subagent")

	messages, err := database.GetAllMessages(t.Context(), childID)
	require.NoError(t, err)
	var contents []string
	for _, message := range messages {
		contents = append(contents, message.Content)
	}
	assert.Equal(t, []string{"working", "subtask done"}, contents)
}
