package activity

import (
	"testing"

	"go.kenn.io/agentsview/internal/export"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSanitizeProjectLabelsSanitizesBranchProjects(t *testing.T) {
	raw := "/home/example/private/repo"
	report := Report{ByBranch: []BranchKeyMinutes{{
		Project: raw,
		Branch:  "main",
		Cost:    1,
	}}}
	projects := map[string]export.ProjectMapEntry{
		raw: {ProjectKey: "pl1-path"},
	}

	SanitizeProjectLabels(&report, projects)

	require.Len(t, report.ByBranch, 1)
	assert.Empty(t, report.ByBranch[0].Project)
	assert.Equal(t, "main", report.ByBranch[0].Branch)
}

func TestSanitizeProjectLabelsSanitizesSessionTitles(t *testing.T) {
	report := Report{BySession: []SessionRow{
		{SessionID: "path", Title: "/Users/alice/private/repo", Project: "/Users/alice/private/repo"},
		{SessionID: "url", Title: "file:/Users/alice/private/repo", Project: "safe"},
		{SessionID: "safe", Title: "Review project identity", Project: "safe"},
		{SessionID: "colon", Title: "Fix: project identity", Project: "safe"},
	}}
	projects := map[string]export.ProjectMapEntry{
		"/Users/alice/private/repo": {ProjectKey: "pl1-path"},
		"safe":                      {ProjectKey: "pl1-safe"},
	}

	SanitizeProjectLabels(&report, projects)

	assert.Equal(t, "path", report.BySession[0].Title)
	assert.Empty(t, report.BySession[0].Project)
	assert.Equal(t, "url", report.BySession[1].Title)
	assert.Equal(t, "safe", report.BySession[1].Project)
	assert.Equal(t, "Review project identity", report.BySession[2].Title)
	assert.Equal(t, "Fix: project identity", report.BySession[3].Title)
}

func TestSessionsTable_TimedAndUntimed(t *testing.T) {
	p := baseParams(t, "2026-06-16", "UTC")
	sessions := []SessionMeta{
		{SessionID: "a", Title: "Fix bug", Project: "proj1", Agent: "claude",
			StartedAt: "2026-06-16T10:00:00Z"},
		{SessionID: "u", Title: "Imported", Project: "proj2", Agent: "codex",
			StartedAt: "2026-06-16T09:00:00Z"}, // no activity, no usage
	}
	act := []ActivityEvent{
		{SessionID: "a", Ordinal: 1, Timestamp: "2026-06-16T10:00:00Z", Role: "user"},
		{SessionID: "a", Ordinal: 2, Timestamp: "2026-06-16T10:04:00Z", Role: "assistant", Model: "opus"},
	}
	usage := []UsageRow{
		{SessionID: "a", Model: "opus", Timestamp: "2026-06-16T10:03:00Z",
			OutputTokens: 50, Cost: 0.5, UsageDedupKey: "k1"},
	}
	r := Aggregate(p, sessions, act, usage)

	require.Len(t, r.BySession, 2)
	bySid := map[string]SessionRow{}
	for _, s := range r.BySession {
		bySid[s.SessionID] = s
	}
	a := bySid["a"]
	assert.Equal(t, "timed", a.TimingQuality)
	require.NotNil(t, a.AgentMinutes)
	assert.InDelta(t, 4.0, *a.AgentMinutes, 1e-9)
	assert.Equal(t, "opus", a.PrimaryModel)
	assert.InDelta(t, 0.5, a.Cost, 1e-9)

	u := bySid["u"]
	assert.Equal(t, "untimed", u.TimingQuality)
	assert.Nil(t, u.AgentMinutes)

	assert.Equal(t, 2, r.Totals.Sessions)
	assert.Equal(t, 1, r.Totals.UntimedSessions)
	assert.Equal(t, 2, r.Totals.DistinctProjects)
	require.Len(t, r.ByProject, 1) // only timed activity contributes minutes
	assert.Equal(t, "proj1", r.ByProject[0].Key)
}

func TestSessionsTable_UntimedKeepsCost(t *testing.T) {
	// A session with usage but no activity is untimed yet still carries cost
	// and output tokens, and contributes to distinct-model counts.
	p := baseParams(t, "2026-06-16", "UTC")
	sessions := []SessionMeta{
		{SessionID: "u", Title: "Imported", Project: "proj1", Agent: "codex"},
	}
	usage := []UsageRow{
		{SessionID: "u", Model: "sonnet", Timestamp: "2026-06-16T11:00:00Z",
			OutputTokens: 30, Cost: 0.25, UsageDedupKey: "k1"},
	}
	r := Aggregate(p, sessions, nil, usage)

	require.Len(t, r.BySession, 1)
	u := r.BySession[0]
	assert.Equal(t, "untimed", u.TimingQuality)
	assert.Nil(t, u.AgentMinutes)
	assert.InDelta(t, 0.25, u.Cost, 1e-9)
	assert.Equal(t, 30, u.OutputTokens)
	assert.Equal(t, "sonnet", u.PrimaryModel)
	assert.Equal(t, []string{"sonnet"}, u.Models)
	assert.Equal(t, 1, r.Totals.DistinctModels)
	// No timed minutes, but the cost rolls up into the cost breakdown (so it
	// sums to Totals.Cost). Each row carries zero minutes and the full cost in
	// the interactive segment (the session is not automated).
	require.Len(t, r.ByModel, 1)
	assert.Equal(t, "sonnet", r.ByModel[0].Key)
	assert.InDelta(t, 0.0, r.ByModel[0].AgentMinutes, 1e-9)
	assert.InDelta(t, 0.25, r.ByModel[0].Cost, 1e-9)
	assert.InDelta(t, 0.25, r.ByModel[0].InteractiveCost, 1e-9)
	require.Len(t, r.ByProject, 1)
	assert.Equal(t, "proj1", r.ByProject[0].Key)
	assert.InDelta(t, 0.25, r.ByProject[0].Cost, 1e-9)
	require.Len(t, r.ByAgent, 1)
	assert.Equal(t, "codex", r.ByAgent[0].Key)
	assert.InDelta(t, 0.25, r.ByAgent[0].Cost, 1e-9)
}

func TestSessionsTable_MixedModelsAndUnknownDropped(t *testing.T) {
	// A session spanning two models plus an unknown-model gap. The interval
	// model breakdown keeps the named models, drops "unknown", and the session
	// surfaces both named models (UI renders "mixed" when len(Models) > 1).
	p := baseParams(t, "2026-06-16", "UTC")
	sessions := []SessionMeta{
		{SessionID: "a", Title: "Multi", Project: "proj1", Agent: "claude"},
	}
	act := []ActivityEvent{
		{SessionID: "a", Ordinal: 1, Timestamp: "2026-06-16T10:00:00Z", Role: "user"},
		// gap 1->2 has no assistant model yet -> "unknown", 2 min.
		{SessionID: "a", Ordinal: 2, Timestamp: "2026-06-16T10:02:00Z", Role: "user"},
		// gap 2->3 attributes to opus, 2 min.
		{SessionID: "a", Ordinal: 3, Timestamp: "2026-06-16T10:04:00Z", Role: "assistant", Model: "opus"},
		// gap 3->4 attributes to sonnet, 4 min.
		{SessionID: "a", Ordinal: 4, Timestamp: "2026-06-16T10:08:00Z", Role: "assistant", Model: "sonnet"},
	}
	r := Aggregate(p, sessions, act, nil)

	require.Len(t, r.BySession, 1)
	a := r.BySession[0]
	assert.Equal(t, "timed", a.TimingQuality)
	assert.Equal(t, []string{"opus", "sonnet"}, a.Models)
	assert.Equal(t, "sonnet", a.PrimaryModel) // sonnet has the most minutes

	// ByModel drops "unknown" and keeps the two named models sorted by minutes.
	require.Len(t, r.ByModel, 2)
	assert.Equal(t, "sonnet", r.ByModel[0].Key)
	assert.InDelta(t, 4.0, r.ByModel[0].AgentMinutes, 1e-9)
	assert.Equal(t, "opus", r.ByModel[1].Key)
	assert.InDelta(t, 2.0, r.ByModel[1].AgentMinutes, 1e-9)
	assert.Equal(t, 2, r.Totals.DistinctModels)
}

func TestSessionsTable_SortByMinutesUntimedLast(t *testing.T) {
	// Two timed sessions (different minutes) and one untimed; rows sort by
	// agent-minutes descending with the untimed (nil) session last.
	p := baseParams(t, "2026-06-16", "UTC")
	sessions := []SessionMeta{
		{SessionID: "small", Project: "p", Agent: "claude"},
		{SessionID: "big", Project: "p", Agent: "claude"},
		{SessionID: "untimed", Project: "p", Agent: "claude"},
	}
	act := []ActivityEvent{
		{SessionID: "small", Ordinal: 1, Timestamp: "2026-06-16T10:00:00Z", Role: "user"},
		{SessionID: "small", Ordinal: 2, Timestamp: "2026-06-16T10:01:00Z", Role: "assistant", Model: "m"},
		{SessionID: "big", Ordinal: 1, Timestamp: "2026-06-16T11:00:00Z", Role: "user"},
		{SessionID: "big", Ordinal: 2, Timestamp: "2026-06-16T11:05:00Z", Role: "assistant", Model: "m"},
	}
	r := Aggregate(p, sessions, act, nil)

	require.Len(t, r.BySession, 3)
	assert.Equal(t, "big", r.BySession[0].SessionID)
	assert.Equal(t, "small", r.BySession[1].SessionID)
	assert.Equal(t, "untimed", r.BySession[2].SessionID)
	assert.Nil(t, r.BySession[2].AgentMinutes)
}
