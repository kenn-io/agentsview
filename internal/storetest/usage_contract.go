package storetest

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/db/bunmodel"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

const (
	usageBaseID      = "usage-base"
	usageBandID      = "usage-band"
	usageAggregateID = "usage-aggregate"
	usageReportedID  = "usage-reported"
	usageDuplicateID = "usage-duplicate"
	usageModel       = "contract-model"
	usageCursorModel = "cursor-model"
)

// UsageStore is the Task 7 pricing and usage surface owned by BunStore.
type UsageStore interface {
	SetCustomPricing(map[string]config.CustomModelRate)
	SetEffectivePricing(map[string]export.ModelRates)
	SetEmptyCatalogPricing(map[string]export.ModelRates)
	GetDailyUsage(context.Context, db.UsageFilter) (db.DailyUsageResult, error)
	GetTopSessionsByCost(context.Context, db.UsageFilter, int) ([]db.TopSessionEntry, error)
	GetUsageSessionCounts(context.Context, db.UsageFilter) (db.UsageSessionCounts, error)
	GetUsageMatchingSessionCount(context.Context, db.UsageFilter) (int, error)
	GetSessionUsage(context.Context, string, bool) (*db.SessionUsage, error)
}

type UsageBackend struct {
	Name string
	Open func(*testing.T) UsageStore
}

// RunUsageContract verifies exact money, filtering, deduplication, pricing
// bands, reported-cost authority, and provenance through the common store.
func RunUsageContract(t *testing.T, backend UsageBackend) {
	t.Helper()
	t.Run(backend.Name, func(t *testing.T) {
		store := backend.Open(t)
		filter := db.UsageFilter{
			From: "2026-08-02", To: "2026-08-02", Timezone: "UTC",
		}

		store.SetEmptyCatalogPricing(usageRates(
			3_000_000, 4_000_000, 7_000_000, 8_000_000,
			export.PricingRowSourceEmbedded,
		))
		assertUsagePricingState(t, store, filter, usagePricingExpectation{
			name: "empty_catalog", source: "embedded", totalCost: 176,
			contractCost: 157, bandCost: 85, aggregateCost: 37, baseCost: 18,
			topOrder:     []string{usageBandID, usageAggregateID, usageBaseID, usageReportedID},
			baseRequests: 2, aggregateRows: 2, bandRequests: 1,
			fallback: true,
		})

		store.SetEffectivePricing(usageRates(
			1_000_000, 2_000_000, 5_000_000, 6_000_000,
			export.PricingRowSourceFetched,
		))
		assertUsagePricingState(t, store, filter, usagePricingExpectation{
			name: "effective_catalog", source: "fetched", totalCost: 118,
			contractCost: 99, bandCost: 61, aggregateCost: 13, baseCost: 8,
			topOrder:     []string{usageBandID, usageReportedID, usageAggregateID, usageBaseID},
			baseRequests: 2, aggregateRows: 2, bandRequests: 1,
		})

		store.SetCustomPricing(map[string]config.CustomModelRate{
			usageModel: {
				InputMicrodollarsPerMTok:  2_000_000,
				OutputMicrodollarsPerMTok: 3_000_000,
			},
		})
		assertUsagePricingState(t, store, filter, usagePricingExpectation{
			name: "custom", source: "custom", totalCost: 99,
			contractCost: 80, bandCost: 25, aggregateCost: 25, baseCost: 13,
			topOrder:     []string{usageAggregateID, usageBandID, usageReportedID, usageBaseID},
			baseRequests: 3, aggregateRows: 2,
		})

		counts, err := store.GetUsageSessionCounts(t.Context(), filter)
		require.NoError(t, err)
		assert.Equal(t, db.UsageSessionCounts{
			Total:     4,
			ByProject: map[string]int{"usage-contract": 4},
			ByAgent:   map[string]int{"codex": 3, "copilot": 1},
		}, counts)
		matching, err := store.GetUsageMatchingSessionCount(t.Context(), filter)
		require.NoError(t, err)
		assert.Equal(t, 5, matching,
			"matching sessions include the duplicate session before usage deduplication")
	})
}

