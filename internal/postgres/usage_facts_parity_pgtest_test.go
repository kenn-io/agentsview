//go:build pgtest

package postgres

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
)

type usageParitySnapshot struct {
	Daily                usageParityDaily
	Top                  []usageParityTopSession
	Counts               db.UsageSessionCounts
	MatchingSessionCount int
	Session              usageParitySession
}

type usageParityDaily struct {
	Dates               []string
	InputTokens         int
	OutputTokens        int
	CacheCreationTokens int
	CacheReadTokens     int
	CostMicrodollars    int64
	Models              []string
	SessionCounts       db.UsageSessionCounts
}

type usageParityTopSession struct {
	SessionID        string
	InputTokens      int
	OutputTokens     int
	TotalTokens      int
	CostMicrodollars int64
}

type usageParitySession struct {
	SessionID         string
	TotalOutputTokens int
	PeakContextTokens int
	HasTokenData      bool
	CostMicrodollars  int64
	HasCost           bool
	Models            []string
	UnpricedModels    []string
	BreakdownCount    int
	Breakdown         []usageParityBreakdown
}

type usageParityBreakdown struct {
	Source           string
	Timestamp        string
	Model            string
	InputTokens      int
	OutputTokens     int
	CostMicrodollars int64
	HasCost          bool
}

func TestSQLiteFactsAndPostgresLiveUsageParity(t *testing.T) {
	const schema = "agentsview_usage_facts_parity_test"
	pgURL := testPGURL(t)
	cleanNamedPGSchema(t, pgURL, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })

	local := testDB(t)
	seedUsageParityFixture(t, local)

	syncer, err := New(
		pgURL, schema, local, "parity-machine", true, SyncOptions{},
	)
	require.NoError(t, err, "create PostgreSQL sync")
	t.Cleanup(func() { require.NoError(t, syncer.Close()) })
	_, err = syncer.Push(t.Context(), false, nil)
	require.NoError(t, err, "push parity fixture")

	remote, err := NewStore(pgURL, schema, true)
	require.NoError(t, err, "open PostgreSQL store")
	t.Cleanup(func() { require.NoError(t, remote.Close()) })

	want := usageParitySnapshot{
		Daily: usageParityDaily{
			Dates:       []string{"2026-08-12"},
			InputTokens: 57, OutputTokens: 18,
			CacheCreationTokens: 2, CacheReadTokens: 3,
			CostMicrodollars: 260_040,
			Models:           []string{"model-priced", "model-reported", "model-unpriced"},
			SessionCounts: db.UsageSessionCounts{
				Total:   3,
				ByAgent: map[string]int{"claude": 2, "hermes": 1},
			},
		},
		Top: []usageParityTopSession{
			{SessionID: "reported", InputTokens: 30, OutputTokens: 5, TotalTokens: 40, CostMicrodollars: 250_000},
			{SessionID: "snapshot-loser", InputTokens: 20, OutputTokens: 10, TotalTokens: 30, CostMicrodollars: 10_040},
			{SessionID: "blank-timestamp", InputTokens: 7, OutputTokens: 3, TotalTokens: 10},
		},
		Counts: db.UsageSessionCounts{
			Total:     3,
			ByProject: map[string]int{"project-a": 1, "project-c": 1, "project-d": 1},
			ByAgent:   map[string]int{"claude": 2, "hermes": 1},
		},
		MatchingSessionCount: 5,
		Session: usageParitySession{
			SessionID: "snapshot-winner", TotalOutputTokens: 10,
			PeakContextTokens: 20, HasTokenData: true,
			CostMicrodollars: 10_040, HasCost: true,
			Models: []string{"model-priced"}, BreakdownCount: 1,
			Breakdown: []usageParityBreakdown{{
				Source: "message", Timestamp: "2026-08-12T10:01:00Z",
				Model: "model-priced", InputTokens: 20, OutputTokens: 10,
				CostMicrodollars: 10_040, HasCost: true,
			}},
		},
	}

	filter := db.UsageFilter{
		From: "2026-08-12", To: "2026-08-12", Timezone: "UTC",
	}
	localGot := captureUsageParitySnapshot(t, local, filter)
	remoteGot := captureUsageParitySnapshot(t, remote, filter)
	require.Equal(t, want, localGot, "SQLite facts result")
	require.Equal(t, want, remoteGot, "PostgreSQL live result")
	require.Equal(t, localGot, remoteGot, "cross-backend result")
	requireCompleteUsageParity(t, local, remote, filter)
}

