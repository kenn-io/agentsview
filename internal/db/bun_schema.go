package db

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

const duckDBPrivateDialectName dialect.Name = -1

// CommonSchemaCompatibilityMetadataKey stamps completion of the canonical
// common-schema convergence transaction on persistent backends.
const CommonSchemaCompatibilityMetadataKey = "bun_common_schema_v1"

var sqliteCommonSchemaColumnMigrations = []schemaColumnMigration{
	{
		"sessions", "source_archive_id",
		"ALTER TABLE sessions ADD COLUMN source_archive_id TEXT NOT NULL DEFAULT ''",
	},
	{
		"sessions", "source_database_generation",
		"ALTER TABLE sessions ADD COLUMN source_database_generation TEXT NOT NULL DEFAULT ''",
	},
	{
		"tool_calls", "message_ordinal",
		"ALTER TABLE tool_calls ADD COLUMN message_ordinal INTEGER",
	},
	{
		"pinned_messages", "source_uuid",
		"ALTER TABLE pinned_messages ADD COLUMN source_uuid TEXT NOT NULL DEFAULT ''",
	},
}

// CreateCommonSchema creates the canonical serving tables and ordinary indexes
// in registry order. Adapter-owned operational, FTS, and vector schema remains
// outside this function.
func CreateCommonSchema(ctx context.Context, db bun.IDB) error {
	includeForeignKeys := db.Dialect().Name() != duckDBPrivateDialectName
	for _, table := range bunmodel.CommonTables() {
		create := db.NewCreateTable().Model(table.Model).IfNotExists()
		if includeForeignKeys {
			for _, foreignKey := range table.ForeignKeys {
				create.ForeignKey(bunmodel.ForeignKeyDefinition(foreignKey, true))
			}
		}
		if _, err := create.Exec(ctx); err != nil {
			return fmt.Errorf("creating common table %s: %w", table.Name, err)
		}
		for _, index := range table.Indexes {
			createIndex := db.NewCreateIndex().Model(table.Model).
				Index(index.Name).IfNotExists()
			if index.Unique {
				createIndex.Unique()
			}
			for _, column := range index.Columns {
				createIndex.Column(column)
			}
			for _, expression := range index.Expressions {
				createIndex.ColumnExpr(expression)
			}
			if _, err := createIndex.Exec(ctx); err != nil {
				return fmt.Errorf(
					"creating common index %s on %s: %w",
					index.Name, table.Name, err,
				)
			}
		}
	}
	return nil
}

