package sync

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestWatcherLinkWorkDoesNotScaleWithArchive(t *testing.T) {
	for _, count := range []int{8, 80} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			database := openTestDB(t)
			root := t.TempDir()
			writeGroupedClaudeFixture(t, root, "changed")
			for i := range count {
				path := filepath.Join(root, "absent", fmt.Sprintf("%d.jsonl", i))
				session := db.Session{
					ID: fmt.Sprintf("archive-%d", i), Agent: "claude",
					Project: "fixture", Machine: "local", FilePath: &path,
				}
				require.NoError(t, database.UpsertSession(t.Context(), session))
				if i%2 == 0 {
					require.NoError(t, database.SoftDeleteSession(t.Context(), session.ID))
				}
			}
			engine := NewEngine(t.Context(), database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
				Machine:   "local",
			})
			t.Cleanup(engine.Close)
			metrics := &reconciliationRuntimeMetrics{}
			ctx := context.WithValue(t.Context(), reconciliationMetricsContextKey{}, metrics)
			path := filepath.Join(root, "project", "changed.jsonl")
			for range 3 {
				require.NoError(t, engine.SyncPathsContext(ctx, []string{path}))
			}
			assert.Zero(t, metrics.snapshot(ReconciliationMetrics{}).GlobalLinkPasses,
				"watcher batches must never link the entire archive")
			session, err := database.GetSession(t.Context(), "changed")
			require.NoError(t, err)
			require.NotNil(t, session)
			assert.Equal(t, 1, session.MessageCount)
			retained, err := database.GetSession(t.Context(), "archive-1")
			require.NoError(t, err)
			require.NotNil(t, retained)
			assert.Nil(t, retained.DeletedAt, "an unrelated missing source must remain archived")
			trashed, err := database.GetSessionFull(t.Context(), "archive-0")
			require.NoError(t, err)
			require.NotNil(t, trashed)
			assert.NotNil(t, trashed.DeletedAt, "watcher work must preserve unrelated tombstones")
		})
	}
}

func TestPollingRetriesFailedLinkWithoutNewSourceChanges(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	writeGroupedClaudeFixture(t, root, "polled-link-retry")
	seedGroupedSubagentFixture(t, database)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}, Machine: "local",
	})
	t.Cleanup(engine.Close)
	raw, err := sql.Open("sqlite3", database.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	_, err = raw.ExecContext(t.Context(), `CREATE TRIGGER fail_polled_link
		BEFORE UPDATE OF parent_session_id ON sessions
		WHEN NEW.id = 'grouped-child'
		BEGIN SELECT RAISE(FAIL, 'injected poll link failure'); END`)
	require.NoError(t, err)
	groups := []ProviderRootsGroup{{Agent: parser.AgentClaude, Roots: []string{root}}}
	require.ErrorContains(t, engine.ReconcileProviderRootsGrouped(t.Context(), groups), "injected poll link failure")
	requireGroupedChildParent(t, database, false, "the failed link must remain pending")
	_, err = raw.ExecContext(t.Context(), `DROP TRIGGER fail_polled_link`)
	require.NoError(t, err)
	require.NoError(t, engine.ReconcileProviderRootsGrouped(t.Context(), groups))
	requireGroupedChildParent(t, database, true, "unchanged sources must not suppress a failed link retry")
	metrics := &reconciliationRuntimeMetrics{}
	ctx := context.WithValue(t.Context(), reconciliationMetricsContextKey{}, metrics)
	require.NoError(t, engine.ReconcileProviderRootsGrouped(ctx, groups))
	assert.Zero(t, metrics.snapshot(ReconciliationMetrics{}).GlobalLinkPasses,
		"a successful retry must let polling return to idle")
}

