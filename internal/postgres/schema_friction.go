package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/agentsview/internal/friction"
)

const frictionFindingsSchemaDDL = `
CREATE TABLE IF NOT EXISTS friction_findings (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    session_id       TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    kind             TEXT NOT NULL,
    detector         TEXT NOT NULL,
    message_ordinal  INTEGER,
    call_index       INTEGER,
    tool_name        TEXT NOT NULL DEFAULT '',
    label            TEXT NOT NULL DEFAULT '',
    text             TEXT NOT NULL DEFAULT '',
    evidence         TEXT NOT NULL DEFAULT '',
    title            TEXT NOT NULL,
    fingerprint      TEXT NOT NULL,
    occurred_at      TIMESTAMPTZ,
    seq              INTEGER NOT NULL,
    rules_version    TEXT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_friction_findings_session
    ON friction_findings (session_id);

CREATE INDEX IF NOT EXISTS idx_friction_findings_fingerprint
    ON friction_findings (fingerprint);

CREATE INDEX IF NOT EXISTS idx_friction_findings_kind
    ON friction_findings (kind);

CREATE TABLE IF NOT EXISTS friction_session_dims (
    session_id       TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    seat             TEXT NOT NULL DEFAULT '',
    persona          TEXT NOT NULL DEFAULT '',
    channel          TEXT NOT NULL DEFAULT '',
    dims_source      TEXT NOT NULL DEFAULT '',
    review_excluded  BOOLEAN NOT NULL DEFAULT FALSE
);
`

const frictionReviewSchemaDDL = `
CREATE TABLE IF NOT EXISTS friction_digests (
    date             TEXT PRIMARY KEY,
    timezone         TEXT NOT NULL,
    rules_version    TEXT NOT NULL,
    built_at         TIMESTAMPTZ NOT NULL,
    revision         INTEGER NOT NULL DEFAULT 1,
    sessions_scanned INTEGER NOT NULL,
    snapshot_json    TEXT NOT NULL,
    summary_json     TEXT NOT NULL,
    markdown         TEXT NOT NULL,
    markdown_sha256  TEXT NOT NULL,
    run_id           TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS friction_digest_sessions (
    subject_id   TEXT PRIMARY KEY,
    date         TEXT NOT NULL,
    subject_kind TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_friction_digest_sessions_date
    ON friction_digest_sessions (date);

CREATE TABLE IF NOT EXISTS friction_digest_fingerprints (
    date        TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    PRIMARY KEY (date, fingerprint)
);

CREATE INDEX IF NOT EXISTS idx_friction_digest_fingerprints_fingerprint
    ON friction_digest_fingerprints (fingerprint, date);

CREATE TABLE IF NOT EXISTS friction_patterns (
    fingerprint      TEXT PRIMARY KEY,
    kind             TEXT NOT NULL,
    title            TEXT NOT NULL,
    first_seen_date  TEXT NOT NULL,
    last_seen_date   TEXT NOT NULL,
    occurrence_count INTEGER NOT NULL,
    session_count    INTEGER NOT NULL,
    last_subject_id  TEXT NOT NULL,
    last_ordinal     INTEGER
);

CREATE INDEX IF NOT EXISTS idx_friction_patterns_last_seen
    ON friction_patterns (last_seen_date);

CREATE TABLE IF NOT EXISTS friction_issue_links (
    fingerprint          TEXT PRIMARY KEY,
    state                TEXT NOT NULL,
    kata_instance_uid    TEXT NOT NULL DEFAULT '',
    kata_project_uid     TEXT NOT NULL DEFAULT '',
    issue_uid            TEXT NOT NULL DEFAULT '',
    qualified_id         TEXT NOT NULL DEFAULT '',
    web_url              TEXT NOT NULL DEFAULT '',
    link_source          TEXT NOT NULL DEFAULT '',
    attempts             INTEGER NOT NULL DEFAULT 0,
    first_failed_at      TIMESTAMPTZ,
    next_attempt_at      TIMESTAMPTZ,
    last_error_code      TEXT NOT NULL DEFAULT '',
    last_error           TEXT NOT NULL DEFAULT '',
    candidates_json      TEXT NOT NULL DEFAULT '',
    last_recurrence_date TEXT NOT NULL DEFAULT '',
    create_idempotency_key TEXT NOT NULL DEFAULT '',
    updated_at           TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_friction_issue_links_due
    ON friction_issue_links (state, next_attempt_at);
`

