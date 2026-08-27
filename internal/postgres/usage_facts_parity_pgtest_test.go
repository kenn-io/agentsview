//go:build pgtest

package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
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
	ProjectBreakdowns   []usageParityProjectBreakdown
	AgentBreakdowns     []usageParityAgentBreakdown
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

type usageParityProjectBreakdown struct {
	ProjectKey, Project                  string
	InputTokens, OutputTokens            int
	CacheCreationTokens, CacheReadTokens int
	CostMicrodollars                     int64
}

type usageParityAgentBreakdown struct {
	Agent                                string
	InputTokens, OutputTokens            int
	CacheCreationTokens, CacheReadTokens int
	CostMicrodollars                     int64
}

func TestSQLiteFactsAndPostgresLiveUsageParity(t *testing.T) {
	const schema = "agentsview_usage_facts_parity_test"
	pgURL := testPGURL(t)
	cleanNamedPGSchema(t, pgURL, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })

	local := testDB(t)
	seedUsageParityFixture(t, local)
	seedUsageParity1hCacheFixture(t, local)

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
			InputTokens: 59, OutputTokens: 80,
			CacheCreationTokens: 8_991, CacheReadTokens: 15_895,
			CostMicrodollars: 458_832,
			Models: []string{
				"model-1h-cache", "model-priced",
				"model-reported", "model-unpriced",
			},
			SessionCounts: db.UsageSessionCounts{
				Total:   4,
				ByAgent: map[string]int{"claude": 3, "hermes": 1},
			},
		},
		Top: []usageParityTopSession{
			{SessionID: "reported", InputTokens: 30, OutputTokens: 5, TotalTokens: 40, CostMicrodollars: 250_000},
			{SessionID: "cache-1h", InputTokens: 2, OutputTokens: 62, TotalTokens: 24_945, CostMicrodollars: 198_792},
			{SessionID: "snapshot-loser", InputTokens: 20, OutputTokens: 10, TotalTokens: 30, CostMicrodollars: 10_040},
			{SessionID: "blank-timestamp", InputTokens: 7, OutputTokens: 3, TotalTokens: 10},
		},
		Counts: db.UsageSessionCounts{
			Total:     4,
			ByProject: map[string]int{"project-a": 1, "project-c": 1, "project-d": 1, "project-g": 1},
			ByAgent:   map[string]int{"claude": 3, "hermes": 1},
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
	wantWithoutBreakdown := want.Session
	wantWithoutBreakdown.Breakdown = nil
	localWithoutBreakdown := captureUsageParitySession(t, local, false)
	remoteWithoutBreakdown := captureUsageParitySession(t, remote, false)
	require.Equal(t, wantWithoutBreakdown, localWithoutBreakdown,
		"SQLite session result without breakdown")
	require.Equal(t, wantWithoutBreakdown, remoteWithoutBreakdown,
		"PostgreSQL session result without breakdown")
	require.Equal(t, localWithoutBreakdown, remoteWithoutBreakdown,
		"cross-backend session result without breakdown")
	requireCompleteUsageParity(t, local, remote, filter)

	breakdownFilter := filter
	breakdownFilter.Breakdowns = true
	localWithBreakdowns := captureUsageParitySnapshot(t, local, breakdownFilter)
	remoteWithBreakdowns := captureUsageParitySnapshot(t, remote, breakdownFilter)
	require.NotEmpty(t, localWithBreakdowns.Daily.ProjectBreakdowns)
	require.NotEmpty(t, localWithBreakdowns.Daily.AgentBreakdowns)
	require.NotEmpty(t, remoteWithBreakdowns.Daily.ProjectBreakdowns)
	require.NotEmpty(t, remoteWithBreakdowns.Daily.AgentBreakdowns)
	require.Equal(t, localWithBreakdowns, remoteWithBreakdowns,
		"cross-backend daily breakdown result")
}

