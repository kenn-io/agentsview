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

// Source keys and entry paths may be up to 4096 bytes, which exceeds the
// PostgreSQL B-tree index entry limit (about 2704 bytes on 8 kB pages) once
// combined with the other key columns. Composite keys therefore use fixed-size
// SHA-256 digests of those values; the full text is stored beside them.
const rawIngestDDL = `
CREATE TABLE IF NOT EXISTS raw_devices (
    device_id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL,
    display_name TEXT NOT NULL
        CHECK (octet_length(display_name) BETWEEN 1 AND 256),
    credential_sha256 BYTEA NOT NULL UNIQUE
        CHECK (octet_length(credential_sha256) = 32),
    created_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    UNIQUE (tenant_id, device_id),
    CHECK (revoked_at IS NULL OR revoked_at >= created_at)
);

CREATE TABLE IF NOT EXISTS raw_device_tokens (
    token_sha256 BYTEA PRIMARY KEY
        CHECK (octet_length(token_sha256) = 32),
    tenant_id TEXT NOT NULL,
    device_id TEXT NOT NULL,
    scope_bits SMALLINT NOT NULL CHECK (scope_bits BETWEEN 1 AND 15),
    issued_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > issued_at),
    FOREIGN KEY (tenant_id, device_id)
        REFERENCES raw_devices (tenant_id, device_id) ON DELETE RESTRICT
);

CREATE TABLE IF NOT EXISTS raw_objects (
    tenant_id TEXT NOT NULL,
    sha256 TEXT NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    size_bytes BIGINT NOT NULL CHECK (size_bytes >= 0),
    verified_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, sha256),
    UNIQUE (tenant_id, sha256, size_bytes)
);

CREATE TABLE IF NOT EXISTS raw_manifests (
    tenant_id TEXT NOT NULL,
    manifest_id TEXT NOT NULL CHECK (manifest_id ~ '^[0-9a-f]{64}$'),
    device_id TEXT NOT NULL,
    provider TEXT NOT NULL,
    configured_root_id TEXT NOT NULL,
    source_key TEXT NOT NULL,
    source_key_sha256 TEXT NOT NULL CHECK (source_key_sha256 ~ '^[0-9a-f]{64}$'),
    capture_id TEXT NOT NULL,
    parent_receipt TEXT NOT NULL DEFAULT ''
        CHECK (parent_receipt = '' OR parent_receipt ~ '^[0-9a-f]{64}$'),
    receipt TEXT NOT NULL CHECK (receipt ~ '^[0-9a-f]{64}$'),
    generation BIGINT NOT NULL CHECK (generation > 0),
    kind TEXT NOT NULL CHECK (kind IN ('snapshot', 'tombstone')),
    captured_at TIMESTAMPTZ NOT NULL,
    accepted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    canonical_json BYTEA NOT NULL,
    PRIMARY KEY (tenant_id, manifest_id),
    UNIQUE (tenant_id, receipt),
    UNIQUE (
        tenant_id, device_id, provider, configured_root_id, source_key_sha256,
        generation
    ),
    UNIQUE (
        tenant_id, device_id, provider, configured_root_id, source_key_sha256,
        capture_id
    )
);

CREATE TABLE IF NOT EXISTS raw_manifest_entries (
    tenant_id TEXT NOT NULL,
    manifest_id TEXT NOT NULL,
    entry_index INTEGER NOT NULL CHECK (entry_index >= 0),
    path TEXT NOT NULL,
    path_sha256 TEXT NOT NULL CHECK (path_sha256 ~ '^[0-9a-f]{64}$'),
    entry_type TEXT NOT NULL CHECK (entry_type = 'file'),
    size_bytes BIGINT NOT NULL CHECK (size_bytes >= 0),
    PRIMARY KEY (tenant_id, manifest_id, entry_index),
    UNIQUE (tenant_id, manifest_id, path_sha256),
    FOREIGN KEY (tenant_id, manifest_id)
        REFERENCES raw_manifests (tenant_id, manifest_id) ON DELETE RESTRICT
);

CREATE TABLE IF NOT EXISTS raw_manifest_objects (
    tenant_id TEXT NOT NULL,
    manifest_id TEXT NOT NULL,
    entry_index INTEGER NOT NULL,
    object_index INTEGER NOT NULL CHECK (object_index >= 0),
    sha256 TEXT NOT NULL,
    size_bytes BIGINT NOT NULL CHECK (size_bytes >= 0),
    PRIMARY KEY (tenant_id, manifest_id, entry_index, object_index),
    FOREIGN KEY (tenant_id, manifest_id, entry_index)
        REFERENCES raw_manifest_entries (tenant_id, manifest_id, entry_index)
        ON DELETE RESTRICT,
    FOREIGN KEY (tenant_id, sha256, size_bytes)
        REFERENCES raw_objects (tenant_id, sha256, size_bytes)
        ON DELETE RESTRICT
);

CREATE TABLE IF NOT EXISTS raw_source_heads (
    tenant_id TEXT NOT NULL,
    device_id TEXT NOT NULL,
    provider TEXT NOT NULL,
    configured_root_id TEXT NOT NULL,
    source_key TEXT NOT NULL,
    source_key_sha256 TEXT NOT NULL CHECK (source_key_sha256 ~ '^[0-9a-f]{64}$'),
    manifest_id TEXT,
    receipt TEXT CHECK (receipt IS NULL OR receipt ~ '^[0-9a-f]{64}$'),
    generation BIGINT NOT NULL DEFAULT 0 CHECK (generation >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (
        tenant_id, device_id, provider, configured_root_id, source_key_sha256
    ),
    CHECK (
        (generation = 0 AND manifest_id IS NULL AND receipt IS NULL)
        OR (generation > 0 AND manifest_id IS NOT NULL AND receipt IS NOT NULL)
    ),
    FOREIGN KEY (tenant_id, manifest_id)
        REFERENCES raw_manifests (tenant_id, manifest_id) ON DELETE RESTRICT
);

CREATE TABLE IF NOT EXISTS raw_ingest_jobs (
    id BIGSERIAL PRIMARY KEY,
    tenant_id TEXT NOT NULL,
    manifest_id TEXT NOT NULL,
    stage TEXT NOT NULL CHECK (stage IN ('parse')),
    processing_version TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'ready'
        CHECK (
            state IN (
                'ready', 'leased', 'retrying', 'complete', 'failed',
                'superseded'
            )
        ),
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    lease_owner TEXT NOT NULL DEFAULT '',
    lease_expires_at TIMESTAMPTZ,
    last_error_class TEXT NOT NULL DEFAULT '',
    last_error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, manifest_id, stage, processing_version),
    FOREIGN KEY (tenant_id, manifest_id)
        REFERENCES raw_manifests (tenant_id, manifest_id) ON DELETE RESTRICT
);

CREATE INDEX IF NOT EXISTS idx_raw_ingest_jobs_ready
    ON raw_ingest_jobs (state, available_at, id)
    WHERE state IN ('ready', 'retrying');
CREATE INDEX IF NOT EXISTS idx_raw_ingest_jobs_lease
    ON raw_ingest_jobs (lease_expires_at, id)
    WHERE state = 'leased';
CREATE INDEX IF NOT EXISTS idx_raw_device_tokens_expiry
    ON raw_device_tokens (expires_at, tenant_id, device_id);
`