type usagePricingExpectation struct {
	name                                      string
	source                                    string
	totalCost, contractCost                   int64
	bandCost, aggregateCost, baseCost         int64
	topOrder                                  []string
	baseRequests, aggregateRows, bandRequests int
	fallback                                  bool
}

func assertUsagePricingState(
	t *testing.T, store UsageStore, filter db.UsageFilter,
	want usagePricingExpectation,
) {
	t.Helper()
	t.Run(want.name, func(t *testing.T) {
		result, err := store.GetDailyUsage(t.Context(), filter)
		require.NoError(t, err)
		require.Len(t, result.Daily, 1)
		day := result.Daily[0]
		assert.Equal(t, "2026-08-02", day.Date)
		assert.Equal(t, 29, day.InputTokens)
		assert.Equal(t, 6, day.OutputTokens)
		assert.Equal(t, want.totalCost, day.TotalCost.Microdollars)
		assert.Equal(t, want.totalCost, result.Totals.TotalCost.Microdollars)
		assert.Equal(t, 4, result.SessionCounts.Total)
		assert.Equal(t, []string{usageModel, usageCursorModel}, day.ModelsUsed)
		require.Len(t, day.ModelBreakdowns, 2)
		assert.Equal(t, usageModel, day.ModelBreakdowns[0].ModelName)
		assert.Equal(t, 25, day.ModelBreakdowns[0].InputTokens)
		assert.Equal(t, 6, day.ModelBreakdowns[0].OutputTokens)
		assert.Equal(t, want.contractCost, day.ModelBreakdowns[0].Cost.Microdollars)
		assert.Equal(t, usageCursorModel, day.ModelBreakdowns[1].ModelName)
		assert.Equal(t, 4, day.ModelBreakdowns[1].InputTokens)
		assert.Equal(t, int64(19), day.ModelBreakdowns[1].Cost.Microdollars)

		require.NotNil(t, result.Pricing)
		assert.Equal(t, want.source, result.Pricing.Source)
		assert.Equal(t, export.CostSourceMixed, result.Pricing.CostSource)
		assert.Equal(t, want.fallback, result.Pricing.Fallback.Used)
		if want.fallback {
			assert.Equal(t, []string{usageModel}, result.Pricing.Fallback.Models)
		} else {
			assert.Empty(t, result.Pricing.Fallback.Models)
		}
		contractProvenance, ok := result.Pricing.Models[usageModel]
		require.True(t, ok)
		assert.Equal(t, export.CostSourceComputed, contractProvenance.CostSource)
		require.Len(t, contractProvenance.Resolutions, 1)
		resolution := contractProvenance.Resolutions[0]
		require.NotNil(t, resolution.MatchedPattern)
		assert.Equal(t, usageModel, *resolution.MatchedPattern)
		assert.Equal(t, want.baseRequests, resolution.Application.BaseRequestCount)
		assert.Equal(t, want.aggregateRows, resolution.Application.AggregateRowCount)
		if want.bandRequests > 0 {
			assert.Equal(t, []export.AppliedPricingBand{{
				AboveInputTokens: 10, RequestCount: want.bandRequests,
			}}, resolution.Application.Bands)
		} else {
			assert.Empty(t, resolution.Application.Bands)
		}
		cursorProvenance, ok := result.Pricing.Models[usageCursorModel]
		require.True(t, ok)
		assert.Equal(t, export.CostSourceReported, cursorProvenance.CostSource)

		top, err := store.GetTopSessionsByCost(t.Context(), filter, 10)
		require.NoError(t, err)
		require.Len(t, top, 4)
		assert.Equal(t, want.topOrder, usageTopSessionIDs(top))
		costs := map[string]int64{
			usageBandID: want.bandCost, usageAggregateID: want.aggregateCost,
			usageBaseID: want.baseCost, usageReportedID: 17,
		}
		for _, row := range top {
			assert.Equal(t, costs[row.SessionID], row.Cost.Microdollars, row.SessionID)
		}

		band, err := store.GetSessionUsage(t.Context(), usageBandID, true)
		require.NoError(t, err)
		require.NotNil(t, band)
		assert.True(t, band.HasCost)
		assert.Equal(t, export.CostSourceComputed, band.CostSource)
		assert.Equal(t, want.bandCost, band.Cost.Microdollars)
		require.Len(t, band.Breakdown, 1)
		require.NotNil(t, band.Breakdown[0].MessageOrdinal)
		assert.Equal(t, 0, *band.Breakdown[0].MessageOrdinal)

		aggregate, err := store.GetSessionUsage(t.Context(), usageAggregateID, true)
		require.NoError(t, err)
		require.NotNil(t, aggregate)
		assert.Equal(t, want.aggregateCost, aggregate.Cost.Microdollars)
		require.Len(t, aggregate.Breakdown, 1)
		assert.Nil(t, aggregate.Breakdown[0].MessageOrdinal)

		reported, err := store.GetSessionUsage(t.Context(), usageReportedID, true)
		require.NoError(t, err)
		require.NotNil(t, reported)
		assert.True(t, reported.HasCost)
		assert.Equal(t, export.CostSourceReported, reported.CostSource)
		assert.Equal(t, int64(17), reported.Cost.Microdollars)
		require.Len(t, reported.Breakdown, 1)
		assert.True(t, reported.Breakdown[0].HasCost)
		assert.Equal(t, int64(17), reported.Breakdown[0].Cost.Microdollars)
	})
}

