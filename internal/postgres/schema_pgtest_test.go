//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const schemaTestSchema = "agentsview_schema_test"

func cleanSchemaTestPG(t *testing.T, pgURL string) {
	t.Helper()
	pg, err := sql.Open("pgx", pgURL)
	require.NoError(t, err, "connecting to pg")
	defer pg.Close()
	_, _ = pg.Exec(
		"DROP SCHEMA IF EXISTS " + schemaTestSchema + " CASCADE",
	)
}

// TestSecretFindingsSchema verifies that EnsureSchema creates the
// secret_findings table with all required columns, and that the
// sessions table has the secret_leak_count and
// secrets_rules_version columns. Also asserts idempotency.
func TestSecretFindingsSchema(t *testing.T) {
	pgURL := testPGURL(t)
	cleanSchemaTestPG(t, pgURL)
	t.Cleanup(func() { cleanSchemaTestPG(t, pgURL) })

	pg, err := Open(pgURL, schemaTestSchema, true)
	require.NoError(t, err, "connecting to pg")
	defer pg.Close()

	ctx := context.Background()

	// Run EnsureSchema twice to verify idempotency.
	require.NoError(t, EnsureSchema(ctx, pg, schemaTestSchema),
		"EnsureSchema (first)")
	require.NoError(t, EnsureSchema(ctx, pg, schemaTestSchema),
		"EnsureSchema (second, idempotency check)")

	// Verify secret_findings table exists.
	var tableExists bool
	err = pg.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = $1
			  AND table_name = 'secret_findings'
		)`, schemaTestSchema).Scan(&tableExists)
	require.NoError(t, err, "checking secret_findings table")
	require.True(t, tableExists, "secret_findings table does not exist")

	// Verify all required columns on secret_findings.
	requiredFindingsCols := []string{
		"id", "session_id", "rule_name", "confidence",
		"location_kind", "message_ordinal", "call_index",
		"event_index", "match_start", "match_end",
		"match_index", "redacted_match", "rules_version",
		"created_at",
	}
	for _, col := range requiredFindingsCols {
		var exists bool
		err = pg.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = $1
				  AND table_name = 'secret_findings'
				  AND column_name = $2
			)`, schemaTestSchema, col).Scan(&exists)
		require.NoError(t, err, "checking secret_findings.%s", col)
		assert.True(t, exists, "secret_findings.%s column missing", col)
	}

	// Verify sessions has both secret-scan state columns.
	requiredSessionCols := []string{
		"secret_leak_count",
		"secrets_rules_version",
	}
	for _, col := range requiredSessionCols {
		var exists bool
		err = pg.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = $1
				  AND table_name = 'sessions'
				  AND column_name = $2
			)`, schemaTestSchema, col).Scan(&exists)
		require.NoError(t, err, "checking sessions.%s", col)
		assert.True(t, exists, "sessions.%s column missing", col)
	}
}

// TestToolCallsFilePathIndex verifies EnsureSchema creates the partial
// idx_tool_calls_file_path index that backs the cross-session Recent Edits
// feed, mirroring SQLite's index so the query surface has parity on PG.
func TestToolCallsFilePathIndex(t *testing.T) {
	pgURL := testPGURL(t)
	cleanSchemaTestPG(t, pgURL)
	t.Cleanup(func() { cleanSchemaTestPG(t, pgURL) })

	pg, err := Open(pgURL, schemaTestSchema, true)
	require.NoError(t, err, "connecting to pg")
	defer pg.Close()

	ctx := context.Background()
	// Twice to confirm CREATE INDEX IF NOT EXISTS stays idempotent.
	require.NoError(t, EnsureSchema(ctx, pg, schemaTestSchema), "EnsureSchema (first)")
	require.NoError(t, EnsureSchema(ctx, pg, schemaTestSchema), "EnsureSchema (second)")

	var exists bool
	err = pg.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_indexes
			WHERE schemaname = $1
			  AND tablename = 'tool_calls'
			  AND indexname = 'idx_tool_calls_file_path'
		)`, schemaTestSchema).Scan(&exists)
	require.NoError(t, err, "checking idx_tool_calls_file_path")
	assert.True(t, exists, "idx_tool_calls_file_path index missing")
}

