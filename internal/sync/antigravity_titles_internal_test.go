package sync

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

type antigravityTitleQuerySpy struct {
	db.Store
	reader  sessionWriteIdentityReader
	queries [][]string
}

func (s *antigravityTitleQuerySpy) ListSessionWriteIdentitiesByID(ctx context.Context, ids []string) (map[string]db.SessionWriteIdentity, error) {
	s.queries = append(s.queries, append([]string(nil), ids...))
	return s.reader.ListSessionWriteIdentitiesByID(ctx, ids)
}

func TestAntigravityUnrelatedChangedPathSkipsUnchangedStoreQueries(t *testing.T) {
	root := t.TempDir()
	transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
	store := dbtest.SetAntigravityTitle(t, root, "First")
	archive := openTestDB(t)
	engine := NewEngine(t.Context(), archive, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentAntigravity: {root}}, Machine: "host-a"})
	t.Cleanup(engine.Close)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
	spy := &antigravityTitleQuerySpy{Store: archive, reader: archive}
	engine.archiveStore = spy
	engine.deferredNativeTitles = make(map[string]*string)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	require.Equal(t, [][]string{{"antigravity:" + dbtest.AntigravityTitleID}}, spy.queries)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{filepath.Join(root, "unrelated")}))
	assert.Len(t, spy.queries, 1)
	plan, err := engine.PlanChangedPathsContext(t.Context(), []string{filepath.Join(root, "unrelated")})
	require.NoError(t, err)
	_, err = engine.SyncChangedPathPlanContext(t.Context(), plan, nil)
	require.NoError(t, err)
	assert.Len(t, spy.queries, 1)
}

func TestAntigravityFramelessWALEventsDoNotRereadStore(t *testing.T) {
	root := t.TempDir()
	transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
	store := dbtest.SetAntigravityTitle(t, root, "First")
	archive := openTestDB(t)
	engine := NewEngine(t.Context(), archive, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentAntigravity: {root}}, Machine: "host-a"})
	t.Cleanup(engine.Close)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
	spy := &antigravityTitleQuerySpy{Store: archive, reader: archive}
	engine.archiveStore = spy
	engine.deferredNativeTitles = make(map[string]*string)
	for _, size := range []int{0, 32} {
		require.NoError(t, os.WriteFile(store+"-wal", make([]byte, size), 0o600))
		require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store + "-wal"}))
		require.NoError(t, os.Remove(store+"-wal"))
		require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store + "-wal"}))
		plan, err := engine.PlanChangedPathsContext(t.Context(), []string{store + "-wal"})
		require.NoError(t, err)
		_, err = engine.SyncChangedPathPlanContext(t.Context(), plan, nil)
		require.NoError(t, err)
	}
	assert.Empty(t, spy.queries, "reader-created WAL events must not reread titles")
}

func TestAntigravityCLIFramelessWALEventsDoNotRereadStore(t *testing.T) {
	container := filepath.Join(t.TempDir(), ".t3", "userdata", "providers", "antigravity")
	sessionRoot := filepath.Join(container, "inst-a", "antigravity-acp")
	convDir := filepath.Join(sessionRoot, "conversations")
	require.NoError(t, os.MkdirAll(convDir, 0o755))
	dbPath := filepath.Join(convDir, dbtest.AntigravityTitleID+".db")
	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(),
		"CREATE TABLE steps (idx INTEGER PRIMARY KEY, step_type INTEGER, step_payload BLOB)")
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	dbtest.WriteAntigravityTitleTranscript(t, sessionRoot, "hello")
	store := dbtest.SetAntigravityTitle(t, sessionRoot, "First")
	archive := openTestDB(t)
	engine := NewEngine(t.Context(), archive, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentAntigravityCLI: {container}},
		Machine:   "host-a",
	})
	t.Cleanup(engine.Close)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{dbPath}))
	spy := &antigravityTitleQuerySpy{Store: archive, reader: archive}
	engine.archiveStore = spy
	engine.deferredNativeTitles = make(map[string]*string)
	for _, size := range []int{0, 32} {
		require.NoError(t, os.WriteFile(store+"-wal", make([]byte, size), 0o600))
		require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store + "-wal"}))
		require.NoError(t, os.Remove(store+"-wal"))
		require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store + "-wal"}))
		plan, err := engine.PlanChangedPathsContext(t.Context(), []string{store + "-wal"})
		require.NoError(t, err)
		assert.Empty(t, plan.Files)
		assert.Empty(t, plan.FallbackProviders)
		_, err = engine.SyncChangedPathPlanContext(t.Context(), plan, nil)
		require.NoError(t, err)
	}
	assert.Empty(t, spy.queries, "reader-created WAL events must not reread CLI titles")
}

