package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateMoneyColumnsConvertsLegacyFloatsTransactionally(t *testing.T) {
	d := testDB(t)
	legacyDDL := `
DROP TABLE usage_events;
DROP TABLE cursor_usage_events;
DROP TABLE model_pricing;
CREATE TABLE usage_events (
 id INTEGER PRIMARY KEY, session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
 message_ordinal INTEGER, source TEXT NOT NULL, model TEXT NOT NULL,
 input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0,
 cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0,
 cache_read_input_tokens INTEGER NOT NULL DEFAULT 0, reasoning_tokens INTEGER NOT NULL DEFAULT 0,
 cost_usd REAL, cost_status TEXT NOT NULL DEFAULT '', cost_source TEXT NOT NULL DEFAULT '',
 occurred_at TEXT, dedup_key TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX idx_usage_events_dedup ON usage_events(session_id, source, dedup_key) WHERE dedup_key != '';
CREATE INDEX idx_usage_events_session ON usage_events(session_id);
CREATE INDEX idx_usage_events_occurred ON usage_events(occurred_at);
CREATE TABLE cursor_usage_events (
 id INTEGER PRIMARY KEY, occurred_at TEXT NOT NULL, model TEXT NOT NULL, kind TEXT NOT NULL DEFAULT '',
 input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0,
 cache_write_tokens INTEGER NOT NULL DEFAULT 0, cache_read_tokens INTEGER NOT NULL DEFAULT 0,
 charged_cents REAL NOT NULL DEFAULT 0, cursor_token_fee REAL NOT NULL DEFAULT 0,
 user_id TEXT NOT NULL DEFAULT '', user_email TEXT NOT NULL DEFAULT '', is_headless INTEGER NOT NULL DEFAULT 0,
 dedup_key TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX idx_cursor_usage_events_dedup ON cursor_usage_events(dedup_key) WHERE dedup_key != '';
CREATE INDEX idx_cursor_usage_events_occurred ON cursor_usage_events(occurred_at);
CREATE INDEX idx_cursor_usage_events_model ON cursor_usage_events(model);
CREATE TABLE model_pricing (
 model_pattern TEXT PRIMARY KEY, input_per_mtok REAL NOT NULL DEFAULT 0,
 output_per_mtok REAL NOT NULL DEFAULT 0, cache_creation_per_mtok REAL NOT NULL DEFAULT 0,
 cache_read_per_mtok REAL NOT NULL DEFAULT 0, updated_at TEXT NOT NULL
);`
	_, err := d.rawWriter().Exec(legacyDDL)
	require.NoError(t, err)

	insertSession(t, d, "money-migration", "project")
	_, err = d.rawWriter().Exec(`
INSERT INTO usage_events (id, session_id, source, model, cost_usd, dedup_key)
VALUES (41, 'money-migration', 'provider', 'model', 0.0123456, 'usage-key'),
       (42, 'money-migration', 'provider', 'model', NULL, 'usage-null');
INSERT INTO cursor_usage_events (id, occurred_at, model, charged_cents, cursor_token_fee, dedup_key)
VALUES (51, '2026-07-21T12:00:00Z', 'model', 15.66, 3.32, 'cursor-key');
INSERT INTO model_pricing (model_pattern, input_per_mtok, output_per_mtok, cache_creation_per_mtok, cache_read_per_mtok, updated_at)
VALUES ('model', 3, 15, 3.75, 0.3, '2026-07-21T12:00:00Z');`)
	require.NoError(t, err)

	require.NoError(t, migrateMoneyColumnsLocked(d.getWriter()))

	assertSQLiteMoneyColumn(t, d, "usage_events", "cost_microdollars", "INTEGER")
	assertSQLiteMoneyColumn(t, d, "cursor_usage_events", "charged_microdollars", "INTEGER")
	assertSQLiteMoneyColumn(t, d, "model_pricing", "input_microdollars_per_mtok", "INTEGER")
	assertSQLiteColumnAbsent(t, d, "usage_events", "cost_usd")
	assertSQLiteColumnAbsent(t, d, "cursor_usage_events", "charged_cents")
	assertSQLiteColumnAbsent(t, d, "model_pricing", "input_per_mtok")

	var usageID, usageCost int64
	require.NoError(t, d.rawWriter().QueryRow(
		`SELECT id, cost_microdollars FROM usage_events WHERE dedup_key = 'usage-key'`,
	).Scan(&usageID, &usageCost))
	assert.Equal(t, int64(41), usageID)
	assert.Equal(t, int64(12_346), usageCost)
	var nullCount int
	require.NoError(t, d.rawWriter().QueryRow(
		`SELECT count(*) FROM usage_events WHERE id = 42 AND cost_microdollars IS NULL`,
	).Scan(&nullCount))
	assert.Equal(t, 1, nullCount)

	var cursorID, charged, fee int64
	require.NoError(t, d.rawWriter().QueryRow(`
SELECT id, charged_microdollars, cursor_token_fee_microdollars
FROM cursor_usage_events WHERE dedup_key = 'cursor-key'`,
	).Scan(&cursorID, &charged, &fee))
	assert.Equal(t, int64(51), cursorID)
	assert.Equal(t, int64(156_600), charged)
	assert.Equal(t, int64(33_200), fee)

	var input, output, creation, read int64
	require.NoError(t, d.rawWriter().QueryRow(`
SELECT input_microdollars_per_mtok, output_microdollars_per_mtok,
       cache_creation_microdollars_per_mtok, cache_read_microdollars_per_mtok
FROM model_pricing WHERE model_pattern = 'model'`,
	).Scan(&input, &output, &creation, &read))
	assert.Equal(t, int64(3_000_000), input)
	assert.Equal(t, int64(15_000_000), output)
	assert.Equal(t, int64(3_750_000), creation)
	assert.Equal(t, int64(300_000), read)

	// The migration is one-way and idempotent once the legacy columns are gone.
	require.NoError(t, migrateMoneyColumnsLocked(d.getWriter()))
}

