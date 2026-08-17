package db

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db/bunmodel"
	"go.kenn.io/agentsview/internal/money"
)

type alternatingUsageBackend struct {
	first, second bun.IDB
	views         int
	attempts      int
}

type optionalUsageBackend struct {
	*sessionContractBackend
	missing map[string]bool
}

func (b *optionalUsageBackend) BunTableExists(
	_ context.Context, _ bun.IDB, table string,
) (bool, error) {
	return !b.missing[table], nil
}

func (*alternatingUsageBackend) Name() string { return "alternating-usage" }

func (*alternatingUsageBackend) ReadOnly() bool { return true }

func (*alternatingUsageBackend) Capabilities() BackendCapabilities {
	return BackendCapabilities{}
}

func (*alternatingUsageBackend) TimestampOrderExpr(column string) string {
	return "julianday(NULLIF(" + column + ", ''))"
}

func (*alternatingUsageBackend) SessionVersion(
	context.Context, bun.IDB, string,
) (int, int64, error) {
	return 0, 0, sql.ErrNoRows
}

func (b *alternatingUsageBackend) View(
	_ context.Context, fn func(bun.IDB) error,
) error {
	return fn(b.second)
}

func (b *alternatingUsageBackend) ConsistentView(
	_ context.Context, fn func(bun.IDB) error,
) error {
	b.views++
	b.attempts++
	if err := fn(b.first); err != nil {
		return err
	}
	b.attempts++
	return fn(b.second)
}

func (*alternatingUsageBackend) Update(
	context.Context, func(bun.IDB) error,
) error {
	return ErrReadOnly
}

func TestGetDailyUsageKeepsPricingRowsAndIdentityInOneView(t *testing.T) {
	first := testDB(t)
	second := testDB(t)
	seedUsageSnapshot := func(database *DB, inputTokens int, inputRate int64) {
		t.Helper()
		startedAt := "2026-08-03T12:00:00Z"
		require.NoError(t, database.UpsertSession(Session{
			ID: "snapshot-usage", Project: "snapshot", Machine: "host", Agent: "codex",
			StartedAt: &startedAt, MessageCount: 1, UserMessageCount: 1,
		}))
		require.NoError(t, database.InsertMessages([]Message{{
			SessionID: "snapshot-usage", Ordinal: 0, Role: "assistant",
			Content: "usage", ContentLength: 5, Timestamp: startedAt,
			Model: "snapshot-model", TokenUsage: fmt.Appendf(
				nil, `{"input_tokens":%d}`, inputTokens,
			),
		}}))
		require.NoError(t, database.UpsertModelPricing([]ModelPricing{{
			ModelPattern: "snapshot-model",
			InputPerMTok: money.Money{Microdollars: inputRate},
		}}))
	}
	seedUsageSnapshot(first, 1, 1_000_000)
	seedUsageSnapshot(second, 100, 100_000_000)

	backend := &alternatingUsageBackend{
		first: first.bunReader, second: second.bunReader,
	}
	store := NewBunStore(backend)
	result, err := store.GetDailyUsage(t.Context(), UsageFilter{
		From: "2026-08-03", To: "2026-08-03", Timezone: "UTC",
	})
	require.NoError(t, err)
	assert.Equal(t, 100, result.Totals.InputTokens)
	assert.Equal(t, int64(10_000), result.Totals.TotalCost.Microdollars)
	assert.Equal(t, 1, backend.views)
	assert.Equal(t, 2, backend.attempts)
}

