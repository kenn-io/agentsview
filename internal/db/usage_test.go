package db

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/parsertest"
	pricingpkg "go.kenn.io/agentsview/internal/pricing"
)

func TestDailyUsageAmountsPricingBandRequestScope(t *testing.T) {
	tests := []struct {
		name            string
		usageSource     string
		messageOrdinal  sql.NullInt64
		wantCost        int64
		wantApplication export.PricingApplication
	}{
		{
			name:           "ordinal-bound request uses band",
			usageSource:    "usage-event",
			messageOrdinal: sql.NullInt64{Int64: 1, Valid: true},
			wantCost:       600_000,
			wantApplication: export.PricingApplication{
				Bands: []export.AppliedPricingBand{{
					AboveInputTokens: 200_000,
					RequestCount:     1,
				}},
			},
		},
		{
			name:        "Goose request uses band without message ordinal",
			usageSource: "goose-request",
			wantCost:    600_000,
			wantApplication: export.PricingApplication{
				Bands: []export.AppliedPricingBand{{
					AboveInputTokens: 200_000,
					RequestCount:     1,
				}},
			},
		},
		{
			name:        "DeepSeek Harness compaction uses band without message ordinal",
			usageSource: "deepseek-harness",
			wantCost:    600_000,
			wantApplication: export.PricingApplication{
				Bands: []export.AppliedPricingBand{{
					AboveInputTokens: 200_000,
					RequestCount:     1,
				}},
			},
		},
		{
			name:        "unbound aggregate uses base",
			usageSource: "usage-event",
			wantCost:    300_000,
			wantApplication: export.PricingApplication{
				AggregateRowCount: 1,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolver := pricingBandTestResolver()
			_, _, _, _, cost, _, err := dailyUsageAmounts(dailyUsageScanRow{
				messageOrdinal: tt.messageOrdinal,
				usageSource:    tt.usageSource,
				model:          "banded-model",
				inputTokens:    300_000,
			}, resolver)
			require.NoError(t, err)
			block, err := resolver.BuildBlock()
			require.NoError(t, err)
			provenance := block.Models["banded-model"]
			require.Len(t, provenance.Resolutions, 1)

			assert.Equal(t, money.Money{Microdollars: tt.wantCost}, cost)
			assert.Equal(t, tt.wantApplication,
				provenance.Resolutions[0].Application)
		})
	}
}

func TestDailyUsageAmountsPricingBandSavings(t *testing.T) {
	resolver := pricingBandTestResolver()
	_, _, _, _, cost, savings, err := dailyUsageAmounts(dailyUsageScanRow{
		messageOrdinal:           sql.NullInt64{Int64: 1, Valid: true},
		usageSource:              "usage-event",
		model:                    "banded-model",
		inputTokens:              100_001,
		cacheCreationInputTokens: 50_000,
		cacheReadInputTokens:     50_000,
	}, resolver)
	require.NoError(t, err)

	assert.Equal(t, money.Money{Microdollars: 260_002}, cost)
	assert.Equal(t, money.Money{Microdollars: 140_000}, savings)
}

func TestDailyUsageAmountsPricingBandApplicationCounts(t *testing.T) {
	resolver := pricingBandTestResolver()
	rows := []dailyUsageScanRow{
		{
			messageOrdinal: sql.NullInt64{Int64: 1, Valid: true},
			usageSource:    "usage-event",
			model:          "banded-model",
			inputTokens:    150_000,
		},
		{
			messageOrdinal: sql.NullInt64{Int64: 2, Valid: true},
			usageSource:    "usage-event",
			model:          "banded-model",
			inputTokens:    150_000,
		},
		{
			usageSource: "message",
			model:       "banded-model",
			tokenJSON:   `{"input_tokens":300000}`,
		},
		{
			usageSource: "session",
			model:       "banded-model",
			inputTokens: 300_000,
		},
	}
	var total money.Money
	for _, row := range rows {
		_, _, _, _, cost, _, err := dailyUsageAmounts(row, resolver)
		require.NoError(t, err)
		total = money.MustAdd(total, cost)
	}
	block, err := resolver.BuildBlock()
	require.NoError(t, err)
	provenance := block.Models["banded-model"]
	require.Len(t, provenance.Resolutions, 1)

	assert.Equal(t, money.Money{Microdollars: 1_200_000}, total)
	assert.Equal(t, export.PricingApplication{
		BaseRequestCount:  2,
		AggregateRowCount: 1,
		Bands: []export.AppliedPricingBand{{
			AboveInputTokens: 200_000,
			RequestCount:     1,
		}},
	}, provenance.Resolutions[0].Application)
}

func TestSessionRowCostPricingBandRequestScope(t *testing.T) {
	resolver := pricingBandTestResolver()
	cost, priced, contributes, err := sessionRowCost(usageScanRow{
		messageOrdinal: sql.NullInt64{Int64: 1, Valid: true},
		usageSource:    "usage-event",
		model:          "banded-model",
		inputTokens:    300_000,
	}, resolver)
	require.NoError(t, err)

	assert.Equal(t, money.Money{Microdollars: 600_000}, cost)
	assert.True(t, priced)
	assert.True(t, contributes)
}

func TestSQLiteActivityReportRowStatusPricingBandRequestScope(t *testing.T) {
	resolver := pricingBandTestResolver()
	cost, priced, contributes, err := sqliteActivityReportRowStatus(dailyUsageScanRow{
		messageOrdinal: sql.NullInt64{Int64: 1, Valid: true},
		usageSource:    "usage-event",
		model:          "banded-model",
		inputTokens:    300_000,
	}, resolver)
	require.NoError(t, err)

	assert.Equal(t, money.Money{Microdollars: 600_000}, cost)
	assert.True(t, priced)
	assert.True(t, contributes)
}

func pricingBandTestResolver() *export.PricingResolver {
	return export.NewPricingResolver([]export.EffectivePricingRow{{
		ModelPattern: "banded-model",
		Rates: export.ModelRates{
			InputPerMTok:      money.MustParseDollars("1"),
			OutputPerMTok:     money.MustParseDollars("2"),
			CacheWritePerMTok: money.MustParseDollars("0.50"),
			CacheReadPerMTok:  money.MustParseDollars("0.10"),
			Bands: []export.PricingBand{{
				AboveInputTokens:  200_000,
				InputPerMTok:      money.MustParseDollars("2"),
				OutputPerMTok:     money.MustParseDollars("3"),
				CacheWritePerMTok: money.MustParseDollars("1"),
				CacheReadPerMTok:  money.MustParseDollars("0.20"),
			}},
		},
	}})
}

var (
	dailyUsageFixtureOnce sync.Once
	dailyUsageFixtureDir  string
	dailyUsageFixturePath string
)

func TestDailyUsageResultOmitsUnsetPricingMetadata(t *testing.T) {
	b, err := json.Marshal(DailyUsageResult{})
	require.NoError(t, err)

	assert.NotContains(t, string(b), `"pricing"`)
}

func TestDailyUsageResultEmitsEmptyProjectsMap(t *testing.T) {
	b, err := json.Marshal(DailyUsageResult{
		SchemaVersion: export.UsageDailySchemaVersion,
		Projects:      map[string]export.ProjectMapEntry{},
		Daily:         []DailyUsageEntry{},
	})
	require.NoError(t, err)

	assert.Contains(t, string(b), `"projects":{}`)
}

func TestGetDailyUsageReturnsAggregateCostOverflow(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "usage-overflow", "project")
	large := money.Money{Microdollars: 1 << 62}
	require.NoError(t, d.ReplaceSessionUsageEvents("usage-overflow", []UsageEvent{
		{
			Source: "provider", Model: "model", Cost: &large,
			OccurredAt: "2026-07-26T12:00:00Z", DedupKey: "overflow-1",
		},
		{
			Source: "provider", Model: "model", Cost: &large,
			OccurredAt: "2026-07-26T12:01:00Z", DedupKey: "overflow-2",
		},
	}))

	_, err := d.GetDailyUsage(t.Context(), UsageFilter{
		From: "2026-07-26", To: "2026-07-26", Timezone: "UTC",
	})

	require.ErrorIs(t, err, money.ErrOverflow)
}

func TestUsageDailyEmptyProjectsMapExcludesUnrelatedObservations(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedProjectIdentityObservation(t, d, "unrelated-project")

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2026-06-01", To: "2026-06-01", Timezone: "UTC",
	})
	require.NoError(t, err)
	assert.Empty(t, result.Daily)
	assert.Empty(t, result.Projects)
}

func openDailyUsageFixtureDB(t *testing.T) *DB {
	t.Helper()

	dailyUsageFixtureOnce.Do(func() {
		dailyUsageFixtureDir, dailyUsageFixturePath =
			buildDailyUsageFixtureTemplate(t)
	})

	dst := filepath.Join(t.TempDir(), "test.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		require.NoError(t,
			copyTemplateDBFile(
				dailyUsageFixturePath+suffix, dst+suffix, suffix == "",
			),
			"copy daily usage fixture %q", suffix)
	}
	d, err := OpenPreparedTestDB(dst)
	require.NoError(t, err, "open daily usage fixture")
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	return d
}

func buildDailyUsageFixtureTemplate(t *testing.T) (string, string) {
	t.Helper()

	dir, err := os.MkdirTemp("", "agentsview-daily-usage-*")
	require.NoError(t, err, "create daily usage fixture dir")
	path := filepath.Join(dir, "test.db")
	require.NoError(t, copyTestDBTemplate(path),
		"copy base db template for daily usage fixture")

	d, err := OpenPreparedTestDB(path)
	require.NoError(t, err, "open daily usage template")
	seedDailyUsageFixture(t, d)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, d.CheckpointWALTruncate(ctx),
		"checkpoint daily usage template")
	require.NoError(t, d.Close(), "close daily usage template")
	return dir, path
}

func seedDailyUsageFixture(t *testing.T, d *DB) {
	t.Helper()

	require.NoError(t, d.UpsertModelPricing([]ModelPricing{
		{ModelPattern: "model-a", InputPerMTok: money.MustParseDollars("2.0"),
			OutputPerMTok: money.MustParseDollars("10.0")},
		{ModelPattern: "gpt-5", InputPerMTok: money.MustParseDollars("2.5"),
			OutputPerMTok: money.MustParseDollars("10.0")},
	}), "UpsertModelPricing")

	type combo struct {
		project string
		agent   string
	}
	combos := []combo{
		{"proj-a", "claude"},
		{"proj-a", "codex"},
		{"proj-b", "claude"},
		{"proj-b", "codex"},
	}
	for i, c := range combos {
		sid := "usage-fixture-" + strconv.Itoa(i)
		insertSession(t, d, sid, c.project, func(s *Session) {
			s.Agent = c.agent
			s.StartedAt = Ptr("2024-06-15T10:00:00Z")
		})
		insertMessages(t, d,
			Message{
				SessionID:  sid,
				Ordinal:    0,
				Role:       "assistant",
				Timestamp:  "2024-06-15T10:30:00Z",
				Model:      "model-a",
				TokenUsage: jsontext.Value(`{"input_tokens":1000,"output_tokens":500}`),
			},
			Message{
				SessionID:  sid,
				Ordinal:    1,
				Role:       "assistant",
				Timestamp:  "2024-06-15T10:31:00Z",
				Model:      "gpt-5",
				TokenUsage: jsontext.Value(`{"input_tokens":1000,"output_tokens":500}`),
			},
		)
	}

	insertSession(t, d, "usage-fixture-no-price", "proj-unknown", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2024-07-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID:  "usage-fixture-no-price",
		Ordinal:    0,
		Role:       "assistant",
		Timestamp:  "2024-07-15T10:30:00Z",
		Model:      "unknown-model",
		TokenUsage: jsontext.Value(`{"input_tokens":500,"output_tokens":250}`),
	})
}

func TestGetDailyUsageEmpty(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2024-01-01",
		To:   "2024-12-31",
	})
	requireNoError(t, err, "GetDailyUsage empty")

	require.NotNil(t, result.Daily, "Daily should be non-nil empty slice")
	assert.Len(t, result.Daily, 0, "got")
	assert.Equal(t, money.Money{}, result.Totals.TotalCost, "TotalCost")
}

func TestUsageRowQueryPushesDateBoundsIntoUnion(t *testing.T) {
	query, args := usageRowQuery(UsageFilter{
		From:             "2024-06-01",
		To:               "2024-06-30",
		ExcludeAutomated: true,
	})

	normalized := strings.ToLower(query)
	assert.NotContains(t, normalized, "and u.ts >=")
	assert.NotContains(t, normalized, "and u.ts <=")
	assert.NotContains(t, normalized, " or ")
	assert.NotContains(t, normalized, "display_name")
	assert.NotContains(t, normalized, "first_message")
	assert.NotContains(t, normalized, "cost_status")
	assert.Contains(t, normalized, "u.cost_source")
	assert.NotContains(t, normalized, "user_message_count")
	assert.NotContains(t, normalized, "session_activity_at")
	assert.NotContains(t, normalized, " as started_at")
	assert.NotContains(t, normalized, "u.machine")
	assert.Contains(t, normalized, "message_timestamp_rows as materialized")
	assert.Contains(t, normalized, "usage_event_timestamp_rows as materialized")
	assert.Contains(t, normalized, "from message_timestamp_rows m\njoin sessions s")
	assert.Contains(t, normalized, "from usage_event_timestamp_rows ue\njoin sessions s")
	assert.Contains(t, normalized, "m.timestamp is not null")
	assert.Contains(t, normalized, "m.timestamp != ''")
	assert.Contains(t, normalized, "ue.occurred_at is not null")
	assert.Contains(t, normalized, "nullif(m.timestamp, '') is null")
	assert.Contains(t, normalized, "ue.occurred_at is null")
	assert.Contains(t, normalized, "m.timestamp >= ?")
	assert.Contains(t, normalized, "ue.occurred_at >= ?")
	assert.Contains(t, normalized, "s.started_at >= ?")
	assert.Contains(t, normalized, "m.timestamp <= ?")
	assert.Contains(t, normalized, "ue.occurred_at <= ?")
	assert.Contains(t, normalized, "s.started_at <= ?")
	require.Len(t, args, 8)
	assert.Equal(t, "2024-05-31T10:00:00Z", args[0])
	assert.Equal(t, "2024-07-01T13:59:59Z", args[1])
	assert.Equal(t, "2024-05-31T10:00:00Z", args[2])
	assert.Equal(t, "2024-07-01T13:59:59Z", args[3])
	assert.Equal(t, "2024-05-31T10:00:00Z", args[4])
	assert.Equal(t, "2024-07-01T13:59:59Z", args[5])
	assert.Equal(t, "2024-05-31T10:00:00Z", args[6])
	assert.Equal(t, "2024-07-01T13:59:59Z", args[7])
}

func TestTopSessionsUsageRowQueryUsesNarrowScan(t *testing.T) {
	query, args := topSessionsUsageRowQuery(UsageFilter{
		From:     "2024-06-01",
		To:       "2024-06-30",
		Timezone: "America/New_York",
	})

	normalized := strings.ToLower(query)
	assert.NotContains(t, normalized, "display_name")
	assert.NotContains(t, normalized, "first_message")
	assert.NotContains(t, normalized, "cost_status")
	assert.Contains(t, normalized, "u.cost_source")
	assert.NotContains(t, normalized, "user_message_count")
	assert.NotContains(t, normalized, "session_activity_at")
	assert.NotContains(t, normalized, " as started_at")
	assert.NotContains(t, normalized, " as machine")
	assert.Contains(t, normalized, "m.timestamp is not null")
	assert.Contains(t, normalized, "m.timestamp != ''")
	assert.Contains(t, normalized, "ue.occurred_at is not null")
	assert.Contains(t, normalized, "nullif(m.timestamp, '') is null")
	assert.Contains(t, normalized, "ue.occurred_at is null")
	assert.Contains(t, normalized, "m.timestamp >= ?")
	assert.Contains(t, normalized, "ue.occurred_at >= ?")
	assert.Contains(t, normalized,
		"nullif(m.timestamp, '') is null\n\tand s.started_at >= ?")
	assert.Contains(t, normalized,
		"ue.occurred_at is null\n\tand s.started_at >= ?")
	assert.Contains(t, normalized, "m.timestamp <= ?")
	assert.Contains(t, normalized, "ue.occurred_at <= ?")
	assert.Contains(t, normalized, "julianday(u.ts) >= julianday(?)")
	assert.Contains(t, normalized, "julianday(u.ts) < julianday(?)")
	padded := []any{"2024-05-31T10:00:00Z", "2024-07-01T13:59:59Z"}
	window := []any{
		"2024-06-01T04:00:00Z", "2024-06-01",
		"2024-07-01T04:00:00Z", "2024-06-30",
	}
	var want []any
	want = append(want, padded...) // duplicate-request pass
	want = append(want, padded...) // ranked rows: m.timestamp
	want = append(want, padded...) // ranked rows: s.started_at
	want = append(want, window...) // ranked rows: exact window
	for range 4 {                  // row source branches
		want = append(want, padded...)
	}
	want = append(want, window...) // survivors: exact window
	assert.Equal(t, want, args)
}

func TestUsageEventsReplaceAndList(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "hermes:event", "proj", func(s *Session) {
		s.Agent = "hermes"
		s.StartedAt = new("2026-05-14T10:00:00Z")
		s.UserMessageCount = 2
	})

	cost := money.MustParseDollars("0.02")
	ordinal := 3
	events := []UsageEvent{{
		SessionID:                "hermes:event",
		MessageOrdinal:           &ordinal,
		Source:                   "session",
		Model:                    "gpt-5.4",
		InputTokens:              100,
		OutputTokens:             50,
		CacheCreationInputTokens: 7,
		CacheReadInputTokens:     11,
		ReasoningTokens:          13,
		Cost:                     &cost,
		CostStatus:               "estimated",
		CostSource:               "hermes",
		OccurredAt:               "2026-05-14T10:05:00Z",
		DedupKey:                 "session:hermes:event",
	}}
	err := d.ReplaceSessionUsageEvents("hermes:event", events)
	require.NoError(t, err, "ReplaceSessionUsageEvents")

	got, err := d.GetUsageEvents(ctx, "hermes:event")
	require.NoError(t, err, "GetUsageEvents")
	require.Len(t, got, 1, "len")
	require.Equal(t, 100, got[0].InputTokens,
		"InputTokens (token fields not round-tripped: %#v)", got[0])
	require.Equal(t, 50, got[0].OutputTokens,
		"OutputTokens (token fields not round-tripped: %#v)", got[0])
	require.Equal(t, 7, got[0].CacheCreationInputTokens,
		"CacheCreationInputTokens (token fields not round-tripped: %#v)", got[0])
	require.Equal(t, 11, got[0].CacheReadInputTokens,
		"CacheReadInputTokens (token fields not round-tripped: %#v)", got[0])
	require.Equal(t, 13, got[0].ReasoningTokens,
		"ReasoningTokens (token fields not round-tripped: %#v)", got[0])
	require.NotNil(t, got[0].MessageOrdinal, "MessageOrdinal want 3")
	require.Equal(t, 3, *got[0].MessageOrdinal, "MessageOrdinal")
	require.NotNil(t, got[0].Cost, "Cost want %v", cost)
	require.Equal(t, cost, *got[0].Cost, "Cost")
	require.Equal(t, "session:hermes:event", got[0].DedupKey, "DedupKey")
	fps, err := d.UsageEventFingerprints([]string{"hermes:event", "missing"})
	require.NoError(t, err, "UsageEventFingerprints")
	require.NotEmpty(t, fps["hermes:event"],
		"expected non-empty usage event fingerprint")
	require.Equal(t, "", fps["missing"], "missing fingerprint")

	err = d.ReplaceSessionUsageEvents("hermes:event", nil)
	require.NoError(t, err, "ReplaceSessionUsageEvents clear")
	got, err = d.GetUsageEvents(ctx, "hermes:event")
	require.NoError(t, err, "GetUsageEvents after clear")
	require.Len(t, got, 0, "usage events after clear =")
}

func TestUsageEventsReplaceRejectsDuplicateDedupKeysAndRollsBack(t *testing.T) {
	// A parser emitting two events with the same dedup key (e.g. Grok
	// retry/replay lines sharing a prompt_id, before the parser-side
	// last-wins dedupe) violates idx_usage_events_dedup and must fail
	// the whole replace, leaving the prior events untouched. Parsers
	// therefore dedupe in memory before handing events to the DB.
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "grok:dup", "proj", func(s *Session) {
		s.Agent = "grok"
	})

	prior := []UsageEvent{{
		SessionID:   "grok:dup",
		Source:      "session",
		Model:       "grok-4.5",
		InputTokens: 1,
		DedupKey:    "session:grok:dup:p-1:grok-4.5",
	}}
	require.NoError(t, d.ReplaceSessionUsageEvents("grok:dup", prior))

	dup := []UsageEvent{{
		SessionID:   "grok:dup",
		Source:      "session",
		Model:       "grok-4.5",
		InputTokens: 2,
		DedupKey:    "session:grok:dup:p-2:grok-4.5",
	}, {
		SessionID:   "grok:dup",
		Source:      "session",
		Model:       "grok-4.5",
		InputTokens: 3,
		DedupKey:    "session:grok:dup:p-2:grok-4.5",
	}}
	err := d.ReplaceSessionUsageEvents("grok:dup", dup)
	require.Error(t, err, "duplicate dedup keys must violate idx_usage_events_dedup")

	got, err := d.GetUsageEvents(ctx, "grok:dup")
	require.NoError(t, err, "GetUsageEvents after failed replace")
	require.Len(t, got, 1, "failed replace must roll back to the prior events")
	assert.Equal(t, 1, got[0].InputTokens, "prior event must survive the rollback")
}

func TestGetDailyUsageWithData(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	err := d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:         "claude-sonnet-4-20250514",
		InputPerMTok:         money.MustParseDollars("3.0"),
		OutputPerMTok:        money.MustParseDollars("15.0"),
		CacheCreationPerMTok: money.MustParseDollars("3.75"),
		CacheReadPerMTok:     money.MustParseDollars("0.30"),
	}})
	requireNoError(t, err, "UpsertModelPricing")

	insertSession(t, d, "sess1", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
		s.EndedAt = new("2024-06-15T11:00:00Z")
	})

	tokenUsage := `{
		"input_tokens": 1000,
		"output_tokens": 500,
		"cache_creation_input_tokens": 200,
		"cache_read_input_tokens": 300
	}`
	insertMessages(t, d, Message{
		SessionID:  "sess1",
		Ordinal:    0,
		Role:       "assistant",
		Timestamp:  "2024-06-15T10:30:00Z",
		Model:      "claude-sonnet-4-20250514",
		TokenUsage: jsontext.Value(tokenUsage),
	})

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2024-06-01",
		To:   "2024-06-30",
	})
	requireNoError(t, err, "GetDailyUsage")

	require.Len(t, result.Daily, 1, "got")

	day := result.Daily[0]
	assert.Equal(t, "2024-06-15", day.Date, "Date")
	assert.Equal(t, 1000, day.InputTokens, "InputTokens")
	assert.Equal(t, 500, day.OutputTokens, "OutputTokens")
	assert.Equal(t, 200, day.CacheCreationTokens, "CacheCreationTokens")
	assert.Equal(t, 300, day.CacheReadTokens, "CacheReadTokens")

	// Cost = (1000*3.0 + 500*15.0 + 200*3.75 + 300*0.30) / 1_000_000
	//      = (3000 + 7500 + 750 + 90) / 1_000_000
	//      = 11340 / 1_000_000
	//      = 0.01134
	wantCost := money.MustParseDollars("0.01134")
	assert.Equal(t, wantCost, day.TotalCost, "TotalCost")

	assert.Equal(t, []string{"claude-sonnet-4-20250514"},
		day.ModelsUsed, "ModelsUsed")

	// Totals should match single day
	assert.Equal(t, 1000, result.Totals.InputTokens, "Totals.InputTokens")
	assert.Equal(t, wantCost, result.Totals.TotalCost,
		"Totals.TotalCost")
}

func TestUsageRowsHandleBlankMessageTimestampWithoutSessionStart(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "blank-ts", "proj", func(s *Session) {
		s.Agent = "claude"
		s.MessageCount = 1
		s.StartedAt = nil
	})
	insertMessages(t, d, Message{
		SessionID:  "blank-ts",
		Ordinal:    0,
		Role:       "assistant",
		Timestamp:  "",
		Model:      "claude-sonnet-4-20250514",
		TokenUsage: jsontext.Value(`{"input_tokens":100,"output_tokens":50}`),
	})

	daily, err := d.GetDailyUsage(ctx, UsageFilter{})
	requireNoError(t, err, "GetDailyUsage")
	assert.Equal(t, 100, daily.Totals.InputTokens)
	assert.Equal(t, 50, daily.Totals.OutputTokens)

	usage, err := d.GetSessionUsage(ctx, "blank-ts", true)
	requireNoError(t, err, "GetSessionUsage")
	require.NotNil(t, usage)
	assert.Equal(t, []string{"claude-sonnet-4-20250514"}, usage.Models)
}

func TestUsagePreservesSessionSummaryUsageEventTokens(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	rawInput := MaxPlausibleTokens + 250_000
	rawOutput := MaxPlausibleTokens + 500_000

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "gpt-5.4",
		InputPerMTok:  money.MustParseDollars("1.0"),
		OutputPerMTok: money.MustParseDollars("2.0"),
	}}), "UpsertModelPricing")

	insertSession(t, d, "hermes:summary", "proj", func(s *Session) {
		s.Agent = "hermes"
		s.StartedAt = new("2026-05-14T10:00:00Z")
		s.UserMessageCount = 2
		s.TotalOutputTokens = rawOutput
		s.PeakContextTokens = rawInput
		s.HasTotalOutputTokens = true
		s.HasPeakContextTokens = true
	})
	requireNoError(t, d.ReplaceSessionUsageEvents(
		"hermes:summary",
		[]UsageEvent{{
			SessionID:    "hermes:summary",
			Source:       "session",
			Model:        "gpt-5.4",
			InputTokens:  rawInput,
			OutputTokens: rawOutput,
			OccurredAt:   "2026-05-14T10:05:00Z",
			DedupKey:     "session:hermes:summary",
		}},
	), "ReplaceSessionUsageEvents")

	daily, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2026-05-14",
		To:   "2026-05-14",
	})
	requireNoError(t, err, "GetDailyUsage")
	require.Len(t, daily.Daily, 1, "daily entries")
	assert.Equal(t, rawInput, daily.Totals.InputTokens, "daily input")
	assert.Equal(t, rawOutput, daily.Totals.OutputTokens, "daily output")

	usage, err := d.GetSessionUsage(ctx, "hermes:summary", true)
	requireNoError(t, err, "GetSessionUsage")
	require.NotNil(t, usage, "session usage")
	assert.Equal(t, rawOutput, usage.TotalOutputTokens, "session output total")
	assert.Equal(t, rawInput, usage.PeakContextTokens, "session peak context")
	require.True(t, usage.HasCost, "HasCost")
	wantCost, err := money.CostPerMillion([]money.RatedTokens{
		{Tokens: int64(rawInput), Rate: money.MustParseDollars("1")},
		{Tokens: int64(rawOutput), Rate: money.MustParseDollars("2")},
	})
	require.NoError(t, err)
	assert.Equal(t, wantCost, usage.Cost, "session usage cost")
}