func TestMigrateMoneyColumnsRejectsInvalidLegacyValueWithoutChangingSchema(t *testing.T) {
	d := testDB(t)
	_, err := d.rawWriter().Exec(`
DROP TABLE usage_events;
CREATE TABLE usage_events (
 id INTEGER PRIMARY KEY, session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
 message_ordinal INTEGER, source TEXT NOT NULL, model TEXT NOT NULL,
 input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0,
 cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0,
 cache_read_input_tokens INTEGER NOT NULL DEFAULT 0, reasoning_tokens INTEGER NOT NULL DEFAULT 0,
 cost_usd REAL, cost_status TEXT NOT NULL DEFAULT '', cost_source TEXT NOT NULL DEFAULT '',
 occurred_at TEXT, dedup_key TEXT NOT NULL DEFAULT ''
);`)
	require.NoError(t, err)
	insertSession(t, d, "invalid-money-migration", "project")
	_, err = d.rawWriter().Exec(`
INSERT INTO usage_events (session_id, source, model, cost_usd)
VALUES ('invalid-money-migration', 'provider', 'model', -0.01)`)
	require.NoError(t, err)

	err = migrateMoneyColumnsLocked(d.getWriter())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "usage_events.cost_usd")
	assertSQLiteMoneyColumn(t, d, "usage_events", "cost_usd", "REAL")
	assertSQLiteColumnAbsent(t, d, "usage_events", "cost_microdollars")
}

func assertSQLiteMoneyColumn(t *testing.T, d *DB, table, column, wantType string) {
	t.Helper()
	var gotType string
	err := d.rawWriter().QueryRow(
		`SELECT type FROM pragma_table_info(?) WHERE name = ?`, table, column,
	).Scan(&gotType)
	require.NoError(t, err)
	assert.Equal(t, wantType, gotType)
}

func assertSQLiteColumnAbsent(t *testing.T, d *DB, table, column string) {
	t.Helper()
	var count int
	require.NoError(t, d.rawWriter().QueryRow(
		`SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`, table, column,
	).Scan(&count))
	assert.Zero(t, count)
}
