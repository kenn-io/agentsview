package pgsync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

const toolCallsSchemaVersionKey = "tool_calls_call_index_version"
const toolCallsSchemaVersionValue = "1"

// pgSchema uses TEXT for timestamp columns so values round-trip
// unchanged between SQLite (which stores ISO-8601 strings) and PG.
// The updated_at column is compared lexicographically, which works
// for ISO-8601 formatted UTC timestamps.
var pgSchema = `
CREATE SCHEMA IF NOT EXISTS agentsview;

CREATE TABLE IF NOT EXISTS agentsview.sync_metadata (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS agentsview.sessions (
    id                 TEXT PRIMARY KEY,
    machine            TEXT NOT NULL,
    project            TEXT NOT NULL,
    agent              TEXT NOT NULL,
    first_message      TEXT,
    display_name       TEXT,
    created_at         TEXT NOT NULL DEFAULT '',
    started_at         TEXT,
    ended_at           TEXT,
    deleted_at         TEXT,
    message_count      INT NOT NULL DEFAULT 0,
    user_message_count INT NOT NULL DEFAULT 0,
    parent_session_id  TEXT,
    relationship_type  TEXT NOT NULL DEFAULT '',
    updated_at         TEXT NOT NULL DEFAULT ` + pgTimestampSQL("NOW() AT TIME ZONE 'UTC'") + `
);

CREATE TABLE IF NOT EXISTS agentsview.messages (
    session_id     TEXT NOT NULL,
    ordinal        INT NOT NULL,
    role           TEXT NOT NULL,
    content        TEXT NOT NULL,
    timestamp      TEXT,
    has_thinking   BOOLEAN NOT NULL DEFAULT FALSE,
    has_tool_use   BOOLEAN NOT NULL DEFAULT FALSE,
    content_length INT NOT NULL DEFAULT 0,
    PRIMARY KEY (session_id, ordinal),
    FOREIGN KEY (session_id)
        REFERENCES agentsview.sessions(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS agentsview.tool_calls (
    id                    BIGSERIAL PRIMARY KEY,
    session_id            TEXT NOT NULL,
    tool_name             TEXT NOT NULL,
    category              TEXT NOT NULL,
    call_index            INT NOT NULL DEFAULT 0,
    tool_use_id           TEXT NOT NULL DEFAULT '',
    input_json            TEXT,
    skill_name            TEXT,
    result_content_length INT,
    result_content        TEXT,
    subagent_session_id   TEXT,
    message_ordinal       INT NOT NULL,
    FOREIGN KEY (session_id)
        REFERENCES agentsview.sessions(id) ON DELETE CASCADE
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_tool_calls_dedup
    ON agentsview.tool_calls (session_id, message_ordinal, call_index);

CREATE INDEX IF NOT EXISTS idx_tool_calls_session
    ON agentsview.tool_calls (session_id);
`

// ensureSchema runs the PG schema DDL idempotently.
func ensureSchema(ctx context.Context, pg *sql.DB) error {
	_, err := pg.ExecContext(ctx, pgSchema)
	if err != nil {
		return fmt.Errorf("creating pg schema: %w", err)
	}
	if _, err := pg.ExecContext(ctx, `
		ALTER TABLE agentsview.sessions
		ADD COLUMN IF NOT EXISTS deleted_at TEXT
	`); err != nil {
		return fmt.Errorf("adding sessions.deleted_at: %w", err)
	}
	if _, err := pg.ExecContext(ctx, `
		ALTER TABLE agentsview.sessions
		ADD COLUMN IF NOT EXISTS created_at TEXT NOT NULL DEFAULT ''
	`); err != nil {
		return fmt.Errorf("adding sessions.created_at: %w", err)
	}
	if _, err := pg.ExecContext(ctx, `
		ALTER TABLE agentsview.tool_calls
		ADD COLUMN IF NOT EXISTS call_index INT NOT NULL DEFAULT 0
	`); err != nil {
		return fmt.Errorf("adding tool_calls.call_index: %w", err)
	}
	if err := ensureToolCallsSchema(ctx, pg); err != nil {
		return err
	}
	// Normalize updated_at before using it as a created_at fallback.
	if err := normalizePGUpdatedAt(ctx, pg); err != nil {
		return err
	}
	// Backfill empty created_at from existing timestamp columns so
	// historical rows sort correctly in PG read mode. Runs after
	// normalizePGUpdatedAt so the updated_at fallback is already
	// in ISO-8601 format.
	if _, err := pg.ExecContext(ctx, `
		UPDATE agentsview.sessions
		SET created_at = COALESCE(
			NULLIF(started_at, ''),
			NULLIF(ended_at, ''),
			NULLIF(updated_at, ''),
			''
		)
		WHERE created_at = '' AND (
			COALESCE(started_at, '') != ''
			OR COALESCE(ended_at, '') != ''
			OR COALESCE(updated_at, '') != ''
		)
	`); err != nil {
		return fmt.Errorf("backfilling sessions.created_at: %w", err)
	}
	return nil
}