func TestGetDailyUsageFallsBackForEmptyMessageTimestamp(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet-4-20250514",
		InputPerMTok:  money.MustParseDollars("3.0"),
		OutputPerMTok: money.MustParseDollars("15.0"),
	}}), "UpsertModelPricing")

	insertSession(t, d, "empty-ts", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "empty-ts",
		Ordinal:   0,
		Role:      "assistant",
		Timestamp: "",
		Model:     "claude-sonnet-4-20250514",
		TokenUsage: jsontext.Value(
			`{"input_tokens":1000,"output_tokens":500}`,
		),
	})

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2024-06-15",
		To:   "2024-06-15",
	})
	requireNoError(t, err, "GetDailyUsage")

	require.Len(t, result.Daily, 1, "daily entries")
	assert.Equal(t, "2024-06-15", result.Daily[0].Date, "Date")
	assert.Equal(t, 1000, result.Totals.InputTokens, "InputTokens")
	assert.Equal(t, 500, result.Totals.OutputTokens, "OutputTokens")
}

func TestBoundedUsagePreservesMalformedTimestampDateFallbackBeforeSnapshotRanking(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet",
		InputPerMTok:  money.MustParseDollars("3"),
		OutputPerMTok: money.MustParseDollars("15"),
	}}))
	insertSession(t, d, "in-range", "project-a", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertSession(t, d, "next-day", "project-b", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-16T10:00:00Z")
	})
	insertMessages(t, d,
		Message{
			SessionID: "in-range", Ordinal: 0, Role: "assistant",
			Timestamp: "2024-06-15-invalid", Model: "claude-sonnet",
			TokenUsage: jsontext.Value(
				`{"input_tokens":1000,"output_tokens":500}`),
			ClaudeMessageID: "shared-message", ClaudeRequestID: "shared-request",
		},
		Message{
			SessionID: "next-day", Ordinal: 0, Role: "assistant",
			Timestamp: "2024-06-16-invalid", Model: "claude-sonnet",
			TokenUsage: jsontext.Value(
				`{"input_tokens":9000,"output_tokens":9000}`),
			ClaudeMessageID: "shared-message", ClaudeRequestID: "shared-request",
		},
	)
	filter := UsageFilter{From: "2024-06-15", To: "2024-06-15"}

	daily, err := d.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	require.Len(t, daily.Daily, 1)
	assert.Equal(t, "2024-06-15", daily.Daily[0].Date)
	assert.Equal(t, 1000, daily.Totals.InputTokens)
	assert.Equal(t, 500, daily.Totals.OutputTokens)

	top, err := d.GetTopSessionsByCost(ctx, filter, 10)
	require.NoError(t, err)
	require.Len(t, top, 1)
	assert.Equal(t, "in-range", top[0].SessionID)
	assert.Equal(t, 1500, top[0].TotalTokens)

	counts, err := d.GetUsageSessionCounts(ctx, filter)
	require.NoError(t, err)
	assert.Equal(t, 1, counts.Total)
	assert.Equal(t, 1, counts.ByProject["project-a"])
}

func TestUsageQueriesUnionMessageAndUsageEvents(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "claude:msg", "proj-a", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2026-05-14T09:00:00Z")
		s.UserMessageCount = 2
	})
	insertMessages(t, d, Message{
		SessionID: "claude:msg",
		Ordinal:   0,
		Role:      "assistant",
		Timestamp: "2026-05-14T09:05:00Z",
		Model:     "claude-sonnet-4-20250514",
		TokenUsage: jsontext.Value(
			`{"input_tokens":100,"output_tokens":40}`,
		),
	})

	insertSession(t, d, "hermes:event", "proj-b", func(s *Session) {
		s.Agent = "hermes"
		s.StartedAt = new("2026-05-14T10:00:00Z")
		s.UserMessageCount = 2
	})
	requireNoError(t, d.ReplaceSessionUsageEvents(
		"hermes:event",
		[]UsageEvent{{
			SessionID:            "hermes:event",
			Source:               "session",
			Model:                "gpt-5.4",
			InputTokens:          300,
			OutputTokens:         70,
			CacheReadInputTokens: 20,
			DedupKey:             "shared-key",
		}},
	), "replace hermes usage event")
	insertSession(t, d, "hermes:event-2", "proj-b", func(s *Session) {
		s.Agent = "hermes"
		s.StartedAt = new("2026-05-14T10:10:00Z")
		s.UserMessageCount = 2
	})
	requireNoError(t, d.ReplaceSessionUsageEvents(
		"hermes:event-2",
		[]UsageEvent{{
			SessionID:    "hermes:event-2",
			Source:       "session",
			Model:        "gpt-5.4",
			InputTokens:  50,
			OutputTokens: 5,
			DedupKey:     "shared-key",
		}},
	), "replace second hermes usage event")

	filter := UsageFilter{
		From:       "2026-05-14",
		To:         "2026-05-14",
		Breakdowns: true,
	}
	daily, err := d.GetDailyUsage(ctx, filter)
	requireNoError(t, err, "GetDailyUsage")
	require.Equal(t, 450, daily.Totals.InputTokens,
		"daily totals: %#v", daily.Totals)
	require.Equal(t, 115, daily.Totals.OutputTokens,
		"daily totals: %#v", daily.Totals)
	require.Equal(t, 20, daily.Totals.CacheReadTokens,
		"daily totals: %#v", daily.Totals)
	require.Len(t, daily.Daily, 1, "daily entries =")
	require.Len(t, daily.Daily[0].AgentBreakdowns, 2,
		"agent breakdowns: %#v", daily.Daily[0].AgentBreakdowns)

	top, err := d.GetTopSessionsByCost(ctx, filter, 10)
	requireNoError(t, err, "GetTopSessionsByCost")
	topByID := make(map[string]TopSessionEntry, len(top))
	for _, entry := range top {
		topByID[entry.SessionID] = entry
	}
	require.Equal(t, 140, topByID["claude:msg"].TotalTokens,
		"claude top tokens: %#v", topByID["claude:msg"])
	require.Equal(t, 390, topByID["hermes:event"].TotalTokens,
		"hermes top tokens: %#v", topByID["hermes:event"])
	require.Equal(t, 55, topByID["hermes:event-2"].TotalTokens,
		"second hermes top tokens: %#v", topByID["hermes:event-2"])

	counts, err := d.GetUsageSessionCounts(ctx, filter)
	requireNoError(t, err, "GetUsageSessionCounts")
	require.Equal(t, 3, counts.Total, "counts: %#v", counts)
	require.Equal(t, 1, counts.ByAgent["claude"], "counts: %#v", counts)
	require.Equal(t, 2, counts.ByAgent["hermes"], "counts: %#v", counts)
	require.Equal(t, 1, counts.ByProject["proj-a"], "counts: %#v", counts)
	require.Equal(t, 2, counts.ByProject["proj-b"], "counts: %#v", counts)
}

func TestGetDailyUsageIncludesCursorUsageEvents(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	require.NoError(t, d.InsertCursorUsageEvents([]CursorUsageEvent{{
		OccurredAt:       "2026-05-14T10:05:00Z",
		Model:            "claude-4.6-opus-high-thinking",
		Kind:             "USAGE_EVENT_KIND_USAGE_BASED",
		InputTokens:      1234,
		OutputTokens:     567,
		CacheWriteTokens: 0,
		CacheReadTokens:  8901,
		Charged:          money.MustParseDollars("0.1566"),
		CursorTokenFee:   money.MustParseDollars("0.0332"),
		UserID:           "152683922",
		UserEmail:        "member@example.com",
		IsHeadless:       false,
	}}), "InsertCursorUsageEvents")

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From:       "2026-05-14",
		To:         "2026-05-14",
		Breakdowns: true,
	})
	require.NoError(t, err, "GetDailyUsage cursor")
	require.Len(t, result.Daily, 1, "daily len =")

	day := result.Daily[0]
	assert.Equal(t, "2026-05-14", day.Date, "Date")
	assert.Equal(t, 1234, day.InputTokens, "InputTokens")
	assert.Equal(t, 567, day.OutputTokens, "OutputTokens")
	assert.Equal(t, 0, day.CacheCreationTokens, "CacheCreationTokens")
	assert.Equal(t, 8901, day.CacheReadTokens, "CacheReadTokens")
	assert.Equal(t, money.MustParseDollars("0.1566"), day.TotalCost, "TotalCost")
	require.Equal(t, []string{"claude-4.6-opus-high-thinking"}, day.ModelsUsed)
	require.Len(t, day.AgentBreakdowns, 1)
	assert.Equal(t, "cursor", day.AgentBreakdowns[0].Agent)
	assert.Equal(t, money.MustParseDollars("0.1566"), day.AgentBreakdowns[0].Cost)
	assert.Empty(t, result.Projects, "cursor-only usage should not emit project identities")
	assert.NotContains(t, result.Projects, "")
	assert.Equal(t, 0, result.SessionCounts.Total, "cursor rows should not count as sessions")
}

func TestGetDailyUsageIncludesCursorUsageEventsWithSessionDefaults(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	require.NoError(t, d.InsertCursorUsageEvents([]CursorUsageEvent{{
		OccurredAt:       "2026-05-14T10:05:00Z",
		Model:            "claude-4.6-opus-high-thinking",
		Kind:             "USAGE_EVENT_KIND_USAGE_BASED",
		InputTokens:      1234,
		OutputTokens:     567,
		CacheWriteTokens: 0,
		CacheReadTokens:  8901,
		Charged:          money.MustParseDollars("0.1566"),
		CursorTokenFee:   money.MustParseDollars("0.0332"),
		UserID:           "152683922",
		UserEmail:        "member@example.com",
		IsHeadless:       false,
	}}), "InsertCursorUsageEvents")

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From:             "2026-05-14",
		To:               "2026-05-14",
		Breakdowns:       true,
		ExcludeAutomated: true,
	})
	require.NoError(t, err, "GetDailyUsage cursor with defaults")
	require.Len(t, result.Daily, 1, "daily len =")
	assert.Equal(t, 1234, result.Daily[0].InputTokens, "InputTokens")
	assert.Equal(t, 0, result.SessionCounts.Total, "cursor rows should not count as sessions")
}

func TestGetDailyUsageSkipsCursorUsageEventsForExcludeOneShot(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	require.NoError(t, d.InsertCursorUsageEvents([]CursorUsageEvent{{
		OccurredAt:       "2026-05-14T10:05:00Z",
		Model:            "claude-4.6-opus-high-thinking",
		Kind:             "USAGE_EVENT_KIND_USAGE_BASED",
		InputTokens:      1234,
		OutputTokens:     567,
		CacheWriteTokens: 0,
		CacheReadTokens:  8901,
		Charged:          money.MustParseDollars("0.1566"),
		CursorTokenFee:   money.MustParseDollars("0.0332"),
		UserID:           "152683922",
		UserEmail:        "member@example.com",
		IsHeadless:       false,
	}}), "InsertCursorUsageEvents")

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From:           "2026-05-14",
		To:             "2026-05-14",
		Breakdowns:     true,
		ExcludeOneShot: true,
	})
	require.NoError(t, err, "GetDailyUsage cursor exclude one-shot")
	assert.Empty(t, result.Daily, "daily entries should be empty")
	assert.Zero(t, result.Totals.InputTokens, "InputTokens")
	assert.Zero(t, result.SessionCounts.Total, "cursor rows should not count as sessions")
	databaseID, err := d.GetDatabaseID(ctx)
	require.NoError(t, err)
	cache, err := d.usageCache.Generation(ctx, databaseID)
	require.NoError(t, err)
	var cursorFacts int
	require.NoError(t, cache.db.QueryRow(
		`SELECT COUNT(*) FROM cursor_usage_facts`).Scan(&cursorFacts))
	assert.Zero(t, cursorFacts,
		"a filtered request must not copy unrelated Cursor history")
}

func TestGetDailyUsageSkipsCursorUsageEventsForTerminationFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern: "claude-sonnet-4-20250514",
		InputPerMTok: money.MustParseDollars("3.0"), OutputPerMTok: money.MustParseDollars("15.0"),
	}}), "UpsertModelPricing")

	insertSession(t, d, "clean-session", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2026-05-14T10:00:00Z")
		s.TerminationStatus = new("clean")
	})
	insertMessages(t, d, Message{
		SessionID: "clean-session",
		Ordinal:   0,
		Role:      "assistant",
		Timestamp: "2026-05-14T10:30:00Z",
		Model:     "claude-sonnet-4-20250514",
		TokenUsage: jsontext.Value(
			`{"input_tokens":100,"output_tokens":40}`,
		),
	})
	require.NoError(t, d.InsertCursorUsageEvents([]CursorUsageEvent{{
		OccurredAt:      "2026-05-14T10:05:00Z",
		Model:           "claude-4.6-opus-high-thinking",
		Kind:            "USAGE_EVENT_KIND_USAGE_BASED",
		InputTokens:     1234,
		OutputTokens:    567,
		CacheReadTokens: 8901,
		Charged:         money.MustParseDollars("0.1566"),
		CursorTokenFee:  money.MustParseDollars("0.0332"),
		UserID:          "152683922",
		UserEmail:       "member@example.com",
	}}), "InsertCursorUsageEvents")

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From:        "2026-05-14",
		To:          "2026-05-14",
		Termination: "clean",
	})
	require.NoError(t, err, "GetDailyUsage clean termination")
	require.Len(t, result.Daily, 1, "daily len =")
	assert.Equal(t, 100, result.Totals.InputTokens, "InputTokens")
	assert.Equal(t, 40, result.Totals.OutputTokens, "OutputTokens")
	assert.Equal(t, 1, result.SessionCounts.Total, "SessionCounts.Total")
}

func TestInsertCursorUsageEventsDedupesAtPostgresTimestampPrecision(t *testing.T) {
	d := testDB(t)

	event := CursorUsageEvent{
		OccurredAt:       "2026-05-14T10:05:00.123456789Z",
		Model:            "claude-4.6-opus-high-thinking",
		Kind:             "USAGE_EVENT_KIND_USAGE_BASED",
		InputTokens:      1234,
		OutputTokens:     567,
		CacheWriteTokens: 0,
		CacheReadTokens:  8901,
		Charged:          money.MustParseDollars("0.1566"),
		CursorTokenFee:   money.MustParseDollars("0.0332"),
		UserID:           "152683922",
		UserEmail:        "member@example.com",
		IsHeadless:       false,
	}
	require.NoError(t, d.InsertCursorUsageEvents([]CursorUsageEvent{event}))
	event.OccurredAt = "2026-05-14T10:05:00.123456Z"
	require.NoError(t, d.InsertCursorUsageEvents([]CursorUsageEvent{event}))

	var count int
	require.NoError(t, d.getReader().QueryRow(
		"SELECT count(*) FROM cursor_usage_events",
	).Scan(&count))
	assert.Equal(t, 1, count,
		"timestamps PostgreSQL stores identically must share one fingerprint")
}

// TestGetDailyUsage_CacheSavingsUsesPerModelRates pins down
// that totals.CacheSavings is computed from each row's actual
// per-model pricing, not a hard-coded proxy. A hard-coded
// Sonnet rate would misreport an Opus-heavy workload because
// Opus rates are roughly 5x Sonnet on both sides.
func TestGetDailyUsage_CacheSavingsUsesPerModelRates(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{
		{
			ModelPattern:         "claude-opus-4-6",
			InputPerMTok:         money.MustParseDollars("15.0"),
			OutputPerMTok:        money.MustParseDollars("75.0"),
			CacheCreationPerMTok: money.MustParseDollars("18.75"),
			CacheReadPerMTok:     money.MustParseDollars("1.50"),
		},
		{
			ModelPattern:         "claude-sonnet-4-20250514",
			InputPerMTok:         money.MustParseDollars("3.0"),
			OutputPerMTok:        money.MustParseDollars("15.0"),
			CacheCreationPerMTok: money.MustParseDollars("3.75"),
			CacheReadPerMTok:     money.MustParseDollars("0.30"),
		},
	}), "UpsertModelPricing")

	// Same 1M/1M mix of cache read + cache creation tokens
	// on both models so the per-model rate difference is the
	// only thing that can move the result.
	tokens := jsontext.Value(
		`{"input_tokens":0,"output_tokens":0,` +
			`"cache_creation_input_tokens":1000000,` +
			`"cache_read_input_tokens":1000000}`)

	insertSession(t, d, "s-opus", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "s-opus", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:30:00Z",
		Model: "claude-opus-4-6", TokenUsage: tokens,
	})

	insertSession(t, d, "s-sonnet", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:05:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "s-sonnet", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:35:00Z",
		Model: "claude-sonnet-4-20250514", TokenUsage: tokens,
	})

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2024-06-01", To: "2024-06-30",
	})
	requireNoError(t, err, "GetDailyUsage")

	// Opus per-token delta: read earns (15 - 1.50) = 13.50,
	// creation earns (15 - 18.75) = -3.75.
	// Opus savings on 1M + 1M = 13.50 + (-3.75) = 9.75.
	// Sonnet per-token delta: read earns (3 - 0.30) = 2.70,
	// creation earns (3 - 3.75) = -0.75.
	// Sonnet savings on 1M + 1M = 2.70 + (-0.75) = 1.95.
	// Net total savings = 9.75 + 1.95 = 11.70.
	wantSavings := money.MustParseDollars("11.70")
	assert.Equal(t, wantSavings, result.Totals.CacheSavings,
		"Totals.CacheSavings")

	// Falsification: if the code had used Sonnet rates for
	// both rows the total would be 2 * 1.95 = 3.90, which
	// differs from wantSavings by >$7. Assert we're nowhere
	// near that value so a regression to a single-rate path
	// trips the test.
	assert.NotEqual(t, money.MustParseDollars("3.90"), result.Totals.CacheSavings,
		"CacheSavings looks like single-rate path; expected per-model math")
}

func TestGetDailyUsageAgentFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	err := d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:         "claude-sonnet-4-20250514",
		InputPerMTok:         money.MustParseDollars("3.0"),
		OutputPerMTok:        money.MustParseDollars("15.0"),
		CacheCreationPerMTok: money.MustParseDollars("3.75"),
		CacheReadPerMTok:     money.MustParseDollars("0.30"),
	}})
	requireNoError(t, err, "UpsertModelPricing")

	// Claude session
	insertSession(t, d, "sess-claude", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID:  "sess-claude",
		Ordinal:    0,
		Role:       "assistant",
		Timestamp:  "2024-06-15T10:30:00Z",
		Model:      "claude-sonnet-4-20250514",
		TokenUsage: jsontext.Value(`{"input_tokens":1000,"output_tokens":500}`),
	})

	// Codex session
	insertSession(t, d, "sess-codex", "proj1", func(s *Session) {
		s.Agent = "codex"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID:  "sess-codex",
		Ordinal:    0,
		Role:       "assistant",
		Timestamp:  "2024-06-15T10:30:00Z",
		Model:      "claude-sonnet-4-20250514",
		TokenUsage: jsontext.Value(`{"input_tokens":2000,"output_tokens":1000}`),
	})

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From:  "2024-06-01",
		To:    "2024-06-30",
		Agent: "claude",
	})
	requireNoError(t, err, "GetDailyUsage agent filter")

	require.Len(t, result.Daily, 1, "got")

	day := result.Daily[0]
	assert.Equal(t, 1000, day.InputTokens, "InputTokens")
	assert.Equal(t, 500, day.OutputTokens, "OutputTokens")
}

func TestGetDailyUsageMultipleDaysAndModels(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	err := d.UpsertModelPricing([]ModelPricing{
		{
			ModelPattern:  "model-a",
			InputPerMTok:  money.MustParseDollars("2.0"),
			OutputPerMTok: money.MustParseDollars("10.0"),
		},
		{
			ModelPattern:  "model-b",
			InputPerMTok:  money.MustParseDollars("4.0"),
			OutputPerMTok: money.MustParseDollars("20.0"),
		},
	})
	requireNoError(t, err, "UpsertModelPricing")

	// Day 1: two models
	insertSession(t, d, "sess-d1", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-10T08:00:00Z")
	})
	insertMessages(t, d,
		Message{
			SessionID:  "sess-d1",
			Ordinal:    0,
			Role:       "assistant",
			Timestamp:  "2024-06-10T08:30:00Z",
			Model:      "model-a",
			TokenUsage: jsontext.Value(`{"input_tokens":100,"output_tokens":50}`),
		},
		Message{
			SessionID:  "sess-d1",
			Ordinal:    1,
			Role:       "assistant",
			Timestamp:  "2024-06-10T09:00:00Z",
			Model:      "model-b",
			TokenUsage: jsontext.Value(`{"input_tokens":200,"output_tokens":100}`),
		},
	)

	// Day 2: one model
	insertSession(t, d, "sess-d2", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-11T08:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID:  "sess-d2",
		Ordinal:    0,
		Role:       "assistant",
		Timestamp:  "2024-06-11T08:30:00Z",
		Model:      "model-a",
		TokenUsage: jsontext.Value(`{"input_tokens":300,"output_tokens":150}`),
	})

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2024-06-01",
		To:   "2024-06-30",
	})
	requireNoError(t, err, "GetDailyUsage multi")

	require.Len(t, result.Daily, 2, "got")

	// Day 1: check totals
	d1 := result.Daily[0]
	assert.Equal(t, "2024-06-10", d1.Date, "day1 Date")
	assert.Equal(t, 300, d1.InputTokens, "day1 InputTokens")
	assert.Equal(t, 150, d1.OutputTokens, "day1 OutputTokens")
	assert.Len(t, d1.ModelsUsed, 2, "day1 ModelsUsed count")

	// Day 2
	d2 := result.Daily[1]
	assert.Equal(t, "2024-06-11", d2.Date, "day2 Date")
	assert.Equal(t, 300, d2.InputTokens, "day2 InputTokens")

	// Totals should sum both days
	wantTotalInput := 600
	assert.Equal(t, wantTotalInput, result.Totals.InputTokens, "Totals.InputTokens")
	wantTotalOutput := 300
	assert.Equal(t, wantTotalOutput, result.Totals.OutputTokens, "Totals.OutputTokens")

	// Cost check: day1 model-a = (100*2+50*10)/1e6 = 0.0007
	//             day1 model-b = (200*4+100*20)/1e6 = 0.0028
	//             day2 model-a = (300*2+150*10)/1e6 = 0.0021
	//             total = 0.0056
	wantTotalCost := money.MustParseDollars("0.0056")
	assert.Equal(t, wantTotalCost, result.Totals.TotalCost,
		"Totals.TotalCost")
}

func TestGetDailyUsageNoPricing(t *testing.T) {
	d := openDailyUsageFixtureDB(t)
	ctx := context.Background()

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2024-07-01",
		To:   "2024-07-31",
	})
	requireNoError(t, err, "GetDailyUsage no pricing")

	require.Len(t, result.Daily, 1, "got")

	day := result.Daily[0]
	assert.Equal(t, 500, day.InputTokens, "InputTokens")
	assert.Equal(t, 250, day.OutputTokens, "OutputTokens")
	assert.Equal(t, money.Money{}, day.TotalCost, "TotalCost")
	assert.Equal(t, []string{"unknown-model"}, day.ModelsUsed,
		"ModelsUsed")
}

func TestGetDailyUsageCostsMessageReasoningTokens(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "gpt-5.4",
		InputPerMTok:  money.MustParseDollars("1"),
		OutputPerMTok: money.MustParseDollars("2"),
	}}))
	insertSession(t, d, "reasoning-message", "proj", func(s *Session) {
		s.Agent = "codex"
		s.StartedAt = Ptr("2026-05-14T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "reasoning-message",
		Ordinal:   0,
		Role:      "assistant",
		Timestamp: "2026-05-14T10:30:00Z",
		Model:     "gpt-5.4",
		TokenUsage: jsontext.Value(
			`{"input_tokens":1000,"output_tokens":0,"reasoning_tokens":500}`),
	})

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2026-05-14", To: "2026-05-14", Timezone: "UTC",
	})
	require.NoError(t, err)
	require.Len(t, result.Daily, 1)
	assert.Equal(t, 1000, result.Totals.InputTokens)
	assert.Zero(t, result.Totals.OutputTokens)
	assert.Equal(t, money.MustParseDollars("0.002"), result.Totals.TotalCost)
	assert.Equal(t, money.MustParseDollars("0.002"), result.Daily[0].TotalCost)
}

// TestGetDailyUsageTruncatedTokenJSON documents what happens when
// a message lands in the DB with truncated token_usage. The hot
// aggregation counter is intentionally permissive and still extracts
// leading fields, so the valid data is preserved. This is why we don't
// require fully valid JSON on the hot aggregation path: the realistic
// corruption modes reachable from our parsers don't produce silent zeros.
func TestGetDailyUsageTruncatedTokenJSON(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet-4-20250514",
		InputPerMTok:  money.MustParseDollars("3.0"),
		OutputPerMTok: money.MustParseDollars("15.0"),
	}}), "UpsertModelPricing")

	insertSession(t, d, "sess1", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})

	insertMessages(t, d,
		Message{
			SessionID: "sess1", Ordinal: 0,
			Role:      "assistant",
			Timestamp: "2024-06-15T10:30:00Z",
			Model:     "claude-sonnet-4-20250514",
			TokenUsage: jsontext.Value(
				`{"input_tokens":1000,"output_tokens":500}`),
		},
		Message{
			SessionID: "sess1", Ordinal: 1,
			Role:      "assistant",
			Timestamp: "2024-06-15T10:31:00Z",
			Model:     "claude-sonnet-4-20250514",
			// Truncated mid-key. The usage counter still finds
			// the two leading numeric fields and extracts them.
			TokenUsage: jsontext.Value(
				`{"input_tokens":9999,"output_tokens":4242,"ca`),
		},
	)

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2024-06-01",
		To:   "2024-06-30",
	})
	requireNoError(t, err, "GetDailyUsage truncated")

	require.Len(t, result.Daily, 1, "got")
	day := result.Daily[0]
	// 1000 (valid row) + 9999 (truncated but still parseable)
	assert.Equal(t, 10999, day.InputTokens,
		"InputTokens want 10999 "+
			"(counter should extract leading fields from truncated JSON)")
	assert.Equal(t, 4742, day.OutputTokens, "OutputTokens")
}