func TestPendingLinkRetryEmitsSessionsWithoutSourceChanges(t *testing.T) {
	for _, grouped := range []bool{false, true} {
		name := "single provider"
		if grouped {
			name = "grouped"
		}
		t.Run(name, func(t *testing.T) {
			database := openTestDB(t)
			root := t.TempDir()
			writeGroupedClaudeFixture(t, root, "polled-link-retry")
			seedGroupedSubagentFixture(t, database)
			emitter := &fakeEmitter{}
			engine := NewEngine(t.Context(), database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{
					parser.AgentClaude: {root},
				},
				Machine: "local",
				Emitter: emitter,
			})
			t.Cleanup(engine.Close)
			raw, err := sql.Open("sqlite3", database.Path())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, raw.Close()) })
			_, err = raw.ExecContext(t.Context(), `CREATE TRIGGER fail_polled_link
				BEFORE UPDATE OF parent_session_id ON sessions
				WHEN NEW.id = 'grouped-child'
				BEGIN SELECT RAISE(FAIL, 'injected poll link failure'); END`)
			require.NoError(t, err)
			reconcile := func() error {
				if grouped {
					return engine.ReconcileProviderRootsGrouped(t.Context(),
						[]ProviderRootsGroup{{
							Agent: parser.AgentClaude, Roots: []string{root},
						}})
				}
				return engine.ReconcileProviderRoots(
					t.Context(), parser.AgentClaude, []string{root},
				)
			}

			require.ErrorContains(t, reconcile(), "injected poll link failure")
			requireGroupedChildParent(t, database, false,
				"the failed link must remain pending")
			_, err = raw.ExecContext(t.Context(), `DROP TRIGGER fail_polled_link`)
			require.NoError(t, err)
			polled, err := database.GetSessionFull(t.Context(), "polled-link-retry")
			require.NoError(t, err)
			require.NotNil(t, polled)
			require.NotNil(t, polled.LocalModifiedAt)
			unchangedAt := *polled.LocalModifiedAt
			emitter.mu.Lock()
			emitter.scopes = nil
			emitter.mu.Unlock()

			require.NoError(t, reconcile())
			requireGroupedChildParent(t, database, true,
				"the pending link must repair the parent")
			polled, err = database.GetSessionFull(t.Context(), "polled-link-retry")
			require.NoError(t, err)
			require.NotNil(t, polled.LocalModifiedAt)
			assert.Equal(t, unchangedAt, *polled.LocalModifiedAt,
				"the retry must leave the unchanged transcript untouched")
			assert.Equal(t, []string{"sessions"}, emitter.got(),
				"a parent-link repair must refresh clients when nothing else changed")

			emitter.mu.Lock()
			emitter.scopes = nil
			emitter.mu.Unlock()
			require.NoError(t, reconcile())
			assert.Empty(t, emitter.got(),
				"an unchanged poll after the repair must not refresh clients")
		})
	}
}

func TestSyncThenRunEmitsForLinkOnlyRepair(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	writeGroupedClaudeFixture(t, root, "unchanged")
	emitter := &fakeEmitter{}
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
		Machine:   "local", Emitter: emitter,
	})
	t.Cleanup(engine.Close)
	initial := engine.SyncAll(t.Context(), nil)
	require.Zero(t, initial.Failed)
	require.False(t, database.NeedsResync())
	seedGroupedSubagentFixture(t, database)
	emitter.mu.Lock()
	emitter.scopes = nil
	emitter.mu.Unlock()

	stats, err := engine.SyncThenRun(t.Context(), false, nil, func(bool) error { return nil })
	require.NoError(t, err)
	requireGroupedChildParent(t, database, true, "the sync must repair the parent")
	require.Zero(t, stats.Synced, "the transcript did not change")
	assert.Equal(t, 1, stats.LinksUpdated)
	assert.Equal(t, []string{"sync"}, emitter.got(),
		"a committed parent repair must notify clients")
}

func TestQueuedParentRepairsNotifyOnUnchangedPoll(t *testing.T) {
	for _, cleanup := range []bool{false, true} {
		name := "link"
		if cleanup {
			name = "cleanup"
		}
		t.Run(name, func(t *testing.T) {
			database := openTestDB(t)
			root := t.TempDir()
			writeGroupedClaudeFixture(t, root, "queued-repair")
			emitter := &fakeEmitter{}
			engine := NewEngine(t.Context(), database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
				Machine:   "local", Emitter: emitter,
			})
			t.Cleanup(engine.Close)
			require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
			seedGroupedSubagentFixture(t, database)
			if cleanup {
				require.NoError(t, database.LinkSubagentSessions())
				require.NoError(t, database.QueueSubagentParentCleanupRepairs(
					t.Context(), []string{"grouped-child"},
				))
				_, err := database.DeleteParserExcludedSessions(t.Context(), []string{"grouped-parent"})
				require.NoError(t, err)
			} else {
				require.NoError(t, database.QueueSubagentParentRepairs(
					t.Context(), []string{"grouped-child"},
				))
			}
			emitter.mu.Lock()
			emitter.scopes = nil
			emitter.mu.Unlock()

			stats, _, err := engine.ReconcileWatchRootsWithStats(t.Context(), []string{root}, false, nil)
			require.NoError(t, err)
			child, err := database.GetSession(t.Context(), "grouped-child")
			require.NoError(t, err)
			require.NotNil(t, child)
			if cleanup {
				assert.Nil(t, child.ParentSessionID)
			} else {
				assert.Equal(t, new("grouped-parent"), child.ParentSessionID)
			}
			require.Zero(t, stats.Synced, "the source did not change")
			assert.Equal(t, 1, stats.LinksUpdated)
			assert.Equal(t, []string{"sessions"}, emitter.got())

			emitter.mu.Lock()
			emitter.scopes = nil
			emitter.mu.Unlock()
			stats, _, err = engine.ReconcileWatchRootsWithStats(t.Context(), []string{root}, false, nil)
			require.NoError(t, err)
			assert.Zero(t, stats.LinksUpdated)
			assert.Empty(t, emitter.got(), "completed repairs must not keep refreshing clients")
		})
	}
}

