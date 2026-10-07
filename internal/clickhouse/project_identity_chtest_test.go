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

func TestHermesCronProjectLabelsRefreshAfterTitlePush(t *testing.T) {
	store, syncer, local := newPushedStore(t)
	ctx := t.Context()
	for _, id := range []string{"job-a", "job-b"} {
		session := fixtureSession("hermes:"+id, "hermes-cron/"+id, "cron run", "2026-10-07T12:00:00Z", 1)
		session.Agent, session.SessionName = "hermes", new("Daily digest · Oct 07 12:00")
		require.NoError(t, local.UpsertSession(ctx, session))
		require.NoError(t, local.ReplaceSessionUsageEvents(ctx, session.ID, []db.UsageEvent{{SessionID: session.ID, Source: "session", Model: "gpt-5.4", InputTokens: 10, OccurredAt: "2026-10-07T12:00:00Z", DedupKey: session.ID}}))
	}
	_, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	labels := []string{"hermes-cron/job-a", "hermes-cron/job-b"}
	identity, err := store.BuildProjectIdentityMap(ctx, labels)
	require.NoError(t, err)
	keyA, keyB := identity[labels[0]].ProjectKey, identity[labels[1]].ProjectKey
	filter := db.UsageFilter{From: "2026-10-07", To: "2026-10-07", Agent: "hermes", Breakdowns: true}
	before, err := store.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	assert.Equal(t, "Daily digest · hermes-cron/job-a", before.Projects[keyA].DisplayLabel)
	assert.Equal(t, "Daily digest · hermes-cron/job-b", before.Projects[keyB].DisplayLabel)
	require.NoError(t, local.RefreshSessionName(ctx, "hermes:job-a", new("Research digest · Oct 07 12:00")))
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	after, err := store.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	assert.Equal(t, "Research digest · hermes-cron/job-a", after.Projects[keyA].DisplayLabel)
	assert.Equal(t, before.Projects[keyA].ProjectKey, after.Projects[keyA].ProjectKey)
	assert.Equal(t, before.Projects[keyB], after.Projects[keyB])
}