func TestParseUsageTokenCounters(t *testing.T) {
	in, out, cacheCreate, cacheRead := parseUsageTokenCounters(
		`{"input_tokens":100,"output_tokens":50,` +
			`"cache_creation_input_tokens":20,` +
			`"cache_read_input_tokens":300,` +
			`"reasoning_tokens":75}`,
	)
	assert.Equal(t, 100, in)
	assert.Equal(t, 50, out)
	assert.Equal(t, 20, cacheCreate)
	assert.Equal(t, 300, cacheRead)

	in, out, cacheCreate, cacheRead, reasoning := parseUsageTokenCountersWithReasoning(
		`{"input_tokens":100,"output_tokens":50,` +
			`"cache_creation_input_tokens":20,` +
			`"cache_read_input_tokens":300,` +
			`"reasoning_tokens":75}`,
	)
	assert.Equal(t, 100, in)
	assert.Equal(t, 50, out)
	assert.Equal(t, 20, cacheCreate)
	assert.Equal(t, 300, cacheRead)
	assert.Equal(t, 75, reasoning)

	in, out, cacheCreate, cacheRead = parseUsageTokenCounters(
		`{"input_tokens":9999,"output_tokens":4242,"ca`,
	)
	assert.Equal(t, 9999, in)
	assert.Equal(t, 4242, out)
	assert.Zero(t, cacheCreate)
	assert.Zero(t, cacheRead)

	in, out, cacheCreate, cacheRead = parseUsageTokenCounters(
		`{"input_tokens":"-5","cache_read_input_tokens":"100",` +
			`"output_tokens":"42"}`,
	)
	assert.Zero(t, in)
	assert.Equal(t, 42, out)
	assert.Zero(t, cacheCreate)
	assert.Equal(t, 100, cacheRead)

	in, out, cacheCreate, cacheRead = parseUsageTokenCounters(
		`{"metadata":{"input_tokens":999},` +
			`"note":"\"output_tokens\":777",` +
			`"output_tokens":42}`,
	)
	assert.Zero(t, in)
	assert.Equal(t, 42, out)
	assert.Zero(t, cacheCreate)
	assert.Zero(t, cacheRead)

	in, out, cacheCreate, cacheRead = parseUsageTokenCounters(
		`{"metadata":{"url":"https:\/\/x"},"output_tokens":42}`,
	)
	assert.Zero(t, in)
	assert.Equal(t, 42, out)
	assert.Zero(t, cacheCreate)
	assert.Zero(t, cacheRead)
}

func TestUsageAggregationClampsMessageTokenJSON(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	const maxTokens = 2_000_000

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:         "claude-sonnet-4-20250514",
		InputPerMTok:         money.MustParseDollars("1.0"),
		OutputPerMTok:        money.MustParseDollars("2.0"),
		CacheCreationPerMTok: money.MustParseDollars("3.0"),
		CacheReadPerMTok:     money.MustParseDollars("4.0"),
	}}), "UpsertModelPricing")

	insertSession(t, d, "sess1", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
		s.TotalOutputTokens = maxTokens
		s.PeakContextTokens = maxTokens
		s.HasTotalOutputTokens = true
		s.HasPeakContextTokens = true
	})
	insertMessages(t, d, Message{
		SessionID: "sess1", Ordinal: 0,
		Role:      "assistant",
		Timestamp: "2024-06-15T10:30:00Z",
		Model:     "claude-sonnet-4-20250514",
		TokenUsage: jsontext.Value(
			`{"input_tokens":9999999999,` +
				`"output_tokens":9999999999,` +
				`"cache_creation_input_tokens":9999999999,` +
				`"cache_read_input_tokens":9999999999}`),
		ContextTokens:    maxTokens,
		OutputTokens:     maxTokens,
		HasContextTokens: true,
		HasOutputTokens:  true,
	})

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2024-06-01",
		To:   "2024-06-30",
	})
	requireNoError(t, err, "GetDailyUsage")

	assert.Equal(t, maxTokens, result.Totals.InputTokens, "InputTokens")
	assert.Equal(t, maxTokens, result.Totals.OutputTokens, "OutputTokens")
	assert.Equal(t, maxTokens, result.Totals.CacheCreationTokens,
		"CacheCreationTokens")
	assert.Equal(t, maxTokens, result.Totals.CacheReadTokens,
		"CacheReadTokens")
	assert.Equal(t, money.MustParseDollars("20"), result.Totals.TotalCost,
		"TotalCost")

	usage, err := d.GetSessionUsage(ctx, "sess1", true)
	requireNoError(t, err, "GetSessionUsage")
	require.NotNil(t, usage, "session usage")
	require.True(t, usage.HasCost, "HasCost")
	assert.Equal(t, money.MustParseDollars("20"), usage.Cost, "Cost")
}

func TestGetDailyUsage_DedupesByClaudeMessageAndRequestID(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:         "claude-opus-4-6",
		InputPerMTok:         money.MustParseDollars("15.0"),
		OutputPerMTok:        money.MustParseDollars("75.0"),
		CacheCreationPerMTok: money.MustParseDollars("18.75"),
		CacheReadPerMTok:     money.MustParseDollars("1.50"),
	}}), "seed pricing")

	mustExec := func(q string, args ...any) {
		t.Helper()
		_, err := d.getWriter().Exec(q, args...)
		require.NoError(t, err, "exec %q", q)
	}
	mustExec(`INSERT INTO sessions (id, project, machine, agent, started_at, ended_at)
	          VALUES (?, ?, 'local', 'claude', ?, ?)`,
		"s-main", "proj", "2026-04-10T10:00:00Z", "2026-04-10T10:05:00Z")
	mustExec(`INSERT INTO sessions (id, project, machine, agent, started_at, ended_at, parent_session_id, relationship_type)
	          VALUES (?, ?, 'local', 'claude', ?, ?, 's-main', 'fork')`,
		"s-fork", "proj", "2026-04-10T10:01:00Z", "2026-04-10T10:06:00Z")

	shared := `{"input_tokens":100,"output_tokens":500,"cache_creation_input_tokens":1000,"cache_read_input_tokens":50000}`
	unique := `{"input_tokens":20,"output_tokens":80,"cache_creation_input_tokens":200,"cache_read_input_tokens":5000}`

	for _, row := range []struct {
		sid, ts, usage, mid, rid string
		ord                      int
	}{
		{"s-main", "2026-04-10T10:02:00Z", shared, "msg_dup", "req_dup", 0},
		{"s-fork", "2026-04-10T10:02:00Z", shared, "msg_dup", "req_dup", 0},
		{"s-fork", "2026-04-10T10:03:00Z", unique, "msg_uniq", "req_uniq", 1},
	} {
		mustExec(`INSERT INTO messages
			(session_id, ordinal, role, content, timestamp,
			 model, token_usage,
			 claude_message_id, claude_request_id,
			 has_output_tokens, has_context_tokens)
			VALUES (?, ?, 'assistant', '', ?, 'claude-opus-4-6', ?, ?, ?, 1, 1)`,
			row.sid, row.ord, row.ts, row.usage, row.mid, row.rid)
	}

	result, err := d.GetDailyUsage(context.Background(), UsageFilter{
		From: "2026-04-10", To: "2026-04-10", Timezone: "UTC",
	})
	require.NoError(t, err, "GetDailyUsage")
	require.Len(t, result.Daily, 1, "daily entries =")
	day := result.Daily[0]
	assert.Equal(t, 120, day.InputTokens, "input")
	assert.Equal(t, 580, day.OutputTokens, "output")
	assert.Equal(t, 1200, day.CacheCreationTokens, "cache_cr")
	assert.Equal(t, 55000, day.CacheReadTokens, "cache_rd")
}

func TestGetDailyUsage_DistinguishesClaudeIDsContainingNUL(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "nul-identities", "project", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2026-04-10T10:00:00Z")
	})

	insertMessages(t, d,
		Message{
			SessionID: "nul-identities", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-04-10T10:01:00Z", Model: "model",
			ClaudeMessageID: "a\x00", ClaudeRequestID: "x",
			TokenUsage: jsontext.Value(
				`{"input_tokens":100,"output_tokens":10}`),
		},
		Message{
			SessionID: "nul-identities", Ordinal: 1, Role: "assistant",
			Timestamp: "2026-04-10T10:02:00Z", Model: "model",
			ClaudeMessageID: "a\x00", ClaudeRequestID: "x",
			TokenUsage: jsontext.Value(
				`{"input_tokens":200,"output_tokens":20}`),
		},
		Message{
			SessionID: "nul-identities", Ordinal: 2, Role: "assistant",
			Timestamp: "2026-04-10T10:03:00Z", Model: "model",
			ClaudeMessageID: "a", ClaudeRequestID: "\x00x",
			TokenUsage: jsontext.Value(
				`{"input_tokens":300,"output_tokens":30}`),
		},
	)

	result, err := d.GetDailyUsage(t.Context(), UsageFilter{
		From: "2026-04-10", To: "2026-04-10", Timezone: "UTC",
	})
	require.NoError(t, err)
	require.Len(t, result.Daily, 1)
	assert.Equal(t, 500, result.Daily[0].InputTokens)
	assert.Equal(t, 50, result.Daily[0].OutputTokens)
}

func TestUsageAggregatesPreferCompleteClaudeSnapshotAcrossSessions(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedOpusPricing(t, d)
	insertSession(t, d, "claude:daily-streamed", "parent-project", func(s *Session) {
		s.Agent = "parent-agent"
		s.Machine = "parent-machine"
		s.DisplayName = new("parent display")
		s.StartedAt = new("2026-05-20T10:00:00Z")
	})
	insertSession(t, d, "agent-daily-streamed", "child-project", func(s *Session) {
		s.Agent = "child-agent"
		s.Machine = "child-machine"
		s.DisplayName = new("child display")
		s.StartedAt = new("2026-05-20T10:31:00Z")
	})
	insertMessages(t, d,
		Message{
			SessionID: "claude:daily-streamed", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-05-20T10:30:00Z", Model: "claude-opus-4-6",
			ClaudeMessageID: "msg-stream", ClaudeRequestID: "req-stream",
			TokenUsage: jsontext.Value(`{"input_tokens":1000,"output_tokens":5}`),
		},
		Message{
			SessionID: "agent-daily-streamed", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-05-20T10:31:00Z", Model: "claude-opus-4-6",
			ClaudeMessageID: "msg-stream", ClaudeRequestID: "req-stream",
			TokenUsage: jsontext.Value(`{"input_tokens":1000,"output_tokens":631}`),
		},
		Message{
			SessionID: "agent-daily-streamed", Ordinal: 1, Role: "assistant",
			Timestamp: "2026-05-21T00:00:00Z", Model: "claude-opus-4-6",
			ClaudeMessageID: "msg-stream", ClaudeRequestID: "req-stream",
			TokenUsage: jsontext.Value(`{"input_tokens":1000,"output_tokens":999}`),
		},
	)

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2026-05-20", To: "2026-05-20", Timezone: "UTC", Breakdowns: true,
	})
	requireNoError(t, err, "GetDailyUsage")
	require.Len(t, result.Daily, 1)
	assert.Equal(t, 1000, result.Totals.InputTokens)
	assert.Equal(t, 631, result.Totals.OutputTokens)
	require.Len(t, result.Daily[0].ProjectBreakdowns, 1)
	assert.Equal(t, "parent-project", result.Daily[0].ProjectBreakdowns[0].Project)
	require.Len(t, result.Daily[0].AgentBreakdowns, 1)
	assert.Equal(t, "parent-agent", result.Daily[0].AgentBreakdowns[0].Agent)
	require.Len(t, result.Daily[0].MachineBreakdowns, 1)
	assert.Equal(t, "parent-machine", result.Daily[0].MachineBreakdowns[0].MachineName)

	top, err := d.GetTopSessionsByCost(ctx, UsageFilter{
		From: "2026-05-20", To: "2026-05-20", Timezone: "UTC",
	}, 10)
	requireNoError(t, err, "GetTopSessionsByCost")
	require.Len(t, top, 1)
	assert.Equal(t, "claude:daily-streamed", top[0].SessionID)
	assert.Equal(t, "parent-project", top[0].DisplayName)
	assert.Equal(t, "parent-project", top[0].Project)
	assert.Equal(t, "parent-agent", top[0].Agent)
	assert.Equal(t, "2026-05-20T10:00:00Z", top[0].StartedAt)
	assert.Equal(t, 1000, top[0].InputTokens)
	assert.Equal(t, 631, top[0].OutputTokens)

	filtered, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2026-05-20", To: "2026-05-20", Timezone: "UTC",
		ProjectLabels: []string{"parent-project"},
	})
	requireNoError(t, err, "GetDailyUsage parent project")
	assert.Equal(t, 1000, filtered.Totals.InputTokens)
	assert.Equal(t, 631, filtered.Totals.OutputTokens,
		"the attributed parent filter must retain the complete child snapshot")

	filteredTop, err := d.GetTopSessionsByCost(ctx, UsageFilter{
		From: "2026-05-20", To: "2026-05-20", Timezone: "UTC",
		ProjectLabels: []string{"parent-project"},
	}, 10)
	requireNoError(t, err, "GetTopSessionsByCost parent project")
	require.Len(t, filteredTop, 1)
	assert.Equal(t, 631, filteredTop[0].OutputTokens)

	childFiltered, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2026-05-20", To: "2026-05-20", Timezone: "UTC",
		ProjectLabels: []string{"child-project"},
	})
	requireNoError(t, err, "GetDailyUsage child project")
	assert.Zero(t, childFiltered.Totals.OutputTokens,
		"the source child metadata must not override parent attribution")
}

func TestGetDailyUsageRanksTruncatedClaudeSnapshot(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedOpusPricing(t, d)
	insertSession(t, d, "claude:truncated-snapshot", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2026-05-20T10:00:00Z")
	})
	insertMessages(t, d,
		Message{
			SessionID: "claude:truncated-snapshot", Ordinal: 0,
			Role: "assistant", Timestamp: "2026-05-20T10:30:00Z",
			Model: "claude-opus-4-6", ClaudeMessageID: "msg-truncated",
			ClaudeRequestID: "req-truncated",
			TokenUsage: jsontext.Value(
				`{"input_tokens":1000,"output_tokens":5}`),
		},
		Message{
			SessionID: "claude:truncated-snapshot", Ordinal: 1,
			Role: "assistant", Timestamp: "2026-05-20T10:31:00Z",
			Model: "claude-opus-4-6", ClaudeMessageID: "msg-truncated",
			ClaudeRequestID: "req-truncated",
			TokenUsage: jsontext.Value(
				`{"input_tokens":1000,"output_tokens":631,` +
					`"server_tool_use":{"web_search_requests":2},"ca`),
		},
	)

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2026-05-20", To: "2026-05-20", Timezone: "UTC",
	})
	requireNoError(t, err, "GetDailyUsage")
	assert.Equal(t, 1000, result.Totals.InputTokens)
	assert.Equal(t, 631, result.Totals.OutputTokens)
	assert.Equal(t, money.MustParseDollars("0.040775"), result.Totals.TotalCost)
}

func TestGetDailyUsagePrefersTimestampedEqualClaudeSnapshot(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedOpusPricing(t, d)
	insertSession(t, d, "a-null-snapshot", "proj", func(s *Session) {
		s.Agent = "claude"
	})
	insertSession(t, d, "z-timestamped-snapshot", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2026-05-20T10:00:00Z")
	})
	insertMessages(t, d,
		Message{
			SessionID: "a-null-snapshot", Ordinal: 0, Role: "assistant",
			Model: "claude-opus-4-6", ClaudeMessageID: "msg-null-ts",
			ClaudeRequestID: "req-null-ts",
			TokenUsage: jsontext.Value(
				`{"input_tokens":10,"output_tokens":100}`),
		},
		Message{
			SessionID: "z-timestamped-snapshot", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-05-20T10:30:00Z", Model: "claude-opus-4-6",
			ClaudeMessageID: "msg-null-ts", ClaudeRequestID: "req-null-ts",
			TokenUsage: jsontext.Value(
				`{"input_tokens":900,"output_tokens":100}`),
		},
	)

	result, err := d.GetDailyUsage(ctx, UsageFilter{Timezone: "UTC"})
	requireNoError(t, err, "GetDailyUsage")
	assert.Equal(t, 900, result.Totals.InputTokens)
	assert.Equal(t, 100, result.Totals.OutputTokens)
}

// seedSnapshotTiePair stores one Claude request (msg-tie/req-tie) streamed
// into two sessions with equal output tokens: z-snapshot carries the larger
// input count at zTimestamp and a-snapshot the smaller one at aTimestamp.
func seedSnapshotTiePair(t *testing.T, d *DB, zTimestamp, aTimestamp string) {
	t.Helper()
	for _, id := range []string{"z-snapshot", "a-snapshot"} {
		insertSession(t, d, id, "proj", func(s *Session) {
			s.Agent = "claude"
			s.StartedAt = new("2026-05-20T10:00:00Z")
		})
	}
	insertMessages(t, d,
		Message{
			SessionID: "z-snapshot", Ordinal: 0, Role: "assistant",
			Timestamp: zTimestamp, Model: "claude-opus-4-6",
			TokenUsage: jsontext.Value(
				`{"input_tokens":900,"output_tokens":100}`),
			OutputTokens: 100, HasOutputTokens: true,
			ClaudeMessageID: "msg-tie", ClaudeRequestID: "req-tie",
		},
		Message{
			SessionID: "a-snapshot", Ordinal: 0, Role: "assistant",
			Timestamp: aTimestamp, Model: "claude-opus-4-6",
			TokenUsage: jsontext.Value(
				`{"input_tokens":10,"output_tokens":100}`),
			OutputTokens: 100, HasOutputTokens: true,
			ClaudeMessageID: "msg-tie", ClaudeRequestID: "req-tie",
		},
	)
}

// querySnapshotRankedRows runs the snapshot ranking over the real usage row
// source for f and returns the surviving rows as
// (session_id, snapshot_attribution_session_id, token_usage) triples.
func querySnapshotRankedRows(
	t *testing.T, d *DB, f UsageFilter,
) [][3]string {
	t.Helper()
	bounds := usageBoundsForFilter(f)
	rowsSQL, rowsArgs := usageRowsSQLForBounds(
		usageSnapshotInputFilter(f), bounds)
	ranked, args := snapshotRankedDailyUsageRowsSQL(
		rowsSQL, rowsArgs, f, bounds)
	rows, err := d.getReader().Query(`
		SELECT session_id, snapshot_attribution_session_id, token_usage
		FROM (`+ranked+`)
		ORDER BY session_id`, args...)
	require.NoError(t, err)
	defer rows.Close()
	var out [][3]string
	for rows.Next() {
		var row [3]string
		require.NoError(t, rows.Scan(&row[0], &row[1], &row[2]))
		out = append(out, row)
	}
	require.NoError(t, rows.Err())
	return out
}

func TestSnapshotRankedDailyUsageRowsPrefersLatestEqualOutput(t *testing.T) {
	d := testDB(t)
	seedSnapshotTiePair(t, d,
		"2026-05-20T10:31:00Z", "2026-05-20T10:30:00Z")
	got := querySnapshotRankedRows(t, d, UsageFilter{})
	require.Len(t, got, 1)
	assert.Equal(t, "z-snapshot", got[0][0])
	assert.Equal(t, "a-snapshot", got[0][1])
	assert.JSONEq(t, `{"input_tokens":900,"output_tokens":100}`, got[0][2])
}

func TestSnapshotRankedDailyUsageRowsNormalizesRFC3339Timestamps(t *testing.T) {
	tests := []struct {
		name         string
		zTimestamp   string
		aTimestamp   string
		wantSession  string
		wantAttribut string
		wantInput    int
	}{
		{
			name:         "mixed fractional precision",
			zTimestamp:   "2026-05-20T10:30:00.1Z",
			aTimestamp:   "2026-05-20T10:30:00Z",
			wantSession:  "z-snapshot",
			wantAttribut: "a-snapshot",
			wantInput:    900,
		},
		{
			name:         "equivalent offsets use session fallback",
			zTimestamp:   "2026-05-20T05:30:00-05:00",
			aTimestamp:   "2026-05-20T10:30:00Z",
			wantSession:  "z-snapshot",
			wantAttribut: "a-snapshot",
			wantInput:    900,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			seedSnapshotTiePair(t, d, tt.zTimestamp, tt.aTimestamp)
			got := querySnapshotRankedRows(t, d, UsageFilter{})
			require.Len(t, got, 1)
			assert.Equal(t, tt.wantSession, got[0][0])
			assert.Equal(t, tt.wantAttribut, got[0][1])
			assert.JSONEq(t, fmt.Sprintf(
				`{"input_tokens":%d,"output_tokens":100}`, tt.wantInput),
				got[0][2])
		})
	}
}

// Only duplicated Claude requests pass through the ranking; every other row
// survives untouched, attributed to its own session, including rows outside
// the window that the ranking must not drag back in.
func TestSnapshotRankedDailyUsageRowsRanksOnlyDuplicatedRequests(t *testing.T) {
	d := testDB(t)
	seedSnapshotTiePair(t, d,
		"2026-05-20T10:31:00Z", "2026-05-20T10:30:00Z")
	insertSession(t, d, "solo", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2026-05-20T10:00:00Z")
	})
	insertMessages(t, d,
		Message{
			SessionID: "solo", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-05-20T10:32:00Z", Model: "claude-opus-4-6",
			TokenUsage: jsontext.Value(
				`{"input_tokens":5,"output_tokens":7}`),
			OutputTokens: 7, HasOutputTokens: true,
			ClaudeMessageID: "msg-solo", ClaudeRequestID: "req-solo",
		},
		Message{
			SessionID: "solo", Ordinal: 1, Role: "assistant",
			Timestamp: "2026-05-20T10:33:00Z", Model: "claude-opus-4-6",
			TokenUsage: jsontext.Value(
				`{"input_tokens":6,"output_tokens":8}`),
			OutputTokens: 8, HasOutputTokens: true,
		},
		Message{
			SessionID: "solo", Ordinal: 2, Role: "assistant",
			Timestamp: "2026-05-21T10:00:00Z", Model: "claude-opus-4-6",
			TokenUsage: jsontext.Value(
				`{"input_tokens":1,"output_tokens":200}`),
			OutputTokens: 200, HasOutputTokens: true,
			ClaudeMessageID: "msg-tie", ClaudeRequestID: "req-tie",
		},
	)

	got := querySnapshotRankedRows(t, d, UsageFilter{})
	require.Equal(t, [][3]string{
		{"solo", "solo", `{"input_tokens":5,"output_tokens":7}`},
		{"solo", "solo", `{"input_tokens":6,"output_tokens":8}`},
		{"solo", "a-snapshot", `{"input_tokens":1,"output_tokens":200}`},
	}, got)

	got = querySnapshotRankedRows(t, d, UsageFilter{
		From: "2026-05-20", To: "2026-05-20", Timezone: "UTC"})
	require.Equal(t, [][3]string{
		{"solo", "solo", `{"input_tokens":5,"output_tokens":7}`},
		{"solo", "solo", `{"input_tokens":6,"output_tokens":8}`},
		{"z-snapshot", "a-snapshot", `{"input_tokens":900,"output_tokens":100}`},
	}, got)
}

func TestGetDailyUsage_DedupKeyVariants(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:         "claude-opus-4-6",
		InputPerMTok:         money.MustParseDollars("15.0"),
		OutputPerMTok:        money.MustParseDollars("75.0"),
		CacheCreationPerMTok: money.MustParseDollars("18.75"),
		CacheReadPerMTok:     money.MustParseDollars("1.50"),
	}}), "seed pricing")

	insertSession(t, d, "source-main", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2026-04-10T10:00:00Z")
		s.EndedAt = new("2026-04-10T10:05:00Z")
	})
	insertSession(t, d, "source-fork", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2026-04-10T10:01:00Z")
		s.EndedAt = new("2026-04-10T10:06:00Z")
		s.ParentSessionID = new("source-main")
		s.RelationshipType = "fork"
	})
	insertSession(t, d, "missing-keys", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2026-04-11T10:00:00Z")
		s.EndedAt = new("2026-04-11T10:05:00Z")
	})

	shared := jsontext.Value(`{"input_tokens":100,"output_tokens":500,"cache_creation_input_tokens":1000,"cache_read_input_tokens":50000}`)
	unique := jsontext.Value(`{"input_tokens":20,"output_tokens":80,"cache_creation_input_tokens":200,"cache_read_input_tokens":5000}`)
	missingKeysUsage := jsontext.Value(`{"input_tokens":0,"output_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}`)

	insertMessages(t, d,
		Message{
			SessionID: "source-main", Ordinal: 0,
			Role: "assistant", Timestamp: "2026-04-10T10:02:00Z",
			Model: "claude-opus-4-6", TokenUsage: shared, HasOutputTokens: true,
			ClaudeMessageID: "msg_dup", SourceUUID: "source_dup",
		},
		Message{
			SessionID: "source-fork", Ordinal: 0,
			Role: "assistant", Timestamp: "2026-04-10T10:02:00Z",
			Model: "claude-opus-4-6", TokenUsage: shared, HasOutputTokens: true,
			ClaudeMessageID: "msg_dup", SourceUUID: "source_dup",
		},
		Message{
			SessionID: "source-fork", Ordinal: 1,
			Role: "assistant", Timestamp: "2026-04-10T10:03:00Z",
			Model: "claude-opus-4-6", TokenUsage: unique, HasOutputTokens: true,
			ClaudeMessageID: "msg_uniq", SourceUUID: "source_uniq",
		},
		Message{
			SessionID: "missing-keys", Ordinal: 0,
			Role: "assistant", Timestamp: "2026-04-11T10:02:00Z",
			Model: "claude-opus-4-6", TokenUsage: missingKeysUsage,
			HasOutputTokens: true,
		},
		Message{
			SessionID: "missing-keys", Ordinal: 1,
			Role: "assistant", Timestamp: "2026-04-11T10:02:00Z",
			Model: "claude-opus-4-6", TokenUsage: missingKeysUsage,
			HasOutputTokens: true,
		},
	)

	t.Run("dedupes by source uuid when claude pair incomplete", func(t *testing.T) {
		result, err := d.GetDailyUsage(context.Background(), UsageFilter{
			From: "2026-04-10", To: "2026-04-10", Timezone: "UTC",
		})
		require.NoError(t, err, "GetDailyUsage")
		require.Len(t, result.Daily, 1, "daily entries =")
		day := result.Daily[0]
		assert.Equal(t, 120, day.InputTokens, "input")
		assert.Equal(t, 580, day.OutputTokens, "output")
		assert.Equal(t, 1200, day.CacheCreationTokens, "cache_cr")
		assert.Equal(t, 55000, day.CacheReadTokens, "cache_rd")
	})

	t.Run("missing dedup keys counted every time", func(t *testing.T) {
		result, err := d.GetDailyUsage(context.Background(), UsageFilter{
			From: "2026-04-11", To: "2026-04-11", Timezone: "UTC",
		})
		require.NoError(t, err, "GetDailyUsage")
		require.Len(t, result.Daily, 1,
			"output want 20 (both no-key rows counted): %v", result.Daily)
		assert.Equal(t, 20, result.Daily[0].OutputTokens,
			"output want 20 (both no-key rows counted): %v", result.Daily)
	})
}