func TestQoderTitleStoreOutsideWatchedRootsDoesNotRetry(t *testing.T) {
	root, store := testjsonl.QoderTitleRoot(t)
	transcript := testjsonl.WriteQoderTranscript(t, root, testjsonl.QoderTitleID, "hello")
	testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "First")
	archive := openTestDB(t)
	engine := NewEngine(t.Context(), archive, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentQoder: {root}}, Machine: "host-a"})
	t.Cleanup(engine.Close)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
	require.NoError(t, os.WriteFile(store, []byte("not a database"), 0o600))
	stats, err := engine.SyncWatchBatchThenRun(t.Context(), WatchBatch{Paths: []string{filepath.Join(root, "unrelated")}}, nil, nil)
	assert.Equal(t, 1, stats.Failed)
	require.NoError(t, err, "an external Qoder store must not schedule a watch retry")
	require.NoError(t, os.Remove(store))
	testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "Recovered rename")
	stats = engine.SyncAll(t.Context(), nil)
	assert.Zero(t, stats.Failed)
	session, err := archive.GetSessionFull(t.Context(), "qoder:"+testjsonl.QoderTitleID)
	require.NoError(t, err)
	assert.Equal(t, new("Recovered rename"), session.SessionName)
}

func TestNativeTitleFailureDoesNotRetryOtherAgents(t *testing.T) {
	for _, mode := range []string{"watcher", "watcher-read", "plan", "recovery", "grouped"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				root, claudeRoot := t.TempDir(), t.TempDir()
				transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
				store := dbtest.SetAntigravityTitle(t, root, "First")
				claudePath := writeClaudeProjectSession(t, claudeRoot, "project", "missing")
				archive := openTestDB(t)
				engine := NewEngine(t.Context(), archive, EngineConfig{AgentDirs: map[parser.AgentType][]string{
					parser.AgentAntigravity: {root}, parser.AgentClaude: {claudeRoot},
				}, Machine: "host-a"})
				t.Cleanup(engine.Close)
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript, claudePath}))
				require.NoError(t, os.Remove(claudePath))
				if mode == "plan" {
					require.NoError(t, engine.SyncPathsContext(t.Context(), []string{claudePath}))
				}
				beforeSync, beforeStats := engine.LastSync(), engine.LastSyncStats()
				dbtest.SetAntigravityTitle(t, root, "Later")
				var raw *sql.DB
				var savedStore []byte
				if mode == "watcher-read" {
					var err error
					savedStore, err = os.ReadFile(store)
					require.NoError(t, err)
					require.NoError(t, os.WriteFile(store, []byte("not a database"), 0o600))
				} else {
					var err error
					raw, err = sql.Open("sqlite3", archive.Path())
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, raw.Close()) })
					_, err = raw.ExecContext(t.Context(), `CREATE TRIGGER fail_title_write BEFORE UPDATE OF session_name ON sessions BEGIN SELECT RAISE(FAIL, 'injected title write failure'); END`)
					require.NoError(t, err)
				}
				spy := &antigravityTitleQuerySpy{Store: archive, reader: archive}
				engine.archiveStore = spy
				engine.deferredNativeTitles = make(map[string]*string)
				calls := make(chan WatchBatch, 2)
				switch mode {
				case "watcher", "watcher-read":
					watcher, err := newWatcherWithBackend(0, 20*time.Millisecond, func(ctx context.Context, batch WatchBatch) error {
						err := ApplyWatchBatch(ctx, engine, batch, nil)
						calls <- batch
						return err
					}, newFakeWatchBackend(), 8, 10_000)
					require.NoError(t, err)
					watcher.Start()
					defer watcher.Stop()
					watcher.QueueRetryBatch(WatchBatch{Paths: []string{claudePath, store}})
					requireReceiveWithin(t, calls, time.Second)
					synctest.Wait()
				case "plan":
					plan, err := engine.PlanChangedPathsContext(t.Context(), []string{store})
					require.NoError(t, err)
					result, err := engine.SyncChangedPathPlanContext(t.Context(), plan, nil)
					require.NoError(t, err)
					assert.False(t, result.Stats.ProcessingComplete())
				case "recovery":
					stats, _, err := engine.ReconcileWatchRootsWithStats(t.Context(), []string{claudeRoot}, false, nil)
					require.NoError(t, err)
					assert.Equal(t, 1, stats.Failed)
				case "grouped":
					require.NoError(t, engine.ReconcileProviderRootsGrouped(t.Context(), []ProviderRootsGroup{
						{Agent: parser.AgentClaude, Roots: []string{claudeRoot}},
						{Agent: parser.AgentAntigravity, Roots: []string{root}},
					}))
				}
				if mode != "watcher-read" {
					assert.Len(t, spy.queries, 1, "the grouped pass attempts each title store once")
				}
				assert.Equal(t, 1, engine.LastSyncStats().Failed)
				assert.Empty(t, engine.SnapshotAntigravityTitleStates())
				session, err := archive.GetSessionFull(t.Context(), "antigravity:"+dbtest.AntigravityTitleID)
				require.NoError(t, err)
				assert.Equal(t, new("First"), session.SessionName)
				if mode == "plan" {
					assert.Equal(t, beforeSync, engine.LastSync())
				}
				var missing sql.NullString
				require.NoError(t, archive.Reader().QueryRow(t.Context(), "SELECT source_missing_at FROM sessions WHERE id = 'missing'").Scan(&missing))
				assert.True(t, missing.Valid, "title failures preserve other agents' deletion proof")
				if mode == "watcher-read" {
					require.NoError(t, os.WriteFile(store, savedStore, 0o600))
				} else {
					_, err = raw.ExecContext(t.Context(), "DROP TRIGGER fail_title_write")
					require.NoError(t, err)
				}
				if mode == "watcher" || mode == "watcher-read" {
					time.Sleep(20 * time.Millisecond)
					synctest.Wait()
					retry := requireReceiveWithin(t, calls, time.Second)
					assert.Equal(t, []string{store}, retry.Paths)
					assert.Empty(t, retry.ReconcileRoots)
					assert.False(t, retry.FullSync)
				} else {
					time.Sleep(time.Nanosecond)
					require.NoError(t, engine.SyncPathsContext(t.Context(), []string{filepath.Join(root, "unrelated")}))
				}
				session, err = archive.GetSessionFull(t.Context(), "antigravity:"+dbtest.AntigravityTitleID)
				require.NoError(t, err)
				assert.Equal(t, new("Later"), session.SessionName)
				assert.Equal(t, beforeStats.TitlesUpdated+1, engine.LastSyncStats().TitlesUpdated)
				assert.True(t, engine.LastSync().After(beforeSync))
			})
		})
	}
}