const rawIngestAppendOnlyDDL = `
CREATE OR REPLACE FUNCTION raw_ingest_reject_accepted_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $raw_ingest_immutable$
BEGIN
    RAISE EXCEPTION 'accepted raw custody metadata is append-only'
        USING ERRCODE = '55000';
END;
$raw_ingest_immutable$;

DO $raw_ingest_triggers$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_trigger
        WHERE tgname = 'raw_manifests_append_only'
            AND tgrelid = 'raw_manifests'::regclass
    ) THEN
        CREATE TRIGGER raw_manifests_append_only
        BEFORE UPDATE OR DELETE ON raw_manifests
        FOR EACH ROW EXECUTE FUNCTION raw_ingest_reject_accepted_mutation();
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_trigger
        WHERE tgname = 'raw_manifest_entries_append_only'
            AND tgrelid = 'raw_manifest_entries'::regclass
    ) THEN
        CREATE TRIGGER raw_manifest_entries_append_only
        BEFORE UPDATE OR DELETE ON raw_manifest_entries
        FOR EACH ROW EXECUTE FUNCTION raw_ingest_reject_accepted_mutation();
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_trigger
        WHERE tgname = 'raw_manifest_objects_append_only'
            AND tgrelid = 'raw_manifest_objects'::regclass
    ) THEN
        CREATE TRIGGER raw_manifest_objects_append_only
        BEFORE UPDATE OR DELETE ON raw_manifest_objects
        FOR EACH ROW EXECUTE FUNCTION raw_ingest_reject_accepted_mutation();
    END IF;
END;
$raw_ingest_triggers$;
`

func ensureRawIngestSchemaPG(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, rawIngestDDL); err != nil {
		return fmt.Errorf("creating raw ingest schema: %w", err)
	}
	if _, err := db.ExecContext(ctx, rawIngestAppendOnlyDDL); err != nil {
		if !rawIngestAppendOnlyUnsupported(err) {
			return fmt.Errorf("installing raw ingest append-only guards: %w", err)
		}
		log.Printf(
			"pg schema: raw custody append-only triggers unsupported; " +
				"immutability remains application-enforced",
		)
	}
	return nil
}

func rawIngestAppendOnlyUnsupported(err error) bool {
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		return pgErr.Code == "0A000"
	}
	return strings.Contains(strings.ToUpper(err.Error()), "SQLSTATE 0A000")
}