func TestGetDailyUsageLongLivedSession(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet-4-6",
		InputPerMTok:  money.MustParseDollars("3.0"),
		OutputPerMTok: money.MustParseDollars("15.0"),
	}}), "upsert pricing")

	// Session started on Apr 1 but has messages on Apr 10.
	requireNoError(t, d.UpsertSession(Session{
		ID: "long-lived", Project: "proj", Machine: "local",
		Agent:     "claude",
		StartedAt: new("2026-04-01T10:00:00Z"),
	}), "upsert session")

	insertMessages(t, d,
		Message{
			SessionID: "long-lived", Ordinal: 0,
			Role: "assistant", Content: "early",
			ContentLength: 5,
			Timestamp:     "2026-04-01T10:00:00Z",
			Model:         "claude-sonnet-4-6",
			TokenUsage: jsontext.Value(
				`{"input_tokens":100,"output_tokens":50}`),
			ContextTokens:    100,
			OutputTokens:     50,
			HasContextTokens: true,
			HasOutputTokens:  true,
		},
		Message{
			SessionID: "long-lived", Ordinal: 1,
			Role: "assistant", Content: "late",
			ContentLength: 4,
			Timestamp:     "2026-04-10T14:00:00Z",
			Model:         "claude-sonnet-4-6",
			TokenUsage: jsontext.Value(
				`{"input_tokens":2000,"output_tokens":500}`),
			ContextTokens:    2000,
			OutputTokens:     500,
			HasContextTokens: true,
			HasOutputTokens:  true,
		},
	)

	// Query Apr 10 only — should include the late message even
	// though the session started on Apr 1.
	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From:     "2026-04-10",
		To:       "2026-04-10",
		Timezone: "UTC",
	})
	requireNoError(t, err, "GetDailyUsage long-lived")

	require.Len(t, result.Daily, 1, "expected 1 day")
	assert.Equal(t, 2000, result.Daily[0].InputTokens, "InputTokens")
}

func TestGetDailyUsageProjectFilter(t *testing.T) {

	d := openDailyUsageFixtureDB(t)
	ctx := context.Background()

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From:    "2024-06-01",
		To:      "2024-06-30",
		Project: "proj-a",
	})
	requireNoError(t, err, "GetDailyUsage project filter")

	require.Len(t, result.Daily, 1, "got")
	day := result.Daily[0]
	assert.Equal(t, 4000, day.InputTokens, "InputTokens")
	assert.Equal(t, 4000, result.Totals.InputTokens, "Totals.InputTokens")
}

func TestGetDailyUsageModelFilter(t *testing.T) {

	d := openDailyUsageFixtureDB(t)
	ctx := context.Background()

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From:  "2024-06-01",
		To:    "2024-06-30",
		Model: "gpt-5",
	})
	requireNoError(t, err, "GetDailyUsage model filter")

	require.Len(t, result.Daily, 1, "got")
	day := result.Daily[0]
	assert.Equal(t, 4000, day.InputTokens, "InputTokens")
	assert.Equal(t, []string{"gpt-5"}, day.ModelsUsed, "ModelsUsed")
}

func TestGetDailyUsageProjectBreakdowns(t *testing.T) {

	d := openDailyUsageFixtureDB(t)
	ctx := context.Background()

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From:       "2024-06-01",
		To:         "2024-06-30",
		Breakdowns: true,
	})
	requireNoError(t, err, "GetDailyUsage project breakdowns")

	require.Len(t, result.Daily, 1, "got")
	day := result.Daily[0]
	require.Len(t, day.ProjectBreakdowns, 2, "ProjectBreakdowns len")

	projMap := make(map[string]ProjectBreakdown)
	var projCostSum money.Money
	for _, pb := range day.ProjectBreakdowns {
		projMap[pb.Project] = pb
		projCostSum = money.MustAdd(projCostSum, pb.Cost)
	}
	for _, name := range []string{"proj-a", "proj-b"} {
		pb, ok := projMap[name]
		if !assert.Truef(t, ok,
			"missing ProjectBreakdown for %s", name) {
			continue
		}
		assert.Equal(t, 4000, pb.InputTokens,
			"%s InputTokens", name)
	}
	assert.Equal(t, day.TotalCost, projCostSum,
		"sum(ProjectBreakdowns.Cost) want TotalCost")
}

func TestDailyUsageEntryBreakdownSlicesMarshalAsEmptyArrays(t *testing.T) {
	data, err := json.Marshal(DailyUsageEntry{
		Date:       "2026-07-03",
		ModelsUsed: []string{},
	})
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Equal(t, []any{}, got["modelBreakdowns"])
	assert.Equal(t, []any{}, got["projectBreakdowns"])
	assert.Equal(t, []any{}, got["agentBreakdowns"])
	assert.Equal(t, []any{}, got["machineBreakdowns"])
}

func TestGetDailyUsageMachineBreakdowns(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "model-a",
		InputPerMTok:  money.MustParseDollars("1"),
		OutputPerMTok: money.MustParseDollars("5"),
	}}))

	type machineUsage struct {
		name         string
		inputTokens  int
		outputTokens int
	}
	fixtures := []machineUsage{
		{name: "host-a", inputTokens: 2000, outputTokens: 200},
		{name: "host-b", inputTokens: 1000, outputTokens: 100},
	}
	for i, fixture := range fixtures {
		sessionID := fmt.Sprintf("machine-breakdown-%d", i)
		insertSession(t, d, sessionID, "project-a", func(s *Session) {
			s.Machine = fixture.name
			s.Agent = "claude"
			s.StartedAt = Ptr("2026-07-15T10:00:00Z")
		})
		insertMessages(t, d, Message{
			SessionID: sessionID,
			Ordinal:   0,
			Role:      "assistant",
			Timestamp: "2026-07-15T10:30:00Z",
			Model:     "model-a",
			TokenUsage: jsontext.Value(fmt.Sprintf(
				`{"input_tokens":%d,"output_tokens":%d}`,
				fixture.inputTokens, fixture.outputTokens,
			)),
		})
	}

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From:       "2026-07-15",
		To:         "2026-07-15",
		Timezone:   "UTC",
		Breakdowns: true,
	})
	require.NoError(t, err)
	require.Len(t, result.Daily, 1)
	day := result.Daily[0]
	require.Len(t, day.MachineBreakdowns, 2)
	assert.Equal(t, "host-a", day.MachineBreakdowns[0].MachineName)
	assert.Equal(t, 2000, day.MachineBreakdowns[0].InputTokens)
	assert.Equal(t, 200, day.MachineBreakdowns[0].OutputTokens)
	assert.Equal(t, money.MustParseDollars("0.003"), day.MachineBreakdowns[0].Cost)
	assert.Equal(t, "host-b", day.MachineBreakdowns[1].MachineName)
	assert.Equal(t, 1000, day.MachineBreakdowns[1].InputTokens)
	assert.Equal(t, 100, day.MachineBreakdowns[1].OutputTokens)
	assert.Equal(t, money.MustParseDollars("0.0015"), day.MachineBreakdowns[1].Cost)
	assert.Equal(t, day.TotalCost,
		money.MustAdd(day.MachineBreakdowns[0].Cost, day.MachineBreakdowns[1].Cost))

	fastResult, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2026-07-15", To: "2026-07-15", Timezone: "UTC",
	})
	require.NoError(t, err)
	require.Len(t, fastResult.Daily, 1)
	assert.Empty(t, fastResult.Daily[0].MachineBreakdowns)
}

func TestGetDailyUsageAgentBreakdowns(t *testing.T) {

	d := openDailyUsageFixtureDB(t)
	ctx := context.Background()

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From:       "2024-06-01",
		To:         "2024-06-30",
		Breakdowns: true,
	})
	requireNoError(t, err, "GetDailyUsage agent breakdowns")

	require.Len(t, result.Daily, 1, "got")
	day := result.Daily[0]
	require.Len(t, day.AgentBreakdowns, 2, "AgentBreakdowns len")

	agentMap := make(map[string]AgentBreakdown)
	var agentCostSum money.Money
	for _, ab := range day.AgentBreakdowns {
		agentMap[ab.Agent] = ab
		agentCostSum = money.MustAdd(agentCostSum, ab.Cost)
	}
	for _, name := range []string{"claude", "codex"} {
		ab, ok := agentMap[name]
		if !assert.Truef(t, ok,
			"missing AgentBreakdown for %s", name) {
			continue
		}
		assert.Equal(t, 4000, ab.InputTokens,
			"%s InputTokens", name)
	}
	assert.Equal(t, day.TotalCost, agentCostSum,
		"sum(AgentBreakdowns.Cost) want TotalCost")
}

func TestGetDailyUsageBreakdownInvariant(t *testing.T) {

	d := openDailyUsageFixtureDB(t)
	ctx := context.Background()

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From:       "2024-06-01",
		To:         "2024-06-30",
		Breakdowns: true,
	})
	requireNoError(t, err, "GetDailyUsage breakdown invariant")

	require.Len(t, result.Daily, 1, "got")
	day := result.Daily[0]

	var modelCostSum money.Money
	for _, mb := range day.ModelBreakdowns {
		modelCostSum = money.MustAdd(modelCostSum, mb.Cost)
	}
	var projectCostSum money.Money
	for _, pb := range day.ProjectBreakdowns {
		projectCostSum = money.MustAdd(projectCostSum, pb.Cost)
	}
	var agentCostSum money.Money
	for _, ab := range day.AgentBreakdowns {
		agentCostSum = money.MustAdd(agentCostSum, ab.Cost)
	}

	assert.Equal(t, day.TotalCost, modelCostSum,
		"sum(ModelBreakdowns.Cost) want TotalCost")
	assert.Equal(t, day.TotalCost, projectCostSum,
		"sum(ProjectBreakdowns.Cost) want TotalCost")
	assert.Equal(t, day.TotalCost, agentCostSum,
		"sum(AgentBreakdowns.Cost) want TotalCost")
	assert.Equal(t, projectCostSum, modelCostSum,
		"model cost sum != project cost sum")
	assert.Equal(t, agentCostSum, modelCostSum,
		"model cost sum != agent cost sum")
}

// BenchmarkGetDailyUsage measures the hot-path scan over a realistic
// synthetic dataset. The baseline number (captured against the commit
// that introduces this benchmark) is the non-regression budget for all
// subsequent changes to GetDailyUsage: new code must land within +10%.
//
// See docs/specs/2026-04-12-token-usage-ui-design.md for the full
// non-destructive benchmark procedure.
func TestGetTopSessionsByCost(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:         "claude-sonnet",
		InputPerMTok:         money.MustParseDollars("3.0"),
		OutputPerMTok:        money.MustParseDollars("15.0"),
		CacheCreationPerMTok: money.MustParseDollars("3.75"),
		CacheReadPerMTok:     money.MustParseDollars("0.30"),
	}}), "UpsertModelPricing")

	// Expensive session
	insertSession(t, d, "sBig", "proj-a", func(s *Session) {
		s.Agent = "claude"
		s.SessionName = new("Big Session")
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "sBig", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:30:00Z",
		Model: "claude-sonnet",
		TokenUsage: jsontext.Value(
			`{"input_tokens":5000,"output_tokens":2000,` +
				`"cache_creation_input_tokens":1000,` +
				`"cache_read_input_tokens":3000}`),
	})

	// Cheap session
	insertSession(t, d, "sSmall", "proj-b", func(s *Session) {
		s.Agent = "codex"
		s.SessionName = new("Small Session")
		s.StartedAt = new("2024-06-15T11:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "sSmall", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T11:30:00Z",
		Model: "claude-sonnet",
		TokenUsage: jsontext.Value(
			`{"input_tokens":100,"output_tokens":50,` +
				`"cache_creation_input_tokens":10,` +
				`"cache_read_input_tokens":20}`),
	})

	top, err := d.GetTopSessionsByCost(ctx, UsageFilter{
		From: "2024-06-01",
		To:   "2024-06-30",
	}, 20)
	requireNoError(t, err, "GetTopSessionsByCost")

	require.Len(t, top, 2, "len")

	// Ordered cost desc — sBig first
	assert.Equal(t, "sBig", top[0].SessionID, "top[0].SessionID")
	assert.Equal(t, "Big Session", top[0].DisplayName, "top[0].DisplayName")
	assert.Equal(t, "proj-a", top[0].Project, "top[0].Project")
	assert.Equal(t, "claude", top[0].Agent, "top[0].Agent")
	// TotalTokens = 5000 + 2000 + 1000 + 3000 = 11000
	assert.Equal(t, 11000, top[0].TotalTokens, "top[0].TotalTokens")
	assert.Positive(t, top[0].Cost.Microdollars, "top[0].Cost want > 0")

	assert.Equal(t, "sSmall", top[1].SessionID, "top[1].SessionID")
	assert.Greater(t, top[0].Cost.Microdollars, top[1].Cost.Microdollars,
		"top[0].Cost should be > top[1].Cost")
}

// TestGetTopSessionsByTokens ranks by total tokens and applies limit
// against the token order, not a re-sort of a cost-truncated top-N.
// A high-token/low-cost session must outrank a low-token/high-cost one.
func TestGetTopSessionsByTokens(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{
		{
			ModelPattern:  "expensive-model",
			InputPerMTok:  money.MustParseDollars("100.0"),
			OutputPerMTok: money.MustParseDollars("100.0"),
		},
		{
			ModelPattern:  "cheap-model",
			InputPerMTok:  money.MustParseDollars("0.01"),
			OutputPerMTok: money.MustParseDollars("0.01"),
		},
	}), "UpsertModelPricing")

	// High cost, low tokens.
	insertSession(t, d, "sCostly", "proj-a", func(s *Session) {
		s.Agent = "claude"
		s.SessionName = new("Costly")
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "sCostly", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:30:00Z",
		Model: "expensive-model",
		TokenUsage: jsontext.Value(
			`{"input_tokens":100,"output_tokens":100}`),
	})

	// Low cost, high tokens.
	insertSession(t, d, "sTokeny", "proj-b", func(s *Session) {
		s.Agent = "codex"
		s.SessionName = new("Tokeny")
		s.StartedAt = new("2024-06-15T11:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "sTokeny", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T11:30:00Z",
		Model: "cheap-model",
		TokenUsage: jsontext.Value(
			`{"input_tokens":50000,"output_tokens":50000}`),
	})

	filter := UsageFilter{From: "2024-06-01", To: "2024-06-30"}

	byCost, err := d.GetTopSessionsByCost(ctx, filter, 1)
	requireNoError(t, err, "by cost")
	require.Len(t, byCost, 1, "by cost limit")
	assert.Equal(t, "sCostly", byCost[0].SessionID, "cost rank")

	filter.TopSessionsSort = TopSessionsSortTokens
	byTokens, err := d.GetTopSessionsByCost(ctx, filter, 1)
	requireNoError(t, err, "by tokens")
	require.Len(t, byTokens, 1, "by tokens limit")
	assert.Equal(t, "sTokeny", byTokens[0].SessionID, "token rank")
	assert.Equal(t, 100000, byTokens[0].TotalTokens, "token total")
}

func TestSortAndLimitTopSessions(t *testing.T) {
	in := []TopSessionEntry{
		{SessionID: "a", InputTokens: 10, TotalTokens: 10, Cost: money.MustParseDollars("5")},
		{SessionID: "b", InputTokens: 100, TotalTokens: 100, Cost: money.MustParseDollars("1")},
		{SessionID: "c", InputTokens: 50, TotalTokens: 50, Cost: money.MustParseDollars("3")},
	}
	got := SortAndLimitTopSessions(
		in, 2, TopSessionsSortTokens, UsageTokenTypesAll,
	)
	require.Len(t, got, 2)
	assert.Equal(t, "b", got[0].SessionID)
	assert.Equal(t, "c", got[1].SessionID)

	gotCost := SortAndLimitTopSessions(
		in, 2, TopSessionsSortCost, UsageTokenTypesAll,
	)
	require.Len(t, gotCost, 2)
	assert.Equal(t, "a", gotCost[0].SessionID)
	assert.Equal(t, "c", gotCost[1].SessionID)
}

func TestGetTopSessionsByCost_DisplayNameFallback(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:         "claude-sonnet",
		InputPerMTok:         money.MustParseDollars("3.0"),
		OutputPerMTok:        money.MustParseDollars("15.0"),
		CacheCreationPerMTok: money.MustParseDollars("3.75"),
		CacheReadPerMTok:     money.MustParseDollars("0.30"),
	}}), "UpsertModelPricing")

	tokenJSON := `{"input_tokens":100,"output_tokens":50,` +
		`"cache_creation_input_tokens":0,"cache_read_input_tokens":0}`

	// Session with session_name set — should use session_name via COALESCE.
	insertSession(t, d, "s-dn", "proj-a", func(s *Session) {
		s.Agent = "claude"
		s.SessionName = new("My Custom Name")
		s.FirstMessage = new("some first message")
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "s-dn", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:01:00Z",
		Model:      "claude-sonnet",
		TokenUsage: jsontext.Value(tokenJSON),
	})

	// Session with no display_name — should fall back to first_message.
	insertSession(t, d, "s-fm", "proj-a", func(s *Session) {
		s.Agent = "claude"
		s.FirstMessage = new("fix the login bug")
		s.StartedAt = new("2024-06-15T11:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "s-fm", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T11:01:00Z",
		Model:      "claude-sonnet",
		TokenUsage: jsontext.Value(tokenJSON),
	})

	// Session with no display_name and no first_message — should
	// fall back to project.
	insertSession(t, d, "s-proj", "my-project", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T12:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "s-proj", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T12:01:00Z",
		Model:      "claude-sonnet",
		TokenUsage: jsontext.Value(tokenJSON),
	})

	// Session with no display_name, no first_message, and empty
	// project — should fall back to session ID.
	insertSession(t, d, "s-id", "", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T13:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "s-id", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T13:01:00Z",
		Model:      "claude-sonnet",
		TokenUsage: jsontext.Value(tokenJSON),
	})

	top, err := d.GetTopSessionsByCost(ctx, UsageFilter{
		From: "2024-06-01",
		To:   "2024-06-30",
	}, 20)
	requireNoError(t, err, "GetTopSessionsByCost fallback")

	require.Len(t, top, 4, "len")

	// Build a map for easy lookup (order is by cost, all equal
	// here so secondary sort is by session ID).
	byID := make(map[string]TopSessionEntry)
	for _, e := range top {
		byID[e.SessionID] = e
	}

	assert.Equal(t, "My Custom Name", byID["s-dn"].DisplayName,
		"s-dn DisplayName")
	assert.Equal(t, "fix the login bug", byID["s-fm"].DisplayName,
		"s-fm DisplayName")
	assert.Equal(t, "my-project", byID["s-proj"].DisplayName,
		"s-proj DisplayName")
	assert.Equal(t, "s-id", byID["s-id"].DisplayName,
		"s-id DisplayName")
}

// TestGetTopSessionsByCost_DedupesByClaudeMessageAndRequestID
// mirrors TestGetDailyUsage_DedupesByClaudeMessageAndRequestID
// for the top-sessions query: a parent session and a forked
// session that both replay the same Claude message should only
// count that message once in the per-session totals. The
// earliest-timestamp session wins the credit.
func TestGetTopSessionsByCost_DedupesByClaudeMessageAndRequestID(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:         "claude-sonnet",
		InputPerMTok:         money.MustParseDollars("3.0"),
		OutputPerMTok:        money.MustParseDollars("15.0"),
		CacheCreationPerMTok: money.MustParseDollars("3.75"),
		CacheReadPerMTok:     money.MustParseDollars("0.30"),
	}}), "UpsertModelPricing")

	// Parent session starts first.
	insertSession(t, d, "s-parent", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	// Forked session starts a minute later.
	insertSession(t, d, "s-fork", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:01:00Z")
		s.ParentSessionID = new("s-parent")
		s.RelationshipType = "fork"
	})

	shared := jsontext.Value(
		`{"input_tokens":1000,"output_tokens":500,` +
			`"cache_creation_input_tokens":200,` +
			`"cache_read_input_tokens":3000}`)
	unique := jsontext.Value(
		`{"input_tokens":10,"output_tokens":20,` +
			`"cache_creation_input_tokens":0,` +
			`"cache_read_input_tokens":0}`)

	// The shared message exists on both sessions with the same
	// Claude IDs; the parent's timestamp is earlier so it should
	// win the dedup.
	insertMessages(t, d, Message{
		SessionID: "s-parent", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:02:00Z",
		Model: "claude-sonnet", TokenUsage: shared,
		ClaudeMessageID: "msg_dup", ClaudeRequestID: "req_dup",
	})
	insertMessages(t, d, Message{
		SessionID: "s-fork", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:03:00Z",
		Model: "claude-sonnet", TokenUsage: shared,
		ClaudeMessageID: "msg_dup", ClaudeRequestID: "req_dup",
	})
	// Plus a unique fork-only message so the fork still appears.
	insertMessages(t, d, Message{
		SessionID: "s-fork", Ordinal: 1,
		Role: "assistant", Timestamp: "2024-06-15T10:04:00Z",
		Model: "claude-sonnet", TokenUsage: unique,
		ClaudeMessageID: "msg_uniq", ClaudeRequestID: "req_uniq",
	})

	top, err := d.GetTopSessionsByCost(ctx, UsageFilter{
		From: "2024-06-15", To: "2024-06-15", Timezone: "UTC",
	}, 20)
	requireNoError(t, err, "GetTopSessionsByCost")

	require.Len(t, top, 2, "len")

	byID := map[string]TopSessionEntry{}
	for _, e := range top {
		byID[e.SessionID] = e
	}

	parent, ok := byID["s-parent"]
	require.True(t, ok, "s-parent missing from top sessions")
	// Parent owns shared: 1000+500+200+3000 = 4700 tokens.
	assert.Equal(t, 4700, parent.TotalTokens, "parent.TotalTokens")

	fork, ok := byID["s-fork"]
	require.True(t, ok, "s-fork missing from top sessions")
	// Fork should only own the unique message: 10+20 = 30
	// tokens. If the dedup were missing, the shared row would
	// be counted again and this would jump to 4730.
	assert.Equal(t, 30, fork.TotalTokens,
		"fork.TotalTokens want 30 "+
			"(shared message should be deduped)")

	// Total across both entries must equal the undeduped
	// message sum: parent 4700 + fork 30 = 4730.
	total := parent.TotalTokens + fork.TotalTokens
	assert.Equal(t, 4730, total, "sum of per-session totals")
}

func TestGetTopSessionsByCost_DedupesBySourceUUIDWhenClaudePairIncomplete(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:         "claude-sonnet",
		InputPerMTok:         money.MustParseDollars("3.0"),
		OutputPerMTok:        money.MustParseDollars("15.0"),
		CacheCreationPerMTok: money.MustParseDollars("3.75"),
		CacheReadPerMTok:     money.MustParseDollars("0.30"),
	}}), "UpsertModelPricing")

	insertSession(t, d, "s-parent", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertSession(t, d, "s-fork", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:01:00Z")
		s.ParentSessionID = new("s-parent")
		s.RelationshipType = "fork"
	})

	shared := jsontext.Value(
		`{"input_tokens":1000,"output_tokens":500,` +
			`"cache_creation_input_tokens":200,` +
			`"cache_read_input_tokens":3000}`)
	unique := jsontext.Value(
		`{"input_tokens":10,"output_tokens":20,` +
			`"cache_creation_input_tokens":0,` +
			`"cache_read_input_tokens":0}`)

	insertMessages(t, d, Message{
		SessionID: "s-parent", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:02:00Z",
		Model: "claude-sonnet", TokenUsage: shared,
		ClaudeMessageID: "msg_dup", SourceUUID: "source_dup",
	})
	insertMessages(t, d, Message{
		SessionID: "s-fork", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:03:00Z",
		Model: "claude-sonnet", TokenUsage: shared,
		ClaudeMessageID: "msg_dup", SourceUUID: "source_dup",
	})
	insertMessages(t, d, Message{
		SessionID: "s-fork", Ordinal: 1,
		Role: "assistant", Timestamp: "2024-06-15T10:04:00Z",
		Model: "claude-sonnet", TokenUsage: unique,
		ClaudeMessageID: "msg_uniq", SourceUUID: "source_uniq",
	})

	top, err := d.GetTopSessionsByCost(ctx, UsageFilter{
		From: "2024-06-15", To: "2024-06-15", Timezone: "UTC",
	}, 20)
	requireNoError(t, err, "GetTopSessionsByCost")

	require.Len(t, top, 2, "len")
	byID := map[string]TopSessionEntry{}
	for _, e := range top {
		byID[e.SessionID] = e
	}

	parent, ok := byID["s-parent"]
	require.True(t, ok, "s-parent missing from top sessions")
	assert.Equal(t, 4700, parent.TotalTokens, "parent.TotalTokens")

	fork, ok := byID["s-fork"]
	require.True(t, ok, "s-fork missing from top sessions")
	assert.Equal(t, 30, fork.TotalTokens, "fork.TotalTokens want 30")
}

func TestGetTopSessionsByCostLimit(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet",
		InputPerMTok:  money.MustParseDollars("3.0"),
		OutputPerMTok: money.MustParseDollars("15.0"),
	}}), "UpsertModelPricing")

	for i := range 5 {
		sid := "sess-" + strconv.Itoa(i)
		insertSession(t, d, sid, "proj", func(s *Session) {
			s.Agent = "claude"
			s.StartedAt = new("2024-06-15T10:00:00Z")
		})
		insertMessages(t, d, Message{
			SessionID: sid, Ordinal: 0,
			Role: "assistant", Timestamp: "2024-06-15T10:30:00Z",
			Model: "claude-sonnet",
			TokenUsage: jsontext.Value(
				`{"input_tokens":1000,"output_tokens":500}`),
		})
	}

	top, err := d.GetTopSessionsByCost(ctx, UsageFilter{
		From: "2024-06-01",
		To:   "2024-06-30",
	}, 3)
	requireNoError(t, err, "GetTopSessionsByCost limit")

	require.Len(t, top, 3, "len")
}

