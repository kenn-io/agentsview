package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// ledgerDDL mirrors the SQLite ledger tables (internal/db/schema.sql).
// events_json is TEXT, not JSONB: it must keep the exact bytes the CRC
// covers.
const ledgerDDL = `
CREATE TABLE IF NOT EXISTS ledger_segments (
    zone        TEXT NOT NULL,
    source      TEXT NOT NULL,
    source_seq  BIGINT NOT NULL,
    checksum    BIGINT NOT NULL,
    created_at  TEXT NOT NULL,
    event_count INTEGER NOT NULL,
    events_json TEXT NOT NULL,
    origin      TEXT NOT NULL,
    ingested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (zone, source, source_seq)
);
CREATE INDEX IF NOT EXISTS idx_ledger_segments_ingested
    ON ledger_segments (ingested_at);

CREATE TABLE IF NOT EXISTS ledger_events (
    event_id       TEXT PRIMARY KEY,
    zone           TEXT NOT NULL,
    source         TEXT NOT NULL,
    source_seq     BIGINT NOT NULL,
    timestamp      TIMESTAMPTZ NOT NULL,
    correlation_id TEXT,
    causation_id   TEXT,
    actor_ref      TEXT,
    object_ref     TEXT,
    event_class    TEXT NOT NULL,
    payload_tier   TEXT NOT NULL,
    payload        TEXT,
    subsystem      TEXT NOT NULL DEFAULT '',
    summary        TEXT NOT NULL DEFAULT '',
    segment_source TEXT NOT NULL,
    segment_seq    BIGINT NOT NULL,
    event_json     TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_ledger_events_timestamp
    ON ledger_events (timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_ledger_events_zone_time
    ON ledger_events (zone, timestamp DESC, event_id DESC);
CREATE INDEX IF NOT EXISTS idx_ledger_events_zone_class
    ON ledger_events (zone, event_class);
CREATE INDEX IF NOT EXISTS idx_ledger_events_subsystem
    ON ledger_events (subsystem);
CREATE INDEX IF NOT EXISTS idx_ledger_events_actor
    ON ledger_events (actor_ref) WHERE actor_ref IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_ledger_events_object
    ON ledger_events (object_ref) WHERE object_ref IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_ledger_events_correlation
    ON ledger_events (correlation_id) WHERE correlation_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_ledger_events_segment
    ON ledger_events (zone, segment_source, segment_seq);

CREATE TABLE IF NOT EXISTS ledger_verify_state (
    zone          TEXT NOT NULL,
    source        TEXT NOT NULL,
    verified_seq  BIGINT NOT NULL,
    failures_json TEXT NOT NULL,
    missing_json  TEXT NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (zone, source)
);

CREATE TABLE IF NOT EXISTS ledger_import_state (
    zone             TEXT NOT NULL,
    path             TEXT NOT NULL,
    last_report_json TEXT NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (zone, path)
);
`

const ledgerAppendOnlyFunctionBody = `
BEGIN
    RAISE EXCEPTION 'ledger is append-only'
        USING ERRCODE = '55000';
END;
`

// ledgerAppendOnlyDDL follows rawIngestAppendOnlyDDL
// (raw_ingest_schema.go:185-227).
const ledgerAppendOnlyDDL = `
CREATE OR REPLACE FUNCTION ledger_reject_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $ledger_immutable$` + ledgerAppendOnlyFunctionBody + `$ledger_immutable$;

DROP TRIGGER IF EXISTS ledger_segments_append_only ON ledger_segments;
CREATE TRIGGER ledger_segments_append_only
BEFORE UPDATE OR DELETE ON ledger_segments
FOR EACH ROW EXECUTE FUNCTION ledger_reject_mutation();

DROP TRIGGER IF EXISTS ledger_events_append_only ON ledger_events;
CREATE TRIGGER ledger_events_append_only
BEFORE UPDATE OR DELETE ON ledger_events
FOR EACH ROW EXECUTE FUNCTION ledger_reject_mutation();
`

