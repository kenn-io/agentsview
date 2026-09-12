package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

// DataStore is the Task 6 inventory, rules, and candidate-read slice.
type DataStore interface {
	IdentityStore
	GetProjectInventory(context.Context) (db.ProjectInventory, error)
	ListProjectRules(context.Context, string) (db.ProjectRules, error)
	ListArchiveWorktreeCandidates(
		context.Context, db.ArchiveWorktreeCandidateRequest,
	) ([]db.WorktreeReclassificationCandidate, error)
}

// DataBackend registers one embedded BunStore and its fixture setup.
type DataBackend struct {
	Name string
	Open func(*testing.T) (DataStore, IdentityFixture)
}

// RunDataContract verifies inventory, rule, and candidate behavior against one
// shared BunStore rather than a concrete backend shadow.
func RunDataContract(t *testing.T, backend DataBackend) {
	t.Helper()
	t.Run(backend.Name, func(t *testing.T) {
		store, fixture := backend.Open(t)
		ctx := t.Context()

		inventory, err := store.GetProjectInventory(ctx)
		require.NoError(t, err)
		assert.Equal(t, 2, inventory.TotalProjects)
		assert.Equal(t, 3, inventory.TotalSessions)
		assert.Equal(t, 3, inventory.GovernedSessions)
		require.Len(t, inventory.Projects, 2)
		assert.Equal(t,
			[]string{fixture.AlphaProject, fixture.BetaProject},
			[]string{inventory.Projects[0].Label, inventory.Projects[1].Label},
		)
		alphaInventory := inventory.Projects[0]
		assert.Equal(t, 2, alphaInventory.Sessions)
		assert.Equal(t, 1, alphaInventory.Machines)
		assert.Equal(t, 2, alphaInventory.Agents)
		assert.Equal(t, 2, alphaInventory.DistinctCwds)
		assert.Equal(t, 1, alphaInventory.EnabledRulesTargeting)
		assert.True(t, alphaInventory.RecordedAsOriginal)
		require.NotNil(t, alphaInventory.FirstActivity)
		require.NotNil(t, alphaInventory.LastActivity)
		assert.Equal(t, "2026-08-01T12:00:00Z",
			alphaInventory.FirstActivity.UTC().Format(time.RFC3339))
		assert.Equal(t, "2026-08-01T13:30:00Z",
			alphaInventory.LastActivity.UTC().Format(time.RFC3339))
		betaInventory := inventory.Projects[1]
		assert.Equal(t, 1, betaInventory.Sessions)
		assert.Equal(t, 1, betaInventory.Machines)
		assert.Equal(t, 1, betaInventory.Agents)
		assert.Equal(t, 1, betaInventory.DistinctCwds)
		assert.Equal(t, 1, betaInventory.EnabledRulesTargeting)
		assert.False(t, betaInventory.RecordedAsOriginal)

		rules, err := store.ListProjectRules(ctx, " identity-host-a ")
		require.NoError(t, err)
		assert.Equal(t, "identity-host-a", rules.Machine)
		assert.Equal(t, []string{"identity-host-a", "identity-host-b"}, rules.Machines)
		require.Len(t, rules.Rules, 2)
		byPrefix := make(map[string]db.ProjectRule, len(rules.Rules))
		for _, rule := range rules.Rules {
			byPrefix[rule.PathPrefix] = rule
			assert.Equal(t, fixture.ArchiveAID, rule.SourceArchiveID)
		}
		assert.True(t, byPrefix["/workspace/alpha"].Enabled)
		assert.Equal(t, 2, byPrefix["/workspace/alpha"].GovernedSessions)
		assert.False(t, byPrefix["/workspace/disabled"].Enabled)
		assert.Zero(t, byPrefix["/workspace/disabled"].GovernedSessions)

		candidates, err := store.ListArchiveWorktreeCandidates(
			ctx, db.ArchiveWorktreeCandidateRequest{
				ProjectLabel: fixture.AlphaProject,
				ProjectKey:   alphaInventory.ProjectKey,
			},
		)
		require.NoError(t, err)
		require.Len(t, candidates, 1)
		candidate := candidates[0]
		assert.Equal(t, "identity-host-a", candidate.Machine)
		assert.Equal(t, "snapshot", candidate.EvidenceKind)
		assert.Equal(t, "/workspace/alpha", candidate.EvidenceRoot)
		assert.Equal(t, "/workspace/alpha", candidate.SuggestedPrefix)
		assert.Equal(t, 2, candidate.ContributingSessions)
		assert.Equal(t, 2, candidate.DistinctCwds)
		assert.True(t, candidate.Available)
		assert.Equal(t, []db.WorktreeCandidateExample{
			{SessionID: fixture.AlphaSessionAID, Cwd: "/workspace/alpha/cmd"},
			{SessionID: fixture.AlphaSessionBID, Cwd: "/workspace/alpha/frontend"},
		}, candidate.Examples)
	})
}
