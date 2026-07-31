package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func writeGroupedClaudeFixture(t *testing.T, root, name string) {
	t.Helper()
	path := filepath.Join(root, "project", name+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(
		path,
		[]byte(testjsonl.NewSessionBuilder().
			AddClaudeUser("2024-01-01T00:00:00Z", "grouped fixture").
			String()),
		0o644,
	))
}

// seedGroupedSubagentFixture inserts a parent/child pair whose tool call
// references the child, so the shared epilogue's subagent linking pass has
// observable work. The rows live outside every reconciled root and under a
// different agent, keeping them out of scoped tombstoning.
func seedGroupedSubagentFixture(t *testing.T, database *db.DB) {
	t.Helper()
	fixturePath := filepath.Join(t.TempDir(), "fixture.jsonl")
	size := int64(1)
	mtime := int64(1)
	require.NoError(t, database.UpsertSession(db.Session{
		ID: "grouped-parent", Agent: "zencoder", Project: "project",
		Machine: "local", FilePath: &fixturePath, FileSize: &size,
		FileMtime: &mtime, MessageCount: 1,
	}))
	require.NoError(t, database.UpsertSession(db.Session{
		ID: "grouped-child", Agent: "zencoder", Project: "project",
		Machine: "local", FilePath: &fixturePath, FileSize: &size,
		FileMtime: &mtime, MessageCount: 1,
		RelationshipType: "continuation",
	}))
	require.NoError(t, database.InsertMessages([]db.Message{{
		SessionID: "grouped-parent", Ordinal: 0, Role: "assistant",
		Content: "spawning subagent", HasToolUse: true,
		ToolCalls: []db.ToolCall{{
			ToolUseID: "call-1", ToolName: "Task",
			SubagentSessionID: "grouped-child",
		}},
	}}))
}

func requireGroupedChildParent(
	t *testing.T, database *db.DB, wantLinked bool, msg string,
) {
	t.Helper()
	child, err := database.GetSession(context.Background(), "grouped-child")
	require.NoError(t, err)
	require.NotNil(t, child)
	if wantLinked {
		require.NotNil(t, child.ParentSessionID, msg)
		assert.Equal(t, "grouped-parent", *child.ParentSessionID, msg)
	} else {
		if child.ParentSessionID != nil {
			assert.NotEqual(t, "grouped-parent", *child.ParentSessionID, msg)
		}
	}
}

// TestReconcileProviderRootsGroupedRunsSharedEpilogueOnce is the
// providers-times-archive regression: each per-group pass must defer the
// archive-sized epilogue (global subagent linking and skip-cache persistence),
// and the grouped call must run that epilogue exactly once after every group,
// so per-poll database work does not multiply with provider-group count.
func TestReconcileProviderRootsGroupedRunsSharedEpilogueOnce(t *testing.T) {
	database := openTestDB(t)
	rootA := filepath.Join(t.TempDir(), "claude-a")
	rootB := filepath.Join(t.TempDir(), "claude-b")
	writeGroupedClaudeFixture(t, rootA, "grouped-a")
	writeGroupedClaudeFixture(t, rootB, "grouped-b")
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {rootA, rootB},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	seedGroupedSubagentFixture(t, database)
	// Seed a skip entry directly: persistSkipCache snapshots this map, so the
	// skipped_files table observably distinguishes "persisted" from "deferred".
	engine.cacheSkip(filepath.Join(rootA, "seeded-skip.jsonl"), 42)

	// A single deferred pass must sync its scope but leave the epilogue to
	// the grouped caller: no skip-cache persistence, no subagent linking.
	deferredCtx := context.WithValue(
		t.Context(), deferPassEpilogueContextKey{}, true,
	)
	require.NoError(t, engine.ReconcileProviderRoots(
		deferredCtx, parser.AgentClaude, []string{rootA},
	))
	synced, err := database.GetSession(t.Context(), "grouped-a")
	require.NoError(t, err)
	require.NotNil(t, synced, "a deferred pass must still sync its scope")
	skipped, err := database.LoadSkippedFiles()
	require.NoError(t, err)
	assert.Empty(t, skipped,
		"a deferred pass must not persist the archive-sized skip cache")
	requireGroupedChildParent(t, database, false,
		"a deferred pass must not run global subagent linking")

	// The grouped call reconciles every group and then runs the shared
	// epilogue once: both scopes synced, skip cache persisted, links set.
	require.NoError(t, engine.ReconcileProviderRootsGrouped(t.Context(),
		[]ProviderRootsGroup{
			{Agent: parser.AgentClaude, Roots: []string{rootA}},
			{Agent: parser.AgentClaude, Roots: []string{rootB}},
		},
	))
	syncedB, err := database.GetSession(t.Context(), "grouped-b")
	require.NoError(t, err)
	require.NotNil(t, syncedB, "the grouped call must sync every group's scope")
	skipped, err = database.LoadSkippedFiles()
	require.NoError(t, err)
	assert.NotEmpty(t, skipped,
		"the grouped call must persist the skip cache after the last group")
	requireGroupedChildParent(t, database, true,
		"the grouped call must run subagent linking in the shared epilogue")
}

