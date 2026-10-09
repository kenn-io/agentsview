//go:build chtest

package clickhouse

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/readbase"
)

func TestProjectInventoryRulesAndCandidates(t *testing.T) {
	store, syncer, _ := newPushedStore(t)
	ctx := context.Background()
	_, err := syncer.syncProjectIdentityObservations(ctx, 0, true, nil)
	require.NoError(t, err)
	_, err = syncer.syncWorktreeMappings(ctx, 0, true)
	require.NoError(t, err)

	inventory, err := store.GetProjectInventory(ctx, db.ProjectDateFilter{})
	require.NoError(t, err)
	assert.Equal(t, 2, inventory.TotalProjects)
	assert.Equal(t, 3, inventory.TotalSessions, "inventory includes the alpha subagent child")
	assert.Equal(t, 0, inventory.GovernedSessions, "no worktree mapping rules seeded")

	rules, err := store.ListProjectRules(ctx, fixtureMachine)
	require.NoError(t, err)
	assert.Equal(t, fixtureMachine, rules.Machine)
	assert.Empty(t, rules.Rules, "no worktree mapping rules seeded")

	projects, err := store.BuildProjectIdentityMap(ctx, []string{"alpha"})
	require.NoError(t, err)
	require.Contains(t, projects, "alpha")
	candidates, err := store.ListArchiveWorktreeCandidates(ctx, db.ArchiveWorktreeCandidateRequest{
		ProjectLabel: export.SafeProjectDisplayLabel("alpha"),
		ProjectKey:   projects["alpha"].ProjectKey,
	})
	require.NoError(t, err)
	require.Len(t, candidates, 1, "the cwd-less alpha sessions form one fallback group")
	assert.Equal(t, fixtureMachine, candidates[0].Machine)
	assert.Equal(t, "unavailable", candidates[0].EvidenceKind, "no cwd or identity evidence seeded")
	assert.False(t, candidates[0].Available)
	assert.Equal(t, 2, candidates[0].ContributingSessions, "alpha root plus subagent child")
}

// The project map depends on the set of labels, so callers that list the
// same labels in another order share one kept map.
func TestProjectIdentityMapKeptPerLabelSet(t *testing.T) {
	store, _, _ := newPushedStore(t)
	ctx := t.Context()
	first, err := store.BuildProjectIdentityMap(ctx, []string{"alpha", "beta"})
	require.NoError(t, err)
	second, err := store.BuildProjectIdentityMap(ctx, []string{"beta", "alpha", "beta"})
	require.NoError(t, err)
	require.Equal(t, first, second)
	require.Len(t, store.projectIdentityMaps.order, 1)
}

func TestProjectRuleMachinesSortCombinedUnion(t *testing.T) {
	store, _, _ := newPushedStore(t)
	for _, machine := range []string{"z-machine", "a-machine", "m-machine"} {
		_, err := store.conn.ExecContext(t.Context(), `INSERT INTO source_worktree_project_mappings (source_archive_id, machine, path_prefix, layout, project, original_project, enabled, updated_at, push_version) VALUES ('archive-order', ?, '/repo', 'explicit', 'alpha', '', true, '', 1)`, machine)
		require.NoError(t, err)
	}
	rules, err := store.ListProjectRules(t.Context(), "")
	require.NoError(t, err)
	assert.Equal(t, []string{"a-machine", "m-machine", "test-machine", "z-machine"}, rules.Machines)
}

func TestProjectWorktreeCandidatesReloadPreservesSelection(t *testing.T) {
	store, syncer, local := newPushedStore(t)
	ctx := t.Context()
	projects, err := store.BuildProjectIdentityMap(ctx, []string{"alpha"})
	require.NoError(t, err)
	request := db.ArchiveWorktreeCandidateRequest{ProjectLabel: "alpha", ProjectKey: projects["alpha"].ProjectKey, ProjectDateFilter: db.ProjectDateFilter{DateFrom: "2026-01-01", DateTo: "2026-01-31", Timezone: "UTC"}}
	before, err := store.ListArchiveWorktreeCandidates(ctx, request)
	require.NoError(t, err)
	require.Len(t, before, 1)
	require.Equal(t, 2, before[0].ContributingSessions)
	catalog := readbase.NewCatalog(dbtest.PublishingCatalog{CatalogBackend: catalogSQL{store}, Publish: func() {
		_, err := local.AssignSessionProject(ctx, fixtureAlphaID, "beta")
		require.NoError(t, err)
		child, err := local.GetSession(ctx, fixtureChildID)
		require.NoError(t, err)
		require.NotNil(t, child)
		child.StartedAt, child.EndedAt = new("2026-02-01T00:00:00Z"), new("2026-02-01T01:00:00Z")
		require.NoError(t, local.UpsertSession(ctx, *child))
		_, err = syncer.Push(ctx, true, nil)
		require.NoError(t, err)
	}}, "clickhouse")
	candidates, err := catalog.ListArchiveWorktreeCandidates(ctx, request)
	require.NoError(t, err)
	assert.Empty(t, candidates)
}