func TestGetUsageSessionCounts(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// s1: proj-a / claude — TWO messages across TWO days
	insertSession(t, d, "s1", "proj-a", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d,
		Message{
			SessionID: "s1", Ordinal: 0,
			Role: "assistant", Timestamp: "2024-06-15T10:30:00Z",
			Model: "claude-sonnet",
			TokenUsage: jsontext.Value(
				`{"input_tokens":100,"output_tokens":50}`),
		},
		Message{
			SessionID: "s1", Ordinal: 1,
			Role: "assistant", Timestamp: "2024-06-16T10:30:00Z",
			Model: "claude-sonnet",
			TokenUsage: jsontext.Value(
				`{"input_tokens":200,"output_tokens":100}`),
		},
	)

	// s2: proj-a / codex
	insertSession(t, d, "s2", "proj-a", func(s *Session) {
		s.Agent = "codex"
		s.StartedAt = new("2024-06-15T11:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "s2", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T11:30:00Z",
		Model: "claude-sonnet",
		TokenUsage: jsontext.Value(
			`{"input_tokens":100,"output_tokens":50}`),
	})

	// s3: proj-b / claude
	insertSession(t, d, "s3", "proj-b", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T12:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "s3", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T12:30:00Z",
		Model: "claude-sonnet",
		TokenUsage: jsontext.Value(
			`{"input_tokens":100,"output_tokens":50}`),
	})

	counts, err := d.GetUsageSessionCounts(ctx, UsageFilter{
		From: "2024-06-01",
		To:   "2024-06-30",
	})
	requireNoError(t, err, "GetUsageSessionCounts")

	assert.Equal(t, 3, counts.Total, "Total")
	assert.Equal(t, 2, counts.ByProject["proj-a"], "ByProject[proj-a]")
	assert.Equal(t, 1, counts.ByProject["proj-b"], "ByProject[proj-b]")
	assert.Equal(t, 2, counts.ByAgent["claude"], "ByAgent[claude]")
	assert.Equal(t, 1, counts.ByAgent["codex"], "ByAgent[codex]")

	daily, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2024-06-01",
		To:   "2024-06-30",
	})
	requireNoError(t, err, "GetDailyUsage")
	assert.Equal(t, counts.Total, daily.SessionCounts.Total)
	assert.Equal(t, counts.ByAgent, daily.SessionCounts.ByAgent)
	projectCounts := make(map[string]int, len(daily.Projects))
	for key, project := range daily.Projects {
		projectCounts[project.DisplayLabel] = daily.SessionCounts.ByProject[key]
		assert.NotContains(t, key, project.DisplayLabel)
	}
	assert.Equal(t, counts.ByProject, projectCounts)

	dailyNoCounts, err := d.GetDailyUsage(ctx, UsageFilter{
		From:              "2024-06-01",
		To:                "2024-06-30",
		SkipSessionCounts: true,
	})
	requireNoError(t, err, "GetDailyUsage skip counts")
	assert.Equal(t, daily.Daily, dailyNoCounts.Daily)
	assert.Equal(t, daily.Totals, dailyNoCounts.Totals)
	assert.Zero(t, dailyNoCounts.SessionCounts.Total)
	assert.Nil(t, dailyNoCounts.SessionCounts.ByProject)
	assert.Nil(t, dailyNoCounts.SessionCounts.ByAgent)
}

func TestGetUsageSessionCountsFiltersAfterCrossSessionSnapshotSelection(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "count-parent", "parent-project", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2026-05-20T10:00:00Z")
	})
	insertSession(t, d, "count-child", "child-project", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2026-05-20T10:01:00Z")
	})
	insertMessages(t, d,
		Message{
			SessionID: "count-parent", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-05-20T10:00:00Z", Model: "partial-model",
			TokenUsage: jsontext.Value(
				`{"input_tokens":10,"output_tokens":5}`),
			ClaudeMessageID: "count-message", ClaudeRequestID: "count-request",
		},
		Message{
			SessionID: "count-child", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-05-20T10:01:00Z", Model: "complete-model",
			TokenUsage: jsontext.Value(
				`{"input_tokens":1000,"output_tokens":631}`),
			ClaudeMessageID: "count-message", ClaudeRequestID: "count-request",
		},
	)

	partialCounts, err := d.GetUsageSessionCounts(ctx, UsageFilter{
		From: "2026-05-20", To: "2026-05-20", Timezone: "UTC",
		Model: "partial-model",
	})
	require.NoError(t, err)
	assert.Zero(t, partialCounts.Total,
		"the discarded partial model must not count a session")

	completeParentCounts, err := d.GetUsageSessionCounts(ctx, UsageFilter{
		From: "2026-05-20", To: "2026-05-20", Timezone: "UTC",
		Model: "complete-model", ProjectLabels: []string{"parent-project"},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, completeParentCounts.Total)
	assert.Equal(t, 1, completeParentCounts.ByProject["parent-project"])
	assert.NotContains(t, completeParentCounts.ByProject, "child-project")
}

func TestGetUsageMatchingSessionCount(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "copilot-empty", "proj-a", func(s *Session) {
		s.Agent = "copilot"
		s.StartedAt = new("2024-06-15T10:00:00Z")
		s.EndedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "copilot-empty", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:00:00Z",
		Model: "gpt-5.3-codex",
	})

	insertSession(t, d, "claude-usage", "proj-a", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T11:00:00Z")
		s.EndedAt = new("2024-06-15T11:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "claude-usage", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T11:00:00Z",
		Model: "claude-sonnet",
		TokenUsage: jsontext.Value(
			`{"input_tokens":100,"output_tokens":50}`),
	})

	tests := []struct {
		name   string
		filter UsageFilter
		want   int
	}{
		{
			name: "counts copilot sessions without usage rows",
			filter: UsageFilter{
				From: "2024-06-15", To: "2024-06-15",
				Timezone: "UTC", Agent: "copilot",
			},
			want: 1,
		},
		{
			name: "respects model filters from session messages",
			filter: UsageFilter{
				From: "2024-06-15", To: "2024-06-15",
				Timezone: "UTC", Agent: "copilot", Model: "gpt-5.3-codex",
			},
			want: 1,
		},
		{
			name: "excludes sessions when model is excluded",
			filter: UsageFilter{
				From: "2024-06-15", To: "2024-06-15",
				Timezone: "UTC", Agent: "copilot", ExcludeModel: "gpt-5.3-codex",
			},
			want: 0,
		},
		{
			name: "respects date range",
			filter: UsageFilter{
				From: "2024-06-16", To: "2024-06-16",
				Timezone: "UTC", Agent: "copilot",
			},
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := d.GetUsageMatchingSessionCount(ctx, tt.filter)
			requireNoError(t, err, "GetUsageMatchingSessionCount")
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestGetUsageMatchingSessionCount_UsesMessageTimestampNotSessionActivity
// guards against regressing to session-activity bounding: a Copilot
// session whose started_at/ended_at fall outside the requested window but
// whose message is timestamped inside it must still be counted, because
// GetDailyUsage and GetUsageSessionCounts already bound on message/event
// timestamps, not session activity.
func TestGetUsageMatchingSessionCount_UsesMessageTimestampNotSessionActivity(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "copilot-late-message", "proj-a", func(s *Session) {
		s.Agent = "copilot"
		s.StartedAt = new("2026-02-08T10:00:00Z")
		s.EndedAt = new("2026-02-08T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "copilot-late-message", Ordinal: 0,
		Role: "assistant", Timestamp: "2026-02-10T12:00:00Z",
		Model:      "gpt-5.3-codex",
		TokenUsage: nil,
	})

	insertSession(t, d, "copilot-out-of-range", "proj-a", func(s *Session) {
		s.Agent = "copilot"
		s.StartedAt = new("2026-02-08T10:00:00Z")
		s.EndedAt = new("2026-02-08T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "copilot-out-of-range", Ordinal: 0,
		Role: "assistant", Timestamp: "2026-02-08T10:00:00Z",
		Model:      "gpt-5.3-codex",
		TokenUsage: nil,
	})

	filter := UsageFilter{
		From: "2026-02-10", To: "2026-02-10",
		Timezone: "UTC", Agent: "copilot",
	}

	got, err := d.GetUsageMatchingSessionCount(ctx, filter)
	requireNoError(t, err, "GetUsageMatchingSessionCount")
	assert.Equal(t, 1, got)
}

// TestGetUsageMatchingSessionCount_ModelFilterAppliesToBoundedRow guards
// against the model/exclude-model predicate being applied session-wide
// instead of to the in-range message/event row: a session with an
// out-of-range message on the filtered model but an in-range message on a
// different model must not match a Model filter for the out-of-range
// model, and must not be excluded by an ExcludeModel filter for the
// out-of-range model either.
func TestGetUsageMatchingSessionCount_ModelFilterAppliesToBoundedRow(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "copilot-mixed-model", "proj-a", func(s *Session) {
		s.Agent = "copilot"
		s.StartedAt = new("2026-02-08T10:00:00Z")
		s.EndedAt = new("2026-02-10T12:00:00Z")
	})
	insertMessages(t, d,
		Message{
			SessionID: "copilot-mixed-model", Ordinal: 0,
			Role: "assistant", Timestamp: "2026-02-08T10:00:00Z",
			Model:      "gpt-5.3-codex",
			TokenUsage: nil,
		},
		Message{
			SessionID: "copilot-mixed-model", Ordinal: 1,
			Role: "assistant", Timestamp: "2026-02-10T12:00:00Z",
			Model:      "claude-sonnet",
			TokenUsage: nil,
		},
	)

	inRangeFilter := UsageFilter{
		From: "2026-02-10", To: "2026-02-10",
		Timezone: "UTC", Agent: "copilot",
	}

	got, err := d.GetUsageMatchingSessionCount(ctx, UsageFilter{
		From: inRangeFilter.From, To: inRangeFilter.To,
		Timezone: inRangeFilter.Timezone, Agent: inRangeFilter.Agent,
		Model: "gpt-5.3-codex",
	})
	requireNoError(t, err, "GetUsageMatchingSessionCount with Model")
	assert.Equal(t, 0, got,
		"out-of-range message's model must not match the bounded window")

	got, err = d.GetUsageMatchingSessionCount(ctx, UsageFilter{
		From: inRangeFilter.From, To: inRangeFilter.To,
		Timezone: inRangeFilter.Timezone, Agent: inRangeFilter.Agent,
		ExcludeModel: "gpt-5.3-codex",
	})
	requireNoError(t, err, "GetUsageMatchingSessionCount with ExcludeModel")
	assert.Equal(t, 1, got,
		"in-range message's model is not excluded, so the session must still count")
}

// TestGetUsageMatchingSessionCount_CountsAssistantMessageWithNoModel
// guards against gating matching-session eligibility on m.model != ”:
// some Copilot assistant messages parse before a model name is known, so
// an assistant message with an empty Model must still count toward the
// matching-session total when no Model/ExcludeModel filter narrows it.
func TestGetUsageMatchingSessionCount_CountsAssistantMessageWithNoModel(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "copilot-no-model", "proj-a", func(s *Session) {
		s.Agent = "copilot"
		s.StartedAt = new("2026-02-10T10:00:00Z")
		s.EndedAt = new("2026-02-10T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "copilot-no-model", Ordinal: 0,
		Role: "assistant", Timestamp: "2026-02-10T10:00:00Z",
		Model:      "",
		TokenUsage: nil,
	})

	got, err := d.GetUsageMatchingSessionCount(ctx, UsageFilter{
		From: "2026-02-10", To: "2026-02-10",
		Timezone: "UTC", Agent: "copilot",
	})
	requireNoError(t, err, "GetUsageMatchingSessionCount")
	assert.Equal(t, 1, got,
		"assistant message with no model must still count without a model filter")
}

// TestGetUsageMatchingSessionCount_UnboundedMatchesBoundedSemantics guards
// against the unbounded (no From/To) branch drifting from the bounded
// branch: both must require an assistant, non-synthetic message (or a
// usage_events row with a model), both must admit empty-model assistant
// messages, and Model/ExcludeModel must narrow the same rows.
func TestGetUsageMatchingSessionCount_UnboundedMatchesBoundedSemantics(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "copilot-user-only", "proj-a", func(s *Session) {
		s.Agent = "copilot"
		s.StartedAt = new("2026-03-01T10:00:00Z")
		s.EndedAt = new("2026-03-01T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "copilot-user-only", Ordinal: 0,
		Role: "user", Timestamp: "2026-03-01T10:00:00Z",
	})

	insertSession(t, d, "copilot-synthetic-only", "proj-a", func(s *Session) {
		s.Agent = "copilot"
		s.StartedAt = new("2026-03-01T11:00:00Z")
		s.EndedAt = new("2026-03-01T11:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "copilot-synthetic-only", Ordinal: 0,
		Role: "assistant", Timestamp: "2026-03-01T11:00:00Z",
		Model: "<synthetic>",
	})

	insertSession(t, d, "copilot-no-model-msg", "proj-a", func(s *Session) {
		s.Agent = "copilot"
		s.StartedAt = new("2026-03-01T12:00:00Z")
		s.EndedAt = new("2026-03-01T12:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "copilot-no-model-msg", Ordinal: 0,
		Role: "assistant", Timestamp: "2026-03-01T12:00:00Z",
		Model: "",
	})

	insertSession(t, d, "copilot-no-messages", "proj-a", func(s *Session) {
		s.Agent = "copilot"
		s.StartedAt = new("2026-03-01T13:00:00Z")
		s.EndedAt = new("2026-03-01T13:00:00Z")
	})

	tests := []struct {
		name   string
		filter UsageFilter
		want   int
	}{
		{
			name:   "unbounded requires assistant or event activity",
			filter: UsageFilter{Timezone: "UTC", Agent: "copilot"},
			// Only copilot-no-model-msg has a qualifying assistant
			// message; user-only, synthetic-only, and message-less
			// sessions must not count.
			want: 1,
		},
		{
			name: "unbounded exclude-model keeps empty-model assistant messages",
			filter: UsageFilter{
				Timezone: "UTC", Agent: "copilot",
				ExcludeModel: "gpt-5.3-codex",
			},
			want: 1,
		},
		{
			name: "unbounded model filter narrows to matching rows",
			filter: UsageFilter{
				Timezone: "UTC", Agent: "copilot", Model: "gpt-5.3-codex",
			},
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := d.GetUsageMatchingSessionCount(ctx, tt.filter)
			requireNoError(t, err, "GetUsageMatchingSessionCount")
			assert.Equal(t, tt.want, got)

			bounded := tt.filter
			bounded.From = "2026-03-01"
			bounded.To = "2026-03-01"
			boundedGot, err := d.GetUsageMatchingSessionCount(ctx, bounded)
			requireNoError(t, err, "GetUsageMatchingSessionCount bounded")
			assert.Equal(t, got, boundedGot,
				"bounded and unbounded requests must match the same sessions")
		})
	}
}

func TestNewUsageSessionCounts(t *testing.T) {
	counts := NewUsageSessionCounts(map[string]UsageSessionInfo{
		"s1": {Project: "proj-a", Agent: "claude"},
		"s2": {Project: "proj-a", Agent: "codex"},
		"s3": {Project: "proj-b", Agent: "claude"},
	})

	assert.Equal(t, 3, counts.Total, "Total")
	assert.Equal(t, map[string]int{
		"proj-a": 2,
		"proj-b": 1,
	}, counts.ByProject, "ByProject")
	assert.Equal(t, map[string]int{
		"claude": 2,
		"codex":  1,
	}, counts.ByAgent, "ByAgent")
}

// TestGetUsageSessionCounts_DedupesByClaudeMessageAndRequestID
// mirrors the dedup regression coverage on the other two usage
// queries. A fork session whose only qualifying messages are
// replays of its parent's (same claude_message_id +
// claude_request_id) contributes zero cost after dedup in
// GetDailyUsage, so it must also NOT be counted in
// GetUsageSessionCounts — otherwise the summary cards disagree
// with the charts.
func TestGetUsageSessionCounts_DedupesByClaudeMessageAndRequestID(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	// Parent starts first.
	insertSession(t, d, "s-parent", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	// Fork starts a minute later.
	insertSession(t, d, "s-fork", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:01:00Z")
		s.ParentSessionID = new("s-parent")
		s.RelationshipType = "fork"
	})

	shared := jsontext.Value(
		`{"input_tokens":100,"output_tokens":50}`)

	// Parent has one unique message.
	insertMessages(t, d, Message{
		SessionID: "s-parent", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:02:00Z",
		Model: "claude-sonnet", TokenUsage: shared,
		ClaudeMessageID: "msg_dup", ClaudeRequestID: "req_dup",
	})
	// Fork's ONLY qualifying message is a replay of the parent
	// row — same claude IDs. After dedup the fork contributes
	// nothing and must not be counted.
	insertMessages(t, d, Message{
		SessionID: "s-fork", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:03:00Z",
		Model: "claude-sonnet", TokenUsage: shared,
		ClaudeMessageID: "msg_dup", ClaudeRequestID: "req_dup",
	})

	counts, err := d.GetUsageSessionCounts(ctx, UsageFilter{
		From: "2024-06-15", To: "2024-06-15", Timezone: "UTC",
	})
	requireNoError(t, err, "GetUsageSessionCounts")

	assert.Equal(t, 1, counts.Total,
		"Total want 1 (fork should dedup out)")
	assert.Equal(t, 1, counts.ByProject["proj"], "ByProject[proj]")
	assert.Equal(t, 1, counts.ByAgent["claude"], "ByAgent[claude]")
}

func TestGetUsageSessionCounts_DedupesBySourceUUIDWhenClaudePairIncomplete(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "s-parent", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertSession(t, d, "s-fork", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:01:00Z")
		s.ParentSessionID = new("s-parent")
		s.RelationshipType = "fork"
	})

	shared := jsontext.Value(`{"input_tokens":100,"output_tokens":50}`)

	insertMessages(t, d, Message{
		SessionID: "s-parent", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:02:00Z",
		Model: "claude-sonnet", TokenUsage: shared,
		ClaudeMessageID: "msg_dup", SourceUUID: "source_dup",
	})
	insertMessages(t, d, Message{
		SessionID: "s-fork", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:03:00Z",
		Model: "claude-sonnet", TokenUsage: shared,
		ClaudeMessageID: "msg_dup", SourceUUID: "source_dup",
	})

	counts, err := d.GetUsageSessionCounts(ctx, UsageFilter{
		From: "2024-06-15", To: "2024-06-15", Timezone: "UTC",
	})
	requireNoError(t, err, "GetUsageSessionCounts")

	assert.Equal(t, 1, counts.Total, "Total want 1 (fork should dedup out)")
	assert.Equal(t, 1, counts.ByProject["proj"], "ByProject[proj]")
	assert.Equal(t, 1, counts.ByAgent["claude"], "ByAgent[claude]")
}

// TestUsageQueryEligibilityParity seeds messages that fail each
// disqualification predicate and asserts all three usage queries
// ignore them. Guardrail against drift between usage queries.
func TestUsageQueryEligibilityParity(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet",
		InputPerMTok:  money.MustParseDollars("3.0"),
		OutputPerMTok: money.MustParseDollars("15.0"),
	}}), "UpsertModelPricing")

	// Good session — should be visible to all queries.
	insertSession(t, d, "good", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "good", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:30:00Z",
		Model: "claude-sonnet",
		TokenUsage: jsontext.Value(
			`{"input_tokens":1000,"output_tokens":500}`),
	})

	// Bad: empty token_usage
	insertSession(t, d, "bad-empty", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "bad-empty", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:30:00Z",
		Model:      "claude-sonnet",
		TokenUsage: jsontext.Value(""),
	})

	// Bad: synthetic model
	insertSession(t, d, "bad-synth", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "bad-synth", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:30:00Z",
		Model: "<synthetic>",
		TokenUsage: jsontext.Value(
			`{"input_tokens":999,"output_tokens":999}`),
	})

	// Bad: soft-deleted session
	insertSession(t, d, "bad-deleted", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "bad-deleted", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:30:00Z",
		Model: "claude-sonnet",
		TokenUsage: jsontext.Value(
			`{"input_tokens":999,"output_tokens":999}`),
	})
	requireNoError(t,
		d.SoftDeleteSession("bad-deleted"),
		"SoftDeleteSession")

	filter := UsageFilter{
		From:       "2024-06-01",
		To:         "2024-06-30",
		Breakdowns: true,
	}

	// GetDailyUsage
	daily, err := d.GetDailyUsage(ctx, filter)
	requireNoError(t, err, "GetDailyUsage parity")
	assert.Equal(t, 1000, daily.Totals.InputTokens, "GetDailyUsage InputTokens")

	// GetUsageSessionCounts
	counts, err := d.GetUsageSessionCounts(ctx, filter)
	requireNoError(t, err, "GetUsageSessionCounts parity")
	assert.Equal(t, 1, counts.Total, "GetUsageSessionCounts Total")

	// GetTopSessionsByCost
	top, err := d.GetTopSessionsByCost(ctx, filter, 20)
	requireNoError(t, err, "GetTopSessionsByCost parity")
	require.Len(t, top, 1, "GetTopSessionsByCost len")
	assert.Equal(t, "good", top[0].SessionID,
		"GetTopSessionsByCost[0].SessionID")
}

// TestExcludeProjectFilter verifies that ExcludeProject removes
// matching projects from all three usage queries.
func TestExcludeProjectFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet",
		InputPerMTok:  money.MustParseDollars("3.0"),
		OutputPerMTok: money.MustParseDollars("15.0"),
	}}), "UpsertModelPricing")

	insertSession(t, d, "sA", "proj-a", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertSession(t, d, "sB", "proj-b", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertSession(t, d, "sC", "proj-c", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})

	usage := `{"input_tokens":1000,"output_tokens":500}`
	insertMessages(t, d,
		Message{SessionID: "sA", Ordinal: 0, Role: "assistant",
			Timestamp: "2024-06-15T10:30:00Z", Model: "claude-sonnet",
			TokenUsage: jsontext.Value(usage)},
		Message{SessionID: "sB", Ordinal: 0, Role: "assistant",
			Timestamp: "2024-06-15T10:30:00Z", Model: "claude-sonnet",
			TokenUsage: jsontext.Value(usage)},
		Message{SessionID: "sC", Ordinal: 0, Role: "assistant",
			Timestamp: "2024-06-15T10:30:00Z", Model: "claude-sonnet",
			TokenUsage: jsontext.Value(usage)},
	)

	base := UsageFilter{From: "2024-06-01", To: "2024-06-30"}

	// Exclude one project.
	f1 := base
	f1.ExcludeProject = "proj-b"
	daily, err := d.GetDailyUsage(ctx, f1)
	requireNoError(t, err, "GetDailyUsage exclude one")
	assert.Equal(t, 2000, daily.Totals.InputTokens, "exclude proj-b: InputTokens")

	// Exclude two projects (comma-separated).
	f2 := base
	f2.ExcludeProject = "proj-a,proj-c"
	daily, err = d.GetDailyUsage(ctx, f2)
	requireNoError(t, err, "GetDailyUsage exclude two")
	assert.Equal(t, 1000, daily.Totals.InputTokens, "exclude a+c: InputTokens")

	// GetTopSessionsByCost with exclude.
	top, err := d.GetTopSessionsByCost(ctx, f1, 10)
	requireNoError(t, err, "GetTopSessionsByCost exclude")
	require.Len(t, top, 2, "exclude proj-b: top len =")
	for _, ts := range top {
		assert.NotEqual(t, "proj-b", ts.Project,
			"excluded proj-b still in top sessions")
	}

	// GetUsageSessionCounts with exclude.
	counts, err := d.GetUsageSessionCounts(ctx, f1)
	requireNoError(t, err, "GetUsageSessionCounts exclude")
	assert.Equal(t, 2, counts.Total, "exclude proj-b: Total")
	assert.Equal(t, 0, counts.ByProject["proj-b"], "excluded proj-b count")
}