func TestEnsureSchemaMigratesLegacyMoneyColumns(t *testing.T) {
	pgURL := testPGURL(t)
	cleanSchemaTestPG(t, pgURL)
	t.Cleanup(func() { cleanSchemaTestPG(t, pgURL) })

	pg, err := Open(pgURL, schemaTestSchema, true)
	require.NoError(t, err, "connecting to pg")
	defer pg.Close()

	ctx := context.Background()
	require.NoError(t, EnsureSchema(ctx, pg, schemaTestSchema),
		"create current schema")
	_, err = pg.ExecContext(ctx, `
		INSERT INTO sessions (id, machine, project, agent)
		VALUES ('legacy-money-session', 'host', 'project', 'codex');
		INSERT INTO usage_events (
			session_id, source, model, cost_microdollars, dedup_key
		) VALUES (
			'legacy-money-session', 'provider', 'model', 1234567, 'priced'
		), (
			'legacy-money-session', 'provider', 'model', NULL, 'unpriced'
		);
		INSERT INTO cursor_usage_events (
			occurred_at, model, kind, charged_microdollars,
			cursor_token_fee_microdollars, dedup_key
		) VALUES (
			NOW(), 'cursor-model', 'usage', 156600, 33200, 'cursor-priced'
		);
		INSERT INTO model_pricing (
			model_pattern, input_microdollars_per_mtok,
			output_microdollars_per_mtok,
			cache_creation_microdollars_per_mtok,
			cache_read_microdollars_per_mtok, updated_at
		) VALUES ('legacy-rate', 1250000, 9876543, 2500000, 125000, 'seed');

		ALTER TABLE usage_events
			ALTER COLUMN cost_microdollars TYPE DOUBLE PRECISION
			USING cost_microdollars / 1000000.0;
		ALTER TABLE usage_events
			RENAME COLUMN cost_microdollars TO cost_usd;
		ALTER TABLE cursor_usage_events
			ALTER COLUMN charged_microdollars TYPE DOUBLE PRECISION
			USING charged_microdollars / 10000.0;
		ALTER TABLE cursor_usage_events
			RENAME COLUMN charged_microdollars TO charged_cents;
		ALTER TABLE cursor_usage_events
			ALTER COLUMN cursor_token_fee_microdollars TYPE DOUBLE PRECISION
			USING cursor_token_fee_microdollars / 10000.0;
		ALTER TABLE cursor_usage_events
			RENAME COLUMN cursor_token_fee_microdollars TO cursor_token_fee;
		ALTER TABLE model_pricing
			ALTER COLUMN input_microdollars_per_mtok TYPE DOUBLE PRECISION
			USING input_microdollars_per_mtok / 1000000.0;
		ALTER TABLE model_pricing
			RENAME COLUMN input_microdollars_per_mtok TO input_per_mtok;
		ALTER TABLE model_pricing
			ALTER COLUMN output_microdollars_per_mtok TYPE DOUBLE PRECISION
			USING output_microdollars_per_mtok / 1000000.0;
		ALTER TABLE model_pricing
			RENAME COLUMN output_microdollars_per_mtok TO output_per_mtok;
		ALTER TABLE model_pricing
			ALTER COLUMN cache_creation_microdollars_per_mtok TYPE DOUBLE PRECISION
			USING cache_creation_microdollars_per_mtok / 1000000.0;
		ALTER TABLE model_pricing
			RENAME COLUMN cache_creation_microdollars_per_mtok TO cache_creation_per_mtok;
		ALTER TABLE model_pricing
			ALTER COLUMN cache_read_microdollars_per_mtok TYPE DOUBLE PRECISION
			USING cache_read_microdollars_per_mtok / 1000000.0;
		ALTER TABLE model_pricing
			RENAME COLUMN cache_read_microdollars_per_mtok TO cache_read_per_mtok;
	`)
	require.NoError(t, err, "simulate legacy money schema")

	require.NoError(t, EnsureSchema(ctx, pg, schemaTestSchema),
		"migrate legacy money schema")
	require.NoError(t, EnsureSchema(ctx, pg, schemaTestSchema),
		"money migration idempotency")

	var cost sql.NullInt64
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT cost_microdollars FROM usage_events WHERE dedup_key = 'priced'
	`).Scan(&cost))
	require.True(t, cost.Valid)
	assert.Equal(t, int64(1234567), cost.Int64)
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT cost_microdollars FROM usage_events WHERE dedup_key = 'unpriced'
	`).Scan(&cost))
	assert.False(t, cost.Valid)

	var charged, fee int64
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT charged_microdollars, cursor_token_fee_microdollars
		FROM cursor_usage_events WHERE dedup_key = 'cursor-priced'
	`).Scan(&charged, &fee))
	assert.Equal(t, int64(156600), charged)
	assert.Equal(t, int64(33200), fee)

	var input, output, cacheCreation, cacheRead int64
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT input_microdollars_per_mtok, output_microdollars_per_mtok,
			cache_creation_microdollars_per_mtok,
			cache_read_microdollars_per_mtok
		FROM model_pricing WHERE model_pattern = 'legacy-rate'
	`).Scan(&input, &output, &cacheCreation, &cacheRead))
	assert.Equal(t, int64(1250000), input)
	assert.Equal(t, int64(9876543), output)
	assert.Equal(t, int64(2500000), cacheCreation)
	assert.Equal(t, int64(125000), cacheRead)

	for table, columns := range map[string][]string{
		"usage_events":        {"cost_usd"},
		"cursor_usage_events": {"charged_cents", "cursor_token_fee"},
		"model_pricing": {
			"input_per_mtok", "output_per_mtok",
			"cache_creation_per_mtok", "cache_read_per_mtok",
		},
	} {
		for _, column := range columns {
			var exists bool
			require.NoError(t, pg.QueryRowContext(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM information_schema.columns
					WHERE table_schema = $1 AND table_name = $2
						AND column_name = $3
				)
			`, schemaTestSchema, table, column).Scan(&exists))
			assert.False(t, exists, "%s.%s still exists", table, column)
		}
	}
}

func TestEnsureSchemaRejectsInvalidLegacyMoneyWithoutChangingSchema(t *testing.T) {
	pgURL := testPGURL(t)
	cleanSchemaTestPG(t, pgURL)
	t.Cleanup(func() { cleanSchemaTestPG(t, pgURL) })

	pg, err := Open(pgURL, schemaTestSchema, true)
	require.NoError(t, err, "connecting to pg")
	defer pg.Close()
	ctx := context.Background()
	require.NoError(t, EnsureSchema(ctx, pg, schemaTestSchema))
	_, err = pg.ExecContext(ctx, `
		INSERT INTO sessions (id, machine, project, agent)
		VALUES ('invalid-legacy-money', 'host', 'project', 'codex');
		INSERT INTO usage_events (session_id, source, model, cost_microdollars)
		VALUES ('invalid-legacy-money', 'provider', 'model', -10000);
		ALTER TABLE usage_events
			ALTER COLUMN cost_microdollars TYPE DOUBLE PRECISION
			USING cost_microdollars / 1000000.0;
		ALTER TABLE usage_events
			RENAME COLUMN cost_microdollars TO cost_usd;
	`)
	require.NoError(t, err, "simulate invalid legacy money")

	err = EnsureSchema(ctx, pg, schemaTestSchema)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "usage_events.cost_usd")

	var cost float64
	require.NoError(t, pg.QueryRowContext(ctx,
		`SELECT cost_usd FROM usage_events WHERE session_id = 'invalid-legacy-money'`,
	).Scan(&cost))
	assert.Equal(t, -0.01, cost)
}
