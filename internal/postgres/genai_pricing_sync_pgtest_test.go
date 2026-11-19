//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/storage"
)

// A pricing sync whose mirror already holds the archive's GenAI document
// receives its digest, not the document, which is close to a megabyte. The
// digest must agree with the archive's, or every push would rewrite the
// document and its observation time.
func TestSyncModelPricingComparesGenAIByDigest(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	const schema = usageTestSchema
	url := testPGSchemaURL(t, schema)
	local := testDB(t)
	// One model price keeps the push from copying the fallback catalog,
	// which would swamp the byte count this test measures.
	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern: "acme/model", InputPerMTok: money.MustParseDollars("1"),
	}}))
	syncer, err := New(ctx, url, schema, local, "machine", true, storage.PusherOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, syncer.Close()) })
	require.NoError(t, syncer.EnsureSchema(ctx))
	mirrored := func() (data []byte, updatedAt sql.NullTime) {
		t.Helper()
		require.NoError(t, syncer.pg.QueryRowContext(ctx,
			`SELECT data_json, updated_at FROM genai_pricing WHERE singleton = 1`,
		).Scan(&data, &updatedAt))
		return data, updatedAt
	}

	// The archive has no document, so the push installs the embedded one.
	require.NoError(t, syncer.syncModelPricing(ctx))
	data, _ := mirrored()
	embedded := db.EmbeddedGenAIPricingDocument().Data
	require.Equal(t, embedded, data, "a missing mirror document is inserted")
	_, err = syncer.pg.ExecContext(ctx,
		`UPDATE genai_pricing SET updated_at = '2026-07-01T00:00:00Z' WHERE singleton = 1`)
	require.NoError(t, err)

	quoted, err := quoteIdentifier(schema)
	require.NoError(t, err)
	connStr, err := appendConnParams(url, map[string]string{"search_path": quoted, "TimeZone": "UTC"})
	require.NoError(t, err)
	config, err := pgx.ParseConfig(connStr)
	require.NoError(t, err)
	var received atomic.Int64
	config.AfterNetConnect = func(_ context.Context, _ *pgconn.Config, conn net.Conn) (net.Conn, error) {
		return &pushResponseConn{Conn: conn, received: &received}, nil
	}
	metered := stdlib.OpenDB(*config)
	require.NoError(t, syncer.pg.Close())
	syncer.pg = metered
	require.NoError(t, metered.PingContext(ctx))
	received.Store(0)
	require.NoError(t, syncer.syncModelPricing(ctx))
	t.Logf("PostgreSQL sent %d bytes for an unchanged %d-byte GenAI document", received.Load(), len(embedded))
	assert.Less(t, received.Load(), int64(64<<10), "an unchanged document is not transferred")
	_, updatedAt := mirrored()
	assert.True(t, updatedAt.Time.Equal(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)),
		"a matching document is not rewritten: %v", updatedAt)

	require.NoError(t, local.UpsertGenAIPricing(ctx, db.GenAIPricingDocument{
		Version: "genai-prices-test", SourceRef: "upstream-ref",
		Source: db.GenAIPricingSourceFetched, Data: []byte(`{"test":"changed"}`),
	}))
	require.NoError(t, syncer.syncModelPricing(ctx))
	data, _ = mirrored()
	assert.JSONEq(t, `{"test":"changed"}`, string(data), "changed data is pushed")
}
