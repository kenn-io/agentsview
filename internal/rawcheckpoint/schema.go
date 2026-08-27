package rawcheckpoint

import (
	"context"
	"fmt"
)

const schemaVersion = 5

func (s *Store) init(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("rawcheckpoint: read schema version: %w", err)
	}
	if version > schemaVersion {
		return fmt.Errorf("rawcheckpoint: database schema %d is newer than supported %d",
			version, schemaVersion)
	}
	if version == schemaVersion {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("rawcheckpoint: begin schema: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, statement := range versionOneSchemaStatements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("rawcheckpoint: create base schema: %w", err)
		}
	}
	if version < 2 {
		for _, statement := range versionTwoMigrationStatements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("rawcheckpoint: migrate schema to version 2: %w", err)
			}
		}
	}
	if version < 3 {
		for _, statement := range versionThreeMigrationStatements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("rawcheckpoint: migrate schema to version 3: %w", err)
			}
		}
	}
	if version < 4 {
		for _, statement := range versionFourMigrationStatements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("rawcheckpoint: migrate schema to version 4: %w", err)
			}
		}
	}
	if version < 5 {
		for _, statement := range versionFiveMigrationStatements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("rawcheckpoint: migrate schema to version 5: %w", err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return fmt.Errorf("rawcheckpoint: set schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("rawcheckpoint: commit schema: %w", err)
	}
	return nil
}

var versionOneSchemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS device_config (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		device_id TEXT NOT NULL,
		created_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS raw_sources (
		provider TEXT NOT NULL,
		configured_root_id TEXT NOT NULL,
		source_key TEXT NOT NULL,
		head_manifest_id TEXT NOT NULL DEFAULT '',
		head_receipt TEXT NOT NULL DEFAULT '',
		head_generation INTEGER NOT NULL DEFAULT 0,
		updated_at TEXT NOT NULL,
		PRIMARY KEY (provider, configured_root_id, source_key)
	)`,
}

var versionTwoMigrationStatements = []string{
	`ALTER TABLE raw_sources ADD COLUMN latest_capture_id TEXT NOT NULL DEFAULT ''`,
	`CREATE TABLE outbox_config (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		spool_path TEXT NOT NULL
	)`,
	`CREATE TABLE configured_roots (
		id TEXT PRIMARY KEY,
		provider TEXT NOT NULL,
		local_root TEXT NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		UNIQUE (provider, local_root)
	)`,
	`CREATE TABLE outbox_reservations (
		id TEXT PRIMARY KEY,
		provider TEXT NOT NULL,
		configured_root_id TEXT NOT NULL,
		source_key TEXT NOT NULL,
		reserved_bytes INTEGER NOT NULL CHECK (reserved_bytes >= 0),
		created_at TEXT NOT NULL,
		FOREIGN KEY (configured_root_id) REFERENCES configured_roots(id) ON DELETE CASCADE
	)`,
	`CREATE TABLE outbox_objects (
		sha256 TEXT NOT NULL,
		length INTEGER NOT NULL CHECK (length >= 0),
		spool_name TEXT NOT NULL,
		ref_count INTEGER NOT NULL CHECK (ref_count >= 0),
		state TEXT NOT NULL CHECK (state IN ('live', 'garbage_pending', 'remote')),
		created_at TEXT NOT NULL,
		PRIMARY KEY (sha256, length)
	)`,
	`CREATE TABLE outbox_generations (
		capture_id TEXT PRIMARY KEY,
		provider TEXT NOT NULL,
		configured_root_id TEXT NOT NULL,
		source_key TEXT NOT NULL,
		predecessor_capture_id TEXT,
		captured_at TEXT NOT NULL,
		kind TEXT NOT NULL CHECK (kind IN ('snapshot', 'tombstone')),
		state TEXT NOT NULL CHECK (state IN ('queued', 'finalized', 'acknowledged', 'invalid')),
		expected_parent_receipt TEXT NOT NULL DEFAULT '',
		manifest_id TEXT NOT NULL DEFAULT '',
		ack_receipt TEXT NOT NULL DEFAULT '',
		ack_generation INTEGER NOT NULL DEFAULT 0,
		metadata_bytes INTEGER NOT NULL CHECK (metadata_bytes >= 0),
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		FOREIGN KEY (configured_root_id) REFERENCES configured_roots(id),
		FOREIGN KEY (predecessor_capture_id) REFERENCES outbox_generations(capture_id) ON DELETE CASCADE,
		UNIQUE (provider, configured_root_id, source_key, capture_id)
	)`,
	`CREATE TABLE outbox_entries (
		capture_id TEXT NOT NULL,
		entry_ordinal INTEGER NOT NULL CHECK (entry_ordinal >= 0),
		path TEXT NOT NULL,
		length INTEGER NOT NULL CHECK (length >= 0),
		mod_time_ns INTEGER NOT NULL,
		file_identity TEXT NOT NULL,
		prefix_sha256 TEXT NOT NULL,
		appendable INTEGER NOT NULL CHECK (appendable IN (0, 1)),
		PRIMARY KEY (capture_id, entry_ordinal),
		UNIQUE (capture_id, path),
		FOREIGN KEY (capture_id) REFERENCES outbox_generations(capture_id) ON DELETE CASCADE
	)`,
	`CREATE TABLE outbox_entry_objects (
		capture_id TEXT NOT NULL,
		entry_ordinal INTEGER NOT NULL,
		object_ordinal INTEGER NOT NULL CHECK (object_ordinal >= 0),
		sha256 TEXT NOT NULL,
		length INTEGER NOT NULL CHECK (length >= 0),
		PRIMARY KEY (capture_id, entry_ordinal, object_ordinal),
		FOREIGN KEY (capture_id, entry_ordinal)
			REFERENCES outbox_entries(capture_id, entry_ordinal) ON DELETE CASCADE,
		FOREIGN KEY (sha256, length)
			REFERENCES outbox_objects(sha256, length)
	)`,
	`CREATE TABLE raw_source_base_entries (
		provider TEXT NOT NULL,
		configured_root_id TEXT NOT NULL,
		source_key TEXT NOT NULL,
		entry_ordinal INTEGER NOT NULL CHECK (entry_ordinal >= 0),
		path TEXT NOT NULL,
		length INTEGER NOT NULL CHECK (length >= 0),
		mod_time_ns INTEGER NOT NULL,
		file_identity TEXT NOT NULL,
		prefix_sha256 TEXT NOT NULL,
		appendable INTEGER NOT NULL CHECK (appendable IN (0, 1)),
		PRIMARY KEY (provider, configured_root_id, source_key, entry_ordinal),
		UNIQUE (provider, configured_root_id, source_key, path),
		FOREIGN KEY (configured_root_id) REFERENCES configured_roots(id)
	)`,
	`CREATE TABLE raw_source_base_objects (
		provider TEXT NOT NULL,
		configured_root_id TEXT NOT NULL,
		source_key TEXT NOT NULL,
		entry_ordinal INTEGER NOT NULL,
		object_ordinal INTEGER NOT NULL CHECK (object_ordinal >= 0),
		sha256 TEXT NOT NULL,
		length INTEGER NOT NULL CHECK (length >= 0),
		PRIMARY KEY (
			provider, configured_root_id, source_key, entry_ordinal, object_ordinal
		),
		FOREIGN KEY (provider, configured_root_id, source_key, entry_ordinal)
			REFERENCES raw_source_base_entries(
				provider, configured_root_id, source_key, entry_ordinal
			) ON DELETE CASCADE
	)`,
	`CREATE TABLE raw_coverage (
		provider TEXT NOT NULL,
		configured_root_id TEXT NOT NULL,
		state TEXT NOT NULL CHECK (state IN ('complete', 'degraded')),
		reason TEXT NOT NULL DEFAULT '',
		degraded_at TEXT,
		recovered_at TEXT,
		updated_at TEXT NOT NULL,
		PRIMARY KEY (provider, configured_root_id),
		FOREIGN KEY (configured_root_id) REFERENCES configured_roots(id)
	)`,
	`CREATE INDEX outbox_generations_ready_idx
		ON outbox_generations(state, captured_at, capture_id)`,
	`CREATE INDEX outbox_generations_source_idx
		ON outbox_generations(provider, configured_root_id, source_key, captured_at)`,
	`CREATE INDEX outbox_objects_state_idx ON outbox_objects(state)`,
}

var versionThreeMigrationStatements = []string{
	`ALTER TABLE outbox_generations
		ADD COLUMN retry_at TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE outbox_generations
		ADD COLUMN error_class TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE outbox_generations
		ADD COLUMN blocked INTEGER NOT NULL DEFAULT 0 CHECK (blocked IN (0, 1))`,
	`CREATE INDEX outbox_generations_due_idx
		ON outbox_generations(blocked, retry_at, state, captured_at, capture_id)`,
}

var versionFourMigrationStatements = []string{
	`ALTER TABLE raw_sources
		ADD COLUMN head_capture_id TEXT NOT NULL DEFAULT ''`,
	`UPDATE raw_sources SET head_capture_id = COALESCE((
		SELECT generation.capture_id FROM outbox_generations AS generation
		WHERE generation.provider = raw_sources.provider
			AND generation.configured_root_id = raw_sources.configured_root_id
			AND generation.source_key = raw_sources.source_key
			AND generation.state = 'acknowledged'
			AND generation.manifest_id = raw_sources.head_manifest_id
		LIMIT 1
	), '')`,
	`WITH RECURSIVE invalid_suffix(capture_id) AS (
		SELECT capture_id FROM outbox_generations WHERE state = 'invalid'
		UNION
		SELECT generation.capture_id FROM outbox_generations AS generation
		JOIN invalid_suffix
			ON generation.predecessor_capture_id = invalid_suffix.capture_id
	), discarded(capture_id) AS (
		SELECT capture_id FROM invalid_suffix
		UNION
		SELECT capture_id FROM outbox_generations WHERE state = 'acknowledged'
	)
	UPDATE outbox_objects SET ref_count = ref_count - (
		SELECT count(*) FROM outbox_entry_objects AS entry_object
		JOIN discarded ON discarded.capture_id = entry_object.capture_id
		WHERE entry_object.sha256 = outbox_objects.sha256
			AND entry_object.length = outbox_objects.length
	)
	WHERE EXISTS (
		SELECT 1 FROM outbox_entry_objects AS entry_object
		JOIN discarded ON discarded.capture_id = entry_object.capture_id
		WHERE entry_object.sha256 = outbox_objects.sha256
			AND entry_object.length = outbox_objects.length
	)`,
	`UPDATE outbox_objects SET state = 'garbage_pending'
		WHERE ref_count = 0 AND state != 'remote'`,
	`DELETE FROM outbox_generations WHERE state = 'invalid'`,
	`UPDATE outbox_generations SET predecessor_capture_id = NULL
		WHERE predecessor_capture_id IN (
			SELECT capture_id FROM outbox_generations WHERE state = 'acknowledged'
		)`,
	`DELETE FROM outbox_generations WHERE state = 'acknowledged'`,
	`UPDATE raw_sources SET latest_capture_id = head_capture_id
		WHERE latest_capture_id != '' AND NOT EXISTS (
			SELECT 1 FROM outbox_generations AS generation
			WHERE generation.capture_id = raw_sources.latest_capture_id
		)`,
}

var versionFiveMigrationStatements = []string{
	`CREATE TABLE raw_coverage_failures (
		provider TEXT NOT NULL,
		configured_root_id TEXT NOT NULL,
		source_key TEXT NOT NULL,
		reason TEXT NOT NULL,
		degraded_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		PRIMARY KEY (provider, configured_root_id, source_key),
		FOREIGN KEY (configured_root_id) REFERENCES configured_roots(id) ON DELETE CASCADE
	)`,
	`INSERT INTO raw_coverage_failures
		(provider, configured_root_id, source_key, reason, degraded_at, updated_at)
		SELECT provider, configured_root_id, '', reason,
			COALESCE(degraded_at, updated_at), updated_at
		FROM raw_coverage WHERE state = 'degraded'`,
}