// CheckCommonSchema verifies that every canonical table exposes every model
// column required by shared Bun reads. Engine-specific compatibility checks
// additionally validate physical keys and operational extensions.
func CheckCommonSchema(ctx context.Context, db bun.IDB) error {
	for _, table := range bunmodel.CommonTables() {
		columns := bunmodel.ModelColumns(table.Model)
		rows, err := db.NewSelect().Table(table.Name).Column(columns...).Limit(0).
			Rows(ctx)
		if err != nil {
			return fmt.Errorf("checking common table %s: %w", table.Name, err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("closing common table %s check: %w", table.Name, err)
		}
	}
	checks := []struct {
		name  string
		query string
	}{
		{
			"tool call message ordinal",
			`SELECT EXISTS (
				SELECT 1 FROM tool_calls WHERE message_ordinal IS NULL
			)`,
		},
		{
			"message logical key",
			`SELECT EXISTS (
				SELECT 1 FROM messages
				GROUP BY session_id, ordinal HAVING COUNT(*) > 1
			)`,
		},
		{
			"tool call logical key",
			`SELECT EXISTS (
				SELECT 1 FROM tool_calls
				GROUP BY session_id, message_ordinal, call_index
				HAVING COUNT(*) > 1
			)`,
		},
		{
			"pin logical key",
			`SELECT EXISTS (
				SELECT 1 FROM pinned_messages
				GROUP BY session_id, ordinal HAVING COUNT(*) > 1
			)`,
		},
	}
	for _, check := range checks {
		var invalid bool
		if err := db.NewRaw(check.query).Scan(ctx, &invalid); err != nil {
			return fmt.Errorf("checking common %s: %w", check.name, err)
		}
		if invalid {
			return fmt.Errorf("common %s is invalid", check.name)
		}
	}
	return nil
}

func (db *DB) convergeSQLiteCommonSchemaLocked(
	ctx context.Context, beforeStamp func() error,
) error {
	tx, err := db.bunWriter.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting common SQLite schema migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	complete, err := sqliteCommonSchemaStamped(ctx, tx)
	if err != nil {
		return err
	}
	if complete {
		if err := installSQLiteCanonicalIdentityTriggers(ctx, tx); err != nil {
			return err
		}
		if err := CheckCommonSchema(ctx, tx); err != nil {
			return fmt.Errorf("validating stamped common SQLite schema: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("closing common SQLite schema validation: %w", err)
		}
		return nil
	}

	if err := applyColumnMigrations(
		sqliteCommonSchemaColumnMigrations,
		func(query string, args ...any) rowScanner {
			return tx.QueryRowContext(ctx, query, args...)
		},
		func(query string, args ...any) (sql.Result, error) {
			return tx.ExecContext(ctx, query, args...)
		},
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE tool_calls
		SET message_ordinal = (
			SELECT ordinal FROM messages WHERE messages.id = tool_calls.message_id
		)
		WHERE message_ordinal IS NULL`); err != nil {
		return fmt.Errorf("backfilling tool call message ordinals: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		CREATE TRIGGER IF NOT EXISTS tool_calls_fill_message_ordinal
		AFTER INSERT ON tool_calls
		WHEN NEW.message_ordinal IS NULL
		BEGIN
			UPDATE tool_calls
			SET message_ordinal = (
				SELECT ordinal FROM messages WHERE messages.id = NEW.message_id
			)
			WHERE id = NEW.id;
		END`); err != nil {
		return fmt.Errorf("installing tool call message ordinal trigger: %w", err)
	}
	if err := convergeSQLitePricingMetadata(ctx, tx); err != nil {
		return err
	}
	if err := CreateCommonSchema(ctx, tx); err != nil {
		return err
	}

	databaseGeneration, err := sqliteMetadataValue(
		ctx, tx, archiveMetadataDatabaseIDKey,
	)
	if err != nil {
		return err
	}
	archiveID, err := sqliteMetadataValue(ctx, tx, archiveMetadataArchiveIDKey)
	if err != nil {
		return err
	}
	archiveSalt, err := sqliteMetadataValue(ctx, tx, archiveMetadataArchiveSaltKey)
	if err != nil {
		return err
	}
	if databaseGeneration == "" || archiveID == "" || archiveSalt == "" {
		return fmt.Errorf("common SQLite schema migration requires archive identity")
	}
	if _, err := tx.NewInsert().Model(&bunmodel.SourceArchive{
		SourceArchiveID: archiveID, SourceArchiveSalt: archiveSalt,
	}).On("CONFLICT (source_archive_id) DO UPDATE").
		Set("source_archive_salt = EXCLUDED.source_archive_salt").Exec(ctx); err != nil {
		return fmt.Errorf("recording common source archive: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE sessions
		SET source_archive_id = ?, source_database_generation = ?
		WHERE source_archive_id = '' OR source_database_generation = ''`,
		archiveID, databaseGeneration,
	); err != nil {
		return fmt.Errorf("backfilling session source provenance: %w", err)
	}
	if err := backfillSQLiteCommonIdentity(
		ctx, tx, archiveID, archiveSalt, databaseGeneration,
	); err != nil {
		return err
	}
	if err := installSQLiteCanonicalIdentityTriggers(ctx, tx); err != nil {
		return err
	}
	if err := CheckCommonSchema(ctx, tx); err != nil {
		return err
	}
	if beforeStamp != nil {
		if err := beforeStamp(); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO archive_metadata (key, value)
		VALUES (?, '1')
		ON CONFLICT(key) DO UPDATE SET
			value = excluded.value,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')`,
		CommonSchemaCompatibilityMetadataKey,
	); err != nil {
		return fmt.Errorf("stamping common SQLite schema: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing common SQLite schema migration: %w", err)
	}
	return nil
}

func convergeSQLitePricingMetadata(ctx context.Context, db bun.IDB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS pricing_metadata (
			key        TEXT PRIMARY KEY,
			value      TEXT NOT NULL,
			updated_at TEXT NOT NULL
				DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		);
		INSERT INTO pricing_metadata (key, value)
		SELECT model_pattern, updated_at
		FROM model_pricing
		WHERE model_pattern IN (
			'_fallback_version',
			'_litellm_last_attempt',
			'_pricing_storage_version'
		)
		ON CONFLICT(key) DO UPDATE SET
			value = excluded.value,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now');
		DELETE FROM model_pricing
		WHERE model_pattern IN (
			'_fallback_version',
			'_litellm_last_attempt',
			'_pricing_storage_version'
		);
	`); err != nil {
		return fmt.Errorf("converging SQLite pricing metadata: %w", err)
	}
	return nil
}

const sqliteCanonicalIdentityTriggerDDL = `
DROP TRIGGER IF EXISTS trg_project_identity_observations_revision_insert;
DROP TRIGGER IF EXISTS trg_project_identity_observations_revision_update;
DROP TRIGGER IF EXISTS trg_project_identity_observations_revision_delete;
DROP TRIGGER IF EXISTS trg_session_project_identity_snapshots_revision_insert;
DROP TRIGGER IF EXISTS trg_session_project_identity_snapshots_revision_update;
DROP TRIGGER IF EXISTS trg_session_project_identity_snapshots_revision_delete;
DROP TRIGGER IF EXISTS trg_sessions_create_project_identity_snapshot;
DROP TRIGGER IF EXISTS trg_sessions_delete_project_identity_snapshot;
DROP TRIGGER IF EXISTS trg_worktree_project_mappings_revision_insert;
DROP TRIGGER IF EXISTS trg_worktree_project_mappings_revision_update;
DROP TRIGGER IF EXISTS trg_worktree_project_mappings_revision_delete;

CREATE TRIGGER trg_project_identity_observations_revision_insert
AFTER INSERT ON source_project_identity_observations BEGIN
    INSERT INTO archive_metadata (key, value)
    VALUES ('project_identity_publication_revision', '1')
    ON CONFLICT(key) DO UPDATE SET
        value = CAST(CAST(value AS INTEGER) + 1 AS TEXT),
        updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now');
    INSERT INTO project_identity_observation_changes (
        project, machine, root_path, git_remote, revision, deleted
    ) VALUES (
        NEW.project, NEW.machine, NEW.root_path, NEW.git_remote,
        (SELECT CAST(value AS INTEGER) FROM archive_metadata
         WHERE key = 'project_identity_publication_revision'), 0
    ) ON CONFLICT(project, machine, root_path, git_remote) DO UPDATE SET
        revision = excluded.revision, deleted = 0;
END;

CREATE TRIGGER trg_project_identity_observations_revision_update
AFTER UPDATE ON source_project_identity_observations BEGIN
    INSERT INTO archive_metadata (key, value)
    VALUES ('project_identity_publication_revision', '1')
    ON CONFLICT(key) DO UPDATE SET
        value = CAST(CAST(value AS INTEGER) + 1 AS TEXT),
        updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now');
    INSERT INTO project_identity_observation_changes (
        project, machine, root_path, git_remote, revision, deleted
    ) VALUES (
        OLD.project, OLD.machine, OLD.root_path, OLD.git_remote,
        (SELECT CAST(value AS INTEGER) FROM archive_metadata
         WHERE key = 'project_identity_publication_revision'), 1
    ) ON CONFLICT(project, machine, root_path, git_remote) DO UPDATE SET
        revision = excluded.revision, deleted = 1;
    INSERT INTO project_identity_observation_changes (
        project, machine, root_path, git_remote, revision, deleted
    ) VALUES (
        NEW.project, NEW.machine, NEW.root_path, NEW.git_remote,
        (SELECT CAST(value AS INTEGER) FROM archive_metadata
         WHERE key = 'project_identity_publication_revision'), 0
    ) ON CONFLICT(project, machine, root_path, git_remote) DO UPDATE SET
        revision = excluded.revision, deleted = 0;
END;

CREATE TRIGGER trg_project_identity_observations_revision_delete
AFTER DELETE ON source_project_identity_observations BEGIN
    INSERT INTO archive_metadata (key, value)
    VALUES ('project_identity_publication_revision', '1')
    ON CONFLICT(key) DO UPDATE SET
        value = CAST(CAST(value AS INTEGER) + 1 AS TEXT),
        updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now');
    INSERT INTO project_identity_observation_changes (
        project, machine, root_path, git_remote, revision, deleted
    ) VALUES (
        OLD.project, OLD.machine, OLD.root_path, OLD.git_remote,
        (SELECT CAST(value AS INTEGER) FROM archive_metadata
         WHERE key = 'project_identity_publication_revision'), 1
    ) ON CONFLICT(project, machine, root_path, git_remote) DO UPDATE SET
        revision = excluded.revision, deleted = 1;
END;

CREATE TRIGGER trg_session_project_identity_snapshots_revision_insert
AFTER INSERT ON source_session_project_identity_snapshots BEGIN
    INSERT INTO archive_metadata (key, value)
    VALUES ('project_identity_publication_revision', '1')
    ON CONFLICT(key) DO UPDATE SET
        value = CAST(CAST(value AS INTEGER) + 1 AS TEXT),
        updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now');
    INSERT INTO session_project_identity_snapshot_changes (
        session_id, project, revision, deleted
    ) VALUES (
        NEW.source_session_id, NEW.project,
        (SELECT CAST(value AS INTEGER) FROM archive_metadata
         WHERE key = 'project_identity_publication_revision'), 0
    ) ON CONFLICT(session_id, project) DO UPDATE SET
        revision = excluded.revision, deleted = 0;
END;

CREATE TRIGGER trg_session_project_identity_snapshots_revision_update
AFTER UPDATE ON source_session_project_identity_snapshots BEGIN
    INSERT INTO archive_metadata (key, value)
    VALUES ('project_identity_publication_revision', '1')
    ON CONFLICT(key) DO UPDATE SET
        value = CAST(CAST(value AS INTEGER) + 1 AS TEXT),
        updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now');
    INSERT INTO session_project_identity_snapshot_changes (
        session_id, project, revision, deleted
    ) VALUES (
        OLD.source_session_id, OLD.project,
        (SELECT CAST(value AS INTEGER) FROM archive_metadata
         WHERE key = 'project_identity_publication_revision'), 1
    ) ON CONFLICT(session_id, project) DO UPDATE SET
        revision = excluded.revision, deleted = 1;
    INSERT INTO session_project_identity_snapshot_changes (
        session_id, project, revision, deleted
    ) VALUES (
        NEW.source_session_id, NEW.project,
        (SELECT CAST(value AS INTEGER) FROM archive_metadata
         WHERE key = 'project_identity_publication_revision'), 0
    ) ON CONFLICT(session_id, project) DO UPDATE SET
        revision = excluded.revision, deleted = 0;
END;

CREATE TRIGGER trg_session_project_identity_snapshots_revision_delete
AFTER DELETE ON source_session_project_identity_snapshots BEGIN
    INSERT INTO archive_metadata (key, value)
    VALUES ('project_identity_publication_revision', '1')
    ON CONFLICT(key) DO UPDATE SET
        value = CAST(CAST(value AS INTEGER) + 1 AS TEXT),
        updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now');
    INSERT INTO session_project_identity_snapshot_changes (
        session_id, project, revision, deleted
    ) VALUES (
        OLD.source_session_id, OLD.project,
        (SELECT CAST(value AS INTEGER) FROM archive_metadata
         WHERE key = 'project_identity_publication_revision'), 1
    ) ON CONFLICT(session_id, project) DO UPDATE SET
        revision = excluded.revision, deleted = 1;
END;

CREATE TRIGGER trg_sessions_create_project_identity_snapshot
AFTER INSERT ON sessions
WHEN NEW.source_archive_id <> '' AND NEW.source_database_generation <> '' BEGIN
    INSERT INTO source_session_project_identity_snapshots (
        source_archive_id, source_database_generation, source_session_id,
        project, machine, root_path, worktree_relationship,
        checkout_state, git_branch, remote_resolution, observed_at
    ) VALUES (
        NEW.source_archive_id, NEW.source_database_generation, NEW.id,
        NEW.project, NEW.machine, NEW.cwd, 'unknown',
        CASE WHEN NEW.git_branch <> '' THEN 'branch' ELSE 'unknown' END,
        NEW.git_branch, 'unknown', strftime('%Y-%m-%dT%H:%M:%fZ','now')
    ) ON CONFLICT(
        source_archive_id, source_database_generation, source_session_id
    ) DO NOTHING;
END;

CREATE TRIGGER trg_sessions_delete_project_identity_snapshot
AFTER DELETE ON sessions BEGIN
    DELETE FROM source_session_project_identity_snapshots
    WHERE source_archive_id = OLD.source_archive_id
      AND source_database_generation = OLD.source_database_generation
      AND source_session_id = OLD.id;
END;

CREATE TRIGGER trg_worktree_project_mappings_revision_insert
AFTER INSERT ON source_worktree_project_mappings BEGIN
    INSERT INTO archive_metadata (key, value)
    VALUES ('worktree_mapping_publication_revision', '1')
    ON CONFLICT(key) DO UPDATE SET
        value = CAST(CAST(value AS INTEGER) + 1 AS TEXT),
        updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now');
    INSERT INTO worktree_project_mapping_changes
        (machine, path_prefix, revision, deleted)
    VALUES (NEW.machine, NEW.path_prefix,
        (SELECT CAST(value AS INTEGER) FROM archive_metadata
         WHERE key = 'worktree_mapping_publication_revision'), 0)
    ON CONFLICT(machine, path_prefix) DO UPDATE SET
        revision = excluded.revision, deleted = 0;
END;

CREATE TRIGGER trg_worktree_project_mappings_revision_update
AFTER UPDATE ON source_worktree_project_mappings BEGIN
    INSERT INTO archive_metadata (key, value)
    VALUES ('worktree_mapping_publication_revision', '1')
    ON CONFLICT(key) DO UPDATE SET
        value = CAST(CAST(value AS INTEGER) + 1 AS TEXT),
        updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now');
    INSERT INTO worktree_project_mapping_changes
        (machine, path_prefix, revision, deleted)
    VALUES (OLD.machine, OLD.path_prefix,
        (SELECT CAST(value AS INTEGER) FROM archive_metadata
         WHERE key = 'worktree_mapping_publication_revision'), 1)
    ON CONFLICT(machine, path_prefix) DO UPDATE SET
        revision = excluded.revision, deleted = 1;
    INSERT INTO worktree_project_mapping_changes
        (machine, path_prefix, revision, deleted)
    VALUES (NEW.machine, NEW.path_prefix,
        (SELECT CAST(value AS INTEGER) FROM archive_metadata
         WHERE key = 'worktree_mapping_publication_revision'), 0)
    ON CONFLICT(machine, path_prefix) DO UPDATE SET
        revision = excluded.revision, deleted = 0;
END;

CREATE TRIGGER trg_worktree_project_mappings_revision_delete
AFTER DELETE ON source_worktree_project_mappings BEGIN
    INSERT INTO archive_metadata (key, value)
    VALUES ('worktree_mapping_publication_revision', '1')
    ON CONFLICT(key) DO UPDATE SET
        value = CAST(CAST(value AS INTEGER) + 1 AS TEXT),
        updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now');
    INSERT INTO worktree_project_mapping_changes
        (machine, path_prefix, revision, deleted)
    VALUES (OLD.machine, OLD.path_prefix,
        (SELECT CAST(value AS INTEGER) FROM archive_metadata
         WHERE key = 'worktree_mapping_publication_revision'), 1)
    ON CONFLICT(machine, path_prefix) DO UPDATE SET
        revision = excluded.revision, deleted = 1;
END;`

func installSQLiteCanonicalIdentityTriggers(ctx context.Context, db bun.IDB) error {
	if _, err := db.ExecContext(ctx, sqliteCanonicalIdentityTriggerDDL); err != nil {
		return fmt.Errorf("installing canonical SQLite identity triggers: %w", err)
	}
	return nil
}

func sqliteCommonSchemaStamped(ctx context.Context, db bun.IDB) (bool, error) {
	complete, err := db.NewSelect().Table("archive_metadata").
		Where("key = ?", CommonSchemaCompatibilityMetadataKey).
		Exists(ctx)
	if err != nil {
		return false, fmt.Errorf("probing common SQLite schema stamp: %w", err)
	}
	return complete, nil
}

func sqliteMetadataValue(
	ctx context.Context, db bun.IDB, key string,
) (string, error) {
	var value string
	if err := db.NewSelect().Table("archive_metadata").Column("value").
		Where("key = ?", key).Scan(ctx, &value); err != nil {
		return "", fmt.Errorf("reading SQLite archive metadata %s: %w", key, err)
	}
	return value, nil
}

func backfillSQLiteCommonIdentity(
	ctx context.Context,
	tx bun.Tx,
	archiveID string,
	archiveSalt string,
	databaseGeneration string,
) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO source_project_identity_observations (
			source_archive_id, source_archive_salt,
			project, machine, root_path, git_remote, git_remote_name,
			repository_path, worktree_name, worktree_root_path,
			worktree_relationship, checkout_state, git_branch,
			remote_resolution, remote_candidate_count, observed_at,
			normalized_remote, key_source, key
		)
		SELECT ?, ?, project, machine, root_path, git_remote, git_remote_name,
			repository_path, worktree_name, worktree_root_path,
			worktree_relationship, checkout_state, git_branch,
			remote_resolution, remote_candidate_count, observed_at,
			normalized_remote, key_source, key
		FROM project_identity_observations
		WHERE true
		ON CONFLICT(source_archive_id, project, machine, root_path, git_remote)
		DO UPDATE SET
			source_archive_salt = excluded.source_archive_salt,
			git_remote_name = excluded.git_remote_name,
			repository_path = excluded.repository_path,
			worktree_name = excluded.worktree_name,
			worktree_root_path = excluded.worktree_root_path,
			worktree_relationship = excluded.worktree_relationship,
			checkout_state = excluded.checkout_state,
			git_branch = excluded.git_branch,
			remote_resolution = excluded.remote_resolution,
			remote_candidate_count = excluded.remote_candidate_count,
			observed_at = excluded.observed_at,
			normalized_remote = excluded.normalized_remote,
			key_source = excluded.key_source,
			key = excluded.key`, archiveID, archiveSalt,
	); err != nil {
		return fmt.Errorf("backfilling source project identities: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO source_session_project_identity_snapshots (
			source_archive_id, source_database_generation, source_session_id,
			project, machine, root_path, git_remote, git_remote_name,
			repository_path, worktree_name, worktree_root_path,
			worktree_relationship, checkout_state, git_branch,
			remote_resolution, remote_candidate_count, observed_at,
			normalized_remote, key_source, key
		)
		SELECT ?, ?, session_id, project, machine, root_path, git_remote,
			git_remote_name, repository_path, worktree_name, worktree_root_path,
			worktree_relationship, checkout_state, git_branch,
			remote_resolution, remote_candidate_count, observed_at,
			normalized_remote, key_source, key
		FROM session_project_identity_snapshots
		WHERE true
		ON CONFLICT(source_archive_id, source_database_generation, source_session_id)
		DO UPDATE SET
			project = excluded.project,
			machine = excluded.machine,
			root_path = excluded.root_path,
			git_remote = excluded.git_remote,
			git_remote_name = excluded.git_remote_name,
			repository_path = excluded.repository_path,
			worktree_name = excluded.worktree_name,
			worktree_root_path = excluded.worktree_root_path,
			worktree_relationship = excluded.worktree_relationship,
			checkout_state = excluded.checkout_state,
			git_branch = excluded.git_branch,
			remote_resolution = excluded.remote_resolution,
			remote_candidate_count = excluded.remote_candidate_count,
			observed_at = excluded.observed_at,
			normalized_remote = excluded.normalized_remote,
			key_source = excluded.key_source,
			key = excluded.key`, archiveID, databaseGeneration,
	); err != nil {
		return fmt.Errorf("backfilling source session identities: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO source_worktree_project_mappings (
			id, source_archive_id, machine, path_prefix, layout,
			project, original_project, enabled, created_at, updated_at
		)
		SELECT id, ?, machine, path_prefix, layout,
			project, original_project, enabled, created_at, updated_at
		FROM worktree_project_mappings
		WHERE true
		ON CONFLICT(source_archive_id, machine, path_prefix) DO UPDATE SET
			layout = excluded.layout,
			project = excluded.project,
			original_project = excluded.original_project,
			enabled = excluded.enabled,
			updated_at = excluded.updated_at`, archiveID,
	); err != nil {
		return fmt.Errorf("backfilling source worktree mappings: %w", err)
	}
	return nil
}