func requireCompleteUsageParity(
	t *testing.T, local, remote db.Store, filter db.UsageFilter,
) {
	t.Helper()
	ctx := context.Background()
	localDaily, err := local.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	remoteDaily, err := remote.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	require.Equal(t, localDaily, remoteDaily, "complete daily result")
	localTop, err := local.GetTopSessionsByCost(ctx, filter, 10)
	require.NoError(t, err)
	remoteTop, err := remote.GetTopSessionsByCost(ctx, filter, 10)
	require.NoError(t, err)
	require.Equal(t, localTop, remoteTop, "complete top-session result")
	localSession, err := local.GetSessionUsage(ctx, "snapshot-winner", true)
	require.NoError(t, err)
	remoteSession, err := remote.GetSessionUsage(ctx, "snapshot-winner", true)
	require.NoError(t, err)
	require.Equal(t, localSession, remoteSession, "complete session result")
}

func seedUsageParityFixture(t *testing.T, local *db.DB) {
	t.Helper()
	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{
		{
			ModelPattern:  "model-priced",
			InputPerMTok:  money.Money{Microdollars: 1_000_000},
			OutputPerMTok: money.Money{Microdollars: 2_000_000},
		},
		{
			ModelPattern:  "model-reported",
			InputPerMTok:  money.Money{Microdollars: 1_000_000},
			OutputPerMTok: money.Money{Microdollars: 1_000_000},
		},
	}), "seed pricing")

	sessions := []db.Session{
		usageParitySessionFixture("snapshot-loser", "project-a", "claude", "2026-08-12T10:00:00Z", 5, 10),
		usageParitySessionFixture("snapshot-winner", "project-b", "claude", "2026-08-12T10:01:00Z", 10, 20),
		usageParitySessionFixture("reported", "project-c", "hermes", "2026-08-12T11:00:00Z", 5, 30),
		usageParitySessionFixture("blank-timestamp", "project-d", "claude", "2026-08-12T12:00:00Z", 3, 7),
		usageParitySessionFixture("activity-only", "project-e", "claude", "2026-08-12T13:00:00Z", 0, 0),
	}
	for i := range sessions {
		require.NoError(t, local.UpsertSession(sessions[i]),
			"seed session %s", sessions[i].ID)
	}

	require.NoError(t, local.InsertMessages([]db.Message{
		{
			SessionID: "snapshot-loser", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-08-12T10:00:00Z", Model: "model-priced",
			TokenUsage:      json.RawMessage(`{"input_tokens":10,"output_tokens":5}`),
			ClaudeMessageID: "shared-message", ClaudeRequestID: "shared-request",
		},
		{
			SessionID: "snapshot-winner", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-08-12T10:01:00Z", Model: "model-priced",
			TokenUsage: json.RawMessage(
				`{"input_tokens":20,"output_tokens":10,"server_tool_use":{"web_search_requests":1}}`),
			ClaudeMessageID: "shared-message", ClaudeRequestID: "shared-request",
		},
		{
			SessionID: "blank-timestamp", Ordinal: 0, Role: "assistant",
			Model:      "model-unpriced",
			TokenUsage: json.RawMessage(`{"input_tokens":7,"output_tokens":3}`),
		},
		{
			SessionID: "activity-only", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-08-12T13:01:00Z", Model: "model-activity",
		},
	}), "seed messages")

	reportedCost := money.Money{Microdollars: 250_000}
	require.NoError(t, local.ReplaceSessionUsageEvents("reported", []db.UsageEvent{{
		SessionID: "reported", Source: "provider", Model: "model-reported",
		InputTokens: 30, OutputTokens: 5,
		CacheCreationInputTokens: 2, CacheReadInputTokens: 3, Cost: &reportedCost,
		OccurredAt: "2026-08-12T11:05:00Z", DedupKey: "reported-usage",
	}}), "seed reported usage")
}

