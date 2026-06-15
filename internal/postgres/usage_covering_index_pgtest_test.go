//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

const usageIdxTestSchema = "agentsview_usage_idx_test"

func TestEnsureSchemaMigratesUsageCoveringIndex(t *testing.T) {
	pgURL := testPGURL(t)
	pg, err := Open(pgURL, usageIdxTestSchema, true)
	require.NoError(t, err, "Open")
	defer pg.Close()

	ctx := context.Background()
	_, err = pg.ExecContext(ctx,
		`DROP SCHEMA IF EXISTS `+usageIdxTestSchema+` CASCADE`)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(ctx, pg, usageIdxTestSchema),
		"initial EnsureSchema")

	requirePGIndexPresence(t, pg, "idx_messages_usage_covering", true)
	requirePGIndexPresence(t, pg, "idx_messages_usage_timestamp", false)

	_, err = pg.ExecContext(ctx,
		`DROP INDEX IF EXISTS idx_messages_usage_covering`)
	require.NoError(t, err, "drop covering index")
	_, err = pg.ExecContext(ctx, `CREATE INDEX idx_messages_usage_timestamp
		ON messages(timestamp, session_id, ordinal)
		WHERE token_usage != ''
		  AND model != ''
		  AND model != '<synthetic>'`)
	require.NoError(t, err, "create legacy index")

	require.NoError(t, EnsureSchema(ctx, pg, usageIdxTestSchema),
		"migration EnsureSchema")
	requirePGIndexPresence(t, pg, "idx_messages_usage_covering", true)
	requirePGIndexPresence(t, pg, "idx_messages_usage_timestamp", false)
}

func requirePGIndexPresence(
	t *testing.T,
	pg *sql.DB,
	name string,
	want bool,
) {
	t.Helper()
	var got bool
	err := pg.QueryRowContext(context.Background(), `
		SELECT EXISTS (
			SELECT 1
			FROM pg_indexes
			WHERE schemaname = $1 AND indexname = $2
		)`,
		usageIdxTestSchema, name,
	).Scan(&got)
	require.NoError(t, err, "query pg_indexes")
	require.Equal(t, want, got, "index %s presence", name)
}