func TestAntigravityMatchingMarkersExpireOrRefreshOnRequest(t *testing.T) {
	for _, mode := range []string{"interval", "watcher", "plan", "full"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				now := time.Now()
				root := t.TempDir()
				transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
				store := dbtest.SetAntigravityTitle(t, root, "First")
				conn, err := sql.Open("sqlite3", store)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, conn.Close()) })
				_, err = conn.ExecContext(t.Context(), "PRAGMA journal_mode=WAL; UPDATE conversation_summaries SET title = 'First'")
				require.NoError(t, err)
				checkpoint := func() {
					_, err := conn.ExecContext(t.Context(), "PRAGMA wal_checkpoint(TRUNCATE)")
					require.NoError(t, err)
					for _, path := range []string{store, store + "-wal"} {
						require.NoError(t, os.Chtimes(path, now, now))
					}
				}
				checkpoint()
				archive := openTestDB(t)
				engine := NewEngine(t.Context(), archive, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentAntigravity: {root}}, Machine: "host-a"})
				t.Cleanup(engine.Close)
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
				before, ok := parser.StatSQLiteContainerState(store)
				require.True(t, ok)
				_, err = conn.ExecContext(t.Context(), "UPDATE conversation_summaries SET title = 'Later'")
				require.NoError(t, err)
				checkpoint()
				after, ok := parser.StatSQLiteContainerState(store)
				require.True(t, ok)
				require.Equal(t, before, after, "a checkpointed rename can preserve every marker")
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{filepath.Join(root, "unrelated")}))
				session, err := archive.GetSessionFull(t.Context(), "antigravity:"+dbtest.AntigravityTitleID)
				require.NoError(t, err)
				require.Equal(t, new("First"), session.SessionName)
				switch mode {
				case "interval":
					time.Sleep(parser.SQLiteContainerDigestVerifyInterval)
					require.NoError(t, engine.SyncPathsContext(t.Context(), []string{filepath.Join(root, "unrelated")}))
				case "watcher":
					require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
				case "plan":
					plan, err := engine.PlanChangedPathsContext(t.Context(), []string{store})
					require.NoError(t, err)
					_, err = engine.SyncChangedPathPlanContext(t.Context(), plan, nil)
					require.NoError(t, err)
				case "full":
					engine.SyncAll(t.Context(), nil)
				}
				session, err = archive.GetSessionFull(t.Context(), "antigravity:"+dbtest.AntigravityTitleID)
				require.NoError(t, err)
				assert.Equal(t, new("Later"), session.SessionName)
				assert.Equal(t, 1, engine.LastSyncStats().TitlesUpdated)
			})
		})
	}
}