func ensureToolCallsSchema(ctx context.Context, pg *sql.DB) error {
	var version string
	err := pg.QueryRowContext(ctx, `
		SELECT value
		FROM agentsview.sync_metadata
		WHERE key = $1
	`, toolCallsSchemaVersionKey).Scan(&version)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("reading tool_calls schema version: %w", err)
	}
	if version == toolCallsSchemaVersionValue {
		return nil
	}

	tx, err := pg.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tool_calls schema tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Serialize concurrent schema migrations across processes.
	if _, err := tx.ExecContext(ctx,
		"SELECT pg_advisory_xact_lock(hashtext('agentsview_tool_calls_schema'))",
	); err != nil {
		return fmt.Errorf("acquiring schema migration lock: %w", err)
	}

	// Re-check version after acquiring lock.
	err = tx.QueryRowContext(ctx, `
		SELECT value
		FROM agentsview.sync_metadata
		WHERE key = $1
	`, toolCallsSchemaVersionKey).Scan(&version)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("re-reading tool_calls schema version: %w", err)
	}
	if version == toolCallsSchemaVersionValue {
		return nil
	}

	if _, err := tx.ExecContext(ctx, `
		WITH ranked AS (
			SELECT id,
				ROW_NUMBER() OVER (
					PARTITION BY session_id, message_ordinal
					ORDER BY id
				) - 1 AS call_index
			FROM agentsview.tool_calls
		)
		UPDATE agentsview.tool_calls AS tc
		SET call_index = ranked.call_index
		FROM ranked
		WHERE tc.id = ranked.id
	`); err != nil {
		return fmt.Errorf("backfilling tool_calls.call_index: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		"DROP INDEX IF EXISTS agentsview.idx_tool_calls_dedup",
	); err != nil {
		return fmt.Errorf("dropping old tool_calls dedupe index: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		CREATE UNIQUE INDEX IF NOT EXISTS idx_tool_calls_dedup
		ON agentsview.tool_calls (
			session_id, message_ordinal, call_index
		)
	`); err != nil {
		return fmt.Errorf("creating tool_calls dedupe index: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO agentsview.sync_metadata (key, value)
		VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value
	`, toolCallsSchemaVersionKey, toolCallsSchemaVersionValue); err != nil {
		return fmt.Errorf("recording tool_calls schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tool_calls schema tx: %w", err)
	}
	return nil
}

// IsReadOnlyError returns true when the error indicates a PG
// read-only or insufficient-privilege condition (SQLSTATE 25006
// or 42501). Uses pgconn.PgError for reliable SQLSTATE matching.
func IsReadOnlyError(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "25006" || pgErr.Code == "42501"
	}
	return false
}

// CheckSchemaCompat verifies that the PG schema has all columns
// required by the pg-read query paths. This is a read-only probe
// that works against any PG role. Returns nil if compatible, or
// an error describing what is missing.
func CheckSchemaCompat(ctx context.Context, pg *sql.DB) error {
	// Probe sessions table for required columns.
	rows, err := pg.QueryContext(ctx,
		`SELECT id, created_at, deleted_at, updated_at
		 FROM agentsview.sessions LIMIT 0`)
	if err != nil {
		return fmt.Errorf(
			"sessions table missing required columns: %w", err)
	}
	rows.Close()
	// Probe tool_calls table for call_index.
	rows, err = pg.QueryContext(ctx,
		`SELECT call_index
		 FROM agentsview.tool_calls LIMIT 0`)
	if err != nil {
		return fmt.Errorf(
			"tool_calls table missing required columns: %w", err)
	}
	rows.Close()
	return nil
}
