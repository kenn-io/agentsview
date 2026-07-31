//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
)

// TestPGDataReadsThroughStoreInterface exercises the three Data reads --
// GetProjectInventory, ListProjectRules, and ListArchiveWorktreeCandidates --
// through the db.Store interface rather than the concrete *Store type,
// proving the PG backend satisfies the same server-facing capability surface
// as SQLite and DuckDB (internal/db/store_contract_test.go and
// internal/duckdb/store_contract_test.go run the equivalent case for those
// backends). Task 6 and Task 8's tests already cover per-method parity in
// depth; this test's job is narrower: prove all three methods are reachable
// through db.Store on a store built the way the HTTP server builds it.
func TestPGDataReadsThroughStoreInterface(t *testing.T) {
	const schema = "agentsview_data_reads_test"
	sync, localDB, pg, ctx := newSessionProvenancePushSync(t, schema)

	// seedPGCandidateSessionNoSnapshot (worktree_candidates_pgtest_test.go)
	// deletes the sessions-insert trigger's placeholder identity snapshot, so
	// these sessions have no snapshot or aggregate evidence and fall back to
	// the exact-cwd grouping -- the same fixture shape Task 8's tests use.
	seedPGCandidateSessionNoSnapshot(t, ctx, localDB, "alpha-1", "alpha", "m1",
		"/w/a", "2026-01-01T00:00:00Z")
	seedPGCandidateSessionNoSnapshot(t, ctx, localDB, "alpha-2", "alpha", "m1",
		"/w/a", "2026-01-01T00:00:00Z")
	_, err := localDB.CreateWorktreeProjectMapping(ctx, db.WorktreeProjectMapping{
		Machine: "m1", PathPrefix: "/w/a",
		Layout: db.WorktreeMappingLayoutExplicit, Project: "alpha", Enabled: true,
	})
	require.NoError(t, err, "CreateWorktreeProjectMapping")

	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push")

	var store db.Store = &Store{pg: pg}

	inventory, err := store.GetProjectInventory(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, inventory.TotalProjects)
	assert.Equal(t, 2, inventory.TotalSessions)
	assert.Equal(t, 2, inventory.GovernedSessions,
		"both sessions' cwds match the enabled rule's path prefix")

	rules, err := store.ListProjectRules(ctx, "m1")
	require.NoError(t, err)
	assert.Equal(t, "m1", rules.Machine)
	require.Len(t, rules.Rules, 1)
	assert.Equal(t, "/w/a", rules.Rules[0].PathPrefix)
	assert.Equal(t, 2, rules.Rules[0].GovernedSessions)

	projects, err := store.BuildProjectIdentityMap(ctx, []string{"alpha"})
	require.NoError(t, err)
	candidates, err := store.ListArchiveWorktreeCandidates(ctx, db.ArchiveWorktreeCandidateRequest{
		ProjectLabel: export.SafeProjectDisplayLabel("alpha"),
		ProjectKey:   projects["alpha"].ProjectKey,
	})
	require.NoError(t, err)
	require.Len(t, candidates, 1, "both alpha sessions share the same cwd fallback group")
	assert.Equal(t, "m1", candidates[0].Machine)
	assert.Equal(t, "fallback", candidates[0].EvidenceKind)
	assert.Equal(t, 2, candidates[0].ContributingSessions)
}