const ledgerAppendOnlyGuardsCurrentSQL = `
SELECT EXISTS (
    SELECT 1
    FROM pg_proc p
    JOIN pg_namespace n ON n.oid = p.pronamespace
    WHERE n.nspname = current_schema()
        AND p.proname = 'ledger_reject_mutation'
        AND p.prorettype = 'trigger'::regtype
        AND p.prosrc = $1
) AND (
    SELECT count(*)
    FROM pg_trigger t
    JOIN pg_class c ON c.oid = t.tgrelid
    JOIN pg_namespace n ON n.oid = c.relnamespace
    JOIN pg_proc p ON p.oid = t.tgfoid
    WHERE n.nspname = current_schema()
        AND p.pronamespace = n.oid
        AND p.proname = 'ledger_reject_mutation'
        AND p.prosrc = $1
        AND t.tgenabled IN ('O', 'A')
        -- ROW (1) | BEFORE (2) | DELETE (8) | UPDATE (16).
        AND t.tgtype = 27
        AND ((t.tgname = 'ledger_segments_append_only'
                AND c.relname = 'ledger_segments')
            OR (t.tgname = 'ledger_events_append_only'
                AND c.relname = 'ledger_events'))
) = 2`

// ensureLedgerSchemaPG creates the ledger tables and their append-only
// guards. CockroachDB lacks the trigger support; there the guard stays
// application-enforced, as for raw custody.
func ensureLedgerSchemaPG(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, ledgerDDL); err != nil {
		return fmt.Errorf("creating ledger schema: %w", err)
	}
	if err := installLedgerAppendOnlyGuardsPG(ctx, db); err != nil {
		if !ledgerAppendOnlyUnsupported(err) {
			return fmt.Errorf("installing ledger append-only guards: %w", err)
		}
		log.Printf("pg schema: ledger append-only triggers unsupported; " +
			"immutability remains application-enforced")
	}
	return nil
}

func missingLedgerTablesPG(ctx context.Context, db *sql.DB) ([]string, error) {
	missing := make([]string, 0)
	for _, table := range []string{
		"ledger_segments",
		"ledger_events",
		"ledger_verify_state",
		"ledger_import_state",
	} {
		var exists bool
		if err := db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.tables
				WHERE table_schema = current_schema()
					AND table_name = $1
					AND table_type = 'BASE TABLE'
			)`, table).Scan(&exists); err != nil {
			return nil, fmt.Errorf("checking for ledger table %s: %w", table, err)
		}
		if !exists {
			missing = append(missing, table)
		}
	}
	return missing, nil
}

func ledgerAppendOnlyGuardsCurrentPG(ctx context.Context, db *sql.DB) (bool, error) {
	var current bool
	err := db.QueryRowContext(ctx, ledgerAppendOnlyGuardsCurrentSQL,
		ledgerAppendOnlyFunctionBody,
	).Scan(&current)
	return current, err
}

// installLedgerAppendOnlyGuardsPG probes first to keep compatible schemas on
// the fast path. When guards need repair, serialize installers using a
// schema-scoped sync_metadata row, as the raw custody installer does.
func installLedgerAppendOnlyGuardsPG(ctx context.Context, db *sql.DB) error {
	current, err := ledgerAppendOnlyGuardsCurrentPG(ctx, db)
	if err != nil {
		return fmt.Errorf("probing ledger append-only guards: %w", err)
	}
	if current {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning ledger guard installation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockSyncMetadataRow(ctx, tx, ledgerSchemaLockKey); err != nil {
		return fmt.Errorf("locking ledger schema installation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, ledgerAppendOnlyDDL); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing ledger guard installation: %w", err)
	}
	return nil
}

const ledgerSchemaLockKey = "ledger_schema_lock"

func ledgerAppendOnlyUnsupported(err error) bool {
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		return pgErr.Code == "0A000"
	}
	return strings.Contains(strings.ToUpper(err.Error()), "SQLSTATE 0A000")
}