func TestPGUsageFractionalMicrodollarRoundingParity(t *testing.T) {
	const schema = "agentsview_usage_fractional_rounding_test"
	pgURL := testPGURL(t)
	cleanNamedPGSchema(t, pgURL, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })

	local := testDB(t)
	seedUsageParityFixture(t, local)
	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern: "model-fractional",
		InputPerMTok: money.Money{Microdollars: 500_000},
	}}))
	startedAt := "2026-08-12T13:00:00Z"
	require.NoError(t, local.UpsertSession(db.Session{
		ID: "fractional-rounding", Project: "project-rounding",
		Machine: "parity-machine", Agent: "claude", StartedAt: &startedAt,
		MessageCount: 2, UserMessageCount: 1,
	}))
	require.NoError(t, local.InsertMessages([]db.Message{
		{
			SessionID: "fractional-rounding", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-08-12T13:01:00Z", Model: "model-fractional",
			TokenUsage:      json.RawMessage(`{"input_tokens":1,"output_tokens":0}`),
			ClaudeMessageID: "fractional-message-0", ClaudeRequestID: "fractional-request-0",
		},
		{
			SessionID: "fractional-rounding", Ordinal: 1, Role: "assistant",
			Timestamp: "2026-08-12T13:02:00Z", Model: "model-fractional",
			TokenUsage:      json.RawMessage(`{"input_tokens":1,"output_tokens":0}`),
			ClaudeMessageID: "fractional-message-1", ClaudeRequestID: "fractional-request-1",
		},
	}))

	syncer, err := New(pgURL, schema, local, "parity-machine", true, SyncOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, syncer.Close()) })
	_, err = syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)
	remote, err := NewStore(pgURL, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, remote.Close()) })

	filter := db.UsageFilter{From: "2026-08-12", To: "2026-08-12", Timezone: "UTC"}
	for _, backend := range []struct {
		name  string
		store db.Store
	}{
		{name: "sqlite", store: local},
		{name: "postgres", store: remote},
	} {
		t.Run(backend.name, func(t *testing.T) {
			usage, err := backend.store.GetSessionUsage(
				t.Context(), "fractional-rounding", true)
			require.NoError(t, err)
			require.NotNil(t, usage)
			require.Equal(t, int64(2), usage.Cost.Microdollars)
			require.True(t, usage.HasCost)
			require.Len(t, usage.Breakdown, 2)
			for _, row := range usage.Breakdown {
				require.Equal(t, 1, row.InputTokens)
				require.Equal(t, int64(1), row.Cost.Microdollars)
			}
			daily, err := backend.store.GetDailyUsage(t.Context(), filter)
			require.NoError(t, err)
			require.Equal(t, 59, daily.Totals.InputTokens)
			require.Equal(t, int64(260_042), daily.Totals.TotalCost.Microdollars)
		})
	}
}

func requireCompleteUsageParity(
	t testing.TB, local, remote db.Store, filter db.UsageFilter,
) {
	t.Helper()
	require.NoError(t, completeUsageParity(t.Context(), local, remote, filter))
}

func completeUsageParity(
	ctx context.Context, local, remote db.Store, filter db.UsageFilter,
) error {
	localDaily, err := local.GetDailyUsage(ctx, filter)
	if err != nil {
		return fmt.Errorf("local GetDailyUsage: %w", err)
	}
	remoteDaily, err := remote.GetDailyUsage(ctx, filter)
	if err != nil {
		return fmt.Errorf("remote GetDailyUsage: %w", err)
	}
	if !reflect.DeepEqual(localDaily, remoteDaily) {
		return fmt.Errorf("complete daily result differs")
	}
	localTop, err := local.GetTopSessionsByCost(ctx, filter, 10)
	if err != nil {
		return fmt.Errorf("local GetTopSessionsByCost: %w", err)
	}
	remoteTop, err := remote.GetTopSessionsByCost(ctx, filter, 10)
	if err != nil {
		return fmt.Errorf("remote GetTopSessionsByCost: %w", err)
	}
	if !reflect.DeepEqual(localTop, remoteTop) {
		return fmt.Errorf("complete top-session result differs")
	}
	localCounts, err := local.GetUsageSessionCounts(ctx, filter)
	if err != nil {
		return fmt.Errorf("local GetUsageSessionCounts: %w", err)
	}
	remoteCounts, err := remote.GetUsageSessionCounts(ctx, filter)
	if err != nil {
		return fmt.Errorf("remote GetUsageSessionCounts: %w", err)
	}
	if !reflect.DeepEqual(localCounts, remoteCounts) {
		return fmt.Errorf("complete session-count result differs")
	}
	localMatching, err := local.GetUsageMatchingSessionCount(ctx, filter)
	if err != nil {
		return fmt.Errorf("local GetUsageMatchingSessionCount: %w", err)
	}
	remoteMatching, err := remote.GetUsageMatchingSessionCount(ctx, filter)
	if err != nil {
		return fmt.Errorf("remote GetUsageMatchingSessionCount: %w", err)
	}
	if localMatching != remoteMatching {
		return fmt.Errorf("complete matching-session-count result differs")
	}
	localSession, err := local.GetSessionUsage(ctx, "snapshot-winner", filter.Breakdowns)
	if err != nil {
		return fmt.Errorf("local GetSessionUsage: %w", err)
	}
	remoteSession, err := remote.GetSessionUsage(ctx, "snapshot-winner", filter.Breakdowns)
	if err != nil {
		return fmt.Errorf("remote GetSessionUsage: %w", err)
	}
	if !reflect.DeepEqual(localSession, remoteSession) {
		return fmt.Errorf("complete session result differs")
	}
	return nil
}

