package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"

	"go.kenn.io/agentsview/internal/export"
)

// CopiedProjectLabel replaces labels that expose a host path with stable aliases.
func CopiedProjectLabel(value string) string {
	if value == "" || export.SafeProjectDisplayLabel(value) != "" {
		return value
	}
	return fmt.Sprintf("copied-project-%x", sha256.Sum256([]byte(value)))
}

// ClearHostSources detaches a staged archive from its original host.
func (db *DB) ClearHostSources(ctx context.Context) error {
	return db.Update(ctx, func(tx *sql.Tx) error {
		version, _, err := readUserVersion(ctx, tx)
		if err != nil {
			return err
		}
		for _, statement := range []string{
			`UPDATE sessions SET file_path=NULL, file_size=NULL, file_mtime=NULL, file_inode=NULL, file_device=NULL, file_hash=NULL, cwd=''`,
			`UPDATE recall_entries SET cwd=''`,
			`UPDATE excluded_sessions SET file_path=NULL`,
			`UPDATE commit_links SET repository=''`,
			`UPDATE commit_trailers SET repository=''`,
			`DELETE FROM git_cache`, `DELETE FROM friction_issue_links`, `DELETE FROM friction_filing_state`,
			`DELETE FROM codex_cloud_accounts`, `DELETE FROM cursor_account_polls`, `DELETE FROM artifact_imported_sessions`,
			`DELETE FROM provider_freshness`, `DELETE FROM skipped_files`, `DELETE FROM source_failures`,
			`DELETE FROM local_session_source_baselines`, `DELETE FROM claude_subagent_sources`,
			`DELETE FROM remote_skipped_files`, `DELETE FROM parser_checkpoint_blobs`, `DELETE FROM parser_checkpoints`,
			`DELETE FROM pg_sync_state WHERE key NOT GLOB 'machine_label:*' AND key NOT GLOB 'machine_alias:*' AND key NOT GLOB 'machine_origin:*'`, `DELETE FROM artifact_export_queue`, `DELETE FROM artifact_publications`,
			`DELETE FROM artifact_publication_revisions`, `DELETE FROM artifact_checkpoint_heads`, `DELETE FROM artifact_checkpoint_floors`,
			`DELETE FROM artifact_import_queue`, `DELETE FROM artifact_import_attempt_generations`, `DELETE FROM artifact_peer_checkpoint_heads`,
			`DELETE FROM artifact_checkpoint_landing_sessions`, `DELETE FROM artifact_checkpoint_landings`,
			`DELETE FROM artifact_checkpoint_stage_sessions`, `DELETE FROM artifact_checkpoint_stages`, `DELETE FROM session_project_assignments`,
			`DELETE FROM worktree_project_mappings`, `DELETE FROM project_identity_observations`,
			`DELETE FROM session_project_identity_snapshots`, `DELETE FROM repository_project_names`,
			`DELETE FROM worktree_project_mapping_changes`, `DELETE FROM project_identity_observation_changes`,
			`DELETE FROM session_project_identity_snapshot_changes`,
			`DELETE FROM archive_session_provenance`, `DELETE FROM archive_attachment_sources`,
			`DELETE FROM archive_source_bindings`, `DELETE FROM archive_attachments`,
			`DELETE FROM raw_archive_sessions`, `DELETE FROM raw_archive_root_aliases`,
			`DELETE FROM raw_archive_sources`, `DELETE FROM raw_archive_files`,
			`DELETE FROM raw_archive_roots`, `DELETE FROM raw_archive_devices`,
			`INSERT INTO archive_metadata(key,value) VALUES('host_paths_detached','1') ON CONFLICT(key) DO UPDATE SET value='1'`,
		} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		if err := writeUserVersion(ctx, tx, version); err != nil {
			return err
		}
		for _, table := range []string{"sessions", "recall_entries", "insights", "session_deletion_changes", "commit_links", "commit_trailers"} {
			if err := relabelCopiedProjects(ctx, tx, table); err != nil {
				return err
			}
		}
		return nil
	})
}

func relabelCopiedProjects(ctx context.Context, tx *sql.Tx, table string) error {
	rows, err := tx.QueryContext(ctx, "SELECT DISTINCT project FROM "+table+" WHERE project IS NOT NULL AND project <> ''")
	if err != nil {
		return err
	}
	defer rows.Close()
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			rows.Close()
			return err
		}
		if CopiedProjectLabel(value) != value {
			values = append(values, value)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, value := range values {
		if _, err := tx.ExecContext(ctx, "UPDATE "+table+" SET project=? WHERE project=?", CopiedProjectLabel(value), value); err != nil {
			return err
		}
	}
	return nil
}
