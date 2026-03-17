package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// SchemaVersion is incremented when the PG schema changes in a
// way that requires migration logic. EnsureSchema writes it to
// sync_metadata so future versions can detect what they're
// working with.
const SchemaVersion = 1

// coreDDL creates the tables and indexes. It uses unqualified
// names because Open() sets search_path to the target schema.
const coreDDL = `
CREATE TABLE IF NOT EXISTS sync_metadata (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
    id                 TEXT PRIMARY KEY,
    machine            TEXT NOT NULL,
    project            TEXT NOT NULL,
    agent              TEXT NOT NULL,
    first_message      TEXT,
    display_name       TEXT,
    created_at         TIMESTAMPTZ,
    started_at         TIMESTAMPTZ,
    ended_at           TIMESTAMPTZ,
    deleted_at         TIMESTAMPTZ,
    message_count      INT NOT NULL DEFAULT 0,
    user_message_count INT NOT NULL DEFAULT 0,
    parent_session_id  TEXT,
    relationship_type  TEXT NOT NULL DEFAULT '',
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS messages (
    session_id     TEXT NOT NULL,
    ordinal        INT NOT NULL,
    role           TEXT NOT NULL,
    content        TEXT NOT NULL,
    timestamp      TIMESTAMPTZ,
    has_thinking   BOOLEAN NOT NULL DEFAULT FALSE,
    has_tool_use   BOOLEAN NOT NULL DEFAULT FALSE,
    content_length INT NOT NULL DEFAULT 0,
    PRIMARY KEY (session_id, ordinal),
    FOREIGN KEY (session_id)
        REFERENCES sessions(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS tool_calls (
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
        REFERENCES sessions(id) ON DELETE CASCADE
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_tool_calls_dedup
    ON tool_calls (session_id, message_ordinal, call_index);

CREATE INDEX IF NOT EXISTS idx_tool_calls_session
    ON tool_calls (session_id);
`

// EnsureSchema creates the schema (if needed), then runs
// idempotent CREATE TABLE / ALTER TABLE statements. The schema
// parameter is the unquoted schema name (e.g. "agentsview").
//
// After CREATE SCHEMA, all table DDL uses unqualified names
// because Open() sets search_path to the target schema.
func EnsureSchema(
	ctx context.Context, db *sql.DB, schema string,
) error {
	quoted, err := quoteIdentifier(schema)
	if err != nil {
		return fmt.Errorf("invalid schema name: %w", err)
	}
	if _, err := db.ExecContext(ctx,
		"CREATE SCHEMA IF NOT EXISTS "+quoted,
	); err != nil {
		return fmt.Errorf("creating pg schema: %w", err)
	}
	if _, err := db.ExecContext(ctx, coreDDL); err != nil {
		return fmt.Errorf("creating pg tables: %w", err)
	}

	// Idempotent column additions for forward compatibility.
	alters := []struct {
		stmt string
		desc string
	}{
		{
			`ALTER TABLE sessions
			 ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ`,
			"adding sessions.deleted_at",
		},
		{
			`ALTER TABLE sessions
			 ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ`,
			"adding sessions.created_at",
		},
		{
			`ALTER TABLE tool_calls
			 ADD COLUMN IF NOT EXISTS call_index
			 INT NOT NULL DEFAULT 0`,
			"adding tool_calls.call_index",
		},
	}
	for _, a := range alters {
		if _, err := db.ExecContext(ctx, a.stmt); err != nil {
			return fmt.Errorf("%s: %w", a.desc, err)
		}
	}

	// Record schema version for future migration detection.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO sync_metadata (key, value)
		 VALUES ('schema_version', $1)
		 ON CONFLICT (key) DO UPDATE
		 SET value = EXCLUDED.value
		 WHERE sync_metadata.value::int < EXCLUDED.value::int`,
		fmt.Sprintf("%d", SchemaVersion),
	); err != nil {
		return fmt.Errorf("setting schema version: %w", err)
	}
	return nil
}

// GetSchemaVersion reads the schema version from sync_metadata.
// Returns 0 if the key is missing (pre-versioned schema).
func GetSchemaVersion(
	ctx context.Context, db *sql.DB,
) (int, error) {
	var v int
	err := db.QueryRowContext(ctx,
		`SELECT value::int FROM sync_metadata
		 WHERE key = 'schema_version'`,
	).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf(
			"reading schema version: %w", err,
		)
	}
	return v, nil
}

// CheckSchemaCompat verifies that the PG schema has all columns
// required by query paths. This is a read-only probe that works
// against any PG role. Returns nil if compatible, or an error
// describing what is missing.
func CheckSchemaCompat(
	ctx context.Context, db *sql.DB,
) error {
	rows, err := db.QueryContext(ctx,
		`SELECT id, created_at, deleted_at, updated_at
		 FROM sessions LIMIT 0`)
	if err != nil {
		return fmt.Errorf(
			"sessions table missing required columns: %w",
			err,
		)
	}
	rows.Close()

	rows, err = db.QueryContext(ctx,
		`SELECT call_index FROM tool_calls LIMIT 0`)
	if err != nil {
		return fmt.Errorf(
			"tool_calls table missing required columns: %w",
			err,
		)
	}
	rows.Close()
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
