package activity

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/money"
)

func TestSessionsTable_ByBranch(t *testing.T) {
	p := baseParams(t, "2026-06-16", "UTC")
	sessions := []SessionMeta{
		{SessionID: "a", Project: "proj1", GitBranch: "main", Agent: "claude"},
		{SessionID: "b", Project: "proj1", GitBranch: "feature-x", Agent: "claude"},
		// Same branch name as "a" but a different project: the (project, branch)
		// grain keeps them in separate buckets.
		{SessionID: "c", Project: "proj2", GitBranch: "main", Agent: "claude"},
		{SessionID: "d", Project: "proj1", GitBranch: "", Agent: "claude"},
		{SessionID: "e", Project: "proj1", GitBranch: "unknown", Agent: "claude"},
	}
	usage := []UsageRow{
		{SessionID: "a", Model: "m", Timestamp: "2026-06-16T11:00:00Z", Cost: money.MustParseDollars("1"), UsageDedupKey: "ka"},
		{SessionID: "b", Model: "m", Timestamp: "2026-06-16T11:00:00Z", Cost: money.MustParseDollars("2"), UsageDedupKey: "kb"},
		{SessionID: "c", Model: "m", Timestamp: "2026-06-16T11:00:00Z", Cost: money.MustParseDollars("3"), UsageDedupKey: "kc"},
		{SessionID: "d", Model: "m", Timestamp: "2026-06-16T11:00:00Z", Cost: money.MustParseDollars("4"), UsageDedupKey: "kd"},
		{SessionID: "e", Model: "m", Timestamp: "2026-06-16T11:00:00Z", Cost: money.MustParseDollars("5"), UsageDedupKey: "ke"},
	}
	r, err := Aggregate(p, sessions, nil, usage)
	require.NoError(t, err)

	byBranch := map[branchPair]BranchKeyMinutes{}
	for _, b := range r.ByBranch {
		byBranch[branchPair{Project: b.Project, Branch: b.Branch}] = b
	}
	require.Len(t, r.ByBranch, 5, "one bucket per distinct (project, branch)")
	assert.Equal(t, money.MustParseDollars("1"), byBranch[branchPair{"proj1", "main"}].Cost)
	assert.Equal(t, money.MustParseDollars("2"), byBranch[branchPair{"proj1", "feature-x"}].Cost)
	assert.Equal(t, money.MustParseDollars("3"), byBranch[branchPair{"proj2", "main"}].Cost,
		"proj2/main is distinct from proj1/main")
	assert.Equal(t, money.MustParseDollars("4"), byBranch[branchPair{"proj1", ""}].Cost,
		"empty branch stays distinct from a branch named unknown")
	assert.Equal(t, money.MustParseDollars("5"), byBranch[branchPair{"proj1", "unknown"}].Cost)
}