func usageTopSessionIDs(rows []db.TopSessionEntry) []string {
	ids := make([]string, len(rows))
	for index := range rows {
		ids[index] = rows[index].SessionID
	}
	return ids
}

func usageRates(
	input, output, bandInput, bandOutput int64,
	source export.PricingRowSource,
) map[string]export.ModelRates {
	return map[string]export.ModelRates{
		usageModel: {
			InputPerMTok:  money.Money{Microdollars: input},
			OutputPerMTok: money.Money{Microdollars: output},
			Source:        source,
			Bands: []export.PricingBand{{
				AboveInputTokens: 10,
				InputPerMTok:     money.Money{Microdollars: bandInput},
				OutputPerMTok:    money.Money{Microdollars: bandOutput},
			}},
		},
	}
}

// InsertBunUsageFixture inserts the canonical Task 7 fixture.
func InsertBunUsageFixture(
	ctx context.Context, store bun.IDB, archiveID, generation string,
) error {
	if _, err := store.NewInsert().Model(&bunmodel.SourceArchive{
		SourceArchiveID: archiveID, SourceArchiveSalt: "usage-contract-salt",
	}).Exec(ctx); err != nil {
		return fmt.Errorf("inserting usage source archive: %w", err)
	}
	sessions, messages, events, cursor := bunUsageRows(archiveID, generation)
	for index := range sessions {
		if _, err := store.NewInsert().Model(&sessions[index]).Exec(ctx); err != nil {
			return fmt.Errorf("inserting usage session %s: %w", sessions[index].ID, err)
		}
	}
	for index := range messages {
		if _, err := store.NewInsert().Model(&messages[index]).Exec(ctx); err != nil {
			return fmt.Errorf("inserting usage message %s: %w", messages[index].SessionID, err)
		}
	}
	for index := range events {
		if _, err := store.NewInsert().Model(&events[index]).Exec(ctx); err != nil {
			return fmt.Errorf("inserting usage event %s: %w", events[index].SessionID, err)
		}
	}
	if _, err := store.NewInsert().Model(&cursor).Exec(ctx); err != nil {
		return fmt.Errorf("inserting cursor usage event: %w", err)
	}
	return nil
}

