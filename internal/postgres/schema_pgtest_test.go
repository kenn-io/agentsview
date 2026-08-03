//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	commondb "go.kenn.io/agentsview/internal/db"
)

const schemaTestSchema = "agentsview_schema_test"

func TestEnsureSchemaConvergesCommonColumnsAndRetainsPriorRows(t *testing.T) {
	pgURL := testPGURL(t)
	cleanSchemaTestPG(t, pgURL)
	t.Cleanup(func() { cleanSchemaTestPG(t, pgURL) })
	pg, err := Open(pgURL, schemaTestSchema, true)
	require.NoError(t, err)
	defer pg.Close()
	require.NoError(t, EnsureSchema(t.Context(), pg, schemaTestSchema))
	_, err = pg.ExecContext(t.Context(), `
		INSERT INTO sessions (
			id, machine, project, agent, created_at,
			source_archive_id, source_database_generation
		) VALUES (
			'prior-common-session', 'machine', 'project', 'agent', NOW(),
			'archive-1', 'generation-1'
		);
		ALTER TABLE sessions DROP COLUMN IF EXISTS file_size;
		ALTER TABLE messages DROP COLUMN IF EXISTS id;
		ALTER TABLE tool_calls DROP COLUMN IF EXISTS message_id;
		DELETE FROM sync_metadata WHERE key = 'bun_common_schema_v1';
	`)
	require.NoError(t, err)

	require.NoError(t, EnsureSchema(t.Context(), pg, schemaTestSchema))
	store := bun.NewDB(pg, pgdialect.New())
	require.NoError(t, commondb.CheckCommonSchema(t.Context(), store))
	var project string
	require.NoError(t, pg.QueryRowContext(t.Context(), `
		SELECT project FROM sessions WHERE id = 'prior-common-session'`,
	).Scan(&project))
	assert.Equal(t, "project", project)
	var stamp string
	require.NoError(t, pg.QueryRowContext(t.Context(), `
		SELECT value FROM sync_metadata WHERE key = 'bun_common_schema_v1'`,
	).Scan(&stamp))
	assert.Equal(t, "1", stamp)
}

func TestPostgresCommonConvergenceRollsBackDDLAndStamp(t *testing.T) {
	pgURL := testPGURL(t)
	cleanSchemaTestPG(t, pgURL)
	t.Cleanup(func() { cleanSchemaTestPG(t, pgURL) })
	pg, err := Open(pgURL, schemaTestSchema, true)
	require.NoError(t, err)
	defer pg.Close()
	require.NoError(t, EnsureSchema(t.Context(), pg, schemaTestSchema))
	_, err = pg.ExecContext(t.Context(), `
		INSERT INTO sessions (
			id, machine, project, agent, created_at,
			source_archive_id, source_database_generation
		) VALUES (
			'rollback-common-session', 'machine', 'project', 'agent', NOW(),
			'archive-1', 'generation-1'
		);
		ALTER TABLE sessions DROP COLUMN IF EXISTS file_size;
		ALTER TABLE messages DROP COLUMN IF EXISTS id;
		ALTER TABLE tool_calls DROP COLUMN IF EXISTS message_id;
		DELETE FROM sync_metadata WHERE key = 'bun_common_schema_v1';
	`)
	require.NoError(t, err)

	injected := errors.New("injected PostgreSQL common convergence failure")
	err = convergePostgresCommonSchema(t.Context(), pg, func() error {
		return injected
	})
	require.ErrorIs(t, err, injected)

	for table, column := range map[string]string{
		"sessions": "file_size", "messages": "id", "tool_calls": "message_id",
	} {
		var exists bool
		require.NoError(t, pg.QueryRowContext(t.Context(), `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = $1 AND table_name = $2 AND column_name = $3
			)`, schemaTestSchema, table, column,
		).Scan(&exists))
		assert.False(t, exists, "%s.%s", table, column)
	}
	var stampCount, sessionCount int
	require.NoError(t, pg.QueryRowContext(t.Context(), `
		SELECT count(*) FROM sync_metadata WHERE key = 'bun_common_schema_v1'`,
	).Scan(&stampCount))
	assert.Zero(t, stampCount)
	require.NoError(t, pg.QueryRowContext(t.Context(), `
		SELECT count(*) FROM sessions WHERE id = 'rollback-common-session'`,
	).Scan(&sessionCount))
	assert.Equal(t, 1, sessionCount)
}

