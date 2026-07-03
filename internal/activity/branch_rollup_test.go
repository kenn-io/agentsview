package activity

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		{SessionID: "a", Model: "m", Timestamp: "2026-06-16T11:00:00Z", Cost: 1.0, UsageDedupKey: "ka"},
		{SessionID: "b", Model: "m", Timestamp: "2026-06-16T11:00:00Z", Cost: 2.0, UsageDedupKey: "kb"},
		{SessionID: "c", Model: "m", Timestamp: "2026-06-16T11:00:00Z", Cost: 3.0, UsageDedupKey: "kc"},
		{SessionID: "d", Model: "m", Timestamp: "2026-06-16T11:00:00Z", Cost: 4.0, UsageDedupKey: "kd"},
		{SessionID: "e", Model: "m", Timestamp: "2026-06-16T11:00:00Z", Cost: 5.0, UsageDedupKey: "ke"},
	}
	r := Aggregate(p, sessions, nil, usage)

	byBranch := map[branchPair]BranchKeyMinutes{}
	for _, b := range r.ByBranch {
		byBranch[branchPair{Project: b.Project, Branch: b.Branch}] = b
	}
	require.Len(t, r.ByBranch, 5, "one bucket per distinct (project, branch)")
	assert.InDelta(t, 1.0, byBranch[branchPair{"proj1", "main"}].Cost, 1e-9)
	assert.InDelta(t, 2.0, byBranch[branchPair{"proj1", "feature-x"}].Cost, 1e-9)
	assert.InDelta(t, 3.0, byBranch[branchPair{"proj2", "main"}].Cost, 1e-9,
		"proj2/main is distinct from proj1/main")
	assert.InDelta(t, 4.0, byBranch[branchPair{"proj1", ""}].Cost, 1e-9,
		"empty branch stays distinct from a branch named unknown")
	assert.InDelta(t, 5.0, byBranch[branchPair{"proj1", "unknown"}].Cost, 1e-9)
}
