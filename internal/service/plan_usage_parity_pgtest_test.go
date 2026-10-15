//go:build pgtest || chtest

// The plan usage parity test proves that SQLite, PostgreSQL, ClickHouse, and
// the DuckDB mirror return the same plan-only and partly-captured counts,
// token totals, and plan usage rows for the same archive. Backends that are
// not configured are skipped: PostgreSQL needs TEST_PG_URL, ClickHouse needs
// TEST_CLICKHOUSE_URL. DuckDB is always compared.
package service_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	clickhousestore "go.kenn.io/agentsview/internal/clickhouse"
	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/db"
	duckdbstore "go.kenn.io/agentsview/internal/duckdb"
	"go.kenn.io/agentsview/internal/money"
	postgresstore "go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/storage"
)

const planUsageParitySchema = "agentsview_plan_usage_parity_test"

// seedPlanUsageParity writes a plan-only session with assistant messages,
// a plan-only session with none, a partly captured session, a fully
// captured session, and a Claude session with token messages.
func seedPlanUsageParity(t *testing.T) *db.DB {
	t.Helper()
	local, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "parity.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, local.Close()) })
	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern: "gpt-5-codex", InputPerMTok: money.MustParseDollars("1"),
		OutputPerMTok: money.MustParseDollars("10"),
	}, {
		ModelPattern: "claude-sonnet-4", InputPerMTok: money.MustParseDollars("3"),
		OutputPerMTok: money.MustParseDollars("15"),
	}}))

	session := func(id, agent, project, started string) db.Session {
		return db.Session{
			ID: id, Project: project, Machine: "local", Agent: agent,
			StartedAt: &started, EndedAt: &started, CreatedAt: started,
			LocalModifiedAt: &started, MessageCount: 4, UserMessageCount: 2,
			RelationshipType: "root", DataVersion: 1,
		}
	}
	msg := func(id string, ordinal int, role, model, ts string) db.Message {
		return db.Message{
			SessionID: id, Ordinal: ordinal, Role: role, Content: role + " " + id,
			ContentLength: len(role + " " + id), Model: model, Timestamp: ts,
		}
	}
	userTurns := func(id, ts string) []db.Message {
		return []db.Message{msg(id, 0, "user", "", ts), msg(id, 1, "user", "", ts)}
	}
	captured := func(id string) []db.Message {
		return append(userTurns(id, "2026-09-10T11:00:00Z"),
			msg(id, 2, "assistant", "gpt-5-codex", "2026-09-10T12:00:00Z"))
	}
	event := func(id, ts string) db.UsageEvent {
		return db.UsageEvent{
			SessionID: id, Source: "codex-cloud-request", Model: "gpt-5-codex",
			InputTokens: 1000, OutputTokens: 100, OccurredAt: ts, DedupKey: id,
		}
	}
	coverage := func(uncaptured int) *db.SessionPlanUsage {
		return &db.SessionPlanUsage{
			HasCoverage: true, LastCaptureAt: "2026-09-12T00:00:00Z", UncapturedTurns: uncaptured,
		}
	}
	planUsage := &db.SessionPlanUsage{
		HasPlanUsage: true, DataAsOf: "2026-09-13T09:30:00Z", DataStatus: "available",
		UsageSource: "wham", WeeklyPercent: new(2.25), Credits: new("0"),
		Groups: []db.SessionPlanUsageGroup{{
			Model: "gpt-5-codex", ReasoningEffort: "high", Speed: "standard",
			ProductExperience: "codex", WeeklyPercent: new(2.25), Credits: new("0"),
		}},
		Final: true, OutcomeKind: "left_out", OutcomeAt: "2026-09-14T08:00:00Z",
		HasCoverage: true, LastCaptureAt: "2026-09-12T00:00:00Z", UncapturedTurns: 3,
	}
	planA := append(userTurns("codex-cloud:plan-a", "2026-09-10T10:00:00Z"),
		msg("codex-cloud:plan-a", 2, "assistant", "gpt-5-codex", "2026-09-10T10:01:00Z"),
		msg("codex-cloud:plan-a", 3, "assistant", "gpt-5-mini", "2026-09-12T23:30:00Z"))
	claude := append(userTurns("claude-a", "2026-09-10T09:00:00Z"),
		msg("claude-a", 2, "assistant", "claude-sonnet-4", "2026-09-10T09:01:00Z"))
	claude[2].TokenUsage = []byte(`{"input_tokens":50,"output_tokens":5}`)

	_, err = local.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{
		{
			Session:  session("codex-cloud:plan-a", "codex-cloud", "alpha", "2026-09-10T10:00:00Z"),
			Messages: planA, PlanUsage: planUsage, ReplaceMessages: true, DataVersion: 1,
		},
		{
			Session:   session("codex-cloud:plan-nomsg", "codex-cloud", "beta", "2026-09-11T05:00:00Z"),
			Messages:  userTurns("codex-cloud:plan-nomsg", "2026-09-11T05:00:00Z"),
			PlanUsage: coverage(0), ReplaceMessages: true, DataVersion: 1,
		},
		{
			Session:     session("codex-cloud:partly", "codex-cloud", "alpha", "2026-09-10T11:00:00Z"),
			Messages:    captured("codex-cloud:partly"),
			UsageEvents: []db.UsageEvent{event("codex-cloud:partly", "2026-09-10T12:00:00Z")},
			PlanUsage:   coverage(2), ReplaceMessages: true, DataVersion: 1,
		},
		{
			Session:     session("codex-cloud:full", "codex-cloud", "alpha", "2026-09-10T11:00:00Z"),
			Messages:    captured("codex-cloud:full"),
			UsageEvents: []db.UsageEvent{event("codex-cloud:full", "2026-09-10T12:00:00Z")},
			PlanUsage:   coverage(0), ReplaceMessages: true, DataVersion: 1,
		},
		{
			Session:  session("claude-a", "claude", "alpha", "2026-09-10T09:00:00Z"),
			Messages: claude, ReplaceMessages: true, DataVersion: 1,
		},
	})
	require.NoError(t, err)
	return local
}

