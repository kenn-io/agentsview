package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/parser"
)

// CopiedProjectLabel replaces labels that expose a host path with stable aliases.
func CopiedProjectLabel(value string) string {
	if value == "" || export.SafeProjectDisplayLabel(value) != "" {
		return value
	}
	name := value[strings.LastIndexAny(strings.TrimRight(value, `/\`), `/\`)+1:]
	name = strings.Trim(name, `/\`)
	if name == "" || export.SafeProjectDisplayLabel(name) == "" {
		name = "copied-project"
	}
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%s-%x", name, digest[:4])
}

// ClearHostSources detaches a staged archive from its original host.
func (db *DB) ClearHostSources(ctx context.Context) error {
	return db.Update(ctx, func(tx *sql.Tx) error {
		if err := retainCodexCloudAccounts(ctx, tx); err != nil {
			return err
		}
		for _, statement := range []string{
			`UPDATE sessions SET file_path=NULL, file_size=NULL, file_mtime=NULL, file_inode=NULL, file_device=NULL, file_hash=NULL, cwd=''`,
			`UPDATE recall_entries SET cwd=''`,
			`UPDATE excluded_sessions SET file_path=NULL`,
			`UPDATE commit_links SET repository=''`,
			`UPDATE commit_trailers SET repository=''`,
			`UPDATE cursor_usage_events SET user_id='', user_email='', account_user_id='', account_polled_at='', conversation_id=''`,
			`DELETE FROM diagnostic_events`, `DELETE FROM git_cache`, `DELETE FROM friction_issue_links`, `DELETE FROM friction_filing_state`, `DELETE FROM cursor_account_polls`, `DELETE FROM artifact_imported_sessions`,
			`DELETE FROM provider_freshness`, `DELETE FROM skipped_files`, `DELETE FROM source_failures`,
			`DELETE FROM local_session_source_baselines`, `DELETE FROM claude_subagent_sources`,
			`DELETE FROM remote_skipped_files`, `DELETE FROM parser_checkpoint_blobs`, `DELETE FROM parser_checkpoints`,
			`DELETE FROM pg_sync_state WHERE key NOT GLOB 'machine_label:*' AND key NOT GLOB 'machine_alias:*' AND key NOT GLOB 'machine_origin:*'`, `DELETE FROM artifact_export_queue`, `DELETE FROM artifact_publications`,
			`DELETE FROM artifact_publication_revisions`, `DELETE FROM artifact_checkpoint_heads`, `DELETE FROM artifact_checkpoint_floors`,
			`DELETE FROM artifact_import_queue`, `DELETE FROM artifact_import_attempt_generations`, `DELETE FROM artifact_peer_checkpoint_heads`,
			`DELETE FROM artifact_checkpoint_landing_sessions`, `DELETE FROM artifact_checkpoint_landings`,
			`DELETE FROM artifact_checkpoint_stage_sessions`, `DELETE FROM artifact_checkpoint_stages`,
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
		return relabelCopiedProjects(ctx, tx)
	})
}

func relabelCopiedProjects(ctx context.Context, tx *sql.Tx) error {
	tables := []string{"sessions", "recall_entries", "insights", "session_deletion_changes", "commit_links", "commit_trailers"}
	labels := map[string]bool{}
	for _, table := range tables {
		if err := collectCopiedProjectLabels(ctx, tx, table, "project", labels); err != nil {
			return err
		}
	}
	for _, column := range []string{"project", "original_project"} {
		if err := collectCopiedProjectLabels(ctx, tx, "session_project_assignments", column, labels); err != nil {
			return err
		}
	}
	values := make([]string, 0, len(labels))
	for value := range labels {
		if CopiedProjectLabel(value) != value {
			values = append(values, value)
		}
	}
	sort.Strings(values)
	if err := stageProjectAliases(ctx, tx, values, labels); err != nil {
		return err
	}
	return applyProjectAliases(ctx, tx, tables)
}

// stageProjectAliases records one collision-free alias per project label in a temp table.
func stageProjectAliases(ctx context.Context, tx *sql.Tx, values []string, labels map[string]bool) error {
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE copied_project_aliases(old TEXT PRIMARY KEY, alias TEXT NOT NULL) WITHOUT ROWID`); err != nil {
		return err
	}
	for _, value := range values {
		alias := CopiedProjectLabel(value)
		for labels[alias] {
			alias += "-"
		}
		labels[alias] = true
		if _, err := tx.ExecContext(ctx, `INSERT INTO copied_project_aliases VALUES(?, ?)`, value, alias); err != nil {
			return err
		}
	}
	return nil
}

// applyProjectAliases rewrites each table once, then drops the temp table.
func applyProjectAliases(ctx context.Context, tx *sql.Tx, tables []string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE session_project_assignments SET
 project=COALESCE((SELECT alias FROM copied_project_aliases WHERE old=project), project),
 original_project=COALESCE((SELECT alias FROM copied_project_aliases WHERE old=original_project), original_project)
 WHERE project IN (SELECT old FROM copied_project_aliases) OR original_project IN (SELECT old FROM copied_project_aliases)`); err != nil {
		return err
	}
	for _, table := range tables {
		if _, err := tx.ExecContext(ctx, "UPDATE "+table+" SET project=(SELECT alias FROM copied_project_aliases WHERE old=project) WHERE project IN (SELECT old FROM copied_project_aliases)"); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `DROP TABLE copied_project_aliases`)
	return err
}

// retainCodexCloudAccounts keeps each Codex cloud session's account grouping key,
// which reporting needs to deduplicate copies, before file paths are cleared.
func retainCodexCloudAccounts(ctx context.Context, tx *sql.Tx) (retErr error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, file_path FROM sessions WHERE agent = ? AND file_path IS NOT NULL`, string(parser.AgentCodexCloud))
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	accounts := map[string]string{}
	for rows.Next() {
		var id, path string
		if err := rows.Scan(&id, &path); err != nil {
			return err
		}
		if key := parser.CodexCloudAccountFromPath(path); key != "" {
			accounts[id] = key
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for id, key := range accounts {
		if _, err := tx.ExecContext(ctx, `INSERT INTO codex_cloud_accounts(session_id, account_key) VALUES(?, ?) ON CONFLICT(session_id) DO UPDATE SET account_key = excluded.account_key`, id, key); err != nil {
			return err
		}
	}
	return nil
}

func collectCopiedProjectLabels(ctx context.Context, tx *sql.Tx, table, column string, labels map[string]bool) (retErr error) {
	rows, err := tx.QueryContext(ctx, "SELECT DISTINCT "+column+" FROM "+table+" WHERE "+column+" IS NOT NULL AND "+column+" <> ''")
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return err
		}
		labels[value] = true
	}
	return rows.Err()
}
