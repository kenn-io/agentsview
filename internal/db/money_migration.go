package db

import (
	"database/sql"
	"fmt"
)

// migrateMoneyColumnsLocked performs the one-way, transactional conversion
// from floating-point dollars/cents to signed 64-bit microdollars. SQLite
// cannot change column types in place, so each affected table is rebuilt while
// preserving row IDs, constraints, and indexes.
func migrateMoneyColumnsLocked(w *writerHandle) error {
	tableStates := map[string]bool{}
	for _, table := range []struct {
		name   string
		legacy []string
		final  []string
	}{
		{
			name:   "usage_events",
			legacy: []string{"cost_usd"},
			final:  []string{"cost_microdollars"},
		},
		{
			name:   "cursor_usage_events",
			legacy: []string{"charged_cents", "cursor_token_fee"},
			final: []string{
				"charged_microdollars", "cursor_token_fee_microdollars",
			},
		},
		{
			name: "model_pricing",
			legacy: []string{
				"input_per_mtok", "output_per_mtok",
				"cache_creation_per_mtok", "cache_read_per_mtok",
			},
			final: []string{
				"input_microdollars_per_mtok", "output_microdollars_per_mtok",
				"cache_creation_microdollars_per_mtok",
				"cache_read_microdollars_per_mtok",
			},
		},
	} {
		legacyCount, finalCount := 0, 0
		for _, column := range table.legacy {
			exists, err := sqliteColumnExists(w, table.name, column)
			if err != nil {
				return err
			}
			if exists {
				legacyCount++
			}
		}
		for _, column := range table.final {
			exists, err := sqliteColumnExists(w, table.name, column)
			if err != nil {
				return err
			}
			if exists {
				finalCount++
			}
		}
		switch {
		case legacyCount == len(table.legacy) && finalCount == 0:
			tableStates[table.name] = true
		case legacyCount == 0 && finalCount == len(table.final):
			tableStates[table.name] = false
		default:
			return fmt.Errorf(
				"ambiguous money schema for %s: expected complete legacy or microdollar columns",
				table.name,
			)
		}
	}
	legacy := tableStates["usage_events"]
	legacyCursor := tableStates["cursor_usage_events"]
	legacyPricing := tableStates["model_pricing"]
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
	if legacyPricing {
		var bandCount int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM model_pricing_bands`,
		).Scan(&bandCount); err != nil {
			return fmt.Errorf("checking legacy pricing bands: %w", err)
		}
		if bandCount != 0 {
			return fmt.Errorf(
				"legacy model_pricing migration requires an empty model_pricing_bands table",
			)
		}
		// schema.sql creates this child before the legacy parent is rebuilt.
		// Keep its removal and recreation in the same transaction so a failed
		// money migration cannot leave the archive without the band schema.
		if _, err := tx.Exec(`DROP TABLE model_pricing_bands`); err != nil {
			return fmt.Errorf("preparing legacy pricing bands: %w", err)
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
	if legacyPricing {
		if _, err := tx.Exec(modelPricingBandsSchemaSQL); err != nil {
			return fmt.Errorf("recreating pricing bands: %w", err)
		}
	}
	if legacyCursor {
		if err := rekeyMigratedCursorUsageEvents(tx); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing microdollar migration: %w", err)
	}
	return nil
}

func rekeyMigratedCursorUsageEvents(tx *sql.Tx) error {
	type keyUpdate struct {
		id  int64
		key string
	}
	var lastID int64
	for {
		rows, err := tx.Query(`
			SELECT id, occurred_at, model, kind,
				input_tokens, output_tokens, cache_write_tokens, cache_read_tokens,
				charged_microdollars, cursor_token_fee_microdollars,
				user_id, user_email, is_headless
			FROM cursor_usage_events
			WHERE id > ?
			ORDER BY id
			LIMIT 1000`, lastID)
		if err != nil {
			return fmt.Errorf("querying migrated cursor usage keys: %w", err)
		}
		updates := make([]keyUpdate, 0, 1000)
		for rows.Next() {
			var id int64
			var ev CursorUsageEvent
			if err := rows.Scan(
				&id, &ev.OccurredAt, &ev.Model, &ev.Kind,
				&ev.InputTokens, &ev.OutputTokens,
				&ev.CacheWriteTokens, &ev.CacheReadTokens,
				&ev.Charged.Microdollars, &ev.CursorTokenFee.Microdollars,
				&ev.UserID, &ev.UserEmail, &ev.IsHeadless,
			); err != nil {
				rows.Close()
				return fmt.Errorf("scanning migrated cursor usage key: %w", err)
			}
			updates = append(updates, keyUpdate{
				id: id, key: CursorUsageEventDedupKey(ev),
			})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("iterating migrated cursor usage keys: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("closing migrated cursor usage keys: %w", err)
		}
		if len(updates) == 0 {
			break
		}
		for _, update := range updates {
			if _, err := tx.Exec(
				`UPDATE cursor_usage_events SET dedup_key = ? WHERE id = ?`,
				update.key, update.id,
			); err != nil {
				return fmt.Errorf("updating migrated cursor usage key: %w", err)
			}
		}
		lastID = updates[len(updates)-1].id
	}
	if _, err := tx.Exec(`
		DELETE FROM cursor_usage_events
		WHERE dedup_key != '' AND id NOT IN (
			SELECT MIN(id) FROM cursor_usage_events
			WHERE dedup_key != '' GROUP BY dedup_key
		);
		CREATE UNIQUE INDEX idx_cursor_usage_events_dedup
			ON cursor_usage_events(dedup_key) WHERE dedup_key != '';
	`); err != nil {
		return fmt.Errorf("deduplicating migrated cursor usage keys: %w", err)
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
    provider_id TEXT NOT NULL DEFAULT '',
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
    id, session_id, message_ordinal, source, model, provider_id,
    input_tokens, output_tokens, cache_creation_input_tokens,
    cache_read_input_tokens, reasoning_tokens, cost_microdollars,
    cost_status, cost_source, occurred_at, dedup_key
)
SELECT id, session_id, message_ordinal, source, model, '' AS provider_id,
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
