//go:build chtest

package clickhouse

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
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
	assert.Contains(t, rules.Machines, fixtureMachine)

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
