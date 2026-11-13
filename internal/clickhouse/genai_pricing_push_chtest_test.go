//go:build chtest

package clickhouse

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/storage"
)

// genAIPricingReadsSince counts the finished queries that read genai_pricing
// since start and the largest result one of them returned.
func genAIPricingReadsSince(t *testing.T, syncer *Sync, start time.Time) (reads, maxResultBytes uint64) {
	t.Helper()
	_, err := syncer.conn.ExecContext(t.Context(), "SYSTEM FLUSH LOGS")
	require.NoError(t, err)
	require.NoError(t, syncer.conn.QueryRowContext(t.Context(), `
		SELECT count(), max(result_bytes)
		FROM system.query_log
		WHERE current_database = currentDatabase() AND type = 'QueryFinish'
			AND query_kind = 'Select' AND event_time_microseconds >= ?
			AND has(tables, currentDatabase() || '.genai_pricing')
	`, start).Scan(&reads, &maxResultBytes))
	return reads, maxResultBytes
}

// A push whose mirror already holds the archive's GenAI document reads only
// its digest, prices with the row its own pricing sync parsed, and resolves
// exactly as a reader of the mirror does. Without the last, every push would
// see a different pricing digest than the stored records carry and reprice
// the whole mirror.
func TestPushReadsGenAIPricingDigestOnce(t *testing.T) {
	for name, seed := range map[string]func(t *testing.T) *db.GenAIPricingDocument{
		"embedded fallback": func(*testing.T) *db.GenAIPricingDocument { return nil },
		"fetched": func(t *testing.T) *db.GenAIPricingDocument {
			document := genAIHistoryDocument(t, "1", "1")
			return &document
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			local, target := seedUsagePriceFixture(t)
			document := seed(t)
			if document != nil {
				require.NoError(t, local.UpsertGenAIPricing(ctx, *document))
			} else {
				stored, err := local.GetGenAIPricing(ctx)
				require.NoError(t, err)
				require.Nil(t, stored, "the fixture archive must have no GenAI document")
				embedded := db.EmbeddedGenAIPricingDocument()
				document = &embedded
			}
			syncer := newTestSync(t, local, target, storage.PusherOptions{})
			pushPreparedUsage(t, syncer)
			require.NotNil(t, syncer.genAI)
			parsed := syncer.genAI.row.GenAI

			var start time.Time
			require.NoError(t, syncer.conn.QueryRowContext(ctx, "SELECT now64(6)").Scan(&start))
			pushPreparedUsage(t, syncer)
			reads, maxResultBytes := genAIPricingReadsSince(t, syncer, start)
			assert.EqualValues(t, 1, reads, "one push reads genai_pricing once")
			if name == "embedded fallback" {
				assert.Less(t, maxResultBytes, uint64(64<<10),
					"an unchanged %d-byte document is not transferred", len(document.Data))
			}
			assert.Same(t, parsed, syncer.genAI.row.GenAI, "an unchanged document is not parsed again")

			reader, err := chLoadPricingCatalog(ctx, syncer.conn, nil)
			require.NoError(t, err)
			require.NotEmpty(t, reader.models, "the digest must cover priced models")
			meta, err := readMetadata(ctx, syncer.conn, usagePricedDigestKey)
			require.NoError(t, err)
			assert.Equal(t, reader.digest, syncer.pricer.digest)
			assert.Equal(t, reader.digest, meta[usagePricedDigestKey])
		})
	}
}

// A changed archive document reaches the mirror, and the push prices with
// it rather than with the document it parsed before.
func TestPushRepricesChangedGenAIPricing(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	local, target := seedUsagePriceFixture(t)
	require.NoError(t, local.UpsertGenAIPricing(ctx, genAIHistoryDocument(t, "1", "1")))
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	pushPreparedUsage(t, syncer)
	past := time.Date(2026, 1, 12, 12, 0, 0, 0, time.UTC)
	_, lookup := syncer.pricer.resolver.ResolveAt("gpt-hist-test", "gpt-hist-test", past)
	require.Equal(t, money.MustParseDollars("1"), lookup.Rates.InputPerMTok)

	changed := genAIHistoryDocument(t, "2", "1")
	require.NoError(t, local.UpsertGenAIPricing(ctx, changed))
	pushPreparedUsage(t, syncer)

	mirrored, err := loadGenAIPricing(ctx, syncer.conn)
	require.NoError(t, err)
	require.NotNil(t, mirrored)
	assert.Equal(t, changed.Data, mirrored.Data)
	_, lookup = syncer.pricer.resolver.ResolveAt("gpt-hist-test", "gpt-hist-test", past)
	assert.Equal(t, money.MustParseDollars("2"), lookup.Rates.InputPerMTok)
	reader, err := chLoadPricingCatalog(ctx, syncer.conn, nil)
	require.NoError(t, err)
	assert.Equal(t, reader.digest, syncer.pricer.digest)
}
