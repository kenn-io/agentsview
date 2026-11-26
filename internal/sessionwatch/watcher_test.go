package sessionwatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	gosync "sync"
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

type treeStore struct {
	baselineStore
	parentReads atomic.Int32
	unavailable atomic.Bool
}

func (s *treeStore) GetSessionVersion(context.Context, string) (int, int64, bool) {
	s.parentReads.Add(1)
	return 0, 0, true
}

func (s *treeStore) GetSessionTreeMarker(context.Context, string) (db.SessionTreeSnapshot, error) {
	if s.unavailable.Load() {
		return db.SessionTreeSnapshot{}, errors.New("marker unavailable")
	}
	return db.SessionTreeSnapshot{Marker: strconv.FormatInt(s.count.Load(), 10)}, nil
}

func TestTreeEventsReportReadFailureAndRecoveryOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &treeStore{}
		updates := New(store, nil).TreeEvents(t.Context(), "parent")
		synctest.Wait()
		store.unavailable.Store(true)
		time.Sleep(PollInterval)
		synctest.Wait()
		select {
		case <-updates:
		default:
			require.FailNow(t, "read failure was not reported")
		}
		time.Sleep(3 * PollInterval)
		synctest.Wait()
		select {
		case <-updates:
			require.FailNow(t, "repeated read failure triggered another update")
		default:
		}
		store.unavailable.Store(false)
		time.Sleep(PollInterval)
		synctest.Wait()
		select {
		case <-updates:
		default:
			require.FailNow(t, "recovery was not reported")
		}
	})
}

func TestTreeEventsDetectChildChangeWithoutExtraParentReads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &treeStore{}
		updates := New(store, nil).TreeEvents(t.Context(), "parent")
		store.count.Store(1)
		time.Sleep(PollInterval)
		synctest.Wait()
		select {
		case _, ok := <-updates:
			require.True(t, ok)
		default:
			require.FailNow(t, "child change was not reported")
		}
		assert.Zero(t, store.parentReads.Load())
	})
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

type changingIdentityStore struct {
	baselineStore
	state atomic.Value
}

func (s *changingIdentityStore) GetSessionWatchState(string) (db.SessionWatchState, error) {
	return s.state.Load().(db.SessionWatchState), nil
}

type unavailableVersionStore struct {
	changingIdentityStore
	unavailable atomic.Bool
}

func (s *unavailableVersionStore) GetSessionVersion(ctx context.Context, id string) (int, int64, bool) {
	if s.unavailable.Load() {
		return 0, 0, false
	}
	return s.baselineStore.GetSessionVersion(ctx, id)
}

func TestEventsRevisionChange(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(fmt.Sprintf("version unavailable=%t", unavailable), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				store := &unavailableVersionStore{}
				store.count.Store(1)
				store.state.Store(db.SessionWatchState{State: db.SessionWatchResolved, PublicID: "session-a", ContentRevision: "one"})
				updates := New(store, nil).Events(ctx, "session-a")
				store.unavailable.Store(unavailable)
				store.state.Store(db.SessionWatchState{State: db.SessionWatchResolved, PublicID: "session-a", ContentRevision: "two"})
				time.Sleep(PollInterval)
				synctest.Wait()
				_, ok := <-updates
				require.True(t, ok)
				synctest.Wait()
				if !unavailable {
					store.state.Store(db.SessionWatchState{State: "ambiguous"})
					time.Sleep(PollInterval)
					synctest.Wait()
					_, ok = <-updates
					require.True(t, ok)
					_, ok = <-updates
					assert.False(t, ok)
					return
				}
				store.unavailable.Store(false)
				time.Sleep(PollInterval)
				synctest.Wait()
				select {
				case <-updates:
					require.FailNow(t, "unchanged version emitted a second update")
				default:
				}
			})
		})
	}
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

type splitReadStore struct {
	db.Store
	mu             gosync.Mutex
	state          db.SessionWatchState
	count          int
	failUnresolved bool
	onTreeRead     func()
	onVersionRead  func()
}

func (s *splitReadStore) GetSessionWatchState(string) (db.SessionWatchState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, nil
}