func TestUsageSessionFilters(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet",
		InputPerMTok:  money.MustParseDollars("3.0"),
		OutputPerMTok: money.MustParseDollars("15.0"),
	}}), "UpsertModelPricing")

	tokenUsage := jsontext.Value(
		`{"input_tokens":1000,"output_tokens":500}`,
	)

	insertSession(t, d, "usage-filter-keep", "proj", func(s *Session) {
		s.Machine = "host-a"
		s.Agent = "claude"
		s.MessageCount = 4
		s.UserMessageCount = 3
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertSession(t, d, "usage-filter-machine", "proj", func(s *Session) {
		s.Machine = "host-b"
		s.Agent = "claude"
		s.MessageCount = 4
		s.UserMessageCount = 3
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertSession(t, d, "usage-filter-prompts", "proj", func(s *Session) {
		s.Machine = "host-a"
		s.Agent = "claude"
		s.MessageCount = 4
		s.UserMessageCount = 1
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertSession(t, d, "usage-filter-one-shot", "proj", func(s *Session) {
		s.Machine = "host-a"
		s.Agent = "claude"
		s.MessageCount = 1
		s.UserMessageCount = 1
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertSession(t, d, "usage-filter-automated", "proj", func(s *Session) {
		s.Machine = "host-a"
		s.Agent = "claude"
		s.MessageCount = 4
		s.UserMessageCount = 3
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	_, err := d.getWriter().Exec(
		"UPDATE sessions SET is_automated = 1 WHERE id = ?",
		"usage-filter-automated",
	)
	require.NoError(t, err, "patch automated fixture")

	for _, sid := range []string{
		"usage-filter-keep",
		"usage-filter-machine",
		"usage-filter-prompts",
		"usage-filter-one-shot",
		"usage-filter-automated",
	} {
		insertMessages(t, d, Message{
			SessionID:  sid,
			Ordinal:    0,
			Role:       "assistant",
			Timestamp:  "2024-06-15T10:30:00Z",
			Model:      "claude-sonnet",
			TokenUsage: tokenUsage,
		})
	}

	filter := UsageFilter{
		From:             "2024-06-01",
		To:               "2024-06-30",
		Machine:          "host-a",
		MinUserMessages:  2,
		ExcludeOneShot:   true,
		ExcludeAutomated: true,
	}

	daily, err := d.GetDailyUsage(ctx, filter)
	requireNoError(t, err, "GetDailyUsage session filters")
	assert.Equal(t, 1000, daily.Totals.InputTokens, "InputTokens")

	top, err := d.GetTopSessionsByCost(ctx, filter, 10)
	requireNoError(t, err, "GetTopSessionsByCost session filters")
	require.Len(t, top, 1,
		"top sessions want only usage-filter-keep: %+v", top)
	require.Equal(t, "usage-filter-keep", top[0].SessionID,
		"top sessions want only usage-filter-keep: %+v", top)

	counts, err := d.GetUsageSessionCounts(ctx, filter)
	requireNoError(t, err, "GetUsageSessionCounts session filters")
	assert.Equal(t, 1, counts.Total, "counts.Total")
}

func TestUsageTerminationFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet",
		InputPerMTok:  money.MustParseDollars("3.0"),
		OutputPerMTok: money.MustParseDollars("15.0"),
	}}), "UpsertModelPricing")

	clean := "clean"
	unclean := "tool_call_pending"
	tokenUsage := jsontext.Value(
		`{"input_tokens":1000,"output_tokens":500}`,
	)
	insertSession(t, d, "usage-filter-clean", "proj", func(s *Session) {
		s.MessageCount = 4
		s.UserMessageCount = 3
		s.TerminationStatus = &clean
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertSession(t, d, "usage-filter-unclean", "proj", func(s *Session) {
		s.MessageCount = 4
		s.UserMessageCount = 3
		s.TerminationStatus = &unclean
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	for _, sid := range []string{
		"usage-filter-clean",
		"usage-filter-unclean",
	} {
		insertMessages(t, d, Message{
			SessionID:  sid,
			Ordinal:    0,
			Role:       "assistant",
			Timestamp:  "2024-06-15T10:30:00Z",
			Model:      "claude-sonnet",
			TokenUsage: tokenUsage,
		})
	}

	filter := UsageFilter{
		From:        "2024-06-01",
		To:          "2024-06-30",
		Termination: "clean",
	}
	daily, err := d.GetDailyUsage(ctx, filter)
	requireNoError(t, err, "GetDailyUsage termination filter")
	if daily.Totals.InputTokens != 1000 {
		t.Errorf("InputTokens = %d, want 1000",
			daily.Totals.InputTokens)
	}

	top, err := d.GetTopSessionsByCost(ctx, filter, 10)
	requireNoError(t, err, "GetTopSessionsByCost termination filter")
	if len(top) != 1 || top[0].SessionID != "usage-filter-clean" {
		t.Fatalf("top sessions = %+v, want only usage-filter-clean", top)
	}

	counts, err := d.GetUsageSessionCounts(ctx, filter)
	requireNoError(t, err, "GetUsageSessionCounts termination filter")
	if counts.Total != 1 {
		t.Errorf("counts.Total = %d, want 1", counts.Total)
	}
}

// TestUsageActivityFallbackEmptyEndedAt guards the SQLite usage activity-time
// fallback. A session whose ended_at was never persisted is stored as the empty
// string, so COALESCE(s.ended_at, ...) returned ” and strftime('%s', ”)
// yielded NULL, silently dropping it from active_since and the active/stale/
// unclean termination filters. NULLIF must let the fallback reach started_at.
func TestUsageActivityFallbackEmptyEndedAt(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet",
		InputPerMTok:  money.MustParseDollars("3.0"),
		OutputPerMTok: money.MustParseDollars("15.0"),
	}}), "UpsertModelPricing")

	flagged := "tool_call_pending"
	// started_at set; ended_at persisted as the empty string, the legacy
	// state every other read query guards with NULLIF(ended_at, '').
	insertSession(t, d, "untimed", "proj", func(s *Session) {
		s.MessageCount = 4
		s.UserMessageCount = 3
		s.TerminationStatus = &flagged
		s.StartedAt = new("2024-06-15T10:00:00Z")
		s.EndedAt = new("")
	})
	insertMessages(t, d, Message{
		SessionID:  "untimed",
		Ordinal:    0,
		Role:       "assistant",
		Timestamp:  "2024-06-15T10:30:00Z",
		Model:      "claude-sonnet",
		TokenUsage: jsontext.Value(`{"input_tokens":1000,"output_tokens":500}`),
	})

	// active_since before started_at must keep the session via the fallback.
	activeCounts, err := d.GetUsageSessionCounts(ctx, UsageFilter{
		From:        "2024-06-01",
		To:          "2024-06-30",
		ActiveSince: "2024-06-01T00:00:00Z",
	})
	require.NoError(t, err, "GetUsageSessionCounts active_since")
	assert.Equal(t, 1, activeCounts.Total,
		"active_since must match empty-ended_at session via started_at")

	// The unclean filter evaluates the activity epoch expression; the flagged
	// session with an old started_at must be matched.
	uncleanCounts, err := d.GetUsageSessionCounts(ctx, UsageFilter{
		From:        "2024-06-01",
		To:          "2024-06-30",
		Termination: "unclean",
	})
	require.NoError(t, err, "GetUsageSessionCounts unclean")
	assert.Equal(t, 1, uncleanCounts.Total,
		"unclean must match flagged empty-ended_at session via started_at")
}

func TestUsageExcludeOneShotUsesUserMessageCount(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet",
		InputPerMTok:  money.MustParseDollars("3.0"),
		OutputPerMTok: money.MustParseDollars("15.0"),
	}}), "UpsertModelPricing")

	tokenUsage := jsontext.Value(
		`{"input_tokens":1000,"output_tokens":500}`,
	)

	insertSession(t, d, "usage-one-user-message", "proj", func(s *Session) {
		s.Agent = "claude"
		s.MessageCount = 2
		s.UserMessageCount = 1
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertSession(t, d, "usage-two-user-messages", "proj", func(s *Session) {
		s.Agent = "claude"
		s.MessageCount = 3
		s.UserMessageCount = 2
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})

	for _, sid := range []string{
		"usage-one-user-message",
		"usage-two-user-messages",
	} {
		insertMessages(t, d, Message{
			SessionID:  sid,
			Ordinal:    0,
			Role:       "assistant",
			Timestamp:  "2024-06-15T10:30:00Z",
			Model:      "claude-sonnet",
			TokenUsage: tokenUsage,
		})
	}

	filter := UsageFilter{
		From:           "2024-06-01",
		To:             "2024-06-30",
		ExcludeOneShot: true,
	}

	daily, err := d.GetDailyUsage(ctx, filter)
	requireNoError(t, err, "GetDailyUsage exclude one-shot")
	assert.Equal(t, 1000, daily.Totals.InputTokens, "InputTokens")

	top, err := d.GetTopSessionsByCost(ctx, filter, 10)
	requireNoError(t, err, "GetTopSessionsByCost exclude one-shot")
	require.Len(t, top, 1,
		"top sessions want only usage-two-user-messages: %+v", top)
	require.Equal(t, "usage-two-user-messages", top[0].SessionID,
		"top sessions want only usage-two-user-messages: %+v", top)

	counts, err := d.GetUsageSessionCounts(ctx, filter)
	requireNoError(t, err, "GetUsageSessionCounts exclude one-shot")
	assert.Equal(t, 1, counts.Total, "counts.Total")
}

// TestExcludeAgentFilter verifies ExcludeAgent on GetDailyUsage.
func TestExcludeAgentFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet",
		InputPerMTok:  money.MustParseDollars("3.0"),
		OutputPerMTok: money.MustParseDollars("15.0"),
	}}), "UpsertModelPricing")

	insertSession(t, d, "s1", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertSession(t, d, "s2", "proj", func(s *Session) {
		s.Agent = "codex"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})

	usage := `{"input_tokens":1000,"output_tokens":500}`
	insertMessages(t, d,
		Message{SessionID: "s1", Ordinal: 0, Role: "assistant",
			Timestamp: "2024-06-15T10:30:00Z", Model: "claude-sonnet",
			TokenUsage: jsontext.Value(usage)},
		Message{SessionID: "s2", Ordinal: 0, Role: "assistant",
			Timestamp: "2024-06-15T10:30:00Z", Model: "claude-sonnet",
			TokenUsage: jsontext.Value(usage)},
	)

	f := UsageFilter{
		From:         "2024-06-01",
		To:           "2024-06-30",
		ExcludeAgent: "codex",
	}
	daily, err := d.GetDailyUsage(ctx, f)
	requireNoError(t, err, "GetDailyUsage exclude agent")
	assert.Equal(t, 1000, daily.Totals.InputTokens, "exclude codex: InputTokens")
}

// TestExcludeModelFilter verifies ExcludeModel on GetDailyUsage.
func TestExcludeModelFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{
		{ModelPattern: "sonnet", InputPerMTok: money.MustParseDollars("3.0"),
			OutputPerMTok: money.MustParseDollars("15.0")},
		{ModelPattern: "opus", InputPerMTok: money.MustParseDollars("15.0"),
			OutputPerMTok: money.MustParseDollars("75.0")},
	}), "UpsertModelPricing")

	insertSession(t, d, "s1", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})

	insertMessages(t, d,
		Message{SessionID: "s1", Ordinal: 0, Role: "assistant",
			Timestamp: "2024-06-15T10:30:00Z", Model: "sonnet",
			TokenUsage: jsontext.Value(
				`{"input_tokens":1000,"output_tokens":500}`)},
		Message{SessionID: "s1", Ordinal: 1, Role: "assistant",
			Timestamp: "2024-06-15T11:30:00Z", Model: "opus",
			TokenUsage: jsontext.Value(
				`{"input_tokens":1000,"output_tokens":500}`)},
	)

	f := UsageFilter{
		From:         "2024-06-01",
		To:           "2024-06-30",
		ExcludeModel: "opus",
	}
	daily, err := d.GetDailyUsage(ctx, f)
	requireNoError(t, err, "GetDailyUsage exclude model")
	assert.Equal(t, 1000, daily.Totals.InputTokens, "exclude opus: InputTokens")
	require.Len(t, daily.Daily, 1, "daily len =")
	for _, mb := range daily.Daily[0].ModelBreakdowns {
		assert.NotEqual(t, "opus", mb.ModelName,
			"excluded model opus still in breakdowns")
	}
}

// BenchmarkGetDailyUsage retains the original 100,000-row fixture and name
// so benchgate can compare this branch directly with its merge base.
func BenchmarkGetDailyUsage(b *testing.B) {
	d := testDB(b)
	ctx := context.Background()
	origLog := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(origLog)

	if err := d.UpsertModelPricing([]ModelPricing{
		{ModelPattern: "claude-sonnet-4-20250514",
			InputPerMTok: money.MustParseDollars("3.0"), OutputPerMTok: money.MustParseDollars("15.0"),
			CacheCreationPerMTok: money.MustParseDollars("3.75"), CacheReadPerMTok: money.MustParseDollars("0.30")},
		{ModelPattern: "claude-opus-4-20250514",
			InputPerMTok: money.MustParseDollars("15.0"), OutputPerMTok: money.MustParseDollars("75.0"),
			CacheCreationPerMTok: money.MustParseDollars("18.75"), CacheReadPerMTok: money.MustParseDollars("1.50")},
		{ModelPattern: "gpt-5",
			InputPerMTok: money.MustParseDollars("2.5"), OutputPerMTok: money.MustParseDollars("10.0"),
			CacheCreationPerMTok: money.MustParseDollars("2.5"), CacheReadPerMTok: money.MustParseDollars("0.25")},
		{ModelPattern: "gemini-2.5-pro",
			InputPerMTok: money.MustParseDollars("1.25"), OutputPerMTok: money.MustParseDollars("5.0"),
			CacheCreationPerMTok: money.MustParseDollars("1.25"), CacheReadPerMTok: money.MustParseDollars("0.125")},
	}); err != nil {
		b.Fatalf("UpsertModelPricing: %v", err)
	}

	projects := []string{
		"agentsview", "quokka", "arrow-rs", "side-quests",
		"infrastructure", "blog", "experiments", "docs",
		"dotfiles", "playground",
	}
	agents := []string{"claude", "codex", "openhands"}
	models := []string{
		"claude-sonnet-4-20250514",
		"claude-opus-4-20250514",
		"gpt-5",
		"gemini-2.5-pro",
	}

	// 500 sessions × 200 messages each = 100k rows.
	const sessionCount = 500
	const msgsPerSession = 200

	tokenUsage := `{"input_tokens":1200,"output_tokens":480,` +
		`"cache_creation_input_tokens":300,` +
		`"cache_read_input_tokens":2400}`

	// Pre-parse the anchor timestamp once; the seed loop offsets from it.
	startTime, err := time.Parse(time.RFC3339, "2024-06-01T00:00:00Z")
	if err != nil {
		b.Fatalf("parsing start time: %v", err)
	}

	for i := range sessionCount {
		id := "bench-sess-" + strconv.Itoa(i)
		project := projects[i%len(projects)]
		agent := agents[i%len(agents)]
		// Spread sessions across a 60-day window.
		dayOffset := i % 60
		s := Session{
			ID:           id,
			Project:      project,
			Machine:      defaultMachine,
			Agent:        agent,
			MessageCount: msgsPerSession,
			StartedAt:    new(startTime.Format(time.RFC3339)),
		}
		if err := d.UpsertSession(s); err != nil {
			b.Fatalf("UpsertSession: %v", err)
		}
		msgs := make([]Message, msgsPerSession)
		for j := range msgsPerSession {
			msgs[j] = Message{
				SessionID:  id,
				Ordinal:    j,
				Role:       "assistant",
				Timestamp:  startTime.AddDate(0, 0, dayOffset).Format(time.RFC3339),
				Model:      models[(i+j)%len(models)],
				TokenUsage: jsontext.Value(tokenUsage),
			}
		}
		if err := d.InsertMessages(msgs); err != nil {
			b.Fatalf("InsertMessages: %v", err)
		}
	}

	filter := UsageFilter{From: "2024-06-01", To: "2024-08-01"}
	if _, err := d.GetDailyUsage(ctx, filter); err != nil {
		b.Fatalf("warming GetDailyUsage: %v", err)
	}
	// Logs stay discarded through the timed loop: a slow-request log
	// line interleaving with the benchmark result line corrupts the
	// bench-gate capture (see silenceBenchLogs in internal/sync).
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := d.GetDailyUsage(ctx, filter)
		if err != nil {
			b.Fatalf("GetDailyUsage: %v", err)
		}
	}
}

// BenchmarkGetDailyUsageSnapshotWindows measures the ordinary CLI query
// shape over bounded windows with duplicated Claude streaming snapshots.
func BenchmarkGetDailyUsageSnapshotWindows(b *testing.B) {
	d := testDB(b)
	seedDailyUsageSnapshotBenchmark(b, d)

	windows := []struct {
		name string
		from string
		want benchmarkDailyUsageExpectation
	}{
		{"window=1d", "2024-07-30", benchmarkDailyUsageExpectation{
			days: 1, input: 1_920_000, output: 768_000,
			cacheCreation: 480_000, cacheRead: 3_840_000,
		}},
		{"window=7d", "2024-07-24", benchmarkDailyUsageExpectation{
			days: 7, input: 13_440_000, output: 5_376_000,
			cacheCreation: 3_360_000, cacheRead: 26_880_000,
		}},
		{"window=30d", "2024-07-01", benchmarkDailyUsageExpectation{
			days: 30, input: 57_600_000, output: 23_040_000,
			cacheCreation: 14_400_000, cacheRead: 115_200_000,
		}},
		{"window=60d", "2024-06-01", benchmarkDailyUsageExpectation{
			days: 60, input: 120_000_000, output: 48_000_000,
			cacheCreation: 30_000_000, cacheRead: 240_000_000,
		}},
	}
	for _, window := range windows {
		b.Run(window.name, func(b *testing.B) {
			benchmarkDailyUsageSnapshotWindow(
				b, d, window.from, window.want,
			)
		})
	}
}

// BenchmarkGetDailyUsageSnapshotAutomaticIndexOff keeps the cache query fast
// when SQLite automatic indexes are unavailable.
func BenchmarkGetDailyUsageSnapshotAutomaticIndexOff(b *testing.B) {
	d := testDB(b)
	seedDailyUsageSnapshotBenchmark(b, d)
	reader := d.rawReader()
	reader.SetMaxOpenConns(1)
	_, err := reader.ExecContext(b.Context(), "PRAGMA automatic_index = OFF")
	require.NoError(b, err)
	var automaticIndex int
	require.NoError(b,
		reader.QueryRowContext(b.Context(), "PRAGMA automatic_index").
			Scan(&automaticIndex))
	require.Zero(b, automaticIndex)

	benchmarkDailyUsageSnapshotWindow(
		b, d, "2024-07-01", benchmarkDailyUsageExpectation{
			days: 30, input: 57_600_000, output: 23_040_000,
			cacheCreation: 14_400_000, cacheRead: 115_200_000,
		},
	)
}

func BenchmarkUsageRollup(b *testing.B) {
	// Cold sub-benchmarks exceed the slow-request threshold on CI
	// runners; the resulting log line would interleave with benchmark
	// result lines and corrupt the bench-gate capture.
	origLog := log.Writer()
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(origLog) })
	database := testDB(b)
	seedDailyUsageSnapshotBenchmark(b, database)
	seedLongLivedUsageBenchmark(b, database)
	ctx := context.Background()
	windows := []struct {
		name         string
		filter       UsageFilter
		wantSessions int
		want         benchmarkDailyUsageExpectation
	}{
		{
			name: "1d", filter: usageFactsBenchmarkFilter("2024-07-30"),
			wantSessions: 13,
			want: benchmarkDailyUsageExpectation{
				days: 1, input: 1_920_500, output: 768_250,
				cacheCreation: 480_000, cacheRead: 3_840_000,
			},
		},
		{
			name: "7d", filter: usageFactsBenchmarkFilter("2024-07-24"),
			wantSessions: 69,
			want: benchmarkDailyUsageExpectation{
				days: 7, input: 13_443_500, output: 5_377_750,
				cacheCreation: 3_360_000, cacheRead: 26_880_000,
			},
		},
		{
			name: "30d", filter: usageFactsBenchmarkFilter("2024-07-01"),
			wantSessions: 253,
			want: benchmarkDailyUsageExpectation{
				days: 30, input: 57_615_000, output: 23_047_500,
				cacheCreation: 14_400_000, cacheRead: 115_200_000,
			},
		},
		{
			name:         "all",
			filter:       UsageFilter{Timezone: "UTC", SkipSessionCounts: true},
			wantSessions: 505,
			want: benchmarkDailyUsageExpectation{
				days: 366, input: 120_183_000, output: 48_091_500,
				cacheCreation: 30_000_000, cacheRead: 240_000_000,
			},
		},
	}
	allSnapshot, err := database.captureUsageQuery(
		ctx, windows[len(windows)-1].filter, usageQueryKindToken)
	require.NoError(b, err, "capture all-history snapshot")
	require.Len(b, allSnapshot.Sessions, 505)
	cache, err := database.usageCache.Generation(ctx, allSnapshot.DatabaseID)
	require.NoError(b, err)

	for _, window := range windows {
		b.Run("candidate/"+window.name, func(b *testing.B) {
			captured, captureErr := database.captureUsageQuery(
				ctx, window.filter, usageQueryKindToken)
			require.NoError(b, captureErr)
			require.Len(b, captured.Sessions, window.wantSessions)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				captured, captureErr = database.captureUsageQuery(
					ctx, window.filter, usageQueryKindToken)
				if captureErr != nil {
					b.Fatal(captureErr)
				}
				if len(captured.Sessions) != window.wantSessions {
					b.Fatalf("candidate sessions = %d, want %d",
						len(captured.Sessions), window.wantSessions)
				}
			}
		})
	}

	coldCases := []struct {
		name      string
		filter    UsageFilter
		want      benchmarkDailyUsageExpectation
		wantFacts int
	}{
		{"30d", windows[2].filter, windows[2].want, 53_190},
		{
			"long-lived-30d",
			UsageFilter{
				From: "2024-07-01", To: "2024-07-30", Timezone: "UTC",
				Project: "long-lived", SkipSessionCounts: true,
			},
			benchmarkDailyUsageExpectation{
				days: 30, input: 15_000, output: 7_500,
			},
			53_190,
		},
		{"all", windows[3].filter, windows[3].want, 105_150},
	}
	for _, cold := range coldCases {
		b.Run("cold/"+cold.name, func(b *testing.B) {
			clearUsageFactsBenchmarkCache(b, cache)
			result, queryErr := database.GetDailyUsage(ctx, cold.filter)
			require.NoError(b, queryErr)
			requireDailyUsageBenchmarkResult(b, result, cold.want)
			requireUsageFactsCount(b, cache, cold.wantFacts)
			exceptionRows, exceptionGroups := usageRollupExceptionMetrics(
				b, cache, cold.filter)
			clearUsageFactsBenchmarkCache(b, cache)
			b.ReportAllocs()
			b.ResetTimer()
			b.ReportMetric(float64(cold.wantFacts), "facts/op")
			b.ReportMetric(float64(exceptionRows), "exception-rows/op")
			b.ReportMetric(float64(exceptionGroups), "exception-groups/op")
			for range b.N {
				b.StopTimer()
				clearUsageFactsBenchmarkCache(b, cache)
				b.StartTimer()
				_, queryErr = database.GetDailyUsage(ctx, cold.filter)
				if queryErr != nil {
					b.Fatal(queryErr)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(usageCacheDiskBytes(cache.path)), "cache-bytes")
		})
	}

	_, err = database.GetDailyUsage(ctx, windows[3].filter)
	require.NoError(b, err, "warm all facts")
	for _, window := range windows {
		b.Run("warm/"+window.name, func(b *testing.B) {
			result, queryErr := database.GetDailyUsage(ctx, window.filter)
			require.NoError(b, queryErr)
			requireDailyUsageBenchmarkResult(b, result, window.want)
			exceptionRows, exceptionGroups := usageRollupExceptionMetrics(
				b, cache, window.filter)
			cacheBytes := usageCacheDiskBytes(cache.path)
			b.ReportAllocs()
			b.ResetTimer()
			b.ReportMetric(float64(exceptionRows), "exception-rows/op")
			b.ReportMetric(float64(exceptionGroups), "exception-groups/op")
			b.ReportMetric(float64(cacheBytes), "cache-bytes")
			for range b.N {
				if _, queryErr = database.GetDailyUsage(
					ctx, window.filter); queryErr != nil {
					b.Fatal(queryErr)
				}
			}
		})
	}
	for _, window := range windows[2:] {
		b.Run("exceptions/"+window.name, func(b *testing.B) {
			snapshot, captureErr := database.captureUsageQuery(
				ctx, window.filter, usageQueryKindToken)
			require.NoError(b, captureErr)
			resolver := export.NewPricingResolver(snapshot.PricingRows)
			conn, connErr := cache.db.Conn(ctx)
			require.NoError(b, connErr)
			defer conn.Close()
			identity := usageTimezoneIdentityFor(
				snapshot.location, snapshot.Intervals)
			var timezoneID int64
			require.NoError(b, conn.QueryRowContext(ctx,
				`SELECT id FROM usage_rollup_timezones WHERE timezone_key = ?`,
				identity.Key,
			).Scan(&timezoneID))
			exceptions, readErr := readUsageRollupExceptions(
				ctx, conn, timezoneID, snapshot, window.filter)
			require.NoError(b, readErr)
			groups, aggregateErr := aggregateUsageRollupExceptions(
				exceptions, snapshot, window.filter, resolver)
			require.NoError(b, aggregateErr)
			exceptionRows := len(exceptions)
			exceptionGroups := len(groups)
			require.NotZero(b, exceptionRows)
			b.ReportAllocs()
			b.ResetTimer()
			b.ReportMetric(float64(exceptionRows), "exception-rows/op")
			b.ReportMetric(float64(exceptionGroups), "resolved-groups/op")
			for range b.N {
				exceptions, readErr = readUsageRollupExceptions(
					ctx, conn, timezoneID, snapshot, window.filter)
				if readErr != nil {
					b.Fatal(readErr)
				}
				groups, aggregateErr = aggregateUsageRollupExceptions(
					exceptions, snapshot, window.filter, resolver)
				if aggregateErr != nil {
					b.Fatal(aggregateErr)
				}
				if len(groups) != exceptionGroups {
					b.Fatalf("exception groups = %d, want %d",
						len(groups), exceptionGroups)
				}
			}
		})
	}

	b.Run("fill-throughput", func(b *testing.B) {
		clearUsageFactsBenchmarkCache(b, cache)
		fills, fillErr := cache.fill.Ensure(
			ctx, allSnapshot.Versions, allSnapshot.CursorHighWater)
		require.NoError(b, fillErr)
		require.Len(b, fills, 505)
		requireUsageFactsCount(b, cache, 105_150)
		clearUsageFactsBenchmarkCache(b, cache)
		b.ReportAllocs()
		b.ReportMetric(105_150, "facts/op")
		b.ResetTimer()
		for range b.N {
			b.StopTimer()
			clearUsageFactsBenchmarkCache(b, cache)
			b.StartTimer()
			if _, fillErr = cache.fill.Ensure(
				ctx, allSnapshot.Versions, allSnapshot.CursorHighWater,
			); fillErr != nil {
				b.Fatal(fillErr)
			}
		}
		b.StopTimer()
		b.ReportMetric(
			float64(105_150*b.N)/b.Elapsed().Seconds(), "facts/s")
		b.ReportMetric(float64(usageCacheDiskBytes(cache.path)), "cache-bytes")
	})

	b.Run("relaxed-discovery", func(b *testing.B) {
		filter := windows[2].filter
		captured, captureErr := database.captureUsageQuery(
			ctx, filter, usageQueryKindActivity)
		require.NoError(b, captureErr)
		require.Len(b, captured.Sessions, 253)
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			captured, captureErr = database.captureUsageQuery(
				ctx, filter, usageQueryKindActivity)
			if captureErr != nil {
				b.Fatal(captureErr)
			}
			if len(captured.Sessions) != 253 {
				b.Fatalf("relaxed candidates = %d, want 253",
					len(captured.Sessions))
			}
		}
	})
}

func usageFactsBenchmarkFilter(from string) UsageFilter {
	return UsageFilter{
		From: from, To: "2024-07-30", Timezone: "UTC",
		SkipSessionCounts: true,
	}
}

func requireDailyUsageBenchmarkResult(
	tb testing.TB, result DailyUsageResult, want benchmarkDailyUsageExpectation,
) {
	tb.Helper()
	require.Len(tb, result.Daily, want.days)
	assert.Equal(tb, want.input, result.Totals.InputTokens)
	assert.Equal(tb, want.output, result.Totals.OutputTokens)
	assert.Equal(tb, want.cacheCreation, result.Totals.CacheCreationTokens)
	assert.Equal(tb, want.cacheRead, result.Totals.CacheReadTokens)
	assert.Zero(tb, result.SessionCounts.Total)
}

func requireUsageFactsCount(tb testing.TB, cache *usageCache, want int) {
	tb.Helper()
	var got int
	require.NoError(tb, cache.db.QueryRow(`SELECT COUNT(*) FROM usage_facts`).Scan(&got))
	require.Equal(tb, want, got, "usage facts")
}

func usageRollupExceptionMetrics(
	tb testing.TB, cache *usageCache, filter UsageFilter,
) (int64, int64) {
	tb.Helper()
	identity := usageTimezoneIdentityFor(filter.location(), nil)
	var rows, groups int64
	require.NoError(tb, cache.db.QueryRow(`SELECT COUNT(*),
		COUNT(DISTINCT e.group_kind || char(0) || e.group_key)
		FROM usage_rollup_exceptions e
		JOIN usage_rollup_installs i ON i.id = e.rollup_install_id
		JOIN usage_rollup_timezones z ON z.id = i.timezone_id
		WHERE z.timezone_key = ?
		  AND (? = '' OR e.local_date >= ?)
		  AND (? = '' OR e.local_date <= ?)`,
		identity.Key, filter.From, filter.From, filter.To, filter.To,
	).Scan(&rows, &groups))
	return rows, groups
}