func TestCanceledSyncThenRunNotifiesCommittedParentRepair(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	writeGroupedClaudeFixture(t, root, "unchanged")
	emitter := &fakeEmitter{}
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
		Machine:   "local", Emitter: emitter,
	})
	t.Cleanup(engine.Close)
	require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
	seedGroupedSubagentFixture(t, database)
	require.NoError(t, database.LinkSubagentSessions())
	require.NoError(t, database.QueueSubagentParentCleanupRepairs(t.Context(), []string{"grouped-child"}))
	_, err := database.DeleteParserExcludedSessions(t.Context(), []string{"grouped-parent"})
	require.NoError(t, err)
	emitter.mu.Lock()
	emitter.scopes = nil
	emitter.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stats, err := engine.SyncThenRun(ctx, false, func(p Progress) {
		if p.Phase == PhaseSyncing {
			cancel()
		}
	}, func(bool) error {
		require.FailNow(t, "canceled sync must not start the follow-up work")
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, stats.Aborted)
	require.Zero(t, stats.Synced)
	assert.Equal(t, 1, stats.LinksUpdated)
	child, err := database.GetSession(t.Context(), "grouped-child")
	require.NoError(t, err)
	require.NotNil(t, child)
	assert.Nil(t, child.ParentSessionID, "queued cleanup committed despite cancellation")
	assert.Equal(t, []string{"sync"}, emitter.got())
}

func TestEmptyReconciliationRetriesQueuedParentCleanup(t *testing.T) {
	for _, mode := range []string{"partial", "full", "grouped"} {
		t.Run(mode, func(t *testing.T) {
			database := openTestDB(t)
			root := t.TempDir()
			seedGroupedSubagentFixture(t, database)
			require.NoError(t, database.LinkSubagentSessions())
			require.NoError(t, database.QueueSubagentParentCleanupRepairs(
				t.Context(), []string{"grouped-child"},
			))
			_, err := database.DeleteParserExcludedSessions(t.Context(), []string{"grouped-parent"})
			require.NoError(t, err)
			emitter := &fakeEmitter{}
			engine := NewEngine(t.Context(), database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
				Machine:   "local", Emitter: emitter,
			})
			t.Cleanup(engine.Close)
			metrics := &reconciliationRuntimeMetrics{}
			ctx := context.WithValue(t.Context(), reconciliationMetricsContextKey{}, metrics)
			reconcile := func() (SyncStats, error) {
				if mode == "grouped" {
					return SyncStats{}, engine.ReconcileProviderRootsGrouped(ctx, []ProviderRootsGroup{
						{Agent: parser.AgentClaude, Roots: []string{root}},
					})
				}
				stats, _, err := engine.ReconcileWatchRootsWithStats(ctx, []string{root}, mode == "full", nil)
				return stats, err
			}
			raw, err := sql.Open("sqlite3", database.Path())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, raw.Close()) })
			_, err = raw.ExecContext(ctx, `CREATE TRIGGER fail_empty_poll_cleanup
				BEFORE UPDATE OF parent_session_id ON sessions WHEN NEW.id = 'grouped-child'
				BEGIN SELECT RAISE(FAIL, 'injected queued cleanup failure'); END`)
			require.NoError(t, err)

			_, err = reconcile()
			require.ErrorContains(t, err, "injected queued cleanup failure")
			requireGroupedChildParent(t, database, true, "failed cleanup must preserve the stored parent")
			var queued int
			require.NoError(t, raw.QueryRowContext(ctx, "SELECT count(*) FROM subagent_parent_cleanup_queue").Scan(&queued))
			require.Equal(t, 1, queued, "failed cleanup must remain queued")
			assert.Empty(t, emitter.got())
			_, err = raw.ExecContext(ctx, "DROP TRIGGER fail_empty_poll_cleanup")
			require.NoError(t, err)

			stats, err := reconcile()
			require.NoError(t, err)
			requireGroupedChildParent(t, database, false, "an empty poll must retry the queued cleanup")
			if mode != "grouped" {
				assert.Equal(t, 1, stats.LinksUpdated)
			}
			assert.Equal(t, []string{"sessions"}, emitter.got())
			require.NoError(t, raw.QueryRowContext(ctx, "SELECT count(*) FROM subagent_parent_cleanup_queue").Scan(&queued))
			assert.Zero(t, queued)
			stats, err = reconcile()
			require.NoError(t, err)
			if mode != "grouped" {
				assert.Zero(t, stats.LinksUpdated)
			}
			assert.Equal(t, []string{"sessions"}, emitter.got(), "completed cleanup must not repeat notifications")
			switch mode {
			case "partial":
				assert.Zero(t, engine.LastReconciliationResult().Metrics.GlobalLinkPasses)
			case "grouped":
				assert.Zero(t, metrics.snapshot(ReconciliationMetrics{}).GlobalLinkPasses)
			}
		})
	}
}