func usageParitySessionFixture(
	id, project, agent, startedAt string, outputTokens, contextTokens int,
) db.Session {
	return db.Session{
		ID: id, Project: project, Machine: "parity-machine", Agent: agent,
		StartedAt: &startedAt, MessageCount: 1, UserMessageCount: 1,
		TotalOutputTokens: outputTokens, PeakContextTokens: contextTokens,
		HasTotalOutputTokens: true, HasPeakContextTokens: true,
	}
}

func captureUsageParitySnapshot(
	t *testing.T, store db.Store, filter db.UsageFilter,
) usageParitySnapshot {
	t.Helper()
	ctx := context.Background()
	daily, err := store.GetDailyUsage(ctx, filter)
	require.NoError(t, err, "daily usage")
	top, err := store.GetTopSessionsByCost(ctx, filter, 10)
	require.NoError(t, err, "top sessions")
	counts, err := store.GetUsageSessionCounts(ctx, filter)
	require.NoError(t, err, "usage session counts")
	matching, err := store.GetUsageMatchingSessionCount(ctx, filter)
	require.NoError(t, err, "matching session count")
	session, err := store.GetSessionUsage(ctx, "snapshot-winner", true)
	require.NoError(t, err, "session usage")
	require.NotNil(t, session, "session usage result")

	out := usageParitySnapshot{
		Daily: usageParityDaily{
			InputTokens:         daily.Totals.InputTokens,
			OutputTokens:        daily.Totals.OutputTokens,
			CacheCreationTokens: daily.Totals.CacheCreationTokens,
			CacheReadTokens:     daily.Totals.CacheReadTokens,
			CostMicrodollars:    daily.Totals.TotalCost.Microdollars,
			SessionCounts: db.UsageSessionCounts{
				Total:   daily.SessionCounts.Total,
				ByAgent: daily.SessionCounts.ByAgent,
			},
		},
		Counts: counts, MatchingSessionCount: matching,
		Session: usageParitySession{
			SessionID:         session.SessionID,
			TotalOutputTokens: session.TotalOutputTokens,
			PeakContextTokens: session.PeakContextTokens,
			HasTokenData:      session.HasTokenData,
			CostMicrodollars:  session.Cost.Microdollars,
			HasCost:           session.HasCost, Models: session.Models,
			UnpricedModels: session.UnpricedModels,
			BreakdownCount: session.BreakdownCount,
		},
	}
	for _, day := range daily.Daily {
		out.Daily.Dates = append(out.Daily.Dates, day.Date)
		out.Daily.Models = append(out.Daily.Models, day.ModelsUsed...)
	}
	sort.Strings(out.Daily.Models)
	for _, entry := range top {
		out.Top = append(out.Top, usageParityTopSession{
			SessionID: entry.SessionID, InputTokens: entry.InputTokens,
			OutputTokens: entry.OutputTokens, TotalTokens: entry.TotalTokens,
			CostMicrodollars: entry.Cost.Microdollars,
		})
	}
	for _, entry := range session.Breakdown {
		out.Session.Breakdown = append(out.Session.Breakdown, usageParityBreakdown{
			Source: entry.Source, Timestamp: entry.Timestamp, Model: entry.Model,
			InputTokens: entry.InputTokens, OutputTokens: entry.OutputTokens,
			CostMicrodollars: entry.Cost.Microdollars, HasCost: entry.HasCost,
		})
	}
	return out
}