func (s *splitReadStore) GetSessionVersion(context.Context, string) (int, int64, bool) {
	s.mu.Lock()
	cb := s.onVersionRead
	s.onVersionRead = nil
	s.mu.Unlock()
	if cb != nil {
		cb()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count, 1, true
}

func (s *splitReadStore) GetSessionTreeMarker(_ context.Context, id string) (db.SessionTreeSnapshot, error) {
	s.mu.Lock()
	cb := s.onTreeRead
	s.onTreeRead = nil
	s.mu.Unlock()
	if cb != nil {
		cb()
	}
	s.mu.Lock()
	state := s.state
	count := s.count
	failUnresolved := s.failUnresolved
	s.mu.Unlock()
	if failUnresolved && state.State != db.SessionWatchResolved {
		return db.SessionTreeSnapshot{}, &db.SessionIdentityError{State: state.State, Variants: state.Variants}
	}
	return db.NewSessionTreeSnapshot([]db.SessionTreeEntry{{
		ID:      id,
		Count:   count,
		Version: 1,
		State: &db.SessionTreeState{
			State:           state.State,
			PublicID:        state.PublicID,
			Variants:        state.Variants,
			ContentRevision: state.ContentRevision,
		},
	}})
}

func TestWatchSplitReadEmitsOnce(t *testing.T) {
	for _, tc := range []struct {
		name           string
		tree           bool
		failUnresolved bool
		next           db.SessionWatchState
		nextCnt        int
		wantStop       bool
	}{
		{
			name:    "tree revision change between watch state and tree marker",
			tree:    true,
			next:    db.SessionWatchState{State: db.SessionWatchResolved, PublicID: "session-a", ContentRevision: "two"},
			nextCnt: 1,
		},
		{
			name:     "tree unresolved transition between watch state and tree marker",
			tree:     true,
			next:     db.SessionWatchState{State: "ambiguous", Variants: []string{"session-a~one", "session-a~two"}},
			nextCnt:  1,
			wantStop: true,
		},
		{
			name:           "tree unresolved marker error between watch state and tree marker",
			tree:           true,
			failUnresolved: true,
			next:           db.SessionWatchState{State: "ambiguous", Variants: []string{"session-a~one", "session-a~two"}},
			nextCnt:        1,
			wantStop:       true,
		},
		{
			name:    "non-tree revision and version change between watch state and version read",
			tree:    false,
			next:    db.SessionWatchState{State: db.SessionWatchResolved, PublicID: "session-a", ContentRevision: "two"},
			nextCnt: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				store := &splitReadStore{
					state:          db.SessionWatchState{State: db.SessionWatchResolved, PublicID: "session-a", ContentRevision: "one"},
					count:          1,
					failUnresolved: tc.failUnresolved,
				}
				var updates <-chan struct{}
				if tc.tree {
					updates = New(store, nil).TreeEvents(ctx, "session-a")
				} else {
					updates = New(store, nil).Events(ctx, "session-a")
				}
				synctest.Wait()

				mutate := func() {
					store.mu.Lock()
					store.state = tc.next
					store.count = tc.nextCnt
					store.mu.Unlock()
				}
				store.mu.Lock()
				if tc.tree {
					store.onTreeRead = mutate
				} else {
					store.onVersionRead = mutate
				}
				store.mu.Unlock()

				time.Sleep(PollInterval)
				synctest.Wait()
				select {
				case _, ok := <-updates:
					require.True(t, ok, "change must emit one update")
				default:
					require.FailNow(t, "change did not emit an update")
				}
				synctest.Wait()

				if tc.wantStop {
					select {
					case _, ok := <-updates:
						assert.False(t, ok, "unresolved watch must close without a duplicate update")
					default:
						require.FailNow(t, "unresolved watch did not close")
					}
					return
				}

				time.Sleep(PollInterval)
				synctest.Wait()
				select {
				case _, ok := <-updates:
					require.True(t, ok, "resolved watch must stay open")
					require.FailNow(t, "split read emitted a duplicate update")
				default:
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
		false, nil,
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
		false, nil,
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
		false, nil,
	)
	assert.False(t, changed, "the fallback delay must elapse before direct sync")
	require.False(t, fileMtimeChangedAt.IsZero(),
		"the sidecar mtime must start the fallback timer")
	assert.Equal(t, sidecarTime.UnixNano(), lastFileMtime)
	started := fileMtimeChangedAt
	before, err := database.GetSessionTreeMarker(t.Context(), sessionID)
	require.NoError(t, err)
	checkTree := func() (bool, bool) {
		current, err := database.GetSessionTreeMarker(t.Context(), sessionID)
		require.NoError(t, err)
		changed, parentChanged := current.Marker != before.Marker, treeParentChanged(before, current)
		before = current
		return changed, parentChanged
	}
	dbtest.SeedSession(t, database, "child", "proj", func(s *db.Session) {
		s.ParentSessionID = new(sessionID)
	})
	changed = watcher.checkDBForChanges(t.Context(), sessionID, &lastCount, &lastDBVersion, &sourcePath, &lastFileMtime, &fileMtimeChangedAt, true, checkTree)
	assert.True(t, changed)
	assert.Equal(t, started, fileMtimeChangedAt, "child changes must not delay source fallback")

	parent, err := database.GetSessionFull(t.Context(), sessionID)
	require.NoError(t, err)
	parent.MessageCount++
	require.NoError(t, database.UpsertSession(t.Context(), *parent))
	changed = watcher.checkDBForChanges(t.Context(), sessionID, &lastCount, &lastDBVersion, &sourcePath, &lastFileMtime, &fileMtimeChangedAt, true, checkTree)
	assert.True(t, changed)
	assert.True(t, fileMtimeChangedAt.IsZero(), "parent changes must clear source fallback")
	lastCount, lastDBVersion, ok = database.GetSessionVersion(t.Context(), sessionID)
	require.True(t, ok)

	fileMtimeChangedAt = time.Now().Add(-SyncFallbackDelay)
	changed = watcher.checkDBForChanges(t.Context(),
		sessionID,
		&lastCount,
		&lastDBVersion,
		&sourcePath,
		&lastFileMtime,
		&fileMtimeChangedAt,
		false, nil,
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
			&lastCount, &lastDBVersion, &sourcePath, &lastFileMtime, fileMtimeChangedAt, false, nil)
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
