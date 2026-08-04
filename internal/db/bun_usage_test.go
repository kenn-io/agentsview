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

func (*alternatingUsageBackend) SessionQueryDialect() QueryDialect {
	return SQLiteBunSessionQueryDialect()
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
		query, "active,stale,unclean", SQLiteBunSessionQueryDialect(), reference,
	)
	require.NoError(t, query.OrderExpr("s.id ASC").Scan(t.Context(), &ids))
	assert.Equal(t, []string{"active", "stale", "unclean"}, ids)
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