const frictionSchemaDDL = frictionFindingsSchemaDDL + frictionReviewSchemaDDL

var frictionHostedTables = []HostedTable{
	{Name: "friction_findings", Key: []string{"id"}, ForeignKeys: sessionHostedFK()},
	{Name: "friction_session_dims", Key: []string{"session_id"}, ForeignKeys: sessionHostedFK()},
	{Name: "friction_digests", Key: []string{"date"}},
	{Name: "friction_digest_sessions", Key: []string{"subject_id"}},
	{Name: "friction_digest_fingerprints", Key: []string{"date", "fingerprint"}},
	{Name: "friction_patterns", Key: []string{"fingerprint"}},
	{Name: "friction_issue_links", Key: []string{"fingerprint"}},
}

func backfillFrictionDigestFingerprintIndexPG(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT d.date, d.snapshot_json FROM friction_digests d
		WHERE NOT EXISTS (
			SELECT 1 FROM friction_digest_fingerprints i WHERE i.date = d.date
		)
		ORDER BY d.date FOR UPDATE OF d`)
	if err != nil {
		return fmt.Errorf("reading legacy friction digests for fingerprint indexing: %w", err)
	}
	type indexedDigest struct {
		date         string
		fingerprints []string
	}
	var digests []indexedDigest
	if err := func() (retErr error) {
		defer func() {
			if closeErr := rows.Close(); closeErr != nil && retErr == nil {
				retErr = fmt.Errorf("closing legacy friction digest fingerprint sources: %w", closeErr)
			}
		}()
		for rows.Next() {
			var date, snapshot string
			if err := rows.Scan(&date, &snapshot); err != nil {
				return fmt.Errorf("scanning legacy friction digest fingerprint source: %w", err)
			}
			fingerprints, err := friction.SnapshotFingerprintList([]byte(snapshot))
			if err != nil {
				return fmt.Errorf("indexing legacy friction digest %s fingerprints: %w", date, err)
			}
			if len(fingerprints) == 0 {
				fingerprints = []string{""}
			}
			digests = append(digests, indexedDigest{date: date, fingerprints: fingerprints})
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("reading legacy friction digest fingerprint sources: %w", err)
		}
		return nil
	}(); err != nil {
		return err
	}
	for _, digest := range digests {
		for _, fingerprint := range digest.fingerprints {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO friction_digest_fingerprints (date, fingerprint)
				VALUES ($1, $2) ON CONFLICT (date, fingerprint) DO NOTHING`,
				digest.date, fingerprint); err != nil {
				return fmt.Errorf("backfilling friction digest %s fingerprint index: %w", digest.date, err)
			}
		}
	}
	return nil
}

func installHostedFrictionUpgrade(ctx context.Context, tx *sql.Tx, schema, tenant string) error {
	var missing []HostedTable
	for _, table := range frictionHostedTables {
		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT to_regclass(format('%I.%I',$1::text,$2::text)) IS NOT NULL`, schema, table.Name,
		).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			missing = append(missing, table)
		}
	}
	if _, err := tx.ExecContext(ctx, frictionSchemaDDL); err != nil {
		return fmt.Errorf("creating hosted Friction tables: %w", err)
	}
	if err := backfillFrictionDigestFingerprintIndexPG(ctx, tx); err != nil {
		return err
	}
	migrations := []columnMigration{{
		table: "friction_issue_links", column: "create_idempotency_key",
		def:  `create_idempotency_key TEXT NOT NULL DEFAULT ''`,
		desc: "adding friction_issue_links.create_idempotency_key",
	}}
	columns, err := loadExistingColumns(ctx, tx, migrations)
	if err != nil {
		return err
	}
	if _, err := ensureColumns(ctx, tx, columns, migrations); err != nil {
		return err
	}
	if len(missing) > 0 {
		if err := InstallHostedTables(ctx, tx, schema, tenant, missing); err != nil {
			return fmt.Errorf("installing hosted Friction constraints: %w", err)
		}
	}
	return nil
}