func TestAntigravitySummaryReadFailureRetriesWatchBatchWhenTitlesSucceed(t *testing.T) {
	const (
		convID   = "11111111-2222-3333-4444-555555555551"
		parentID = "cccccccc-dddd-eeee-ffff-000000000000"
	)
	for _, agent := range []parser.AgentType{parser.AgentAntigravity, parser.AgentAntigravityCLI} {
		t.Run(string(agent), func(t *testing.T) {
			base := t.TempDir()
			configuredRoot := base
			sessionRoot := base
			if agent == parser.AgentAntigravityCLI {
				configuredRoot = filepath.Join(base, ".t3", "userdata", "providers", "antigravity")
				sessionRoot = filepath.Join(configuredRoot, "inst-a", "antigravity-acp")
			}
			convDir := filepath.Join(sessionRoot, "conversations")
			require.NoError(t, os.MkdirAll(convDir, 0o755))
			dbPath := filepath.Join(convDir, convID+".db")
			conn, err := sql.Open("sqlite3", dbPath)
			require.NoError(t, err)
			_, err = conn.ExecContext(t.Context(),
				"CREATE TABLE steps (idx INTEGER PRIMARY KEY, step_type INTEGER, step_payload BLOB)")
			require.NoError(t, err)
			require.NoError(t, conn.Close())
			testjsonl.WriteAntigravityBrainTranscript(t, sessionRoot, convID,
				`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"hello"}`+"\n")
			store := testjsonl.WriteAntigravitySummaries(t, sessionRoot, map[string]testjsonl.AntigravitySummary{
				convID: {Title: "Alpha", WorkspaceURIs: `["file:///tmp/ws-old"]`},
			})

			archive := openTestDB(t)
			engine := NewEngine(t.Context(), archive, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{agent: {configuredRoot}},
				Machine:   "host-a",
			})
			t.Cleanup(engine.Close)
			require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)

			// Update title, workspace, and parent, then fail the first summary
			// metadata read while letting title reconciliation succeed.
			testjsonl.WriteAntigravitySummaries(t, sessionRoot, map[string]testjsonl.AntigravitySummary{
				convID: {
					Title:         "Alpha renamed",
					WorkspaceURIs: `["file:///tmp/ws-new"]`,
					ParentID:      parentID,
				},
			})
			failedOnce := false
			engine.antigravitySummaryDigests = func(ctx context.Context, root string) (map[string]string, error) {
				if !failedOnce {
					failedOnce = true
					return nil, errors.New("database is locked")
				}
				return parser.AntigravitySummaryDigests(ctx, root)
			}

			stats, err := engine.SyncWatchBatchThenRun(
				t.Context(), WatchBatch{Paths: []string{store}}, nil, nil,
			)
			require.Error(t, err, "transient metadata-read failure must schedule a watcher retry even when title reads succeed")
			assert.Equal(t, 0, stats.Synced)
			assert.Equal(t, 1, stats.TitlesUpdated)
			assert.Equal(t, 1, stats.Failed)
			assert.Empty(t, engine.SnapshotAntigravityTitleStates(), "title state must not persist after metadata-read failure")

			retryErr, ok := errors.AsType[interface {
				error
				WatchRetryBatch() WatchBatch
			}](err)
			require.True(t, ok)
			retry := retryErr.WatchRetryBatch()
			assert.Equal(t, []string{store}, retry.Paths)

			sessionID := string(agent) + ":" + convID
			beforeRetry, err := archive.GetSessionFull(t.Context(), sessionID)
			require.NoError(t, err)
			require.NotNil(t, beforeRetry)
			assert.Equal(t, new("Alpha renamed"), beforeRetry.SessionName)
			assert.Equal(t, "/tmp/ws-old", beforeRetry.Cwd)

			retryStats, err := engine.SyncWatchBatchThenRun(t.Context(), retry, nil, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, retryStats.Synced)
			assert.Equal(t, 0, retryStats.Failed)
			assert.Contains(t, engine.SnapshotAntigravityTitleStates(), store)

			afterRetry, err := archive.GetSessionFull(t.Context(), sessionID)
			require.NoError(t, err)
			require.NotNil(t, afterRetry)
			assert.Equal(t, "/tmp/ws-new", afterRetry.Cwd)
			assert.Equal(t, "ws_new", afterRetry.Project)
			assert.Equal(t, new(string(agent)+":"+parentID), afterRetry.ParentSessionID)
			assert.Equal(t, "subagent", afterRetry.RelationshipType)
			assert.Equal(t, new("Alpha renamed"), afterRetry.SessionName)
		})
	}
}