func cleanSchemaTestPG(t *testing.T, pgURL string) {
	t.Helper()
	pg, err := sql.Open("pgx", pgURL)
	require.NoError(t, err, "connecting to pg")
	defer pg.Close()
	_, _ = pg.Exec(
		"DROP SCHEMA IF EXISTS " + schemaTestSchema + " CASCADE",
	)
}

func TestSessionDeletionCauseSchemaMigrationPreservesRows(t *testing.T) {
	pgURL := testPGURL(t)
	cleanSchemaTestPG(t, pgURL)
	t.Cleanup(func() { cleanSchemaTestPG(t, pgURL) })
	pg, err := Open(pgURL, schemaTestSchema, true)
	require.NoError(t, err)
	defer pg.Close()
	require.NoError(t, EnsureSchema(t.Context(), pg, schemaTestSchema))

	var exists bool
	require.NoError(t, pg.QueryRow(`
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = $1 AND table_name = 'sessions'
			  AND column_name = 'deletion_cause'
		)`, schemaTestSchema).Scan(&exists))
	require.True(t, exists, "fresh schema must include sessions.deletion_cause")
	_, err = pg.Exec(`
		INSERT INTO sessions (id, machine, project, agent, deleted_at)
		VALUES ('preserved-trash', 'machine', 'project', 'claude', NOW())`)
	require.NoError(t, err)
	_, err = pg.Exec(`ALTER TABLE sessions DROP COLUMN deletion_cause`)
	require.NoError(t, err)

	require.NoError(t, EnsureSchema(t.Context(), pg, schemaTestSchema))
	var deleted bool
	var cause sql.NullString
	require.NoError(t, pg.QueryRow(`
		SELECT deleted_at IS NOT NULL, deletion_cause
		FROM sessions WHERE id = 'preserved-trash'`,
	).Scan(&deleted, &cause))
	assert.True(t, deleted, "migration must preserve legacy user trash")
	assert.False(t, cause.Valid, "legacy user trash must retain a NULL cause")
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

// TestEnsureSchemaCreatesMappingMirrorAndSessionProvenance verifies that
// EnsureSchema creates the source_worktree_project_mappings mirror table
// with all required columns, and that sessions gains the
// source_archive_id and file_path provenance columns. Also asserts
// idempotency across two EnsureSchema runs.
func TestEnsureSchemaCreatesMappingMirrorAndSessionProvenance(t *testing.T) {
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

	var count int
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.columns
		WHERE table_schema = $1
		  AND table_name = 'source_worktree_project_mappings'
		  AND column_name IN ('source_archive_id','machine','path_prefix',
		      'layout','project','original_project','enabled','updated_at')`,
		schemaTestSchema,
	).Scan(&count))
	assert.Equal(t, 8, count)

	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.columns
		WHERE table_schema = $1
		  AND table_name = 'sessions'
		  AND column_name IN ('source_archive_id','file_path')`,
		schemaTestSchema,
	).Scan(&count))
	assert.Equal(t, 2, count)
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

		INSERT INTO usage_events (
			session_id, source, model, cost_usd, dedup_key
		) VALUES (
			'legacy-money-session', 'provider', 'model', 0.0000005, 'midpoint'
		);
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
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT cost_microdollars FROM usage_events WHERE dedup_key = 'midpoint'
	`).Scan(&cost))
	require.True(t, cost.Valid)
	assert.Equal(t, int64(1), cost.Int64)

	var charged, fee int64
	var cursorDedupKey string
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT charged_microdollars, cursor_token_fee_microdollars, dedup_key
		FROM cursor_usage_events
	`).Scan(&charged, &fee, &cursorDedupKey))
	assert.Equal(t, int64(156600), charged)
	assert.Equal(t, int64(33200), fee)
	assert.NotEqual(t, "cursor-priced", cursorDedupKey)

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