func TestPendingParentRepairSurvivesUnreadableSource(t *testing.T) {
	for _, grouped := range []bool{false, true} {
		name := "single provider"
		if grouped {
			name = "grouped"
		}
		t.Run(name, func(t *testing.T) {
			database := openTestDB(t)
			root := t.TempDir()
			writeGroupedClaudeFixture(t, root, "polled-link-retry")
			seedGroupedSubagentFixture(t, database)
			emitter := &fakeEmitter{}
			engine := NewEngine(t.Context(), database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
				Machine:   "local", Emitter: emitter,
			})
			t.Cleanup(engine.Close)
			raw, err := sql.Open("sqlite3", database.Path())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, raw.Close()) })
			_, err = raw.ExecContext(t.Context(), `CREATE TRIGGER fail_polled_link
				BEFORE UPDATE OF parent_session_id ON sessions WHEN NEW.id = 'grouped-child'
				BEGIN SELECT RAISE(FAIL, 'injected poll link failure'); END`)
			require.NoError(t, err)
			reconcile := func(ctx context.Context) error {
				if grouped {
					return engine.ReconcileProviderRootsGrouped(ctx, []ProviderRootsGroup{
						{Agent: parser.AgentClaude, Roots: []string{root}},
					})
				}
				return engine.ReconcileProviderRoots(ctx, parser.AgentClaude, []string{root})
			}
			require.ErrorContains(t, reconcile(t.Context()), "injected poll link failure")
			requireGroupedChildParent(t, database, false, "failed linking must leave a retry")
			_, err = raw.ExecContext(t.Context(), "DROP TRIGGER fail_polled_link")
			require.NoError(t, err)
			writeGroupedClaudeFixture(t, root, "unreadable")
			unreadable := filepath.Join(root, "project", "unreadable.jsonl")
			require.NoError(t, os.Chmod(unreadable, 0))
			t.Cleanup(func() { require.NoError(t, os.Chmod(unreadable, 0o600)) })
			file, err := os.Open(unreadable)
			if err == nil {
				require.NoError(t, file.Close())
				t.Skip("this environment can read mode-000 files")
			}
			require.ErrorIs(t, err, os.ErrPermission)
			emitter.mu.Lock()
			emitter.scopes = nil
			emitter.mu.Unlock()
			metrics := &reconciliationRuntimeMetrics{}
			ctx := context.WithValue(t.Context(), reconciliationMetricsContextKey{}, metrics)

			require.ErrorContains(t, reconcile(ctx), "failed processing page: 1 failures")
			requireGroupedChildParent(t, database, true,
				"an unrelated unreadable source must not block the pending link")
			assert.Equal(t, []string{"sessions"}, emitter.got())
			if !grouped {
				assert.Equal(t, 1, engine.LastReconciliationResult().Metrics.GlobalLinkPasses)
			}
			require.ErrorContains(t, reconcile(ctx), "failed processing page: 1 failures")
			if grouped {
				assert.Equal(t, 1, metrics.snapshot(ReconciliationMetrics{}).GlobalLinkPasses)
			} else {
				assert.Zero(t, engine.LastReconciliationResult().Metrics.GlobalLinkPasses)
			}
			assert.Equal(t, []string{"sessions"}, emitter.got(),
				"the successful retry must not repeat while the unrelated failure persists")
		})
	}
}

