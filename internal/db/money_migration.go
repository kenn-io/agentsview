package db

import "fmt"

// migrateMoneyColumnsLocked performs the one-way, transactional conversion
// from floating-point dollars/cents to signed 64-bit microdollars. SQLite
// cannot change column types in place, so each affected table is rebuilt while
// preserving row IDs, constraints, and indexes.
func migrateMoneyColumnsLocked(w *writerHandle) error {
	legacy, err := sqliteColumnExists(w, "usage_events", "cost_usd")
	if err != nil {
		return err
	}
	legacyCursor, err := sqliteColumnExists(w, "cursor_usage_events", "charged_cents")
	if err != nil {
		return err
	}
	legacyPricing, err := sqliteColumnExists(w, "model_pricing", "input_per_mtok")
	if err != nil {
		return err
	}
	if !legacy && !legacyCursor && !legacyPricing {
		return nil
	}
	tx, err := w.Begin()
	if err != nil {
		return fmt.Errorf("beginning microdollar migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, check := range []struct {
		needed bool
		table  string
		column string
		max    string
	}{
		{legacy, "usage_events", "cost_usd", "9223372036854.775"},
		{legacyCursor, "cursor_usage_events", "charged_cents", "922337203685477.5"},
		{legacyCursor, "cursor_usage_events", "cursor_token_fee", "922337203685477.5"},
		{legacyPricing, "model_pricing", "input_per_mtok", "9223372036854.775"},
		{legacyPricing, "model_pricing", "output_per_mtok", "9223372036854.775"},
		{legacyPricing, "model_pricing", "cache_creation_per_mtok", "9223372036854.775"},
		{legacyPricing, "model_pricing", "cache_read_per_mtok", "9223372036854.775"},
	} {
		if !check.needed {
			continue
		}
		query := fmt.Sprintf(`SELECT EXISTS (
			SELECT 1 FROM %s WHERE %s IS NOT NULL AND (
				typeof(%s) NOT IN ('integer', 'real') OR
				NOT (%s >= 0 AND %s <= %s)
			)
		)`, check.table, check.column, check.column,
			check.column, check.column, check.max)
		var invalid bool
		if err := tx.QueryRow(query).Scan(&invalid); err != nil {
			return fmt.Errorf("validating legacy money column %s.%s: %w",
				check.table, check.column, err)
		}
		if invalid {
			return fmt.Errorf("legacy money column %s.%s contains a negative, non-finite, or out-of-range value",
				check.table, check.column)
		}
	}

	for _, migration := range []struct {
		needed bool
		name   string
		sql    string
	}{
		{legacy, "usage_events", sqliteUsageMicrodollarMigrationSQL},
		{legacyCursor, "cursor_usage_events", sqliteCursorMicrodollarMigrationSQL},
		{legacyPricing, "model_pricing", sqlitePricingMicrodollarMigrationSQL},
	} {
		if !migration.needed {
			continue
		}
		if _, err := tx.Exec(migration.sql); err != nil {
			return fmt.Errorf("migrating %s to microdollars: %w", migration.name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing microdollar migration: %w", err)
	}
	return nil
}

func sqliteColumnExists(w *writerHandle, table, column string) (bool, error) {
	var count int
	query := fmt.Sprintf(
		"SELECT count(*) FROM pragma_table_info('%s') WHERE name = ?",
		table,
	)
	if err := w.QueryRow(query, column).Scan(&count); err != nil {
		return false, fmt.Errorf("probing %s.%s: %w", table, column, err)
	}
	return count != 0, nil
}

const sqliteUsageMicrodollarMigrationSQL = `
ALTER TABLE usage_events RENAME TO usage_events_dollar_float_legacy;
CREATE TABLE usage_events (
    id INTEGER PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    message_ordinal INTEGER,
    source TEXT NOT NULL,
    model TEXT NOT NULL,
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0,
    cache_read_input_tokens INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens INTEGER NOT NULL DEFAULT 0,
    cost_microdollars INTEGER,
    cost_status TEXT NOT NULL DEFAULT '',
    cost_source TEXT NOT NULL DEFAULT '',
    occurred_at TEXT,
    dedup_key TEXT NOT NULL DEFAULT ''
);
INSERT INTO usage_events (
    id, session_id, message_ordinal, source, model,
    input_tokens, output_tokens, cache_creation_input_tokens,
    cache_read_input_tokens, reasoning_tokens, cost_microdollars,
    cost_status, cost_source, occurred_at, dedup_key
)
SELECT id, session_id, message_ordinal, source, model,
    input_tokens, output_tokens, cache_creation_input_tokens,
    cache_read_input_tokens, reasoning_tokens,
    CASE WHEN cost_usd IS NULL THEN NULL
         ELSE CAST(ROUND(cost_usd * 1000000.0) AS INTEGER) END,
    cost_status, cost_source, occurred_at, dedup_key
FROM usage_events_dollar_float_legacy;
DROP TABLE usage_events_dollar_float_legacy;
CREATE UNIQUE INDEX idx_usage_events_dedup
    ON usage_events(session_id, source, dedup_key) WHERE dedup_key != '';
CREATE INDEX idx_usage_events_session ON usage_events(session_id);
CREATE INDEX idx_usage_events_occurred ON usage_events(occurred_at);
`

const sqliteCursorMicrodollarMigrationSQL = `
ALTER TABLE cursor_usage_events RENAME TO cursor_usage_events_dollar_float_legacy;
CREATE TABLE cursor_usage_events (
    id INTEGER PRIMARY KEY,
    occurred_at TEXT NOT NULL,
    model TEXT NOT NULL,
    kind TEXT NOT NULL DEFAULT '',
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    cache_write_tokens INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens INTEGER NOT NULL DEFAULT 0,
    charged_microdollars INTEGER NOT NULL DEFAULT 0,
    cursor_token_fee_microdollars INTEGER NOT NULL DEFAULT 0,
    user_id TEXT NOT NULL DEFAULT '',
    user_email TEXT NOT NULL DEFAULT '',
    is_headless INTEGER NOT NULL DEFAULT 0,
    dedup_key TEXT NOT NULL DEFAULT ''
);
INSERT INTO cursor_usage_events (
    id, occurred_at, model, kind, input_tokens, output_tokens,
    cache_write_tokens, cache_read_tokens, charged_microdollars,
    cursor_token_fee_microdollars, user_id, user_email, is_headless, dedup_key
)
SELECT id, occurred_at, model, kind, input_tokens, output_tokens,
    cache_write_tokens, cache_read_tokens,
    CAST(ROUND(charged_cents * 10000.0) AS INTEGER),
    CAST(ROUND(cursor_token_fee * 10000.0) AS INTEGER),
    user_id, user_email, is_headless, dedup_key
FROM cursor_usage_events_dollar_float_legacy;
DROP TABLE cursor_usage_events_dollar_float_legacy;
CREATE UNIQUE INDEX idx_cursor_usage_events_dedup
    ON cursor_usage_events(dedup_key) WHERE dedup_key != '';
CREATE INDEX idx_cursor_usage_events_occurred ON cursor_usage_events(occurred_at);
CREATE INDEX idx_cursor_usage_events_model ON cursor_usage_events(model);
`

const sqlitePricingMicrodollarMigrationSQL = `
ALTER TABLE model_pricing RENAME TO model_pricing_dollar_float_legacy;
CREATE TABLE model_pricing (
    model_pattern TEXT PRIMARY KEY,
    input_microdollars_per_mtok INTEGER NOT NULL DEFAULT 0,
    output_microdollars_per_mtok INTEGER NOT NULL DEFAULT 0,
    cache_creation_microdollars_per_mtok INTEGER NOT NULL DEFAULT 0,
    cache_read_microdollars_per_mtok INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
INSERT INTO model_pricing (
    model_pattern, input_microdollars_per_mtok,
    output_microdollars_per_mtok, cache_creation_microdollars_per_mtok,
    cache_read_microdollars_per_mtok, updated_at
)
SELECT model_pattern,
    CAST(ROUND(input_per_mtok * 1000000.0) AS INTEGER),
    CAST(ROUND(output_per_mtok * 1000000.0) AS INTEGER),
    CAST(ROUND(cache_creation_per_mtok * 1000000.0) AS INTEGER),
    CAST(ROUND(cache_read_per_mtok * 1000000.0) AS INTEGER),
    updated_at
FROM model_pricing_dollar_float_legacy;
DROP TABLE model_pricing_dollar_float_legacy;
`