// A missing ID predicate would admit ignored-session rows, adapter-specific
// ordering would choose the wrong cross-session duplicate, and treating a
// Copilot total as an ordinary row cost would double-count the root session.
func TestBunStoreGetSessionUsageRowsFiltersDeduplicatesAndPrices(t *testing.T) {
	database := testDB(t)
	for _, session := range []Session{
		{ID: "usage-root", Project: "project", Machine: "host", Agent: "copilot"},
		{ID: "usage-child", Project: "project", Machine: "host", Agent: "copilot"},
		{ID: "usage-ignored", Project: "project", Machine: "host", Agent: "copilot"},
	} {
		require.NoError(t, database.UpsertSession(session))
	}
	require.NoError(t, database.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "usage-model",
		InputPerMTok:  money.MustParseDollars("3"),
		OutputPerMTok: money.MustParseDollars("15"),
	}}))
	require.NoError(t, database.InsertMessages([]Message{
		{
			SessionID: "usage-root", Ordinal: 0, Role: "assistant",
			Content: "root", ContentLength: 4, Timestamp: "2026-08-03T09:00:00Z",
			Model: "usage-model", TokenUsage: []byte(`{"input_tokens":1000,"output_tokens":500}`),
			ClaudeMessageID: "shared-message", ClaudeRequestID: "shared-request",
		},
		{
			SessionID: "usage-child", Ordinal: 0, Role: "assistant",
			Content: "duplicate", ContentLength: 9, Timestamp: "2026-08-03T09:00:00Z",
			Model: "usage-model", TokenUsage: []byte(`{"input_tokens":1000,"output_tokens":500}`),
			ClaudeMessageID: "shared-message", ClaudeRequestID: "shared-request",
		},
	}))
	reportedRootCost := money.MustParseDollars("0.03")
	reportedChildCost := money.MustParseDollars("0.02")
	require.NoError(t, database.ReplaceSessionUsageEvents("usage-root", []UsageEvent{
		{
			Source: "shutdown", Model: "usage-model",
			InputTokens: 1000, OutputTokens: 500,
			OccurredAt: "2026-08-03T10:00:00Z", DedupKey: "computed",
		},
		{
			Source: "shutdown", Model: "usage-model",
			InputTokens: 1000, OutputTokens: 500,
			Cost: &reportedRootCost, CostStatus: "exact",
			CostSource: CopilotReportedCostSource,
			OccurredAt: "2026-08-03T10:01:00Z", DedupKey: "reported",
		},
	}))
	require.NoError(t, database.ReplaceSessionUsageEvents("usage-child", []UsageEvent{{
		Source: "provider", Model: "usage-model", Cost: &reportedChildCost,
		CostStatus: "exact", CostSource: "provider",
		OccurredAt: "2026-08-03T10:02:00Z", DedupKey: "child",
	}}))
	require.NoError(t, database.ReplaceSessionUsageEvents("usage-ignored", []UsageEvent{{
		Source: "provider", Model: "usage-model", Cost: &reportedChildCost,
		CostStatus: "exact", CostSource: "provider",
		OccurredAt: "2026-08-03T08:00:00Z", DedupKey: "ignored",
	}}))

	rowSet, err := NewBunStore(&sessionContractBackend{store: database.bunReader}).
		GetSessionUsageRows(t.Context(), []string{"usage-root", "usage-child"})
	require.NoError(t, err)
	require.NotNil(t, rowSet)
	rows := rowSet.Rows
	require.Len(t, rows, 4)
	assert.Equal(t, []string{
		"usage-child", "usage-root", "usage-root", "usage-child",
	}, []string{rows[0].SessionID, rows[1].SessionID, rows[2].SessionID, rows[3].SessionID})
	assert.Equal(t, []int64{10_500, 10_500, 10_500, 20_000}, []int64{
		rows[0].Cost.Microdollars, rows[1].Cost.Microdollars,
		rows[2].Cost.Microdollars, rows[3].Cost.Microdollars,
	})
	require.NotNil(t, rows[2].SessionCost)
	assert.Equal(t, int64(30_000), rows[2].SessionCost.Microdollars)
	assert.Nil(t, rows[0].SessionCost)
	assert.Nil(t, rows[1].SessionCost)
	assert.Nil(t, rows[3].SessionCost)
	assert.Equal(t, "usage-root", rows[0].SourceSessionID)
	assert.Equal(t, map[string]int{"usage-root": 1500, "usage-child": 500},
		rowSet.RawOutputTokensBySession)
	assert.Equal(t, map[string]int{"usage-root": 500, "usage-child": 500},
		rowSet.DeduplicatedOutputTokens)
	assert.Equal(t, map[string]struct{}{"usage-root": {}, "usage-child": {}},
		rowSet.DiscardedContributingSessions)
}