func TestScopedParentRepairsReachSyncResults(t *testing.T) {
	for _, mode := range []string{"watcher", "plan", "single session"} {
		t.Run(mode, func(t *testing.T) {
			database := openTestDB(t)
			root := t.TempDir()
			path := filepath.Join(root, "project", "parent.jsonl")
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			require.NoError(t, os.WriteFile(path, []byte(`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"u1","message":{"content":"start"},"cwd":"/tmp","sessionId":"parent"}
{"type":"assistant","timestamp":"2024-01-01T10:01:00Z","uuid":"a1","parentUuid":"u1","message":{"content":[{"type":"tool_use","id":"spawn","name":"Agent","input":{"prompt":"child work"}}]}}
{"type":"user","timestamp":"2024-01-01T10:02:00Z","uuid":"u2","parentUuid":"a1","message":{"content":[{"type":"tool_result","tool_use_id":"spawn","content":"done"}]},"toolUseResult":{"status":"completed","agentId":"child"}}
`), 0o600))
			emitter := &fakeEmitter{}
			engine := NewEngine(t.Context(), database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
				Machine:   "local", Emitter: emitter,
			})
			t.Cleanup(engine.Close)
			if mode == "single session" {
				// The parent source is already fresh when the child arrives.
				require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
			}
			require.NoError(t, database.UpsertSession(t.Context(), db.Session{
				ID: "agent-child", Agent: "claude", Project: "project", Machine: "local",
			}))
			emitter.mu.Lock()
			emitter.scopes = nil
			emitter.mu.Unlock()
			switch mode {
			case "watcher":
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))
				assert.Equal(t, 1, engine.LastSyncStats().LinksUpdated)
			case "plan":
				plan, err := engine.PlanChangedPathsContext(t.Context(), []string{path})
				require.NoError(t, err)
				result, err := engine.SyncChangedPathPlanContext(t.Context(), plan, nil)
				require.NoError(t, err)
				assert.Equal(t, 1, result.Stats.LinksUpdated)
			case "single session":
				require.NoError(t, engine.SyncSingleSessionContext(t.Context(), "parent"))
				assert.Contains(t, emitter.got(), "sessions")
			}
			child, err := database.GetSession(t.Context(), "agent-child")
			require.NoError(t, err)
			require.NotNil(t, child)
			assert.Equal(t, new("parent"), child.ParentSessionID)
		})
	}
}