type planUsageParityResult struct {
	PlanOnly, PartlyCaptured  int
	InputTokens, OutputTokens int
	CostMicrodollars          int64
	PlanOnlyByModel           map[string]int
}

func planUsageParitySummary(
	t *testing.T, store db.Store, req service.UsageRequest,
) planUsageParityResult {
	t.Helper()
	res, err := service.NewReadOnlyBackend(store).UsageSummary(t.Context(), req)
	require.NoError(t, err)
	out := planUsageParityResult{
		PlanOnly: res.PlanOnlySessions, PartlyCaptured: res.PartlyCapturedSessions,
		InputTokens: res.Totals.InputTokens, OutputTokens: res.Totals.OutputTokens,
		CostMicrodollars: res.Totals.TotalCost.Microdollars,
		PlanOnlyByModel:  map[string]int{},
	}
	for _, total := range res.ModelTotals {
		if total.PlanOnlySessions > 0 {
			out.PlanOnlyByModel[total.Model] = total.PlanOnlySessions
		}
	}
	return out
}

func TestPlanUsageCountsParity(t *testing.T) {
	ctx := t.Context()
	local := seedPlanUsageParity(t)
	const sessions = 5
	stores := map[string]db.Store{}

	duckTarget := filepath.Join(t.TempDir(), "parity.duckdb")
	duckRes, err := duckdbstore.Push(ctx, duckTarget, local, "parity-machine",
		storage.MirrorPushOptions{}, true, nil)
	require.NoError(t, err)
	require.Equal(t, sessions, duckRes.SessionsPushed)
	duck, err := duckdbstore.NewStore(ctx, duckTarget)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, duck.Close()) })
	stores["duckdb"] = duck

	if pgURL := os.Getenv("TEST_PG_URL"); pgURL != "" {
		dropSchema := func() {
			store, err := postgresstore.NewStore(pgURL, planUsageParitySchema, true)
			require.NoError(t, err)
			defer func() { require.NoError(t, store.Close()) }()
			_, err = store.DB().ExecContext(context.WithoutCancel(ctx), "DROP SCHEMA IF EXISTS "+planUsageParitySchema+" CASCADE")
			require.NoError(t, err)
		}
		dropSchema()
		t.Cleanup(dropSchema)
		syncer, err := postgresstore.New(pgURL, planUsageParitySchema, local,
			"parity-machine", true, storage.PusherOptions{})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, syncer.Close()) })
		require.NoError(t, syncer.EnsureSchema(ctx))
		res, err := syncer.Push(ctx, false, nil)
		require.NoError(t, err)
		require.Equal(t, sessions, res.SessionsPushed)
		pg, err := postgresstore.NewStore(pgURL, planUsageParitySchema, true)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, pg.Close()) })
		stores["postgres"] = pg
	}

	if os.Getenv("TEST_CLICKHOUSE_URL") != "" {
		dsn, database := chtest.FreshDatabase(t)
		target := clickhousestore.Target{URL: dsn, Database: database}
		syncer, err := clickhousestore.New(ctx, target, local, "parity-machine",
			storage.PusherOptions{})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, syncer.Close()) })
		res, err := syncer.Push(ctx, true, nil)
		require.NoError(t, err)
		require.Equal(t, sessions, res.SessionsPushed)
		ch, err := clickhousestore.NewStore(ctx, target)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, ch.Close()) })
		stores["clickhouse"] = ch
	}

	for _, tc := range []struct {
		name string
		req  service.UsageRequest
		// planOnly and partly are the counts every backend must return.
		planOnly, partly int
		byModel          map[string]int
	}{
		{
			name:     "whole range",
			req:      service.UsageRequest{From: "2026-09-01", To: "2026-09-30", Timezone: "UTC"},
			planOnly: 2, partly: 1,
			byModel: map[string]int{"gpt-5-codex": 1, "gpt-5-mini": 1},
		},
		{
			name:     "day with only the no-message session",
			req:      service.UsageRequest{From: "2026-09-11", To: "2026-09-11", Timezone: "UTC"},
			planOnly: 1, byModel: map[string]int{},
		},
		{
			name:     "timezone moves a message into the window",
			req:      service.UsageRequest{From: "2026-09-13", To: "2026-09-13", Timezone: "Asia/Tokyo"},
			planOnly: 1, byModel: map[string]int{"gpt-5-mini": 1},
		},
		{
			name:     "include model",
			req:      service.UsageRequest{From: "2026-09-01", To: "2026-09-30", Timezone: "UTC", Model: "gpt-5-codex"},
			planOnly: 1, partly: 1, byModel: map[string]int{"gpt-5-codex": 1},
		},
		{
			name:     "exclude model",
			req:      service.UsageRequest{From: "2026-09-01", To: "2026-09-30", Timezone: "UTC", ExcludeModel: "gpt-5-codex"},
			planOnly: 2, byModel: map[string]int{"gpt-5-mini": 1},
		},
		{
			name: "every assistant message excluded",
			req: service.UsageRequest{
				From: "2026-09-10", To: "2026-09-10", Timezone: "UTC",
				ExcludeModel: "gpt-5-codex,gpt-5-mini",
			},
			byModel: map[string]int{},
		},
		{
			name:     "project",
			req:      service.UsageRequest{From: "2026-09-01", To: "2026-09-30", Timezone: "UTC", Project: "beta"},
			planOnly: 1, byModel: map[string]int{},
		},
		{
			name:    "agent",
			req:     service.UsageRequest{From: "2026-09-01", To: "2026-09-30", Timezone: "UTC", Agent: "claude"},
			byModel: map[string]int{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sqlite := planUsageParitySummary(t, local, tc.req)
			assert.Equal(t, tc.planOnly, sqlite.PlanOnly, "sqlite plan-only sessions")
			assert.Equal(t, tc.partly, sqlite.PartlyCaptured, "sqlite partly captured sessions")
			assert.Equal(t, tc.byModel, sqlite.PlanOnlyByModel, "sqlite plan-only sessions by model")
			for name, store := range stores {
				assert.Equal(t, sqlite, planUsageParitySummary(t, store, tc.req), name)
			}
		})
	}

	wantRow, err := local.GetSessionUsage(ctx, "codex-cloud:plan-a", false)
	require.NoError(t, err)
	require.NotNil(t, wantRow.PlanUsage)
	for name, store := range stores {
		got, err := store.GetSessionUsage(ctx, "codex-cloud:plan-a", false)
		require.NoError(t, err, name)
		require.NotNil(t, got, name)
		assert.Equal(t, wantRow.PlanUsage, got.PlanUsage, name)
	}
}