func TestLoadPricingMapAllowsMissingOptionalBandsTable(t *testing.T) {
	database := testDB(t)
	require.NoError(t, database.UpsertModelPricing([]ModelPricing{{
		ModelPattern: "base-only", InputPerMTok: money.Money{Microdollars: 7},
	}}))
	_, err := database.getWriter().ExecContext(
		t.Context(), "DROP TABLE model_pricing_bands",
	)
	require.NoError(t, err)
	backend := &optionalUsageBackend{
		sessionContractBackend: &sessionContractBackend{store: database.bunReader},
		missing:                map[string]bool{"model_pricing_bands": true},
	}
	rows, err := NewBunStore(backend).LoadPricingMap(t.Context())
	require.NoError(t, err)
	var found bool
	for _, row := range rows {
		if row.ModelPattern == "base-only" {
			found = true
			assert.Equal(t, int64(7), row.Rates.InputPerMTok.Microdollars)
			assert.Empty(t, row.Rates.Bands)
		}
	}
	assert.True(t, found)
}

func TestAppendBunUsageTerminationFilterUsesProvidedReference(t *testing.T) {
	database := testDB(t)
	reference := time.Date(2030, 1, 2, 12, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		id, status string
		age        time.Duration
	}{
		{id: "active", age: 5 * time.Minute},
		{id: "stale", status: "tool_call_pending", age: 30 * time.Minute},
		{id: "unclean", status: "truncated", age: 2 * time.Hour},
		{id: "stale-clean", status: "clean", age: 30 * time.Minute},
	} {
		ended := reference.Add(-row.age).Format(time.RFC3339Nano)
		status := row.status
		require.NoError(t, database.UpsertSession(Session{
			ID: row.id, Project: "clock", Machine: "host", Agent: "codex",
			CreatedAt: ended, StartedAt: &ended, EndedAt: &ended,
			MessageCount: 1, TerminationStatus: &status,
		}))
	}

	var ids []string
	query := database.bunReader.NewSelect().TableExpr("sessions AS s").Column("s.id")
	query = appendBunUsageTerminationFilter(
		query, "active,stale,unclean", sqliteTimestampOrderExpr, reference,
	)
	require.NoError(t, query.OrderExpr("s.id ASC").Scan(t.Context(), &ids))
	assert.Equal(t, []string{"active", "stale", "unclean"}, ids)
}

func TestAppendBunUsageTerminationFilterKeepsExactCutoffSemantics(t *testing.T) {
	database := testDB(t)
	reference := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	flagged := "tool_call_pending"
	for _, row := range []struct {
		id    string
		ended time.Time
	}{
		{id: "active-after", ended: reference.Add(-activeWindow + time.Second)},
		{id: "active-cutoff", ended: reference.Add(-activeWindow)},
		{id: "stale-after", ended: reference.Add(-staleWindow + time.Second)},
		{id: "stale-cutoff", ended: reference.Add(-staleWindow)},
	} {
		value := row.ended.Format(time.RFC3339Nano)
		require.NoError(t, database.UpsertSession(Session{
			ID: row.id, Project: "clock", Machine: "host", Agent: "codex",
			CreatedAt: value, StartedAt: &value, EndedAt: &value,
			MessageCount: 1, TerminationStatus: &flagged,
		}))
	}

	queryIDs := func(status string) []string {
		var ids []string
		query := database.bunReader.NewSelect().
			TableExpr("sessions AS s").Column("s.id")
		query = appendBunUsageTerminationFilter(
			query, status, sqliteTimestampOrderExpr, reference,
		)
		require.NoError(t, query.OrderExpr("s.id ASC").Scan(t.Context(), &ids))
		return ids
	}

	assert.Equal(t, []string{"active-after"}, queryIDs("active"))
	assert.Equal(t, []string{"active-cutoff", "stale-after"}, queryIDs("stale"))
	assert.Equal(t, []string{"stale-cutoff"}, queryIDs("unclean"))
}