func usageCacheDiskBytes(path string) int64 {
	var size int64
	for _, candidate := range []string{path, path + "-wal"} {
		if info, err := os.Stat(candidate); err == nil {
			size += info.Size()
		}
	}
	return size
}

func clearUsageFactsBenchmarkCache(tb testing.TB, cache *usageCache) {
	tb.Helper()
	_, err := cache.db.Exec(`
		DELETE FROM usage_rollup_timezones;
		DELETE FROM usage_cached_sessions;
		DELETE FROM cursor_usage_facts;
		UPDATE usage_cache_metadata SET value = '0'
		WHERE key = 'cursor_high_water_mark';
		UPDATE usage_cache_metadata SET value = '1'
		WHERE key IN ('next_install_revision', 'next_rollup_install_revision')`)
	require.NoError(tb, err)
}

type benchmarkDailyUsageExpectation struct {
	days          int
	input         int
	output        int
	cacheCreation int
	cacheRead     int
}

func benchmarkDailyUsageSnapshotWindow(
	b *testing.B,
	d *DB,
	from string,
	want benchmarkDailyUsageExpectation,
) {
	b.Helper()
	ctx := context.Background()
	filter := UsageFilter{
		From: from, To: "2024-07-30", Timezone: "UTC",
		SkipSessionCounts: true,
	}

	result, err := d.GetDailyUsage(ctx, filter)
	require.NoError(b, err)
	require.Len(b, result.Daily, want.days)
	assert.Equal(b, want.input, result.Totals.InputTokens)
	assert.Equal(b, want.output, result.Totals.OutputTokens)
	assert.Equal(b, want.cacheCreation, result.Totals.CacheCreationTokens)
	assert.Equal(b, want.cacheRead, result.Totals.CacheReadTokens)
	assert.Zero(b, result.SessionCounts.Total)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := d.GetDailyUsage(ctx, filter)
		if err != nil {
			b.Fatalf("GetDailyUsage: %v", err)
		}
	}
}

// seedDailyUsageSnapshotBenchmark adds 100,000 base messages and 3,320
// lower-output snapshots across 500 sessions and 60 days.
func seedDailyUsageSnapshotBenchmark(tb testing.TB, d *DB) {
	tb.Helper()
	origLog := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(origLog)

	require.NoError(tb, d.UpsertModelPricing([]ModelPricing{
		{ModelPattern: "claude-sonnet-4-20250514",
			InputPerMTok: money.MustParseDollars("3.0"), OutputPerMTok: money.MustParseDollars("15.0"),
			CacheCreationPerMTok: money.MustParseDollars("3.75"), CacheReadPerMTok: money.MustParseDollars("0.30")},
		{ModelPattern: "claude-opus-4-20250514",
			InputPerMTok: money.MustParseDollars("15.0"), OutputPerMTok: money.MustParseDollars("75.0"),
			CacheCreationPerMTok: money.MustParseDollars("18.75"), CacheReadPerMTok: money.MustParseDollars("1.50")},
		{ModelPattern: "gpt-5",
			InputPerMTok: money.MustParseDollars("2.5"), OutputPerMTok: money.MustParseDollars("10.0"),
			CacheCreationPerMTok: money.MustParseDollars("2.5"), CacheReadPerMTok: money.MustParseDollars("0.25")},
		{ModelPattern: "gemini-2.5-pro",
			InputPerMTok: money.MustParseDollars("1.25"), OutputPerMTok: money.MustParseDollars("5.0"),
			CacheCreationPerMTok: money.MustParseDollars("1.25"), CacheReadPerMTok: money.MustParseDollars("0.125")},
	}))

	projects := []string{
		"agentsview", "quokka", "arrow-rs", "side-quests",
		"infrastructure", "blog", "experiments", "docs",
		"dotfiles", "playground",
	}
	agents := []string{"claude", "codex", "openhands"}
	claudeModels := []string{
		"claude-sonnet-4-20250514", "claude-opus-4-20250514",
	}
	otherModels := []string{"gpt-5", "gemini-2.5-pro"}

	const sessionCount = 500
	const msgsPerSession = 200
	const snapshotEvery = 10
	tokenUsage := jsontext.Value(
		`{"input_tokens":1200,"output_tokens":480,` +
			`"cache_creation_input_tokens":300,` +
			`"cache_read_input_tokens":2400}`)
	snapshotTokenUsage := jsontext.Value(
		`{"input_tokens":1200,"output_tokens":120,` +
			`"cache_creation_input_tokens":300,` +
			`"cache_read_input_tokens":2400}`)
	startTime, err := time.Parse(time.RFC3339, "2024-06-01T00:00:00Z")
	require.NoError(tb, err)

	for i := range sessionCount {
		id := "bench-sess-" + strconv.Itoa(i)
		agent := agents[i%len(agents)]
		day := startTime.AddDate(0, 0, i%60)
		msgs := make([]Message, 0, msgsPerSession+msgsPerSession/snapshotEvery)
		for j := range msgsPerSession {
			ts := day.Add(time.Duration(j) * time.Minute)
			msg := Message{
				SessionID: id, Ordinal: len(msgs), Role: "assistant",
				Timestamp:  ts.Format(time.RFC3339),
				Model:      otherModels[(i+j)%len(otherModels)],
				TokenUsage: tokenUsage,
			}
			if agent == "claude" {
				msg.Model = claudeModels[(i+j)%len(claudeModels)]
				msg.ClaudeMessageID = benchClaudeID("msg", i, j)
				msg.ClaudeRequestID = benchClaudeID("req", i, j)
				msg.OutputTokens = 480
				msg.HasOutputTokens = true
			}
			msgs = append(msgs, msg)

			prev := i - len(agents)
			if agent == "claude" && prev >= 0 && j%snapshotEvery == 0 {
				prevDay := startTime.AddDate(0, 0, prev%60)
				msgs = append(msgs, Message{
					SessionID: id, Ordinal: len(msgs), Role: "assistant",
					Timestamp: prevDay.Add(time.Duration(j)*time.Minute +
						30*time.Second).Format(time.RFC3339),
					Model:           claudeModels[(prev+j)%len(claudeModels)],
					TokenUsage:      snapshotTokenUsage,
					ClaudeMessageID: benchClaudeID("msg", prev, j),
					ClaudeRequestID: benchClaudeID("req", prev, j),
					OutputTokens:    120, HasOutputTokens: true,
				})
			}
		}

		require.NoError(tb, d.UpsertSession(Session{
			ID: id, Project: projects[i%len(projects)],
			Machine: defaultMachine, Agent: agent,
			MessageCount: len(msgs), StartedAt: new(day.Format(time.RFC3339)),
		}))
		require.NoError(tb, d.InsertMessages(msgs))
	}

	var messageRows, recordedMessages, duplicateRequests int
	require.NoError(tb, d.rawWriter().QueryRow(
		`SELECT COUNT(*) FROM messages`).Scan(&messageRows))
	require.NoError(tb, d.rawWriter().QueryRow(
		`SELECT SUM(message_count) FROM sessions`).Scan(&recordedMessages))
	require.NoError(tb, d.rawWriter().QueryRow(`
		SELECT COUNT(*) FROM (
			SELECT claude_message_id, claude_request_id
			FROM messages
			WHERE claude_message_id != '' AND claude_request_id != ''
			GROUP BY claude_message_id, claude_request_id
			HAVING COUNT(*) > 1
		)`).Scan(&duplicateRequests))
	require.Equal(tb, 103_320, messageRows)
	require.Equal(tb, 103_320, recordedMessages)
	require.Equal(tb, 3_320, duplicateRequests)
}

// seedLongLivedUsageBenchmark adds sessions whose full-history extraction is
// much larger than their 30-day query contribution. This guards the cold-fill
// cost that activity-bound candidate discovery alone cannot predict.
func seedLongLivedUsageBenchmark(tb testing.TB, database *DB) {
	tb.Helper()
	const (
		sessionCount = 5
		messageCount = 366
	)
	start, err := time.Parse(time.RFC3339, "2023-07-31T12:00:00Z")
	require.NoError(tb, err)
	usage := jsontext.Value(`{"input_tokens":100,"output_tokens":50}`)
	for sessionIndex := range sessionCount {
		sessionID := "bench-long-lived-" + strconv.Itoa(sessionIndex)
		messages := make([]Message, messageCount)
		for ordinal := range messageCount {
			messages[ordinal] = Message{
				SessionID: sessionID, Ordinal: ordinal, Role: "assistant",
				Timestamp: start.AddDate(0, 0, ordinal).Format(time.RFC3339),
				Model:     "gpt-5", TokenUsage: usage,
			}
		}
		startedAt := start.Format(time.RFC3339)
		require.NoError(tb, database.UpsertSession(Session{
			ID: sessionID, Project: "long-lived", Machine: defaultMachine,
			Agent: "codex", StartedAt: &startedAt, MessageCount: messageCount,
		}))
		require.NoError(tb, database.InsertMessages(messages))
	}
}

func benchClaudeID(kind string, session, ordinal int) string {
	return kind + "-bench-" + strconv.Itoa(session) + "-" +
		strconv.Itoa(ordinal)
}

func TestGetDailyUsage_PricingPrecedence(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{
		{
			ModelPattern: "db-only-model",
			InputPerMTok: money.MustParseDollars("1.0"), OutputPerMTok: money.MustParseDollars("4.0"),
		},
		{
			ModelPattern: "custom-overrides-model",
			InputPerMTok: money.MustParseDollars("1.0"), OutputPerMTok: money.MustParseDollars("4.0"),
		},
		{
			ModelPattern: "db-model",
			InputPerMTok: money.MustParseDollars("3.0"), OutputPerMTok: money.MustParseDollars("10.0"),
		},
	}), "UpsertModelPricing")
	d.SetCustomPricing(map[string]config.CustomModelRate{
		"custom-overrides-model": {InputMicrodollarsPerMTok: money.MustParseDollars("2.0").Microdollars, OutputMicrodollarsPerMTok: money.MustParseDollars("8.0").Microdollars},
		"my-custom-model":        {InputMicrodollarsPerMTok: money.MustParseDollars("1.5").Microdollars, OutputMicrodollarsPerMTok: money.MustParseDollars("6.0").Microdollars},
		"other-model":            {InputMicrodollarsPerMTok: money.MustParseDollars("99.0").Microdollars, OutputMicrodollarsPerMTok: money.MustParseDollars("99.0").Microdollars},
	})

	tests := []struct {
		name     string
		model    string
		input    int // input tokens
		output   int // output tokens
		wantCost money.Money
	}{
		{
			name:     "db pricing only",
			model:    "db-only-model",
			input:    1_000_000,
			output:   100_000,
			wantCost: money.MustParseDollars("1.4"), // 1M*$1/M + 100k*$4/M
		},
		{
			name:     "custom overrides db for same model",
			model:    "custom-overrides-model",
			input:    1_000_000,
			output:   100_000,
			wantCost: money.MustParseDollars("2.8"), // 1M*$2/M + 100k*$8/M
		},
		{
			name:     "custom for unknown model, no db entry",
			model:    "my-custom-model",
			input:    500_000,
			output:   50_000,
			wantCost: money.MustParseDollars("1.05"), // 500k*$1.5/M + 50k*$6/M
		},
		{
			name:     "no pricing at all yields zero cost",
			model:    "unknown-model",
			input:    1_000_000,
			output:   100_000,
			wantCost: money.Money{},
		},
		{
			name:     "custom only affects targeted model",
			model:    "db-model",
			input:    1_000_000,
			output:   100_000,
			wantCost: money.MustParseDollars("4"), // 1M*$3/M + 100k*$10/M -- db rates, not custom
		},
	}

	for i, tt := range tests {
		sessionID := "pricing-" + strconv.Itoa(i)
		insertSession(t, d, sessionID, "proj", func(s *Session) {
			s.StartedAt = new("2024-06-15T10:00:00Z")
		})
		insertMessages(t, d, Message{
			SessionID: sessionID,
			Ordinal:   0,
			Role:      "assistant",
			Timestamp: "2024-06-15T10:30:00Z",
			Model:     tt.model,
			TokenUsage: jsontext.Value(
				`{"input_tokens":` + strconv.Itoa(tt.input) +
					`,"output_tokens":` + strconv.Itoa(tt.output) + `}`,
			),
		})
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := d.GetDailyUsage(ctx, UsageFilter{
				From:  "2024-06-01",
				To:    "2024-06-30",
				Model: tt.model,
			})
			requireNoError(t, err, "GetDailyUsage")

			assert.Equal(t, tt.input, result.Totals.InputTokens,
				"InputTokens")
			assert.Equal(t, tt.output, result.Totals.OutputTokens,
				"OutputTokens")
			assert.Equal(t, tt.wantCost, result.Totals.TotalCost,
				"TotalCost")
		})
	}
}

func seedOpusPricing(t *testing.T, d *DB) {
	t.Helper()
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern: "claude-opus-4-6",
		InputPerMTok: money.MustParseDollars("5.0"), OutputPerMTok: money.MustParseDollars("25.0"),
		CacheCreationPerMTok: money.MustParseDollars("6.25"), CacheReadPerMTok: money.MustParseDollars("0.5"),
	}}), "UpsertModelPricing")
}

func TestGetSessionUsage_PricedModel(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedOpusPricing(t, d)

	insertSession(t, d, "claude:s1", "proj", func(s *Session) {
		s.Agent = "claude-code"
		s.StartedAt = new("2026-05-20T10:00:00Z")
		s.TotalOutputTokens = 500
		s.PeakContextTokens = 1200
		s.HasTotalOutputTokens = true
		s.HasPeakContextTokens = true
	})
	insertMessages(t, d, Message{
		SessionID: "claude:s1", Ordinal: 0, Role: "assistant",
		Timestamp: "2026-05-20T10:30:00Z", Model: "claude-opus-4-6",
		TokenUsage: jsontext.Value(
			`{"input_tokens":1000,"output_tokens":500}`),
	})

	u, err := d.GetSessionUsage(ctx, "claude:s1", true)
	requireNoError(t, err, "GetSessionUsage")
	require.NotNil(t, u, "usage is nil")
	require.True(t, u.HasCost, "HasCost = false, want true")
	assert.Equal(t, money.MustParseDollars("0.0175"), u.Cost, "Cost")
	// cost_usd is a deprecated compatibility alias for
	// cost.microdollars/1e6 (see db.CostUSDFromCost).
	require.NotNil(t, u.CostUSD, "CostUSD must be present when HasCost")
	assert.InDelta(t, 0.0175, *u.CostUSD, 1e-9, "CostUSD")
	assert.Equal(t, 500, u.TotalOutputTokens,
		"TotalOutputTokens want 500")
	assert.Equal(t, 1200, u.PeakContextTokens,
		"PeakContextTokens want 1200")
	assert.Equal(t, []string{"claude-opus-4-6"}, u.Models, "Models")
	assert.Empty(t, u.UnpricedModels, "UnpricedModels")
	require.Len(t, u.Breakdown, 1, "Breakdown")
	entry := u.Breakdown[0]
	assert.Equal(t, 1, entry.Ordinal, "Breakdown ordinal")
	require.NotNil(t, entry.MessageOrdinal, "MessageOrdinal")
	assert.Equal(t, 0, *entry.MessageOrdinal, "MessageOrdinal")
	assert.Equal(t, "message", entry.Source, "Source")
	assert.Equal(t, "Prompt 1", entry.Label, "Label")
	assert.Equal(t, "2026-05-20T10:30:00Z", entry.Timestamp, "Timestamp")
	assert.Equal(t, "claude-opus-4-6", entry.Model, "Model")
	assert.Equal(t, 1000, entry.InputTokens, "InputTokens")
	assert.Equal(t, 500, entry.OutputTokens, "OutputTokens")
	assert.Equal(t, money.MustParseDollars("0.0175"), entry.Cost, "entry Cost")
	assert.True(t, entry.HasCost, "entry HasCost")
}

func TestGetSessionUsage_UnpricedModel(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "claude:s2", "proj", func(s *Session) {
		s.Agent = "claude-code"
		s.StartedAt = new("2026-05-20T10:00:00Z")
		s.TotalOutputTokens = 500
		s.HasTotalOutputTokens = true
	})
	insertMessages(t, d, Message{
		SessionID: "claude:s2", Ordinal: 0, Role: "assistant",
		Timestamp: "2026-05-20T10:30:00Z", Model: "local-llama-99",
		TokenUsage: jsontext.Value(
			`{"input_tokens":1000,"output_tokens":500}`),
	})

	u, err := d.GetSessionUsage(ctx, "claude:s2", true)
	requireNoError(t, err, "GetSessionUsage")
	assert.False(t, u.HasCost, "HasCost = true, want false (unpriced)")
	assert.Equal(t, money.Money{}, u.Cost, "Cost")
	assert.Equal(t, []string{"local-llama-99"}, u.UnpricedModels,
		"UnpricedModels")
}

func TestGetSessionUsage_MixedPricedUnpriced(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedOpusPricing(t, d)
	insertSession(t, d, "claude:s3", "proj", func(s *Session) {
		s.Agent = "claude-code"
		s.StartedAt = new("2026-05-20T10:00:00Z")
	})
	insertMessages(t, d,
		Message{
			SessionID: "claude:s3", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-05-20T10:30:00Z", Model: "claude-opus-4-6",
			TokenUsage: jsontext.Value(
				`{"input_tokens":1000,"output_tokens":500}`),
		},
		Message{
			SessionID: "claude:s3", Ordinal: 1, Role: "assistant",
			Timestamp: "2026-05-20T10:31:00Z", Model: "local-llama-99",
			TokenUsage: jsontext.Value(
				`{"input_tokens":1000,"output_tokens":500}`),
		},
	)

	u, err := d.GetSessionUsage(ctx, "claude:s3", true)
	requireNoError(t, err, "GetSessionUsage")
	assert.False(t, u.HasCost, "HasCost = true, want false (mixed)")
	assert.Equal(t, money.Money{}, u.Cost, "Cost")
	assert.Equal(t, []string{"local-llama-99"}, u.UnpricedModels,
		"UnpricedModels")
}

func TestGetSessionUsage_ExplicitCostOnly(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "hermes:s4", "proj", func(s *Session) {
		s.Agent = "hermes"
		s.StartedAt = new("2026-05-20T10:00:00Z")
	})
	cost := money.MustParseDollars("0.02")
	require.NoError(t, d.ReplaceSessionUsageEvents("hermes:s4", []UsageEvent{{
		SessionID: "hermes:s4", Source: "session", Model: "gpt-5.4",
		InputTokens: 100, OutputTokens: 50,
		Cost: &cost, CostStatus: "estimated", CostSource: "hermes",
		OccurredAt: "2026-05-20T10:05:00Z", DedupKey: "session:hermes:s4",
	}}), "ReplaceSessionUsageEvents")

	u, err := d.GetSessionUsage(ctx, "hermes:s4", true)
	requireNoError(t, err, "GetSessionUsage")
	assert.True(t, u.HasCost, "HasCost = false, want true (explicit cost)")
	assert.Equal(t, money.MustParseDollars("0.02"), u.Cost, "Cost")
	assert.Equal(t, []string{"gpt-5.4"}, u.Models, "Models")
	require.Len(t, u.Breakdown, 1, "Breakdown")
	entry := u.Breakdown[0]
	assert.Nil(t, entry.MessageOrdinal, "MessageOrdinal")
	assert.Equal(t, "session", entry.Source, "Source")
	assert.Equal(t, "session", entry.Label, "Label")
	assert.Equal(t, 100, entry.InputTokens, "InputTokens")
	assert.Equal(t, 50, entry.OutputTokens, "OutputTokens")
	assert.True(t, entry.HasCost, "entry HasCost")
	assert.Equal(t, money.MustParseDollars("0.02"), entry.Cost, "entry Cost")
}

func TestGetSessionUsage_BreakdownOrderingAndBuckets(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedOpusPricing(t, d)
	insertSession(t, d, "claude:s-breakdown", "proj", func(s *Session) {
		s.Agent = "claude-code"
		s.StartedAt = new("2026-05-20T10:00:00Z")
		s.TotalOutputTokens = 650
		s.PeakContextTokens = 1500
		s.HasTotalOutputTokens = true
		s.HasPeakContextTokens = true
	})
	insertMessages(t, d,
		Message{
			SessionID: "claude:s-breakdown", Ordinal: 1, Role: "assistant",
			Timestamp: "2026-05-20T10:31:00Z", Model: "claude-opus-4-6",
			TokenUsage: jsontext.Value(
				`{"input_tokens":100,"output_tokens":50,` +
					`"cache_creation_input_tokens":10,` +
					`"cache_read_input_tokens":20}`),
		},
		Message{
			SessionID: "claude:s-breakdown", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-05-20T10:30:00Z", Model: "claude-opus-4-6",
			TokenUsage: jsontext.Value(
				`{"input_tokens":200,"output_tokens":60,` +
					`"cache_creation_input_tokens":0,` +
					`"cache_read_input_tokens":40}`),
		},
	)

	u, err := d.GetSessionUsage(ctx, "claude:s-breakdown", true)
	requireNoError(t, err, "GetSessionUsage")
	require.Len(t, u.Breakdown, 2, "Breakdown")
	assert.Equal(t, 650, u.TotalOutputTokens, "TotalOutputTokens")
	assert.Equal(t, 1500, u.PeakContextTokens, "PeakContextTokens")

	first := u.Breakdown[0]
	require.NotNil(t, first.MessageOrdinal, "first MessageOrdinal")
	assert.Equal(t, 0, *first.MessageOrdinal, "first MessageOrdinal")
	assert.Equal(t, "Prompt 1", first.Label, "first Label")
	assert.Equal(t, 200, first.InputTokens, "first InputTokens")
	assert.Equal(t, 60, first.OutputTokens, "first OutputTokens")
	assert.Equal(t, 0, first.CacheCreationInputTokens, "first CacheCreationInputTokens")
	assert.Equal(t, 40, first.CacheReadInputTokens, "first CacheReadInputTokens")

	second := u.Breakdown[1]
	require.NotNil(t, second.MessageOrdinal, "second MessageOrdinal")
	assert.Equal(t, 1, *second.MessageOrdinal, "second MessageOrdinal")
	assert.Equal(t, "Prompt 2", second.Label, "second Label")
	assert.Equal(t, 100, second.InputTokens, "second InputTokens")
	assert.Equal(t, 50, second.OutputTokens, "second OutputTokens")
	assert.Equal(t, 10, second.CacheCreationInputTokens, "second CacheCreationInputTokens")
	assert.Equal(t, 20, second.CacheReadInputTokens, "second CacheReadInputTokens")

	breakdownCost := money.MustAdd(first.Cost, second.Cost)
	assert.Equal(t, breakdownCost, u.Cost,
		"breakdown cost should sum to total cost")
}

func TestGetSessionUsage_DedupesDuplicateClaudeRows(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedOpusPricing(t, d)
	insertSession(t, d, "claude:s6", "proj", func(s *Session) {
		s.Agent = "claude-code"
		s.StartedAt = new("2026-05-20T10:00:00Z")
	})
	// Two rows sharing the same claude message+request id (a
	// fork/replay) must be counted once, not doubled, with the latest copy
	// supplying the surviving row.
	insertMessages(t, d,
		Message{
			SessionID: "claude:s6", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-05-20T10:30:00Z", Model: "claude-opus-4-6",
			ClaudeMessageID: "msg-1", ClaudeRequestID: "req-1",
			TokenUsage: jsontext.Value(`{"input_tokens":1000,"output_tokens":500}`),
		},
		Message{
			SessionID: "claude:s6", Ordinal: 1, Role: "assistant",
			Timestamp: "2026-05-20T10:31:00Z", Model: "claude-opus-4-6",
			ClaudeMessageID: "msg-1", ClaudeRequestID: "req-1",
			TokenUsage: jsontext.Value(`{"input_tokens":1000,"output_tokens":500}`),
		},
	)
	u, err := d.GetSessionUsage(ctx, "claude:s6", true)
	requireNoError(t, err, "GetSessionUsage")
	// One row priced at 1000*5/1e6 + 500*25/1e6 = 0.0175; deduped, not 0.035.
	assert.Equal(t, money.MustParseDollars("0.0175"), u.Cost, "Cost want 0.0175 (deduped)")
	assert.True(t, u.HasCost, "HasCost = false, want true")
	require.Len(t, u.Breakdown, 1, "Breakdown")
	require.NotNil(t, u.Breakdown[0].MessageOrdinal, "MessageOrdinal")
	assert.Equal(t, 1, *u.Breakdown[0].MessageOrdinal, "MessageOrdinal")
}

func TestGetSessionUsage_PrefersCompleteClaudeSnapshot(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedOpusPricing(t, d)
	insertSession(t, d, "claude:streamed", "proj", func(s *Session) {
		s.Agent = "claude-code"
		s.StartedAt = new("2026-05-20T10:00:00Z")
		s.TotalOutputTokens = 636
		s.HasTotalOutputTokens = true
	})
	insertMessages(t, d,
		Message{
			SessionID: "claude:streamed", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-05-20T10:30:00Z", Model: "claude-opus-4-6",
			ClaudeMessageID: "msg-stream", ClaudeRequestID: "req-stream",
			TokenUsage: jsontext.Value(`{"input_tokens":1000,"output_tokens":5}`),
		},
		Message{
			SessionID: "claude:streamed", Ordinal: 1, Role: "assistant",
			Timestamp: "2026-05-20T10:31:00Z", Model: "claude-opus-4-6",
			ClaudeMessageID: "msg-stream", ClaudeRequestID: "req-stream",
			TokenUsage: jsontext.Value(`{"input_tokens":1000,"output_tokens":631}`),
		},
	)

	u, err := d.GetSessionUsage(ctx, "claude:streamed", true)
	requireNoError(t, err, "GetSessionUsage")
	assert.Equal(t, 631, u.TotalOutputTokens)
	assert.Equal(t, money.MustParseDollars("0.020775"), u.Cost)
	require.Len(t, u.Breakdown, 1)
	assert.Equal(t, 631, u.Breakdown[0].OutputTokens)
	require.NotNil(t, u.Breakdown[0].MessageOrdinal)
	assert.Equal(t, 1, *u.Breakdown[0].MessageOrdinal)
}

func TestGetSessionUsage_DedupesBySourceUUIDWhenClaudePairIncomplete(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedOpusPricing(t, d)
	insertSession(t, d, "claude:s7", "proj", func(s *Session) {
		s.Agent = "claude-code"
		s.StartedAt = new("2026-05-20T10:00:00Z")
	})
	insertMessages(t, d,
		Message{
			SessionID: "claude:s7", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-05-20T10:30:00Z", Model: "claude-opus-4-6",
			ClaudeMessageID: "msg-1", SourceUUID: "source-1",
			TokenUsage: jsontext.Value(`{"input_tokens":1000,"output_tokens":500}`),
		},
		Message{
			SessionID: "claude:s7", Ordinal: 1, Role: "assistant",
			Timestamp: "2026-05-20T10:31:00Z", Model: "claude-opus-4-6",
			ClaudeMessageID: "msg-1", SourceUUID: "source-1",
			TokenUsage: jsontext.Value(`{"input_tokens":1000,"output_tokens":500}`),
		},
	)
	u, err := d.GetSessionUsage(ctx, "claude:s7", true)
	requireNoError(t, err, "GetSessionUsage")
	assert.Equal(t, money.MustParseDollars("0.0175"), u.Cost, "Cost want 0.0175 (deduped)")
	assert.True(t, u.HasCost, "HasCost = false, want true")
}