// InsertSQLiteUsageFixture inserts the same rows through shipped SQLite
// aliases, letting SQLite allocate its physical message IDs.
func InsertSQLiteUsageFixture(
	ctx context.Context, tx *sql.Tx, archiveID, generation string,
) error {
	sessions, messages, events, cursor := bunUsageRows(archiveID, generation)
	for _, row := range sessions {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO sessions (
				id, project, machine, agent, started_at, ended_at,
				message_count, user_message_count, total_output_tokens,
				has_total_output_tokens, created_at,
				source_archive_id, source_database_generation
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			row.ID, row.Project, row.Machine, row.Agent,
			usageTimestampValue(row.StartedAt), usageTimestampValue(row.EndedAt),
			row.MessageCount, row.UserMessageCount, row.TotalOutputTokens,
			row.HasTotalOutputTokens, row.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
			archiveID, generation,
		)
		if err != nil {
			return fmt.Errorf("inserting SQLite usage session %s: %w", row.ID, err)
		}
	}
	for _, row := range messages {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO messages (
				session_id, ordinal, role, content, timestamp, model, token_usage,
				claude_message_id, claude_request_id, source_uuid
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			row.SessionID, row.Ordinal, row.Role, row.Content,
			usageTimestampValue(row.Timestamp), row.Model, string(row.TokenUsage),
			row.ClaudeMessageID, row.ClaudeRequestID, row.SourceUUID,
		)
		if err != nil {
			return fmt.Errorf("inserting SQLite usage message %s: %w", row.SessionID, err)
		}
	}
	for _, row := range events {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO usage_events (
				id, session_id, message_ordinal, source, model,
				input_tokens, output_tokens, cache_creation_input_tokens,
				cache_read_input_tokens, reasoning_tokens, cost_microdollars,
				cost_status, cost_source, occurred_at, dedup_key
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			row.ID, row.SessionID, row.MessageOrdinal, row.Source, row.Model,
			row.InputTokens, row.OutputTokens, row.CacheCreationInputTokens,
			row.CacheReadInputTokens, row.ReasoningTokens, row.CostMicrodollars,
			row.CostStatus, row.CostSource, usageTimestampValue(row.OccurredAt), row.DedupKey,
		)
		if err != nil {
			return fmt.Errorf("inserting SQLite usage event %s: %w", row.SessionID, err)
		}
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO cursor_usage_events (
			id, occurred_at, model, kind, input_tokens, output_tokens,
			cache_write_tokens, cache_read_tokens, charged_microdollars,
			cursor_token_fee_microdollars, user_id, user_email, is_headless, dedup_key
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		cursor.ID, cursor.OccurredAt.Format("2006-01-02T15:04:05Z07:00"),
		cursor.Model, cursor.Kind, cursor.InputTokens, cursor.OutputTokens,
		cursor.CacheWriteTokens, cursor.CacheReadTokens, cursor.ChargedMicrodollars,
		cursor.CursorTokenFeeMicrodollars, cursor.UserID, cursor.UserEmail,
		cursor.IsHeadless, cursor.DedupKey,
	)
	return err
}

func bunUsageRows(
	archiveID, generation string,
) ([]bunmodel.Session, []bunmodel.Message, []bunmodel.UsageEvent, bunmodel.CursorUsageEvent) {
	timestamp := func(value string) *bunmodel.Timestamp {
		parsed, err := bunmodel.ParseTimestamp(value)
		if err != nil {
			panic(err)
		}
		return &parsed
	}
	required := func(value string) bunmodel.Timestamp { return *timestamp(value) }
	sessions := []bunmodel.Session{
		{ID: usageBaseID, Project: "usage-contract", Machine: "host-a", Agent: "codex",
			StartedAt: timestamp("2026-08-01T20:00:00Z"), MessageCount: 1,
			UserMessageCount: 1, CreatedAt: required("2026-08-01T20:00:00Z"),
			SourceArchiveID: archiveID, SourceDatabaseGeneration: generation},
		{ID: usageBandID, Project: "usage-contract", Machine: "host-a", Agent: "codex",
			StartedAt: timestamp("2026-08-02T12:00:00Z"), MessageCount: 1,
			UserMessageCount: 1, CreatedAt: required("2026-08-02T12:00:00Z"),
			SourceArchiveID: archiveID, SourceDatabaseGeneration: generation},
		{ID: usageAggregateID, Project: "usage-contract", Machine: "host-b", Agent: "codex",
			StartedAt: timestamp("2026-08-02T13:00:00Z"), MessageCount: 1,
			UserMessageCount: 1, CreatedAt: required("2026-08-02T13:00:00Z"),
			SourceArchiveID: archiveID, SourceDatabaseGeneration: generation},
		{ID: usageReportedID, Project: "usage-contract", Machine: "host-b", Agent: "copilot",
			StartedAt: timestamp("2026-08-02T11:00:00Z"), MessageCount: 1,
			UserMessageCount: 1, TotalOutputTokens: 1, HasTotalOutputTokens: true,
			CreatedAt:       required("2026-08-02T11:00:00Z"),
			SourceArchiveID: archiveID, SourceDatabaseGeneration: generation},
		{ID: usageDuplicateID, Project: "usage-contract", Machine: "host-c", Agent: "codex",
			StartedAt: timestamp("2026-08-01T21:00:00Z"), MessageCount: 1,
			UserMessageCount: 1, CreatedAt: required("2026-08-01T21:00:00Z"),
			SourceArchiveID: archiveID, SourceDatabaseGeneration: generation},
	}
	messageIDs := []int64{2101, 2102, 2103}
	messages := []bunmodel.Message{
		{ID: &messageIDs[0], SessionID: usageBaseID, Ordinal: 0, Role: "assistant",
			Content: "base usage", Timestamp: timestamp("2026-08-02T10:00:00Z"),
			Model: usageModel, TokenUsage: json.RawMessage(`{"input_tokens":2,"output_tokens":3}`),
			ClaudeMessageID: "shared-message", ClaudeRequestID: "shared-request"},
		{ID: &messageIDs[1], SessionID: usageDuplicateID, Ordinal: 0, Role: "assistant",
			Content: "duplicate usage", Timestamp: timestamp("2026-08-02T10:01:00Z"),
			Model: usageModel, TokenUsage: json.RawMessage(`{"input_tokens":2,"output_tokens":3}`),
			ClaudeMessageID: "shared-message", ClaudeRequestID: "shared-request"},
		{ID: &messageIDs[2], SessionID: usageReportedID, Ordinal: 0, Role: "assistant",
			Content: "reported usage", Timestamp: timestamp("2026-08-02T11:00:00Z"),
			Model: usageModel, TokenUsage: json.RawMessage(`{"input_tokens":1,"output_tokens":1}`),
			SourceUUID: "reported-message"},
	}
	ordinal := 0
	reportedCost := int64(17)
	events := []bunmodel.UsageEvent{
		{ID: 2201, SessionID: usageBandID, MessageOrdinal: &ordinal,
			Source: "request", Model: usageModel, InputTokens: 11, OutputTokens: 1,
			OccurredAt: timestamp("2026-08-02T12:00:00Z"), DedupKey: "band"},
		{ID: 2202, SessionID: usageAggregateID, Source: "session", Model: usageModel,
			InputTokens: 11, OutputTokens: 1,
			OccurredAt: timestamp("2026-08-02T13:00:00Z"), DedupKey: "aggregate"},
		{ID: 2203, SessionID: usageReportedID, Source: "copilot", Model: usageModel,
			CostMicrodollars: &reportedCost, CostSource: db.CopilotReportedCostSource,
			OccurredAt: timestamp("2026-08-02T11:01:00Z"), DedupKey: "reported"},
	}
	cursorID := int64(2301)
	cursor := bunmodel.CursorUsageEvent{
		ID: &cursorID, OccurredAt: required("2026-08-02T14:00:00Z"),
		Model: usageCursorModel, Kind: "included", InputTokens: 4,
		ChargedMicrodollars: 19, DedupKey: "cursor-contract",
	}
	return sessions, messages, events, cursor
}

func usageTimestampValue(value *bunmodel.Timestamp) any {
	if value == nil {
		return nil
	}
	return value.Format("2006-01-02T15:04:05Z07:00")
}
