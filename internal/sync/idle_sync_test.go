package sync

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestWatcherLinkWorkDoesNotScaleWithArchive(t *testing.T) {
	for _, count := range []int{8, 800} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
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
	_, err = raw.Exec(`CREATE TRIGGER fail_polled_link
		BEFORE UPDATE OF parent_session_id ON sessions
		WHEN NEW.id = 'grouped-child'
		BEGIN SELECT RAISE(FAIL, 'injected poll link failure'); END`)
	require.NoError(t, err)
	groups := []ProviderRootsGroup{{Agent: parser.AgentClaude, Roots: []string{root}}}
	require.ErrorContains(t, engine.ReconcileProviderRootsGrouped(t.Context(), groups), "injected poll link failure")
	requireGroupedChildParent(t, database, false, "the failed link must remain pending")
	_, err = raw.Exec(`DROP TRIGGER fail_polled_link`)
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
			_, err = raw.Exec(`CREATE TRIGGER fail_polled_link
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
			_, err = raw.Exec(`DROP TRIGGER fail_polled_link`)
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
	_, err = raw.Exec(`CREATE TRIGGER fail_polled_link
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
	_, err = raw.Exec(`DROP TRIGGER fail_polled_link`)
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