func TestUpsertModelPricingRowsReplacesBandsAtomically(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()
	base := bunmodel.ModelPricing{
		ModelPattern: "atomic-model", InputMicrodollarsPerMTok: 1,
		OutputMicrodollarsPerMTok: 2,
		UpdatedAt:                 mustBunTimestamp(t, "2026-08-03T12:00:00Z"),
	}
	require.NoError(t, UpsertModelPricingRows(
		ctx, database.bunWriter,
		[]bunmodel.ModelPricing{base},
		[]bunmodel.ModelPricingBand{
			{ModelPattern: "atomic-model", AboveInputTokens: 100,
				InputMicrodollarsPerMTok: 3,
				UpdatedAt:                mustBunTimestamp(t, "2026-08-03T12:00:00Z")},
			{ModelPattern: "atomic-model", AboveInputTokens: 200,
				InputMicrodollarsPerMTok: 4,
				UpdatedAt:                mustBunTimestamp(t, "2026-08-03T12:00:00Z")},
		},
	))

	require.NoError(t, UpsertModelPricingRows(
		ctx, database.bunWriter,
		[]bunmodel.ModelPricing{{
			ModelPattern: "atomic-model", InputMicrodollarsPerMTok: 5,
			OutputMicrodollarsPerMTok: 6,
			UpdatedAt:                 mustBunTimestamp(t, "2026-08-03T13:00:00Z"),
		}},
		[]bunmodel.ModelPricingBand{{
			ModelPattern: "atomic-model", AboveInputTokens: 300,
			InputMicrodollarsPerMTok: 7,
			UpdatedAt:                mustBunTimestamp(t, "2026-08-03T13:00:00Z"),
		}},
	))
	stored, err := database.GetModelPricing("atomic-model")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, int64(5), stored.InputPerMTok.Microdollars)
	require.Len(t, stored.Bands, 1)
	assert.Equal(t, 300, stored.Bands[0].AboveInputTokens)
	assert.Equal(t, int64(7), stored.Bands[0].InputPerMTok.Microdollars)

	err = UpsertModelPricingRows(
		ctx, database.bunWriter,
		[]bunmodel.ModelPricing{{
			ModelPattern: "atomic-model", InputMicrodollarsPerMTok: 99,
			OutputMicrodollarsPerMTok: 100,
			UpdatedAt:                 mustBunTimestamp(t, "2026-08-03T14:00:00Z"),
		}},
		[]bunmodel.ModelPricingBand{
			{ModelPattern: "atomic-model", AboveInputTokens: 400,
				InputMicrodollarsPerMTok: 8,
				UpdatedAt:                mustBunTimestamp(t, "2026-08-03T14:00:00Z")},
			{ModelPattern: "atomic-model", AboveInputTokens: 400,
				InputMicrodollarsPerMTok: 9,
				UpdatedAt:                mustBunTimestamp(t, "2026-08-03T14:00:00Z")},
		},
	)
	require.Error(t, err)

	stored, err = database.GetModelPricing("atomic-model")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, int64(5), stored.InputPerMTok.Microdollars,
		"base update must roll back with band replacement")
	require.Len(t, stored.Bands, 1)
	assert.Equal(t, 300, stored.Bands[0].AboveInputTokens,
		"deleted bands must roll back with the failed insert")
}

func mustBunTimestamp(t *testing.T, value string) bunmodel.Timestamp {
	t.Helper()
	timestamp, err := bunmodel.ParseTimestamp(value)
	require.NoError(t, err)
	return timestamp
}

