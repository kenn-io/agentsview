package activity

import (
	"testing"

	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSanitizeProjectLabelsSanitizesSessionTitles(t *testing.T) {
	assert := assert.New(t)

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

	assert.Equal("path", report.BySession[0].Title)
	assert.Empty(report.BySession[0].Project)
	assert.Equal("url", report.BySession[1].Title)
	assert.Equal("safe", report.BySession[1].Project)
	assert.Equal("Review project identity", report.BySession[2].Title)
	assert.Equal("Fix: project identity", report.BySession[3].Title)
}

func TestSessionsTable_TimedAndUntimed(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
			OutputTokens: 50, Cost: money.MustParseDollars("0.5"), UsageDedupKey: "k1"},
	}
	r := mustAggregate(t, p, sessions, act, usage)

	require.Len(r.BySession, 2)
	bySid := map[string]SessionRow{}
	for _, s := range r.BySession {
		bySid[s.SessionID] = s
	}
	a := bySid["a"]
	assert.Equal("timed", a.TimingQuality)
	require.NotNil(a.AgentMinutes)
	assert.InDelta(4.0, *a.AgentMinutes, 1e-9)
	assert.Equal("opus", a.PrimaryModel)
	assert.Equal(money.MustParseDollars("0.5"), a.Cost)

	u := bySid["u"]
	assert.Equal("untimed", u.TimingQuality)
	assert.Nil(u.AgentMinutes)

	assert.Equal(2, r.Totals.Sessions)
	assert.Equal(1, r.Totals.UntimedSessions)
	assert.Equal(2, r.Totals.DistinctProjects)
	require.Len(r.ByProject, 1) // only timed activity contributes minutes
	assert.Equal("proj1", r.ByProject[0].Key)
}

func TestSessionsTable_UntimedKeepsCost(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	// A session with usage but no activity is untimed yet still carries cost
	// and output tokens, and contributes to distinct-model counts.
	p := baseParams(t, "2026-06-16", "UTC")
	sessions := []SessionMeta{
		{SessionID: "u", Title: "Imported", Project: "proj1", Agent: "codex"},
	}
	usage := []UsageRow{
		{SessionID: "u", Model: "sonnet", Timestamp: "2026-06-16T11:00:00Z",
			OutputTokens: 30, Cost: money.MustParseDollars("0.25"), UsageDedupKey: "k1"},
	}
	r := mustAggregate(t, p, sessions, nil, usage)

	require.Len(r.BySession, 1)
	u := r.BySession[0]
	assert.Equal("untimed", u.TimingQuality)
	assert.Nil(u.AgentMinutes)
	assert.Equal(money.MustParseDollars("0.25"), u.Cost)
	assert.Equal(30, u.OutputTokens)
	assert.Equal("sonnet", u.PrimaryModel)
	assert.Equal([]string{"sonnet"}, u.Models)
	assert.Equal(1, r.Totals.DistinctModels)
	// No timed minutes, but the cost rolls up into the cost breakdown (so it
	// sums to Totals.Cost). Each row carries zero minutes and the full cost in
	// the interactive segment (the session is not automated).
	require.Len(r.ByModel, 1)
	assert.Equal("sonnet", r.ByModel[0].Key)
	assert.InDelta(0.0, r.ByModel[0].AgentMinutes, 1e-9)
	assert.Equal(money.MustParseDollars("0.25"), r.ByModel[0].Cost)
	assert.Equal(money.MustParseDollars("0.25"), r.ByModel[0].InteractiveCost)
	require.Len(r.ByProject, 1)
	assert.Equal("proj1", r.ByProject[0].Key)
	assert.Equal(money.MustParseDollars("0.25"), r.ByProject[0].Cost)
	require.Len(r.ByAgent, 1)
	assert.Equal("codex", r.ByAgent[0].Key)
	assert.Equal(money.MustParseDollars("0.25"), r.ByAgent[0].Cost)
}

func TestSessionsTable_MixedModelsAndUnknownDropped(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	r := mustAggregate(t, p, sessions, act, nil)

	require.Len(r.BySession, 1)
	a := r.BySession[0]
	assert.Equal("timed", a.TimingQuality)
	assert.Equal([]string{"opus", "sonnet"}, a.Models)
	assert.Equal("sonnet", a.PrimaryModel) // sonnet has the most minutes

	// ByModel drops "unknown" and keeps the two named models sorted by minutes.
	require.Len(r.ByModel, 2)
	assert.Equal("sonnet", r.ByModel[0].Key)
	assert.InDelta(4.0, r.ByModel[0].AgentMinutes, 1e-9)
	assert.Equal("opus", r.ByModel[1].Key)
	assert.InDelta(2.0, r.ByModel[1].AgentMinutes, 1e-9)
	assert.Equal(2, r.Totals.DistinctModels)
}

func TestSessionsTable_SortByMinutesUntimedLast(t *testing.T) {
	assert := assert.New(t)

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
	r := mustAggregate(t, p, sessions, act, nil)

	require.Len(t, r.BySession, 3)
	assert.Equal("big", r.BySession[0].SessionID)
	assert.Equal("small", r.BySession[1].SessionID)
	assert.Equal("untimed", r.BySession[2].SessionID)
	assert.Nil(r.BySession[2].AgentMinutes)
}