func TestPollingRetriesFailedLinkDespiteCachedSourceFailure(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	writeGroupedClaudeFixture(t, root, "polled-link-retry")
	seedGroupedSubagentFixture(t, database)
	geminiRoot := t.TempDir()
	broken := filepath.Join(geminiRoot, "tmp", "project", "chats", "session-broken.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(broken), 0o700))
	require.NoError(t, os.WriteFile(broken, []byte(`{"messages": invalid}`), 0o600))
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {root}, parser.AgentGemini: {geminiRoot},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	raw, err := sql.Open("sqlite3", database.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	_, err = raw.ExecContext(t.Context(), `CREATE TRIGGER fail_polled_link
		BEFORE UPDATE OF parent_session_id ON sessions
		WHEN NEW.id = 'grouped-child'
		BEGIN SELECT RAISE(FAIL, 'injected poll link failure'); END`)
	require.NoError(t, err)
	groups := []ProviderRootsGroup{
		{Agent: parser.AgentClaude, Roots: []string{root}},
		{Agent: parser.AgentGemini, Roots: []string{geminiRoot}},
	}
	require.Error(t, engine.ReconcileProviderRootsGrouped(t.Context(), groups))
	requireGroupedChildParent(t, database, false, "the failed link must remain pending")
	_, err = raw.ExecContext(t.Context(), `DROP TRIGGER fail_polled_link`)
	require.NoError(t, err)
	require.NoError(t, engine.ReconcileProviderRootsGrouped(t.Context(), groups))
	requireGroupedChildParent(t, database, true, "a cached source failure must not block the pending link")
}

func TestUnchangedPollingSkipsGlobalLinking(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	writeGroupedClaudeFixture(t, root, "polled")
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)
	groups := []ProviderRootsGroup{{Agent: parser.AgentClaude, Roots: []string{root}}}
	require.NoError(t, engine.ReconcileProviderRootsGrouped(t.Context(), groups))
	metrics := &reconciliationRuntimeMetrics{}
	ctx := context.WithValue(t.Context(), reconciliationMetricsContextKey{}, metrics)
	for range 3 {
		require.NoError(t, engine.ReconcileProviderRootsGrouped(ctx, groups))
	}
	assert.Zero(t, metrics.snapshot(ReconciliationMetrics{}).GlobalLinkPasses,
		"unchanged polling must not scan archive-wide spawn edges")
}

func TestCanceledChangedPathBatchRetriesLinksOnUnchangedPoll(t *testing.T) {
	for _, watcher := range []bool{false, true} {
		name := "plan"
		if watcher {
			name = "watcher"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				database := openTestDB(t)
				root := t.TempDir()
				path := filepath.Join(root, "project", "parent.jsonl")
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
				require.NoError(t, os.WriteFile(path, []byte(`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"u1","message":{"content":"start"},"cwd":"/tmp","sessionId":"parent"}
{"type":"assistant","timestamp":"2024-01-01T10:01:00Z","uuid":"a1","parentUuid":"u1","message":{"content":[{"type":"tool_use","id":"spawn","name":"Agent","input":{"prompt":"child work"}}]}}
{"type":"user","timestamp":"2024-01-01T10:02:00Z","uuid":"u2","parentUuid":"a1","message":{"content":[{"type":"tool_result","tool_use_id":"spawn","content":"done"}]},"toolUseResult":{"status":"completed","agentId":"child"}}
`), 0o600))
				require.NoError(t, database.UpsertSession(t.Context(), db.Session{
					ID: "agent-child", Agent: "claude", Project: "project", Machine: "local",
				}))
				const blockerAgent parser.AgentType = "cancel-blocker"
				blockedRoot := t.TempDir()
				blockedPath := filepath.Join(blockedRoot, "blocked.jsonl")
				require.NoError(t, os.WriteFile(blockedPath, []byte("{}\n"), 0o600))
				source := parser.SourceRef{Provider: blockerAgent, Key: blockedPath, DisplayPath: blockedPath, FingerprintKey: blockedPath}
				blocker := &directStreamingProvider{
					Def: parser.AgentDef{Type: blockerAgent, FileBased: true},
					Caps: parser.Capabilities{Source: parser.SourceCapabilities{
						DiscoverSources: parser.CapabilitySupported, StreamingDiscovery: parser.CapabilitySupported,
						WatchSources: parser.CapabilitySupported, FindSource: parser.CapabilitySupported,
					}},
					source: &source, parseRelease: make(chan struct{}),
				}
				engine := NewEngine(t.Context(), database, EngineConfig{
					AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}, blockerAgent: {blockedRoot}},
					Machine:   "local",
					ProviderFactories: append(parser.ProviderFactories(),
						directStreamingFactory{provider: blocker}),
					ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
						blockerAgent: parser.ProviderMigrationProviderAuthoritative,
					},
				})
				defer engine.Close()
				paths := []string{path, blockedPath}
				plan, err := engine.PlanChangedPathsContext(t.Context(), paths)
				require.NoError(t, err)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan error, 1)
				go func() {
					if watcher {
						done <- engine.SyncPathsContext(ctx, paths)
					} else {
						_, err := engine.SyncChangedPathPlanContext(ctx, plan, nil)
						done <- err
					}
				}()
				// The second provider blocks on cancellation, so the first result has
				// entered the collector but has not yet reached the final batch flush.
				synctest.Wait()
				progress, active := engine.CurrentProgress()
				require.True(t, active)
				require.Equal(t, 1, progress.SessionsDone)
				cancel()
				require.ErrorIs(t, <-done, context.Canceled)
				require.True(t, engine.LastSyncStats().Aborted)
				require.Equal(t, 1, engine.LastSyncStats().Synced)
				var edgeCount int
				require.NoError(t, database.Reader().QueryRow(t.Context(), `
					SELECT count(*) FROM tool_calls
					WHERE session_id='parent' AND subagent_session_id='agent-child'
				`).Scan(&edgeCount))
				require.Equal(t, 1, edgeCount, "cancellation must preserve the committed spawn edge")
				require.NoError(t, engine.ReconcileProviderRootsGrouped(t.Context(), []ProviderRootsGroup{
					{Agent: parser.AgentClaude, Roots: []string{root}},
				}))
				child, err := database.GetSession(t.Context(), "agent-child")
				require.NoError(t, err)
				require.NotNil(t, child)
				assert.Equal(t, new("parent"), child.ParentSessionID, "an unchanged poll must finish links from the canceled batch")
			})
		})
	}
}