func TestCanonicalModelPricingRowsPreserveBandsAndMoney(t *testing.T) {
	prices, bands, err := CanonicalModelPricingRows([]ModelPricing{{
		ModelPattern:         " model\x00 ",
		InputPerMTok:         money.Money{Microdollars: 11},
		OutputPerMTok:        money.Money{Microdollars: 22},
		CacheCreationPerMTok: money.Money{Microdollars: 33},
		CacheReadPerMTok:     money.Money{Microdollars: 44},
		UpdatedAt:            "2026-08-04T01:02:03Z",
		Bands: []PricingBand{{
			AboveInputTokens: 200_000,
			InputPerMTok:     money.Money{Microdollars: 55},
			OutputPerMTok:    money.Money{Microdollars: 66},
		}},
	}})
	require.NoError(t, err)
	require.Len(t, prices, 1)
	assert.Equal(t, " model ", prices[0].ModelPattern)
	assert.Equal(t, int64(11), prices[0].InputMicrodollarsPerMTok)
	assert.Equal(t, int64(44), prices[0].CacheReadMicrodollarsPerMTok)
	assert.Equal(t,
		time.Date(2026, 8, 4, 1, 2, 3, 0, time.UTC),
		prices[0].UpdatedAt.Time,
	)
	require.Len(t, bands, 1)
	assert.Equal(t, int64(200_000), bands[0].AboveInputTokens)
	assert.Equal(t, int64(55), bands[0].InputMicrodollarsPerMTok)
	assert.Equal(t, prices[0].UpdatedAt, bands[0].UpdatedAt)
}

func TestCanonicalModelPricingRowsRejectInvalidTimestamp(t *testing.T) {
	_, _, err := CanonicalModelPricingRows([]ModelPricing{{
		ModelPattern: "model", UpdatedAt: "not-a-timestamp",
	}})
	require.ErrorContains(t, err, "model pricing timestamp")
}

func TestUpsertModelPricingRowsAdvancesTargetRevision(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()
	initial := []bunmodel.ModelPricing{{
		ModelPattern: "model", InputMicrodollarsPerMTok: 1,
		UpdatedAt: mustBunTimestamp(t, "2026-08-04T02:00:00Z"),
	}}
	require.NoError(t, UpsertModelPricingRows(ctx, database.bunWriter, initial, nil))
	changed := []bunmodel.ModelPricing{{
		ModelPattern: "model", InputMicrodollarsPerMTok: 2,
		UpdatedAt: mustBunTimestamp(t, "2026-08-04T01:00:00Z"),
	}}
	require.NoError(t, UpsertModelPricingRows(ctx, database.bunWriter, changed, nil))
	var stored bunmodel.ModelPricing
	require.NoError(t, database.bunReader.NewSelect().Model(&stored).
		Where("model_pattern = ?", "model").Scan(ctx))
	assert.Equal(t,
		mustBunTimestamp(t, "2026-08-04T02:00:00.000001Z"),
		stored.UpdatedAt,
	)
}

