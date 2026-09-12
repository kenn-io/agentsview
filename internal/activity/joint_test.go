package activity

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJointActivityModelSwitchDoesNotDoubleCountSession(t *testing.T) {
	start := mustStart(t, "2026-07-28T12:00:00Z")
	p := Params{RangeStart: start, RangeEnd: start.Add(10 * time.Minute),
		EffectiveEnd: start.Add(10 * time.Minute), Loc: time.UTC,
		GapCapSeconds: 300, Bucket: BucketSpec{Unit: BucketMinute, NominalSeconds: 300}}
	sessions := []SessionMeta{{SessionID: "a", Project: "project-a", Agent: "agent-a"}}
	candidates := []IntervalCandidate{
		{SessionID: "a", Start: start, End: start.Add(2 * time.Minute), ClosingRole: "assistant", ClosingModel: "model-a"},
		// An overlapping observation extends this session; it is not another agent.
		{SessionID: "a", Start: start.Add(time.Minute), End: start.Add(5 * time.Minute), ClosingRole: "assistant", ClosingModel: "model-b"},
		{SessionID: "a", Start: start.Add(2 * time.Minute), End: start.Add(4 * time.Minute), ClosingRole: "assistant", ClosingModel: "duplicate"},
	}
	report, err := AggregateCandidatesWithJointActivity(t.Context(), p, sessions, candidates, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, report.Buckets[0].MaxAgents)
	assert.Equal(t, 5.0, report.Totals.AgentMinutes)
	assert.Equal(t, []JointActivityCell{
		{BucketStart: start, Project: "project-a", Agent: "agent-a", Model: "model-a", Category: "interactive", AgentMinutes: 2, MaxAgents: 1},
		{BucketStart: start, Project: "project-a", Agent: "agent-a", Model: "model-b", Category: "interactive", AgentMinutes: 3, MaxAgents: 1},
	}, report.JointActivity)
}

func TestJointActivityKeepsDimensionsAndClipsAtBuckets(t *testing.T) {
	start := mustStart(t, "2026-07-28T12:00:00Z")
	p := Params{RangeStart: start, RangeEnd: start.Add(10 * time.Minute),
		EffectiveEnd: start.Add(10 * time.Minute), Loc: time.UTC,
		GapCapSeconds: 300, Bucket: BucketSpec{Unit: BucketMinute, NominalSeconds: 300}}
	sessions := []SessionMeta{
		{SessionID: "a", Project: "project-a", Agent: "agent-a"},
		{SessionID: "b", Project: "project-a", Agent: "agent-a"},
		{SessionID: "c", Project: "project-b", Agent: "agent-b", IsAutomated: true},
	}
	candidates := []IntervalCandidate{
		{SessionID: "a", Start: start.Add(4 * time.Minute), End: start.Add(7 * time.Minute), PriorModel: "model-a"},
		{SessionID: "b", Start: start.Add(5 * time.Minute), End: start.Add(6 * time.Minute), ClosingRole: "assistant", ClosingModel: "model-a"},
		{SessionID: "c", Start: start.Add(5 * time.Minute), End: start.Add(6 * time.Minute)},
	}
	report, err := AggregateCandidatesWithJointActivity(t.Context(), p, sessions, candidates, nil)
	require.NoError(t, err)
	assert.Equal(t, []JointActivityCell{
		{BucketStart: start, Project: "project-a", Agent: "agent-a", Model: "model-a", Category: "interactive", AgentMinutes: 1, MaxAgents: 1},
		{BucketStart: start.Add(5 * time.Minute), Project: "project-a", Agent: "agent-a", Model: "model-a", Category: "interactive", AgentMinutes: 3, MaxAgents: 2},
		{BucketStart: start.Add(5 * time.Minute), Project: "project-b", Agent: "agent-b", Model: "unknown", Category: "automated", AgentMinutes: 1, MaxAgents: 1},
	}, report.JointActivity)
	assert.Equal(t, 3, report.Buckets[1].MaxAgents)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = AggregateCandidatesWithJointActivity(ctx, p, sessions, candidates, nil)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestJointActivityCanonicalProjectAliasesSharePeak(t *testing.T) {
	start := mustStart(t, "2026-07-28T12:00:00Z")
	p := Params{RangeStart: start, RangeEnd: start.Add(5 * time.Minute),
		EffectiveEnd: start.Add(5 * time.Minute), Loc: time.UTC,
		GapCapSeconds: 300, Bucket: BucketSpec{Unit: BucketMinute, NominalSeconds: 300}}
	sessions := []SessionMeta{
		{SessionID: "a", Project: "alias-a", ProjectKey: "canonical-project", Agent: "unknown"},
		{SessionID: "b", Project: "alias-b", ProjectKey: "canonical-project"},
	}
	candidates := []IntervalCandidate{
		{SessionID: "a", Start: start, End: start.Add(time.Minute)},
		{SessionID: "b", Start: start.Add(time.Minute), End: start.Add(2 * time.Minute)},
	}
	report, err := AggregateCandidatesWithJointActivity(t.Context(), p, sessions, candidates, nil)
	require.NoError(t, err)
	require.Len(t, report.JointActivity, 1)
	assert.Equal(t, "canonical-project", report.JointActivity[0].ProjectKey)
	assert.Equal(t, 2.0, report.JointActivity[0].AgentMinutes)
	assert.Equal(t, 1, report.JointActivity[0].MaxAgents)
}