// TestReconcileProviderRootsGroupedSkipsEpilogueOnCancellation: linking and
// skip-cache persistence are not context-aware, so a batch canceled after an
// eligible group must skip the archive-sized epilogue instead of blocking
// shutdown on it, and must report the cancellation rather than succeed.
func TestReconcileProviderRootsGroupedSkipsEpilogueOnCancellation(t *testing.T) {
	database := openTestDB(t)
	rootA := filepath.Join(t.TempDir(), "claude-a")
	rootB := filepath.Join(t.TempDir(), "claude-b")
	writeGroupedClaudeFixture(t, rootA, "grouped-a")
	writeGroupedClaudeFixture(t, rootB, "grouped-b")
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {rootA, rootB},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	seedGroupedSubagentFixture(t, database)
	engine.cacheSkip(filepath.Join(rootA, "seeded-skip.jsonl"), 42)

	// Cancel while the second group's pass starts, after the first group has
	// completed cleanly and made the epilogue eligible.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	defaultFactory := engine.reconciliationSpoolFactory
	spoolCalls := 0
	engine.reconciliationSpoolFactory = func(path string) (reconciliationSpoolStore, error) {
		spoolCalls++
		if spoolCalls == 2 {
			cancel()
		}
		return defaultFactory(path)
	}

	err := engine.ReconcileProviderRootsGrouped(ctx,
		[]ProviderRootsGroup{
			{Agent: parser.AgentClaude, Roots: []string{rootA}},
			{Agent: parser.AgentClaude, Roots: []string{rootB}},
		},
	)
	require.Error(t, err, "a canceled batch must not report success")
	assert.ErrorIs(t, err, context.Canceled)

	synced, err := database.GetSession(context.Background(), "grouped-a")
	require.NoError(t, err)
	require.NotNil(t, synced, "the group completed before cancellation must be synced")
	skipped, err := database.LoadSkippedFiles()
	require.NoError(t, err)
	assert.Empty(t, skipped,
		"a canceled batch must not persist the archive-sized skip cache")
	requireGroupedChildParent(t, database, false,
		"a canceled batch must not run global subagent linking")
}

// TestGroupedReconcileContainerProbesDoNotScaleWithProviderGroups is the
// cardinality regression for the pre-discovery container capture: an
// agent-scoped pass outside the OpenCode SQLite family can never discover a
// shared container, so it must not probe any configured container. Otherwise
// per-poll probe work would multiply as provider groups grow, even with the
// shared epilogue.
func TestGroupedReconcileContainerProbesDoNotScaleWithProviderGroups(t *testing.T) {
	countContainerProbes := func(t *testing.T) *atomic.Int32 {
		t.Helper()
		var probes atomic.Int32
		orig := statSQLiteContainerState
		statSQLiteContainerState = func(dbPath string) (parser.SQLiteContainerState, bool) {
			probes.Add(1)
			return orig(dbPath)
		}
		t.Cleanup(func() { statSQLiteContainerState = orig })
		return &probes
	}
	newContainerEngine := func(t *testing.T, claudeRoots []string) *Engine {
		t.Helper()
		openCodeDir := t.TempDir()
		require.NoError(t, os.WriteFile(
			filepath.Join(openCodeDir, "opencode.db"), []byte("not a real db"), 0o644,
		))
		engine := NewEngine(openTestDB(t), EngineConfig{
			AgentDirs: map[parser.AgentType][]string{
				parser.AgentClaude:   claudeRoots,
				parser.AgentOpenCode: {openCodeDir},
			},
			Machine: "local",
		})
		t.Cleanup(engine.Close)
		return engine
	}

	for _, groupCount := range []int{2, 8} {
		t.Run(fmt.Sprintf("groups=%d", groupCount), func(t *testing.T) {
			var claudeRoots []string
			var groups []ProviderRootsGroup
			for i := range groupCount {
				root := filepath.Join(t.TempDir(), fmt.Sprintf("claude-%d", i))
				writeGroupedClaudeFixture(t, root, fmt.Sprintf("session-%d", i))
				claudeRoots = append(claudeRoots, root)
				groups = append(groups, ProviderRootsGroup{
					Agent: parser.AgentClaude, Roots: []string{root},
				})
			}
			engine := newContainerEngine(t, claudeRoots)
			probes := countContainerProbes(t)

			require.NoError(t, engine.ReconcileProviderRootsGrouped(t.Context(), groups))

			assert.Zero(t, probes.Load(),
				"out-of-family provider groups must not probe any container, "+
					"regardless of group count")
		})
	}

	t.Run("unscoped group still captures containers", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "claude-0")
		writeGroupedClaudeFixture(t, root, "session-0")
		engine := newContainerEngine(t, []string{root})
		probes := countContainerProbes(t)

		require.NoError(t, engine.ReconcileProviderRootsGrouped(t.Context(),
			[]ProviderRootsGroup{{Agent: "", Roots: []string{root}}},
		))

		assert.Positive(t, probes.Load(),
			"the unscoped pass must still capture configured containers")
	})
}