func TestGetSessionUsage_NoTokenRowsKeepsMetadata(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "claude:s5", "proj", func(s *Session) {
		s.Agent = "claude-code"
		s.StartedAt = new("2026-05-20T10:00:00Z")
		s.TotalOutputTokens = 700
		s.PeakContextTokens = 3000
		s.HasTotalOutputTokens = true
		s.HasPeakContextTokens = true
	})

	u, err := d.GetSessionUsage(ctx, "claude:s5", true)
	requireNoError(t, err, "GetSessionUsage")
	require.NotNil(t, u, "usage is nil")
	assert.Equal(t, 700, u.TotalOutputTokens,
		"TotalOutputTokens want 700")
	assert.Equal(t, 3000, u.PeakContextTokens,
		"PeakContextTokens want 3000")
	assert.True(t, u.HasTokenData, "HasTokenData = false, want true")
	assert.False(t, u.HasCost, "HasCost = true, want false (no cost rows)")
	assert.Nil(t, u.CostUSD,
		"CostUSD must be omitted when HasCost is false")
	assert.NotNil(t, u.Models, "Models = nil, want non-nil empty slice")
	assert.Empty(t, u.Breakdown, "Breakdown")
}

func TestGetSessionUsage_NotFound(t *testing.T) {
	d := testDB(t)
	u, err := d.GetSessionUsage(context.Background(), "nope:x", true)
	requireNoError(t, err, "GetSessionUsage")
	assert.Nil(t, u, "usage")
}

func TestGetSessionUsage_AICreditsCapability(t *testing.T) {
	parsertest.StubAgentDefs(t, parser.AgentDef{
		Type:        parser.AgentType("ai-credit-agent"),
		DisplayName: "AI Credit Agent",
		Usage: parser.UsageCapabilities{
			AICreditsDenominated: true,
		},
	})

	d := testDB(t)
	ctx := context.Background()
	seedOpusPricing(t, d)

	insertSession(t, d, "ai-credit-agent:s1", "proj", func(s *Session) {
		s.Agent = "ai-credit-agent"
		s.StartedAt = new("2026-05-20T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "ai-credit-agent:s1",
		Ordinal:   0,
		Role:      "assistant",
		Timestamp: "2026-05-20T10:30:00Z",
		Model:     "claude-opus-4-6",
		TokenUsage: jsontext.Value(
			`{"input_tokens":1000,"output_tokens":500}`),
	})

	u, err := d.GetSessionUsage(ctx, "ai-credit-agent:s1", true)
	requireNoError(t, err, "GetSessionUsage")
	require.NotNil(t, u, "usage is nil")
	assert.True(t, u.HasCost, "HasCost = false, want true")
	assert.Equal(t, money.MustParseDollars("0.0175"), u.Cost, "Cost")
	assert.InDelta(t, 1.75, u.AICredits, 1e-9, "AICredits")
}

func TestCopilotReportedCostSuppressesSessionEstimates(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedOpusPricing(t, d)

	for _, id := range []string{"copilot:reported", "copilot:fallback"} {
		insertSession(t, d, id, "proj", func(s *Session) {
			s.Agent = "copilot"
			s.StartedAt = new("2026-05-20T10:00:00Z")
		})
	}
	reportedCost := money.MustParseDollars("0.0275")
	require.NoError(t, d.ReplaceSessionUsageEvents("copilot:reported", []UsageEvent{
		{
			Source: "shutdown", Model: "claude-opus-4-6",
			InputTokens: 1000, OutputTokens: 500,
			OccurredAt: "2026-05-20T10:10:00Z", DedupKey: "segment-1",
		},
		{
			Source: "shutdown", Model: "claude-opus-4-6",
			InputTokens: 1000, OutputTokens: 500,
			Cost: &reportedCost, CostStatus: "exact",
			CostSource: CopilotReportedCostSource,
			OccurredAt: "2026-05-21T10:20:00Z", DedupKey: "segment-2",
		},
	}))
	require.NoError(t, d.ReplaceSessionUsageEvents("copilot:fallback", []UsageEvent{{
		Source: "shutdown", Model: "claude-opus-4-6",
		InputTokens: 1000, OutputTokens: 500,
		OccurredAt: "2026-05-20T10:30:00Z", DedupKey: "fallback",
	}}))

	session, err := d.GetSessionUsage(ctx, "copilot:reported", true)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.True(t, session.HasCost)
	assert.Equal(t, reportedCost, session.Cost)
	assert.InDelta(t, 2.75, session.AICredits, 1e-9)
	require.Len(t, session.Breakdown, 2)
	assert.Equal(t, money.MustParseDollars("0.01375"), session.Breakdown[0].Cost)
	assert.Equal(t, money.MustParseDollars("0.01375"), session.Breakdown[1].Cost)
	assert.Equal(t, session.Cost,
		money.MustAdd(session.Breakdown[0].Cost, session.Breakdown[1].Cost))

	daily, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2026-05-20", To: "2026-05-21", Timezone: "UTC",
	})
	require.NoError(t, err)
	require.Len(t, daily.Daily, 2)
	assert.Equal(t, money.MustParseDollars("0.03125"), daily.Daily[0].TotalCost)
	assert.Equal(t, money.MustParseDollars("0.01375"), daily.Daily[1].TotalCost)
	for _, day := range daily.Daily {
		require.Len(t, day.ModelBreakdowns, 1)
		assert.Equal(t, day.TotalCost, day.ModelBreakdowns[0].Cost)
	}
	assert.Equal(t, money.MustParseDollars("0.045"), daily.Totals.TotalCost)
	assert.InDelta(t, 4.5, daily.Totals.CopilotAICredits, 1e-9,
		"credits derive from the authoritative reported cost")
	require.NotNil(t, daily.Pricing)
	assert.Equal(t, export.CostSourceMixed, daily.Pricing.CostSource,
		"authoritative reported cost must surface in pricing provenance")
	assert.Equal(t, export.CostSourceComputed,
		daily.Pricing.Models["claude-opus-4-6"].CostSource)

	earlyDay, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2026-05-20", To: "2026-05-20", Timezone: "UTC",
	})
	require.NoError(t, err)
	assert.Equal(t, money.MustParseDollars("0.035"), earlyDay.Totals.TotalCost)

	modelFiltered, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2026-05-20", To: "2026-05-21", Timezone: "UTC",
		Model: "claude-opus-4-6",
	})
	require.NoError(t, err)
	assert.Equal(t, money.MustParseDollars("0.0525"), modelFiltered.Totals.TotalCost)
	assert.InDelta(t, 5.25, modelFiltered.Totals.CopilotAICredits, 1e-9,
		"model-filtered credits track the estimated totals")
	require.NotNil(t, modelFiltered.Pricing)
	assert.Equal(t, export.CostSourceComputed, modelFiltered.Pricing.CostSource,
		"model-filtered totals stay estimated, so provenance stays computed")
}

func TestCopilotReportedZeroCostSuppressesEstimate(t *testing.T) {
	d := testDB(t)
	seedOpusPricing(t, d)
	insertSession(t, d, "copilot:reported-zero", "proj", func(s *Session) {
		s.Agent = "copilot"
		s.StartedAt = new("2026-05-21T10:00:00Z")
	})
	zeroCost := money.Money{}
	require.NoError(t, d.ReplaceSessionUsageEvents(
		"copilot:reported-zero",
		[]UsageEvent{{
			Source: "shutdown", Model: "claude-opus-4-6",
			InputTokens: 1000, OutputTokens: 500,
			Cost: &zeroCost, CostStatus: "exact",
			CostSource: CopilotReportedCostSource,
			OccurredAt: "2026-05-21T10:10:00Z", DedupKey: "final",
		}},
	))

	session, err := d.GetSessionUsage(
		context.Background(), "copilot:reported-zero", false)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.True(t, session.HasCost)
	assert.Zero(t, session.Cost)

	daily, err := d.GetDailyUsage(context.Background(), UsageFilter{
		From: "2026-05-21", To: "2026-05-21", Timezone: "UTC",
	})
	require.NoError(t, err)
	assert.Zero(t, daily.Totals.TotalCost)
	require.Len(t, daily.Daily, 1)
	require.Len(t, daily.Daily[0].ModelBreakdowns, 1)
	assert.Zero(t, daily.Daily[0].ModelBreakdowns[0].Cost)
}

// TestGetDailyUsage_CopilotAICredits verifies AI credits are computed from
// agents with the parser AI-credit capability: costUSD / 0.01.
func TestGetDailyUsage_CopilotAICredits(t *testing.T) {
	parsertest.StubAgentDefs(t, parser.AgentDef{
		Type:        parser.AgentType("ai-credit-agent"),
		DisplayName: "AI Credit Agent",
		Usage: parser.UsageCapabilities{
			AICreditsDenominated: true,
		},
	})

	d := testDB(t)
	ctx := context.Background()

	require.NoError(t, d.UpsertModelPricing([]ModelPricing{
		{
			ModelPattern:         "gpt-4",
			InputPerMTok:         money.MustParseDollars("15.0"),
			OutputPerMTok:        money.MustParseDollars("60.0"),
			CacheCreationPerMTok: money.MustParseDollars("15.0"),
			CacheReadPerMTok:     money.MustParseDollars("6.0"),
		},
		{
			ModelPattern:         "claude-opus-4-6",
			InputPerMTok:         money.MustParseDollars("3.0"),
			OutputPerMTok:        money.MustParseDollars("15.0"),
			CacheCreationPerMTok: money.MustParseDollars("3.75"),
			CacheReadPerMTok:     money.MustParseDollars("0.30"),
		},
	}))

	tests := []struct {
		name        string
		sessionID   string
		agent       string
		model       string
		inputRate   money.Money
		outputRate  money.Money
		wantCredits bool
	}{
		{
			name:        "copilot credits computed",
			sessionID:   "copilot:aicredits",
			agent:       "copilot",
			model:       "gpt-4",
			inputRate:   money.MustParseDollars("15"),
			outputRate:  money.MustParseDollars("60"),
			wantCredits: true,
		},
		{
			name:        "non copilot capability credits computed",
			sessionID:   "ai-credit-agent:aicredits",
			agent:       "ai-credit-agent",
			model:       "gpt-4",
			inputRate:   money.MustParseDollars("15"),
			outputRate:  money.MustParseDollars("60"),
			wantCredits: true,
		},
		{
			name:       "non copilot has no credits",
			sessionID:  "claude:nocredits",
			agent:      "claude-code",
			model:      "claude-opus-4-6",
			inputRate:  money.MustParseDollars("3"),
			outputRate: money.MustParseDollars("15"),
		},
	}

	for _, tt := range tests {
		insertSession(t, d, tt.sessionID, "proj", func(s *Session) {
			s.Agent = tt.agent
			s.StartedAt = new("2024-06-15T10:00:00Z")
			s.EndedAt = new("2024-06-15T11:00:00Z")
		})
		insertMessages(t, d, Message{
			SessionID: tt.sessionID,
			Ordinal:   0,
			Role:      "assistant",
			Timestamp: "2024-06-15T10:30:00Z",
			Model:     tt.model,
			TokenUsage: jsontext.Value(`{
				"input_tokens": 1000,
				"output_tokens": 500
			}`),
		})
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := d.GetDailyUsage(ctx, UsageFilter{
				From:  "2024-06-01",
				To:    "2024-06-30",
				Agent: tt.agent,
			})
			requireNoError(t, err, "GetDailyUsage")

			wantCost, err := money.CostPerMillion([]money.RatedTokens{
				{Tokens: 1000, Rate: tt.inputRate},
				{Tokens: 500, Rate: tt.outputRate},
			})
			require.NoError(t, err)
			wantCredits := 0.0
			if tt.wantCredits {
				wantCredits = float64(wantCost.Microdollars) / 10_000
			}
			assert.Equal(t, wantCost, result.Totals.TotalCost,
				"TotalCost")
			assert.InDelta(t, wantCredits, result.Totals.CopilotAICredits,
				1e-6, "CopilotAICredits")
		})
	}
}

func TestAICreditsFromCost(t *testing.T) {
	cases := []struct {
		name  string
		agent string
		cost  money.Money
		want  float64
	}{
		{"copilot converts at a cent per credit", "copilot", money.MustParseDollars("0.42"), 42},
		{"zero cost yields zero credits", "copilot", money.Money{}, 0},
		{"non-credit agent yields zero", "claude", money.MustParseDollars("3.5"), 0},
		{"unknown agent yields zero", "unknown-agent", money.MustParseDollars("3.5"), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.InDelta(t, tc.want,
				AICreditsFromCost(tc.agent, tc.cost), 1e-9)
		})
	}
}

// TestCostUSDFromCost pins the deprecated cost_usd compatibility
// conversion (see SessionUsage.CostUSD): present and equal to
// microdollars/1e6 exactly when hasCost is true, nil otherwise. Every
// backend (SQLite, PostgreSQL, DuckDB) and the subagent combiner call
// this single function, so this is the one place the arithmetic is
// pinned.
func TestCostUSDFromCost(t *testing.T) {
	cases := []struct {
		name    string
		hasCost bool
		cost    money.Money
		want    *float64
	}{
		{
			name:    "priced cost converts to dollars",
			hasCost: true,
			cost:    money.MustParseDollars("2.41"),
			want:    Ptr(2.41),
		},
		{
			name:    "zero cost with has_cost true still reports 0",
			hasCost: true,
			cost:    money.Money{},
			want:    Ptr(0.0),
		},
		{
			name:    "no cost omits the field",
			hasCost: false,
			cost:    money.MustParseDollars("2.41"),
			want:    nil,
		},
		{
			name:    "sub-cent cost preserves fractional dollars",
			hasCost: true,
			cost:    money.Money{Microdollars: 1},
			want:    Ptr(0.000001),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CostUSDFromCost(tc.hasCost, tc.cost)
			if tc.want == nil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.InDelta(t, *tc.want, *got, 1e-12)
		})
	}
}

// TestGetDailyUsage_CodebuffCostOnly pins the SQLite aggregator path
// for Codebuff/Freebuff's parser-emitted cost-only usage event. The
// Codebuff parser (internal/parser/codebuff.go) attributes the session
// cost to the agent template (e.g. "base2-deepseek") rather than the
// agent name so the per-model breakdown in the usage report stays
// granular. The row's template-attributed Model passes the
// usageEventEligibility filter (non-empty ue.model) and the authoritative
// reported Cost flows into TotalCost at the daily-usage level. The
// per-model and per-agent breakdown shapes are aggregator internals
// covered by other tests; this pins only the cost-flow contract.
func TestGetDailyUsage_CodebuffCostOnly(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "codebuff:cost-only", "proj", func(s *Session) {
		s.Agent = "codebuff"
		s.StartedAt = new("2026-07-15T10:00:00Z")
	})
	cost := money.MustParseDollars("0.05")
	require.NoError(t, d.ReplaceSessionUsageEvents(
		"codebuff:cost-only",
		[]UsageEvent{{
			Source:     "session",
			Model:      "base2-deepseek",
			Cost:       &cost,
			CostStatus: "reported",
			CostSource: "session",
			OccurredAt: "2026-07-15T10:05:00Z",
			DedupKey:   "session:codebuff:cost-only",
		}}))

	daily, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2026-07-15", To: "2026-07-15", Timezone: "UTC",
	})
	requireNoError(t, err, "GetDailyUsage")
	assert.Equal(t, cost, daily.Totals.TotalCost,
		"the codebuff reported cost must surface in daily TotalCost")
	require.Len(t, daily.Daily, 1,
		"the cost-only row should produce one daily entry")
	assert.Equal(t, cost, daily.Daily[0].TotalCost,
		"the day's TotalCost must include the reported cost")
}

// TestGetDailyUsage_KimiAliasPricing proves Kimi alias pricing end to end on
// SQLite: the date-ambiguous aliases
// (kimi-for-coding, daimon-kimi-code, daimon-kimi-messages) price each
// row by its timestamp — K2.6 rates before the 2026-07-19T00:00:00Z
// UTC cutoff and K3 rates at and after it. The explicit k2d6-agent alias stays
// on K2.6, while static k3/k3-agent rows price flat at K3.
func TestGetDailyUsage_KimiAliasPricing(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// K2.6 era rates (matching the moonshot/kimi-k2.6 snapshot row) and
	// K3 era rates (matching the static supplemental rows).
	requireNoError(t, d.UpsertModelPricing([]ModelPricing{
		{
			ModelPattern:     "moonshot/kimi-k2.6",
			InputPerMTok:     money.MustParseDollars("0.95"),
			OutputPerMTok:    money.MustParseDollars("4.0"),
			CacheReadPerMTok: money.MustParseDollars("0.16"),
		},
		{
			ModelPattern:     "kimi-k3",
			InputPerMTok:     money.MustParseDollars("3.00"),
			OutputPerMTok:    money.MustParseDollars("15.00"),
			CacheReadPerMTok: money.MustParseDollars("0.30"),
		},
		{
			ModelPattern:     "k3",
			InputPerMTok:     money.MustParseDollars("3.00"),
			OutputPerMTok:    money.MustParseDollars("15.00"),
			CacheReadPerMTok: money.MustParseDollars("0.30"),
		},
		{
			ModelPattern:     "k3-agent",
			InputPerMTok:     money.MustParseDollars("3.00"),
			OutputPerMTok:    money.MustParseDollars("15.00"),
			CacheReadPerMTok: money.MustParseDollars("0.30"),
		},
	}), "UpsertModelPricing")

	// Token mix per message: 1M input + 100k output + 1M cache read.
	// K2.6 cost: 0.95 + 0.40 + 0.16 = 1.51
	// K3 cost:   3.00 + 1.50 + 0.30 = 4.80
	k26Cost := money.MustParseDollars("1.51")
	k3Cost := money.MustParseDollars("4.80")
	tokenUsage := jsontext.Value(
		`{"input_tokens":1000000,"output_tokens":100000,` +
			`"cache_creation_input_tokens":0,"cache_read_input_tokens":1000000}`)

	tests := []struct {
		name           string
		model          string
		ts             string
		wantCost       money.Money
		wantPriceModel string // key expected in the pricing block
	}{
		{
			name:           "kimi-for-coding one second before cutoff",
			model:          "kimi-for-coding",
			ts:             "2026-07-18T23:59:59Z",
			wantCost:       k26Cost,
			wantPriceModel: "moonshot/kimi-k2.6",
		},
		{
			name:           "kimi-for-coding exactly at cutoff",
			model:          "kimi-for-coding",
			ts:             "2026-07-19T00:00:00Z",
			wantCost:       k3Cost,
			wantPriceModel: "kimi-k3",
		},
		{
			name:           "kimi-for-coding after cutoff",
			model:          "kimi-for-coding",
			ts:             "2026-07-20T12:00:00Z",
			wantCost:       k3Cost,
			wantPriceModel: "kimi-k3",
		},
		{
			name:           "daimon-kimi-code before cutoff",
			model:          "daimon-kimi-code",
			ts:             "2026-07-18T12:00:00Z",
			wantCost:       k26Cost,
			wantPriceModel: "moonshot/kimi-k2.6",
		},
		{
			name:           "daimon-kimi-code after cutoff",
			model:          "daimon-kimi-code",
			ts:             "2026-07-20T12:00:00Z",
			wantCost:       k3Cost,
			wantPriceModel: "kimi-k3",
		},
		{
			name:           "daimon-kimi-messages before cutoff",
			model:          "daimon-kimi-messages",
			ts:             "2026-07-18T12:00:00Z",
			wantCost:       k26Cost,
			wantPriceModel: "moonshot/kimi-k2.6",
		},
		{
			name:           "daimon-kimi-messages after cutoff",
			model:          "daimon-kimi-messages",
			ts:             "2026-07-20T12:00:00Z",
			wantCost:       k3Cost,
			wantPriceModel: "kimi-k3",
		},
		{
			name:           "provider-prefixed alias before cutoff",
			model:          "kimi-code/kimi-for-coding",
			ts:             "2026-07-18T12:00:00Z",
			wantCost:       k26Cost,
			wantPriceModel: "moonshot/kimi-k2.6",
		},
		{
			name:           "explicit k2d6-agent stays K2.6 after cutoff",
			model:          "k2d6-agent",
			ts:             "2026-07-20T12:00:00Z",
			wantCost:       k26Cost,
			wantPriceModel: "moonshot/kimi-k2.6",
		},
		{
			name:           "static k3 stays flat before cutoff",
			model:          "k3",
			ts:             "2026-07-18T12:00:00Z",
			wantCost:       k3Cost,
			wantPriceModel: "k3",
		},
		{
			name:           "static k3-agent stays flat after cutoff",
			model:          "k3-agent",
			ts:             "2026-07-20T12:00:00Z",
			wantCost:       k3Cost,
			wantPriceModel: "k3-agent",
		},
	}

	for i, tt := range tests {
		sessionID := "kimi-dates-" + strconv.Itoa(i)
		insertSession(t, d, sessionID, "proj", func(s *Session) {
			s.Agent = "kimi"
			s.StartedAt = new(tt.ts)
		})
		insertMessages(t, d, Message{
			SessionID:  sessionID,
			Ordinal:    0,
			Role:       "assistant",
			Timestamp:  tt.ts,
			Model:      tt.model,
			TokenUsage: tokenUsage,
		})
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			day := tt.ts[:10]
			result, err := d.GetDailyUsage(ctx, UsageFilter{
				From:     day,
				To:       day,
				Timezone: "UTC",
				Model:    tt.model,
			})
			requireNoError(t, err, "GetDailyUsage")

			assert.Equal(t, tt.wantCost, result.Totals.TotalCost,
				"TotalCost")
			require.NotNil(t, result.Pricing, "pricing block")
			require.Contains(t, result.Pricing.Models, tt.model)
			resolutions := result.Pricing.Models[tt.model].Resolutions
			require.Len(t, resolutions, 1)
			assert.Equal(t, tt.wantPriceModel,
				resolutions[0].PricedModel)
			if tt.wantPriceModel != tt.model {
				assert.NotContains(t, result.Pricing.Models,
					tt.wantPriceModel)
			}
		})
	}
}

// TestGetDailyUsage_KimiDateAliasMixedDaySameModel proves one reported
// model straddling the cutoff sums both eras: a pre-cutoff row prices
// at K2.6 and a post-cutoff row at K3 within the same model breakdown.
func TestGetDailyUsage_KimiDateAliasMixedDaySameModel(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{
		{
			ModelPattern:     "moonshot/kimi-k2.6",
			InputPerMTok:     money.MustParseDollars("0.95"),
			OutputPerMTok:    money.MustParseDollars("4.0"),
			CacheReadPerMTok: money.MustParseDollars("0.16"),
		},
		{
			ModelPattern:     "kimi-k3",
			InputPerMTok:     money.MustParseDollars("3.00"),
			OutputPerMTok:    money.MustParseDollars("15.00"),
			CacheReadPerMTok: money.MustParseDollars("0.30"),
		},
	}), "UpsertModelPricing")

	tokenUsage := jsontext.Value(
		`{"input_tokens":1000000,"output_tokens":100000,` +
			`"cache_creation_input_tokens":0,"cache_read_input_tokens":1000000}`)
	timestamps := []string{
		"2026-07-18T12:00:00Z", // K2.6: 1.51
		"2026-07-19T00:00:00Z", // K3: 4.80
	}
	insertSession(t, d, "kimi-mixed", "proj", func(s *Session) {
		s.Agent = "kimi"
		s.StartedAt = new(timestamps[0])
	})
	for i, ts := range timestamps {
		insertMessages(t, d, Message{
			SessionID:  "kimi-mixed",
			Ordinal:    i,
			Role:       "assistant",
			Timestamp:  ts,
			Model:      "kimi-for-coding",
			TokenUsage: tokenUsage,
		})
	}

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From:     "2026-07-01",
		To:       "2026-07-31",
		Timezone: "UTC",
		Model:    "kimi-for-coding",
	})
	requireNoError(t, err, "GetDailyUsage")

	assert.Equal(t, money.MustParseDollars("6.31"), result.Totals.TotalCost,
		"TotalCost must sum the K2.6 and K3 eras")
	require.Len(t, result.Daily, 2, "one entry per active day")
	assert.Equal(t, money.MustParseDollars("1.51"), result.Daily[0].TotalCost,
		"pre-cutoff day at K2.6 rates")
	assert.Equal(t, money.MustParseDollars("4.80"), result.Daily[1].TotalCost,
		"post-cutoff day at K3 rates")

	require.NotNil(t, result.Pricing, "pricing block")
	require.Contains(t, result.Pricing.Models, "kimi-for-coding")
	resolutions := result.Pricing.Models["kimi-for-coding"].Resolutions
	require.Len(t, resolutions, 2)
	assert.Equal(t, "kimi-k3", resolutions[0].PricedModel)
	assert.Equal(t, "moonshot/kimi-k2.6", resolutions[1].PricedModel)
	assert.NotContains(t, result.Pricing.Models, "moonshot/kimi-k2.6")
	assert.NotContains(t, result.Pricing.Models, "kimi-k3")
}

func TestDailyUsageAmountsPrefersExactCustomKimiAlias(t *testing.T) {
	resolver := export.NewPricingResolver([]export.EffectivePricingRow{
		{
			ModelPattern: "kimi-for-coding",
			Rates: export.ModelRates{
				InputPerMTok: money.MustParseDollars("7"),
				Source:       export.PricingRowSourceCustom,
			},
		},
		{
			ModelPattern: pricingpkg.KimiK3Canonical,
			Rates: export.ModelRates{
				InputPerMTok: money.MustParseDollars("2"),
				Source:       export.PricingRowSourceFetched,
			},
		},
	})

	_, _, _, _, cost, _, err := dailyUsageAmounts(dailyUsageScanRow{
		usageSource: "provider",
		model:       "kimi-for-coding",
		ts:          "2026-07-19T00:00:00Z",
		inputTokens: 1_000_000,
	}, resolver)

	require.NoError(t, err)
	assert.Equal(t, money.MustParseDollars("7"), cost)
	block, err := resolver.BuildBlock()
	require.NoError(t, err)
	require.Contains(t, block.Models, "kimi-for-coding")
	resolutions := block.Models["kimi-for-coding"].Resolutions
	require.Len(t, resolutions, 1)
	assert.Equal(t, "kimi-for-coding", resolutions[0].PricedModel)
}