func TestUpsertModelPricingRowsPreservesUnchangedRevisions(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()
	initial := []bunmodel.ModelPricing{{
		ModelPattern: "model", InputMicrodollarsPerMTok: 1,
		UpdatedAt: mustBunTimestamp(t, "2026-08-04T02:00:00Z"),
	}}
	initialBands := []bunmodel.ModelPricingBand{
		{ModelPattern: "model", AboveInputTokens: 100,
			InputMicrodollarsPerMTok: 10,
			UpdatedAt:                mustBunTimestamp(t, "2026-08-04T02:00:00Z")},
		{ModelPattern: "model", AboveInputTokens: 200,
			InputMicrodollarsPerMTok: 20,
			UpdatedAt:                mustBunTimestamp(t, "2026-08-04T02:00:00Z")},
	}
	require.NoError(t, UpsertModelPricingRows(
		ctx, database.bunWriter, initial, initialBands,
	))

	incoming := []bunmodel.ModelPricing{{
		ModelPattern: "model", InputMicrodollarsPerMTok: 1,
		UpdatedAt: mustBunTimestamp(t, "2026-08-04T03:00:00Z"),
	}}
	incomingBands := []bunmodel.ModelPricingBand{
		{ModelPattern: "model", AboveInputTokens: 100,
			InputMicrodollarsPerMTok: 10,
			UpdatedAt:                mustBunTimestamp(t, "2026-08-04T03:00:00Z")},
		{ModelPattern: "model", AboveInputTokens: 200,
			InputMicrodollarsPerMTok: 21,
			UpdatedAt:                mustBunTimestamp(t, "2026-08-04T01:00:00Z")},
	}
	require.NoError(t, UpsertModelPricingRows(
		ctx, database.bunWriter, incoming, incomingBands,
	))

	var stored bunmodel.ModelPricing
	require.NoError(t, database.bunReader.NewSelect().Model(&stored).
		Where("model_pattern = ?", "model").Scan(ctx))
	assert.Equal(t, mustBunTimestamp(t, "2026-08-04T03:00:00Z"), stored.UpdatedAt,
		"a changed band advances the model catalog revision")
	var bands []bunmodel.ModelPricingBand
	require.NoError(t, database.bunReader.NewSelect().Model(&bands).
		Where("model_pattern = ?", "model").OrderExpr("above_input_tokens ASC").
		Scan(ctx))
	require.Len(t, bands, 2)
	assert.Equal(t, mustBunTimestamp(t, "2026-08-04T02:00:00Z"),
		bands[0].UpdatedAt)
	assert.Equal(t, mustBunTimestamp(t, "2026-08-04T02:00:00.000001Z"),
		bands[1].UpdatedAt)

	incoming[0].UpdatedAt = mustBunTimestamp(t, "2026-08-04T04:00:00Z")
	incomingBands[0].UpdatedAt = mustBunTimestamp(t, "2026-08-04T04:00:00Z")
	incomingBands[1].UpdatedAt = mustBunTimestamp(t, "2026-08-04T04:00:00Z")
	require.NoError(t, UpsertModelPricingRows(
		ctx, database.bunWriter, incoming, incomingBands,
	))
	require.NoError(t, database.bunReader.NewSelect().Model(&stored).
		Where("model_pattern = ?", "model").Scan(ctx))
	assert.Equal(t, mustBunTimestamp(t, "2026-08-04T03:00:00Z"), stored.UpdatedAt,
		"repeating identical catalog content preserves its revision")
}

func TestAppendCursorUsageEventRowsDeduplicatesPortableRows(t *testing.T) {
	database := testDB(t)
	rows, err := CanonicalCursorUsageEventRows([]CursorUsageEvent{{
		ID: 99, OccurredAt: "2026-08-04T01:02:03.123456789Z",
		Model: "model\x00", Kind: "composer", InputTokens: 10,
		Charged: money.Money{Microdollars: 1234}, DedupKey: "cursor-row",
	}})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Nil(t, rows[0].ID)
	assert.Equal(t, "model", rows[0].Model)
	assert.Equal(t, time.Date(
		2026, 8, 4, 1, 2, 3, 123456000, time.UTC,
	), rows[0].OccurredAt.Time)

	require.NoError(t, AppendCursorUsageEventRows(t.Context(), database.bunWriter, rows))
	require.NoError(t, AppendCursorUsageEventRows(t.Context(), database.bunWriter, rows))
	count, err := database.bunReader.NewSelect().
		Model((*bunmodel.CursorUsageEvent)(nil)).Count(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestCanonicalCursorUsageEventRowsValidatesPersistedValues(t *testing.T) {
	rows, err := CanonicalCursorUsageEventRows([]CursorUsageEvent{{
		OccurredAt: "2026-08-04T01:02:03Z", Model: "\x00", DedupKey: "\x00",
	}})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Empty(t, rows[0].Model, "legacy empty models remain replicable")
	assert.NotEmpty(t, rows[0].DedupKey, "sanitized empty keys are regenerated")
}