// TestInFamilyContainerCaptureBoundedByBatchRoots: an in-family scoped pass
// must probe only the configured containers overlapping its reconciliation
// roots. Probe work per pass must stay constant as unrelated configured dirs
// for the same agent grow.
func TestInFamilyContainerCaptureBoundedByBatchRoots(t *testing.T) {
	probeCounts := make(map[int]int32)
	for _, dirCount := range []int{2, 8} {
		t.Run(fmt.Sprintf("dirs=%d", dirCount), func(t *testing.T) {
			dirs := make([]string, 0, dirCount)
			for i := range dirCount {
				dir := filepath.Join(t.TempDir(), fmt.Sprintf("opencode-%d", i))
				require.NoError(t, os.MkdirAll(dir, 0o755))
				require.NoError(t, os.WriteFile(
					filepath.Join(dir, "opencode.db"), []byte("not a real db"), 0o644,
				))
				dirs = append(dirs, dir)
			}
			engine := NewEngine(openTestDB(t), EngineConfig{
				AgentDirs: map[parser.AgentType][]string{
					parser.AgentOpenCode: dirs,
				},
				Machine: "local",
			})
			t.Cleanup(engine.Close)

			var probes atomic.Int32
			orig := statSQLiteContainerState
			statSQLiteContainerState = func(dbPath string) (parser.SQLiteContainerState, bool) {
				probes.Add(1)
				return orig(dbPath)
			}
			t.Cleanup(func() { statSQLiteContainerState = orig })

			// The pass may report provider failures for the unreadable
			// container; only the capture cardinality is under test.
			_ = engine.ReconcileProviderRootsGrouped(t.Context(),
				[]ProviderRootsGroup{
					{Agent: parser.AgentOpenCode, Roots: []string{dirs[0]}},
				},
			)

			require.Positive(t, probes.Load(),
				"the batch root's own container must still be probed")
			probeCounts[dirCount] = probes.Load()
		})
	}
	assert.Equal(t, probeCounts[2], probeCounts[8],
		"container probes per pass must be bounded by the batch roots, "+
			"not by the agent's total configured dirs")
}

// TestReconcileProviderRootsGroupedAttemptsEveryGroupAfterFailure pins the
// attempt-all contract: a failing group must not prevent later groups from
// reconciling, the shared epilogue still runs for the groups that committed,
// and the failure is reported with the provider it belongs to.
func TestReconcileProviderRootsGroupedAttemptsEveryGroupAfterFailure(t *testing.T) {
	database := openTestDB(t)
	rootA := filepath.Join(t.TempDir(), "claude-a")
	rootB := filepath.Join(t.TempDir(), "claude-b")
	writeGroupedClaudeFixture(t, rootA, "grouped-a")
	writeGroupedClaudeFixture(t, rootB, "grouped-b")
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {rootA, rootB},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	engine.cacheSkip(filepath.Join(rootA, "seeded-skip.jsonl"), 42)

	// Fail the first group's pass at the spool, then restore the factory so
	// the second group runs normally.
	defaultFactory := engine.reconciliationSpoolFactory
	failures := 0
	engine.reconciliationSpoolFactory = func(path string) (reconciliationSpoolStore, error) {
		if failures == 0 {
			failures++
			return nil, errors.New("spool unavailable")
		}
		return defaultFactory(path)
	}

	err := engine.ReconcileProviderRootsGrouped(t.Context(),
		[]ProviderRootsGroup{
			{Agent: parser.AgentClaude, Roots: []string{rootA}},
			{Agent: parser.AgentClaude, Roots: []string{rootB}},
		},
	)
	require.Error(t, err, "the failing group's error must be reported")
	assert.ErrorContains(t, err, "spool unavailable")
	assert.ErrorContains(t, err, "claude",
		"the joined error must name the failing group's provider")

	syncedB, err := database.GetSession(t.Context(), "grouped-b")
	require.NoError(t, err)
	require.NotNil(t, syncedB,
		"a later group must still reconcile after an earlier group fails")
	skipped, err := database.LoadSkippedFiles()
	require.NoError(t, err)
	assert.NotEmpty(t, skipped,
		"the shared epilogue must still run for the groups that completed")
}