func seedUsageParityFixture(t testing.TB, local *db.DB) {
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
		usageParitySessionFixture("activity-only", "project-e", "claude", "2026-07-20T13:00:00Z", 0, 0),
		usageParitySessionFixture("historical-usage", "project-f", "claude", "2026-07-20T13:02:00Z", 4, 6),
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
			Timestamp: "2026-07-20T13:01:00Z", Model: "model-activity",
		},
		{
			SessionID: "historical-usage", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-07-20T13:02:00Z", Model: "model-priced",
			TokenUsage: json.RawMessage(`{"input_tokens":6,"output_tokens":4}`),
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

// seedUsageParity1hCacheFixture adds issue #1452's first sample request:
// the nested cache_creation TTL split must price 1h writes at the 1h rate
// identically on every backend.
func seedUsageParity1hCacheFixture(t testing.TB, local *db.DB) {
	t.Helper()
	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:           "model-1h-cache",
		InputPerMTok:           money.Money{Microdollars: 10_000_000},
		OutputPerMTok:          money.Money{Microdollars: 50_000_000},
		CacheCreationPerMTok:   money.Money{Microdollars: 12_500_000},
		CacheCreation1hPerMTok: money.Money{Microdollars: 20_000_000},
		CacheReadPerMTok:       money.Money{Microdollars: 1_000_000},
	}}), "seed 1h pricing")
	session := usageParitySessionFixture(
		"cache-1h", "project-g", "claude", "2026-08-12T12:30:00Z", 62, 20)
	require.NoError(t, local.UpsertSession(session), "seed session cache-1h")
	require.NoError(t, local.InsertMessages([]db.Message{{
		SessionID: "cache-1h", Ordinal: 0, Role: "assistant",
		Timestamp: "2026-08-12T12:31:00Z", Model: "model-1h-cache",
		TokenUsage: json.RawMessage(
			`{"input_tokens":2,"output_tokens":62,` +
				`"cache_creation_input_tokens":8989,` +
				`"cache_read_input_tokens":15892,` +
				`"cache_creation":{"ephemeral_1h_input_tokens":8989,` +
				`"ephemeral_5m_input_tokens":0}}`),
	}}), "seed 1h message")
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
	session := captureUsageParitySession(t, store, true)

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
		Session: session,
	}
	for _, day := range daily.Daily {
		out.Daily.Dates = append(out.Daily.Dates, day.Date)
		out.Daily.Models = append(out.Daily.Models, day.ModelsUsed...)
		for _, breakdown := range day.ProjectBreakdowns {
			out.Daily.ProjectBreakdowns = append(out.Daily.ProjectBreakdowns, usageParityProjectBreakdown{
				ProjectKey: breakdown.ProjectKey, Project: breakdown.Project,
				InputTokens: breakdown.InputTokens, OutputTokens: breakdown.OutputTokens,
				CacheCreationTokens: breakdown.CacheCreationTokens,
				CacheReadTokens:     breakdown.CacheReadTokens,
				CostMicrodollars:    breakdown.Cost.Microdollars,
			})
		}
		for _, breakdown := range day.AgentBreakdowns {
			out.Daily.AgentBreakdowns = append(out.Daily.AgentBreakdowns, usageParityAgentBreakdown{
				Agent: breakdown.Agent, InputTokens: breakdown.InputTokens,
				OutputTokens:        breakdown.OutputTokens,
				CacheCreationTokens: breakdown.CacheCreationTokens,
				CacheReadTokens:     breakdown.CacheReadTokens,
				CostMicrodollars:    breakdown.Cost.Microdollars,
			})
		}
	}
	sort.Strings(out.Daily.Models)
	for _, entry := range top {
		out.Top = append(out.Top, usageParityTopSession{
			SessionID: entry.SessionID, InputTokens: entry.InputTokens,
			OutputTokens: entry.OutputTokens, TotalTokens: entry.TotalTokens,
			CostMicrodollars: entry.Cost.Microdollars,
		})
	}
	return out
}

func captureUsageParitySession(
	t *testing.T, store db.Store, includeBreakdown bool,
) usageParitySession {
	t.Helper()
	session, err := store.GetSessionUsage(
		context.Background(), "snapshot-winner", includeBreakdown)
	require.NoError(t, err, "session usage")
	require.NotNil(t, session, "session usage result")

	out := usageParitySession{
		SessionID:         session.SessionID,
		TotalOutputTokens: session.TotalOutputTokens,
		PeakContextTokens: session.PeakContextTokens,
		HasTokenData:      session.HasTokenData,
		CostMicrodollars:  session.Cost.Microdollars,
		HasCost:           session.HasCost,
		Models:            session.Models,
		UnpricedModels:    session.UnpricedModels,
		BreakdownCount:    session.BreakdownCount,
	}
	for _, entry := range session.Breakdown {
		out.Breakdown = append(out.Breakdown, usageParityBreakdown{
			Source: entry.Source, Timestamp: entry.Timestamp, Model: entry.Model,
			InputTokens: entry.InputTokens, OutputTokens: entry.OutputTokens,
			CostMicrodollars: entry.Cost.Microdollars, HasCost: entry.HasCost,
		})
	}
	return out
}