func TestMigrateMoneyColumnsPGSerializesConcurrentUpgraders(t *testing.T) {
	pgURL := testPGURL(t)
	cleanSchemaTestPG(t, pgURL)
	t.Cleanup(func() { cleanSchemaTestPG(t, pgURL) })

	pg, err := Open(pgURL, schemaTestSchema, true)
	require.NoError(t, err, "connecting to pg")
	defer pg.Close()
	require.NoError(t, EnsureSchema(t.Context(), pg, schemaTestSchema))
	_, err = pg.ExecContext(t.Context(), `
		INSERT INTO sessions (id, machine, project, agent)
		VALUES ('concurrent-money', 'host', 'project', 'codex');
		INSERT INTO usage_events (
			session_id, source, model, cost_microdollars, dedup_key
		) VALUES ('concurrent-money', 'provider', 'model', 31250, 'cost');
		ALTER TABLE usage_events
			ALTER COLUMN cost_microdollars TYPE DOUBLE PRECISION
			USING cost_microdollars / 1000000.0;
		ALTER TABLE usage_events
			RENAME COLUMN cost_microdollars TO cost_usd;
	`)
	require.NoError(t, err, "simulate legacy money schema")

	blocker, err := pg.BeginTx(t.Context(), nil)
	require.NoError(t, err, "begin blocking transaction")
	defer func() { _ = blocker.Rollback() }()
	_, err = blocker.ExecContext(t.Context(),
		`LOCK TABLE usage_events IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err, "lock legacy money table")

	dsnA, err := appendConnParams(pgURL, map[string]string{
		"application_name": "money-migration-a",
	})
	require.NoError(t, err)
	dsnB, err := appendConnParams(pgURL, map[string]string{
		"application_name": "money-migration-b",
	})
	require.NoError(t, err)
	migratorA, err := Open(dsnA, schemaTestSchema, true)
	require.NoError(t, err)
	defer migratorA.Close()
	migratorB, err := Open(dsnB, schemaTestSchema, true)
	require.NoError(t, err)
	defer migratorB.Close()

	start := make(chan struct{})
	results := make(chan error, 2)
	for _, migrator := range []*sql.DB{migratorA, migratorB} {
		go func(conn *sql.DB) {
			<-start
			existingColumns, err := loadExistingColumns(
				t.Context(), conn, nil,
				"usage_events", "cursor_usage_events", "model_pricing",
			)
			if err == nil {
				err = migrateMoneyColumnsPG(
					t.Context(), conn, existingColumns,
				)
			}
			results <- err
		}(migrator)
	}
	close(start)
	require.Eventually(t, func() bool {
		var waiting int
		err := pg.QueryRowContext(t.Context(), `
			SELECT COUNT(*)
			FROM pg_stat_activity
			WHERE application_name IN (
				'money-migration-a', 'money-migration-b'
			)
			AND wait_event_type = 'Lock'
		`).Scan(&waiting)
		return err == nil && waiting == 2
	}, 5*time.Second, 10*time.Millisecond,
		"both migration attempts should reach the locked upgrade boundary")
	require.NoError(t, blocker.Commit(), "release legacy money table")
	require.NoError(t, <-results, "first concurrent migration")
	require.NoError(t, <-results, "second concurrent migration")

	var cost int64
	require.NoError(t, pg.QueryRowContext(t.Context(), `
		SELECT cost_microdollars
		FROM usage_events
		WHERE session_id = 'concurrent-money'
	`).Scan(&cost))
	assert.Equal(t, int64(31250), cost)
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
