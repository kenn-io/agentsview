package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

type sqlContextExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// execWithoutCancel runs cleanup SQL even if the operation context was canceled.
func execWithoutCancel(
	ctx context.Context,
	execer sqlContextExecer,
	query string,
	args ...any,
) (sql.Result, error) {
	return execer.ExecContext(context.WithoutCancel(ctx), query, args...)
}

// CopyOrphanedDataFrom copies sessions and their dependent transcript and
// usage rows that exist in the source database but not in this database.
// This preserves archived sessions whose source files no longer exist on disk.
//
// Orphaned sessions are identified by ID-diff: any session
// present in the source but absent from the target after a
// full file sync. This correctly captures sessions whose
// source files were deleted, moved, or otherwise lost —
// exactly the set that would be dropped by a naive DB swap.
//
// The source database must not have active connections (call
// CloseConnections on its DB handle first). Uses ATTACH
// DATABASE on a pinned connection for atomicity.
func (d *DB) CopyOrphanedDataFrom(
	sourcePath string,
) (int, error) {
	return d.CopyOrphanedDataFromExcluding(sourcePath, nil)
}

// CopyOrphanedDataFromExcluding copies orphaned sessions while
// treating extraExcludedIDs as absent by design. This is used by
// resync for parser-level exclusions: those IDs should not be
// restored as orphans, but they also should not become permanent
// user-deletion entries in excluded_sessions.
func (d *DB) CopyOrphanedDataFromExcluding(
	sourcePath string,
	extraExcludedIDs []string,
) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	ctx := context.Background()
	conn, err := d.getWriter().Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf(
			"acquiring connection: %w", err,
		)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(
		ctx, "ATTACH DATABASE ? AS old_db", sourcePath,
	); err != nil {
		return 0, fmt.Errorf(
			"attaching source db: %w", err,
		)
	}
	defer func() {
		_, _ = execWithoutCancel(
			ctx,
			conn,
			"DETACH DATABASE old_db",
		)
	}()

	if _, err := conn.ExecContext(ctx, `
		CREATE TEMP TABLE _extra_excluded_orphan_ids (
			id TEXT PRIMARY KEY
		)`,
	); err != nil {
		return 0, fmt.Errorf(
			"creating extra orphan exclusions: %w", err,
		)
	}
	defer func() {
		_, _ = execWithoutCancel(
			ctx,
			conn,
			"DROP TABLE IF EXISTS _extra_excluded_orphan_ids",
		)
	}()
	if len(extraExcludedIDs) > 0 {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return 0, fmt.Errorf(
				"begin extra orphan exclusions: %w", err,
			)
		}
		stmt, err := tx.PrepareContext(ctx,
			"INSERT OR IGNORE INTO _extra_excluded_orphan_ids (id) VALUES (?)",
		)
		if err != nil {
			_ = tx.Rollback()
			return 0, fmt.Errorf(
				"prepare extra orphan exclusions: %w", err,
			)
		}
		for _, id := range extraExcludedIDs {
			if id == "" {
				continue
			}
			if _, err := stmt.ExecContext(ctx, id); err != nil {
				_ = stmt.Close()
				_ = tx.Rollback()
				return 0, fmt.Errorf(
					"insert extra orphan exclusion %s: %w",
					id, err,
				)
			}
		}
		if err := stmt.Close(); err != nil {
			_ = tx.Rollback()
			return 0, fmt.Errorf(
				"close extra orphan exclusions: %w", err,
			)
		}
		if err := tx.Commit(); err != nil {
			return 0, fmt.Errorf(
				"commit extra orphan exclusions: %w", err,
			)
		}
	}

	// Snapshot orphaned session IDs before any inserts
	// change main.sessions. Exclude permanently deleted sessions
	// so they are not resurrected as orphans.
	//
	// Also exclude stale Codex rows whose file was reparsed into
	// the new DB under a different session id: before dataVersion
	// 40 a forked rollout's replayed parent session_meta overwrote
	// the fork's id (#643), so the fork file's row was stored under
	// the parent's identity with double-counted totals. That row is
	// a stale duplicate of a live file, not an archive of a lost
	// one. Scoped to Codex because it is strictly one session per
	// file; SQLite-backed agents share a file_path across many
	// sessions, where an id missing from the fresh parse can be a
	// genuinely evicted chat that must survive as an orphan.
	if _, err := conn.ExecContext(ctx, `
		CREATE TEMP TABLE _orphaned_ids AS
		SELECT id FROM old_db.sessions
		WHERE id NOT IN (SELECT id FROM main.sessions)
		  AND id NOT IN (SELECT id FROM main.excluded_sessions)
		  AND id NOT IN (SELECT id FROM _extra_excluded_orphan_ids)
		  AND id NOT IN (
			SELECT old_s.id
			FROM old_db.sessions old_s
			JOIN main.sessions new_s
				ON new_s.file_path = old_s.file_path
			WHERE old_s.agent = 'codex'
			  AND new_s.agent = 'codex'
		  )`,
	); err != nil {
		return 0, fmt.Errorf(
			"identifying orphaned sessions: %w", err,
		)
	}
	defer func() {
		_, _ = execWithoutCancel(
			ctx,
			conn,
			"DROP TABLE IF EXISTS _orphaned_ids",
		)
	}()

	var count int
	if err := conn.QueryRowContext(ctx,
		"SELECT count(*) FROM _orphaned_ids",
	).Scan(&count); err != nil {
		return 0, fmt.Errorf(
			"counting orphaned sessions: %w", err,
		)
	}
	t := time.Now()

	// Reconcile revisions and copy orphans in one transaction. Partial
	// orphan copies would leave sessions without their dependent transcript
	// or usage rows, while a revision update without the matching archive
	// copy could make a failed resync look complete.
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin orphan tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := reconcileTranscriptRevisionsTx(ctx, tx); err != nil {
		return 0, fmt.Errorf("reconciling transcript revisions: %w", err)
	}
	if count > 0 {
		if err := copySessionDataForIDs(ctx, tx, "_orphaned_ids"); err != nil {
			return 0, fmt.Errorf("copying orphaned data: %w", err)
		}
		if err := removeGeneratedIdentitySnapshotsWithoutSource(
			ctx, tx, "_orphaned_ids",
		); err != nil {
			return 0, fmt.Errorf("repairing orphan identity snapshots: %w", err)
		}
		if err := sanitizeCopiedSessionContent(
			ctx, tx, "_orphaned_ids", copiedSourceDataVersion(ctx, tx),
		); err != nil {
			return 0, fmt.Errorf("sanitizing orphaned data: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf(
			"committing orphaned data: %w", err,
		)
	}

	if count > 0 {
		log.Printf(
			"resync: copied %d orphaned sessions in %s",
			count, time.Since(t).Round(time.Millisecond),
		)
	}

	return count, nil
}

// CopyTrashedDataFrom copies soft-deleted sessions and their
// messages from the source database. ResyncAll calls this before
// parsing into a fresh DB so UpsertSession can see trashed rows
// and reject source-file writes that would otherwise overwrite
// the user's trash.
func (d *DB) CopyTrashedDataFrom(sourcePath string) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	ctx := context.Background()
	conn, err := d.getWriter().Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf(
			"acquiring connection: %w", err,
		)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(
		ctx, "ATTACH DATABASE ? AS old_db", sourcePath,
	); err != nil {
		return 0, fmt.Errorf(
			"attaching source db: %w", err,
		)
	}
	defer func() {
		_, _ = conn.ExecContext(
			ctx, "DETACH DATABASE old_db",
		)
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin trashed copy tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if !oldDBHasColumn(ctx, tx, "sessions", "deleted_at") {
		return 0, nil
	}

	if _, err := tx.ExecContext(ctx, `
		CREATE TEMP TABLE _trashed_ids AS
		SELECT id FROM old_db.sessions
		WHERE deleted_at IS NOT NULL
		  AND id NOT IN (SELECT id FROM main.excluded_sessions)`); err != nil {
		return 0, fmt.Errorf(
			"identifying trashed sessions: %w", err,
		)
	}
	defer func() {
		_, _ = tx.ExecContext(
			ctx,
			"DROP TABLE IF EXISTS _trashed_ids",
		)
	}()

	var count int
	if err := tx.QueryRowContext(ctx,
		"SELECT count(*) FROM _trashed_ids",
	).Scan(&count); err != nil {
		return 0, fmt.Errorf(
			"counting trashed sessions: %w", err,
		)
	}
	if count == 0 {
		return 0, nil
	}

	if err := copySessionDataForIDs(ctx, tx, "_trashed_ids"); err != nil {
		return 0, fmt.Errorf("copying trashed data: %w", err)
	}
	if err := removeGeneratedIdentitySnapshotsWithoutSource(
		ctx, tx, "_trashed_ids",
	); err != nil {
		return 0, fmt.Errorf("repairing trashed identity snapshots: %w", err)
	}
	if err := sanitizeCopiedSessionContent(
		ctx, tx, "_trashed_ids", copiedSourceDataVersion(ctx, tx),
	); err != nil {
		return 0, fmt.Errorf("sanitizing trashed data: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing trashed copy: %w", err)
	}
	return count, nil
}

// CopySyncStateFrom copies pg_sync_state rows from the source database into the
// current database. ResyncAll uses this to preserve durable local sync metadata
// such as the PG push owner marker across the temp-DB swap.
func (d *DB) CopySyncStateFrom(sourcePath string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	ctx := context.Background()
	conn, err := d.getWriter().Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquiring connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(
		ctx, "ATTACH DATABASE ? AS old_db", sourcePath,
	); err != nil {
		return fmt.Errorf("attaching source db: %w", err)
	}
	defer func() {
		_, _ = execWithoutCancel(ctx, conn, "DETACH DATABASE old_db")
	}()

	// Older databases may have no pg_sync_state table.
	var tableExists int
	err = conn.QueryRowContext(
		ctx, "SELECT 1 FROM old_db.sqlite_master WHERE type='table' AND name='pg_sync_state'",
	).Scan(&tableExists)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("probing pg_sync_state table: %w", err)
	}

	_, err = conn.ExecContext(ctx, `
		INSERT OR REPLACE INTO main.pg_sync_state (key, value)
		SELECT key, value FROM old_db.pg_sync_state
		WHERE key = 'pg_push_marker_id'`)
	if err != nil {
		return fmt.Errorf("copying sync state: %w", err)
	}
	return nil
}

// CopyExcludedSessionsFrom copies the excluded_sessions table
// from the source DB so permanently deleted sessions survive
// full DB rebuilds. The source must not have active connections.
func (d *DB) CopyExcludedSessionsFrom(
	sourcePath string,
) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	ctx := context.Background()
	conn, err := d.getWriter().Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquiring connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(
		ctx, "ATTACH DATABASE ? AS old_db", sourcePath,
	); err != nil {
		return fmt.Errorf("attaching source db: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(
			ctx, "DETACH DATABASE old_db",
		)
	}()

	// Only copy if the source has the table (older DBs won't).
	var tableExists int
	err = conn.QueryRowContext(ctx,
		"SELECT 1 FROM old_db.sqlite_master WHERE type='table' AND name='excluded_sessions'",
	).Scan(&tableExists)
	if err != nil {
		// sql.ErrNoRows means the table doesn't exist.
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("probing excluded_sessions table: %w", err)
	}

	_, err = conn.ExecContext(ctx, `
		INSERT OR IGNORE INTO excluded_sessions (id, created_at)
		SELECT id, created_at
		FROM old_db.excluded_sessions`)
	if err != nil {
		return fmt.Errorf("copying excluded sessions: %w", err)
	}
	return nil
}

// CopySessionMetadataFrom merges user-managed data from the
// source DB into sessions that were re-synced into this DB.
// This preserves display_name, deleted_at, starred_sessions, pinned_messages,
// archive metadata, project identity observations, and worktree project
// mappings across full DB rebuilds.
func (d *DB) CopySessionMetadataFrom(
	sourcePath string,
) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	ctx := context.Background()
	conn, err := d.getWriter().Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquiring connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(
		ctx, "ATTACH DATABASE ? AS old_db", sourcePath,
	); err != nil {
		return fmt.Errorf("attaching source db: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(
			ctx, "DETACH DATABASE old_db",
		)
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin metadata tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Copy user-managed metadata from the quiesced old DB. deleted_at
	// is copied for all rows. display_name is overlaid ONLY for
	// user-owned rows: the fresh DB already holds re-parsed session_name
	// values, so agent-owned and cleared rows must keep the fresh value.
	// Probe columns first so older source DBs don't abort.
	hasDisplayName := oldDBHasColumn(ctx, tx, "sessions", "display_name")
	hasDeletedAt := oldDBHasColumn(ctx, tx, "sessions", "deleted_at")

	if hasDeletedAt {
		if _, err := tx.ExecContext(ctx, `
			UPDATE main.sessions
			SET deleted_at = old_s.deleted_at
			FROM old_db.sessions old_s
			WHERE main.sessions.id = old_s.id`); err != nil {
			return fmt.Errorf("copying deleted_at: %w", err)
		}
	}

	// Copy user-set display_name (renames via RenameSession) from the old DB.
	// In the two-field design display_name is always user-owned, so any
	// non-NULL value is a user rename worth preserving.
	// session_name is repopulated by re-parse and does not need copying.
	//
	// Note: the name_source discriminator column (which would have distinguished
	// user renames from parser-owned titles) was introduced and removed in the
	// same PR as the two-field split and was never present in any released build.
	// Any non-NULL display_name in an upgrading database therefore came from
	// RenameSession (user action) or a pre-feature import — the latter being
	// acceptable to treat as a user rename since there is no lossless heuristic
	// to separate them without name_source.
	if hasDisplayName {
		if _, err := tx.ExecContext(ctx, `
			UPDATE main.sessions
			SET display_name = old_s.display_name
			FROM old_db.sessions old_s
			WHERE main.sessions.id = old_s.id
			  AND old_s.display_name IS NOT NULL`); err != nil {
			return fmt.Errorf("copying user display_name: %w", err)
		}
	}

	// Copy starred sessions (table may not exist in older DBs).
	if oldDBHasTable(ctx, tx, "starred_sessions") {
		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO main.starred_sessions
				(session_id, created_at)
			SELECT session_id, created_at
			FROM old_db.starred_sessions
			WHERE session_id IN (
				SELECT id FROM main.sessions
			)`); err != nil {
			return fmt.Errorf("copying starred sessions: %w", err)
		}
	}

	// Copy pinned messages (table may not exist in older DBs).
	// Map old message_id to new message_id via the
	// (session_id, ordinal) natural key, since auto-increment
	// IDs differ between DBs.
	if oldDBHasTable(ctx, tx, "pinned_messages") {
		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO main.pinned_messages
				(session_id, message_id, ordinal, note, created_at)
			SELECT
				op.session_id, new_m.id, op.ordinal,
				op.note, op.created_at
			FROM old_db.pinned_messages op
			JOIN old_db.messages old_m
				ON old_m.id = op.message_id
			JOIN main.messages new_m
				ON new_m.session_id = old_m.session_id
				AND new_m.ordinal = old_m.ordinal
			WHERE op.session_id IN (
				SELECT id FROM main.sessions
			)`); err != nil {
			return fmt.Errorf("copying pinned messages: %w", err)
		}
	}

	if oldDBHasTable(ctx, tx, "cursor_usage_events") {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM main.cursor_usage_events`); err != nil {
			return fmt.Errorf("clearing cursor usage events: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO main.cursor_usage_events (
				occurred_at, model, kind,
				input_tokens, output_tokens,
				cache_write_tokens, cache_read_tokens,
				charged_cents, cursor_token_fee,
				user_id, user_email, is_headless, dedup_key
			)
			SELECT
				occurred_at, model, kind,
				input_tokens, output_tokens,
				cache_write_tokens, cache_read_tokens,
				charged_cents, cursor_token_fee,
				user_id, user_email, is_headless, dedup_key
			FROM old_db.cursor_usage_events
			ORDER BY occurred_at, id`); err != nil {
			return fmt.Errorf("copying cursor usage events: %w", err)
		}
	}

	if oldDBHasTable(ctx, tx, "archive_metadata") {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO main.archive_metadata (key, value, created_at, updated_at)
			SELECT key, value, created_at, updated_at
			FROM old_db.archive_metadata
			WHERE key != 'database_id'
			ON CONFLICT(key) DO UPDATE SET
				value = CASE
					WHEN excluded.key = 'project_identity_publication_revision'
					THEN CAST(max(
						CAST(archive_metadata.value AS INTEGER),
						CAST(excluded.value AS INTEGER)
					) AS TEXT)
					ELSE excluded.value
				END,
				created_at = excluded.created_at,
				updated_at = excluded.updated_at`); err != nil {
			return fmt.Errorf("copying archive metadata: %w", err)
		}
	}

	if oldDBHasTable(ctx, tx, "project_identity_observations") {
		identityColumn := func(name, fallback string) string {
			if oldDBHasColumn(ctx, tx, "project_identity_observations", name) {
				return name
			}
			return fallback
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO main.project_identity_observations (
				source_archive_id, source_archive_salt,
				project, machine, root_path, git_remote, git_remote_name,
				repository_path, worktree_name, worktree_root_path,
				worktree_relationship, checkout_state, git_branch,
				remote_resolution, remote_candidate_count, observed_at,
				normalized_remote, key_source, key
			)
			SELECT `+identityColumn("source_archive_id", "''")+`,
				`+identityColumn("source_archive_salt", "''")+`,
				project, machine, root_path, git_remote, git_remote_name,
				`+identityColumn("repository_path", "''")+`,
				worktree_name, worktree_root_path,
				`+identityColumn("worktree_relationship", "'unknown'")+`,
				`+identityColumn("checkout_state", "'unknown'")+`,
				`+identityColumn("git_branch", "''")+`,
				`+identityColumn("remote_resolution", "'unknown'")+`,
				`+identityColumn("remote_candidate_count", "0")+`, observed_at,
				normalized_remote, key_source, key
			FROM old_db.project_identity_observations
			WHERE true
			ON CONFLICT(project, machine, root_path, git_remote) DO NOTHING`); err != nil {
			return fmt.Errorf("copying project identity observations: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM main.project_identity_observations
			WHERE git_remote = ''
			  AND EXISTS (
				SELECT 1
				FROM main.project_identity_observations remote
				WHERE remote.project = main.project_identity_observations.project
				  AND remote.machine = main.project_identity_observations.machine
				  AND remote.root_path = main.project_identity_observations.root_path
				  AND remote.git_remote != ''
			  )`); err != nil {
			return fmt.Errorf(
				"removing stale project identity root fallbacks: %w", err)
		}
		if err := scrubProjectIdentityGitRemoteCredentialsTx(ctx, tx); err != nil {
			return err
		}
	}

	if oldDBHasTable(ctx, tx, "session_project_identity_snapshots") {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO main.session_project_identity_snapshots (
				session_id, project, machine, root_path, git_remote,
				git_remote_name, repository_path, worktree_name,
				worktree_root_path, worktree_relationship, checkout_state,
				git_branch, remote_resolution, remote_candidate_count,
				observed_at, normalized_remote, key_source, key
			)
			SELECT session_id, project, machine, root_path, git_remote,
				git_remote_name, repository_path, worktree_name,
				worktree_root_path, worktree_relationship, checkout_state,
				git_branch, remote_resolution, remote_candidate_count,
				observed_at, normalized_remote, key_source, key
			FROM old_db.session_project_identity_snapshots
			WHERE session_id IN (SELECT id FROM main.sessions)
			ON CONFLICT(session_id) DO UPDATE SET
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
					key = excluded.key`); err != nil {
			return fmt.Errorf("copying session project identity snapshots: %w", err)
		}
	}

	// Copy persistent worktree project mappings. Omit id so
	// primary-key values from old_db cannot shadow existing
	// destination rows. ResyncAll may pre-copy mappings into
	// the temp DB before parsing, so the final metadata copy
	// reconciles the table to the quiesced source state.
	if oldDBHasTable(ctx, tx, "worktree_project_mappings") {
		layoutSelect := "'" + WorktreeMappingLayoutExplicit + "'"
		if oldDBHasColumn(ctx, tx, "worktree_project_mappings", "layout") {
			layoutSelect = "layout"
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM main.worktree_project_mappings
			WHERE NOT EXISTS (
				SELECT 1
				FROM old_db.worktree_project_mappings old_m
				WHERE old_m.machine = main.worktree_project_mappings.machine
				  AND old_m.path_prefix = main.worktree_project_mappings.path_prefix
			)`); err != nil {
			return fmt.Errorf("reconciling worktree project mappings: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO main.worktree_project_mappings
				(machine, path_prefix, layout, project, enabled, created_at, updated_at)
			SELECT machine, path_prefix, `+layoutSelect+`, project, enabled, created_at, updated_at
			FROM old_db.worktree_project_mappings
			WHERE true
			ON CONFLICT(machine, path_prefix) DO UPDATE SET
				layout = excluded.layout,
				project = excluded.project,
				enabled = excluded.enabled,
				created_at = excluded.created_at,
				updated_at = excluded.updated_at`); err != nil {
			return fmt.Errorf("copying worktree project mappings: %w", err)
		}
	}

	return tx.Commit()
}

// oldDBHasTable checks if a table exists in old_db.
// Must be called within a connection that has old_db attached.
func oldDBHasTable(
	ctx context.Context, tx *sql.Tx, name string,
) bool {
	var n int
	err := tx.QueryRowContext(ctx,
		"SELECT 1 FROM old_db.sqlite_master WHERE type='table' AND name=?",
		name,
	).Scan(&n)
	return err == nil && n == 1
}

// orphanSessionCols returns the comma-separated column list for
// copying sessions from old_db, including display_name and
// deleted_at only when the source schema has them.
func orphanSessionCols(ctx context.Context, tx *sql.Tx) string {
	cols := []string{
		"id", "project", "machine", "agent", "first_message",
	}
	if oldDBHasColumn(ctx, tx, "sessions", "display_name") {
		cols = append(cols, "display_name")
	}
	if oldDBHasColumn(ctx, tx, "sessions", "session_name") {
		cols = append(cols, "session_name")
	}
	// name_source was removed from the schema; do not copy it.
	cols = append(cols,
		"started_at", "ended_at", "message_count",
		"user_message_count", "file_path", "file_size",
		"file_mtime", "file_hash", "parent_session_id",
		"relationship_type",
	)
	for _, c := range []string{"agent_label", "entrypoint"} {
		if oldDBHasColumn(ctx, tx, "sessions", c) {
			cols = append(cols, c)
		}
	}
	if oldDBHasColumn(ctx, tx, "sessions", "deleted_at") {
		cols = append(cols, "deleted_at")
	}
	cols = append(cols, "created_at")
	for _, c := range []string{
		"total_output_tokens", "peak_context_tokens",
		"has_total_output_tokens", "has_peak_context_tokens",
		"is_automated",
		"tool_failure_signal_count", "tool_retry_count",
		"edit_churn_count", "consecutive_failure_max",
		"outcome", "outcome_confidence",
		"ended_with_role", "final_failure_streak",
		"signals_pending_since", "compaction_count",
		"mid_task_compaction_count",
		"context_pressure_max", "health_score",
		"health_grade", "has_tool_calls",
		"has_context_data", "data_version",
		"quality_signal_version", "short_prompt_count",
		"unstructured_start",
		"missing_success_criteria_count",
		"missing_verification_count",
		"duplicate_prompt_count", "no_code_context_count",
		"runaway_tool_loop_count",
		"cwd", "git_branch", "source_session_id",
		"source_version", "transcript_fidelity", "parser_malformed_lines",
		"is_truncated", "last_write_incremental",
		"transcript_revision",
		"secret_leak_count", "secrets_rules_version",
	} {
		if oldDBHasColumn(ctx, tx, "sessions", c) {
			cols = append(cols, c)
		}
	}
	return strings.Join(cols, ", ")
}

// reconcileTranscriptRevisionsTx preserves read-progress identity across a
// full resync. Reparsed sessions start with fresh local counters, so matching
// transcript rows inherit the old counter and changed rows advance it once.
// The comparison covers the user-visible message and tool-result fields while
// deliberately excluding session metadata and token/source bookkeeping.
func reconcileTranscriptRevisionsTx(
	ctx context.Context, tx *sql.Tx,
) error {
	if !oldDBHasColumn(ctx, tx, "sessions", "transcript_revision") {
		return nil
	}
	for table, columns := range map[string][]string{
		"messages": {
			"thinking_text", "is_system", "model",
			"context_tokens", "output_tokens",
			"has_context_tokens", "has_output_tokens",
			"source_subtype", "is_compact_boundary",
		},
		"tool_calls": {
			"call_index", "result_content", "file_path",
		},
		"tool_result_events": {
			"call_index", "event_index",
		},
	} {
		if !oldDBHasTable(ctx, tx, table) {
			return nil
		}
		for _, column := range columns {
			if !oldDBHasColumn(ctx, tx, table, column) {
				return nil
			}
		}
	}

	_, err := tx.ExecContext(ctx, `
		UPDATE main.sessions AS current
		SET transcript_revision = (
			SELECT CASE WHEN
				NOT EXISTS (
					SELECT ordinal, role, content, thinking_text, timestamp,
						has_thinking, has_tool_use, is_system, model,
						context_tokens, output_tokens, has_context_tokens,
						has_output_tokens, source_subtype, is_compact_boundary
					FROM main.messages WHERE session_id = current.id
					EXCEPT
					SELECT ordinal, role, content, thinking_text, timestamp,
						has_thinking, has_tool_use, is_system, model,
						context_tokens, output_tokens, has_context_tokens,
						has_output_tokens, source_subtype, is_compact_boundary
					FROM old_db.messages WHERE session_id = current.id
				)
				AND NOT EXISTS (
					SELECT ordinal, role, content, thinking_text, timestamp,
						has_thinking, has_tool_use, is_system, model,
						context_tokens, output_tokens, has_context_tokens,
						has_output_tokens, source_subtype, is_compact_boundary
					FROM old_db.messages WHERE session_id = current.id
					EXCEPT
					SELECT ordinal, role, content, thinking_text, timestamp,
						has_thinking, has_tool_use, is_system, model,
						context_tokens, output_tokens, has_context_tokens,
						has_output_tokens, source_subtype, is_compact_boundary
					FROM main.messages WHERE session_id = current.id
				)
				AND NOT EXISTS (
					SELECT m.ordinal, tc.call_index, tc.tool_name, tc.category,
						tc.tool_use_id, tc.input_json, tc.skill_name,
						tc.result_content, tc.subagent_session_id, tc.file_path
					FROM main.tool_calls tc
					JOIN main.messages m ON m.id = tc.message_id
					WHERE tc.session_id = current.id
					EXCEPT
					SELECT m.ordinal, tc.call_index, tc.tool_name, tc.category,
						tc.tool_use_id, tc.input_json, tc.skill_name,
						tc.result_content, tc.subagent_session_id, tc.file_path
					FROM old_db.tool_calls tc
					JOIN old_db.messages m ON m.id = tc.message_id
					WHERE tc.session_id = current.id
				)
				AND NOT EXISTS (
					SELECT m.ordinal, tc.call_index, tc.tool_name, tc.category,
						tc.tool_use_id, tc.input_json, tc.skill_name,
						tc.result_content, tc.subagent_session_id, tc.file_path
					FROM old_db.tool_calls tc
					JOIN old_db.messages m ON m.id = tc.message_id
					WHERE tc.session_id = current.id
					EXCEPT
					SELECT m.ordinal, tc.call_index, tc.tool_name, tc.category,
						tc.tool_use_id, tc.input_json, tc.skill_name,
						tc.result_content, tc.subagent_session_id, tc.file_path
					FROM main.tool_calls tc
					JOIN main.messages m ON m.id = tc.message_id
					WHERE tc.session_id = current.id
				)
				AND NOT EXISTS (
					SELECT tool_call_message_ordinal, call_index, tool_use_id,
						agent_id, subagent_session_id, source, status, content,
						timestamp, event_index
					FROM main.tool_result_events WHERE session_id = current.id
					EXCEPT
					SELECT tool_call_message_ordinal, call_index, tool_use_id,
						agent_id, subagent_session_id, source, status, content,
						timestamp, event_index
					FROM old_db.tool_result_events WHERE session_id = current.id
				)
				AND NOT EXISTS (
					SELECT tool_call_message_ordinal, call_index, tool_use_id,
						agent_id, subagent_session_id, source, status, content,
						timestamp, event_index
					FROM old_db.tool_result_events WHERE session_id = current.id
					EXCEPT
					SELECT tool_call_message_ordinal, call_index, tool_use_id,
						agent_id, subagent_session_id, source, status, content,
						timestamp, event_index
					FROM main.tool_result_events WHERE session_id = current.id
				)
			THEN old.transcript_revision
			ELSE CAST(CAST(old.transcript_revision AS INTEGER) + 1 AS TEXT)
			END
			FROM old_db.sessions AS old
			WHERE old.id = current.id
		)
		WHERE EXISTS (
			SELECT 1 FROM old_db.sessions AS old WHERE old.id = current.id
		)`)
	return err
}

func copySessionDataForIDs(
	ctx context.Context,
	tx *sql.Tx,
	tempIDsTable string,
) error {
	// Copy session rows. Build column list dynamically so
	// older source DBs missing display_name/deleted_at don't
	// abort the migration.
	orphanCols := orphanSessionCols(ctx, tx)

	if _, err := tx.ExecContext(ctx,
		"INSERT OR IGNORE INTO sessions ("+orphanCols+") "+
			"SELECT "+orphanCols+" FROM old_db.sessions "+
			"WHERE id IN (SELECT id FROM "+tempIDsTable+")",
	); err != nil {
		return fmt.Errorf("copying sessions: %w", err)
	}

	// Copy messages. Omit id to let auto-increment assign
	// new IDs (old IDs may collide with freshly synced
	// messages). Probe is_system so older source DBs that
	// lack the column don't abort the migration.
	var msgCols strings.Builder
	msgCols.WriteString("session_id, ordinal, role, content, " +
		"timestamp, has_thinking, has_tool_use, " +
		"content_length")
	if oldDBHasColumn(ctx, tx, "messages", "is_system") {
		msgCols.WriteString(", is_system")
	}
	for _, c := range []string{
		"model", "token_usage", "context_tokens",
		"output_tokens", "has_context_tokens",
		"has_output_tokens",
		"claude_message_id", "claude_request_id",
		"source_type", "source_subtype",
		"source_uuid", "source_parent_uuid",
		"is_sidechain", "is_compact_boundary",
		"thinking_text",
	} {
		if oldDBHasColumn(ctx, tx, "messages", c) {
			msgCols.WriteString(", " + c)
		}
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO messages ("+msgCols.String()+") "+
			"SELECT "+msgCols.String()+" FROM old_db.messages "+
			"WHERE session_id IN (SELECT id FROM "+tempIDsTable+")",
	); err != nil {
		return fmt.Errorf("copying messages: %w", err)
	}

	if err := copyUsageEventsForIDs(ctx, tx, tempIDsTable); err != nil {
		return err
	}

	// Copy tool_calls. Map old message_id to new
	// message_id via the (session_id, ordinal) natural key.
	toolCallCols := []string{
		"message_id", "session_id", "tool_name", "category",
		"tool_use_id", "input_json", "skill_name",
		"result_content_length",
	}
	toolCallSelect := []string{
		"new_m.id", "otc.session_id", "otc.tool_name",
		"otc.category", "otc.tool_use_id", "otc.input_json",
		"otc.skill_name", "otc.result_content_length",
	}
	if oldDBHasColumn(ctx, tx, "tool_calls", "result_content") {
		toolCallCols = append(toolCallCols, "result_content")
		toolCallSelect = append(toolCallSelect, "otc.result_content")
	}
	toolCallCols = append(toolCallCols, "subagent_session_id")
	toolCallSelect = append(toolCallSelect, "otc.subagent_session_id")
	if oldDBHasColumn(ctx, tx, "tool_calls", "file_path") {
		toolCallCols = append(toolCallCols, "file_path")
		toolCallSelect = append(toolCallSelect, "otc.file_path")
	} else {
		toolCallCols = append(toolCallCols, "file_path")
		toolCallSelect = append(toolCallSelect, "NULL")
	}
	if oldDBHasColumn(ctx, tx, "tool_calls", "call_index") {
		toolCallCols = append(toolCallCols, "call_index")
		toolCallSelect = append(toolCallSelect, "otc.call_index")
	} else {
		toolCallCols = append(toolCallCols, "call_index")
		toolCallSelect = append(toolCallSelect, "NULL")
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tool_calls
			(`+strings.Join(toolCallCols, ", ")+`)
		SELECT
			`+strings.Join(toolCallSelect, ", ")+`
		FROM old_db.tool_calls otc
		JOIN old_db.messages old_m
			ON old_m.id = otc.message_id
		JOIN main.messages new_m
			ON new_m.session_id = old_m.session_id
			AND new_m.ordinal = old_m.ordinal
		WHERE otc.session_id IN (
			SELECT id FROM `+tempIDsTable+`
		)
		ORDER BY otc.id`,
	); err != nil {
		return fmt.Errorf("copying tool_calls: %w", err)
	}

	if oldDBHasTable(ctx, tx, "tool_result_events") {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tool_result_events
				(session_id, tool_call_message_ordinal,
				 call_index, tool_use_id, agent_id,
				 subagent_session_id, source, status,
				 content, content_length, timestamp,
				 event_index)
			SELECT
				session_id, tool_call_message_ordinal,
				call_index, tool_use_id, agent_id,
				subagent_session_id, source, status,
				content, content_length, timestamp,
				event_index
			FROM old_db.tool_result_events
			WHERE session_id IN (
				SELECT id FROM `+tempIDsTable+`
			)`,
		); err != nil {
			return fmt.Errorf(
				"copying tool_result_events: %w", err,
			)
		}
	}

	if oldDBHasTable(ctx, tx, "secret_findings") {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO secret_findings
				(session_id, rule_name, confidence, location_kind,
				 message_ordinal, call_index, event_index,
				 match_start, match_end, match_index,
				 redacted_match, rules_version, created_at)
			SELECT
				session_id, rule_name, confidence, location_kind,
				message_ordinal, call_index, event_index,
				match_start, match_end, match_index,
				redacted_match, rules_version, created_at
			FROM old_db.secret_findings
			WHERE session_id IN (
				SELECT id FROM `+tempIDsTable+`
			)`,
		); err != nil {
			return fmt.Errorf("copying secret_findings: %w", err)
		}
	}

	if err := copyPinnedMessagesForIDs(ctx, tx, tempIDsTable); err != nil {
		return err
	}
	return nil
}

func copyUsageEventsForIDs(
	ctx context.Context,
	tx *sql.Tx,
	tempIDsTable string,
) error {
	if !oldDBHasTable(ctx, tx, "usage_events") {
		return nil
	}

	// Copy the intersection of known columns so archives created before later
	// nullable usage fields were introduced remain safe to resync. Credits-first
	// draft archives keep ai_credits as a compatibility column; translate it
	// while copying because the source file may be gone permanently.
	var targetCols, selectExprs []string
	for _, col := range []string{
		"session_id", "message_ordinal", "source", "model",
		"input_tokens", "output_tokens",
		"cache_creation_input_tokens", "cache_read_input_tokens",
		"reasoning_tokens",
	} {
		if oldDBHasColumn(ctx, tx, "usage_events", col) {
			targetCols = append(targetCols, col)
			selectExprs = append(selectExprs, col)
		}
	}
	hasLegacyCredits := oldDBHasColumn(ctx, tx, "usage_events", "ai_credits")
	hasCostUSD := oldDBHasColumn(ctx, tx, "usage_events", "cost_usd")
	if hasLegacyCredits || hasCostUSD {
		targetCols = append(targetCols, "cost_usd")
		switch {
		case hasLegacyCredits && hasCostUSD:
			selectExprs = append(selectExprs,
				"CASE WHEN ai_credits IS NOT NULL THEN ai_credits * 0.01 ELSE cost_usd END")
		case hasLegacyCredits:
			selectExprs = append(selectExprs, "ai_credits * 0.01")
		default:
			selectExprs = append(selectExprs, "cost_usd")
		}
	}
	for _, col := range []string{"cost_status", "cost_source"} {
		hasColumn := oldDBHasColumn(ctx, tx, "usage_events", col)
		if !hasColumn && !hasLegacyCredits {
			continue
		}
		targetCols = append(targetCols, col)
		fallback := "''"
		if hasColumn {
			fallback = col
		}
		if hasLegacyCredits {
			value := "'exact'"
			if col == "cost_source" {
				value = "'copilot-reported'"
			}
			selectExprs = append(selectExprs,
				"CASE WHEN ai_credits IS NOT NULL THEN "+value+" ELSE "+fallback+" END")
		} else {
			selectExprs = append(selectExprs, fallback)
		}
	}
	for _, col := range []string{"occurred_at", "dedup_key"} {
		if oldDBHasColumn(ctx, tx, "usage_events", col) {
			targetCols = append(targetCols, col)
			selectExprs = append(selectExprs, col)
		}
	}
	if len(targetCols) == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO usage_events ("+strings.Join(targetCols, ", ")+") "+
			"SELECT "+strings.Join(selectExprs, ", ")+" FROM old_db.usage_events "+
			"WHERE session_id IN (SELECT id FROM "+tempIDsTable+")",
	); err != nil {
		return fmt.Errorf("copying usage_events: %w", err)
	}
	return nil
}

// removeGeneratedIdentitySnapshotsWithoutSource removes only placeholder
// snapshots created by the session-insert trigger for the current copy batch.
// Real source snapshots are overlaid later by CopySessionMetadataFrom. The
// temporary ID table and both snapshot primary keys keep the work proportional
// to copied rows rather than total archive size.
func removeGeneratedIdentitySnapshotsWithoutSource(
	ctx context.Context,
	tx *sql.Tx,
	tempIDsTable string,
) error {
	missingSourceSnapshot := "true"
	if oldDBHasTable(ctx, tx, "session_project_identity_snapshots") {
		missingSourceSnapshot = `NOT EXISTS (
			SELECT 1 FROM old_db.session_project_identity_snapshots old_snapshot
			WHERE old_snapshot.session_id =
				session_project_identity_snapshots.session_id
		)`
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM main.session_project_identity_snapshots
		WHERE session_id IN (SELECT id FROM `+tempIDsTable+`)
		  AND `+missingSourceSnapshot); err != nil {
		return fmt.Errorf("removing generated identity snapshots: %w", err)
	}
	return nil
}

// sanitizedSourceDataVersion is the first data version at which write
// paths into an archive sanitize message content, tool result content,
// and tool result events: dataVersion 58 forced a full resync that
// re-ingested live sessions through SanitizeUTF8 and ran the copy-time
// sanitize pass over preserved orphans, and later writers sanitize at
// ingest. Copying from a source at or above this version skips those
// row-by-row passes, which otherwise dominate resync time on large
// archives.
//
// sanitizedInputSourceDataVersion is the same watermark for
// tool_calls.input_json, which ingest did not sanitize until
// dataVersion 59. Sources between the two versions only pay the
// single-column input pass.
//
// Bump the relevant constant to the then-current dataVersion if
// SanitizeUTF8 ever gains rules that must apply to already-stored
// rows.
const (
	sanitizedSourceDataVersion      = 58
	sanitizedInputSourceDataVersion = 59
)

// copiedSourceDataVersion reads the attached old_db's data version.
// Read errors are logged and returned as 0 so the copy conservatively
// re-sanitizes everything.
func copiedSourceDataVersion(ctx context.Context, tx *sql.Tx) int {
	var version int
	if err := tx.QueryRowContext(
		ctx, "PRAGMA old_db.user_version",
	).Scan(&version); err != nil {
		log.Printf("resync: reading source data version: %v", err)
		return 0
	}
	return version
}

func sanitizeCopiedSessionContent(
	ctx context.Context,
	tx *sql.Tx,
	tempIDsTable string,
	sourceVersion int,
) error {
	// Each pass runs only when the source predates the version at
	// which ingest started sanitizing that field, so a v58 source
	// upgrading to v59 pays only the single-column input pass.
	if sourceVersion < sanitizedInputSourceDataVersion {
		if err := sanitizeCopiedToolCallInputs(ctx, tx, tempIDsTable); err != nil {
			return err
		}
	}
	if sourceVersion >= sanitizedSourceDataVersion {
		return nil
	}
	if err := sanitizeCopiedMessageContent(ctx, tx, tempIDsTable); err != nil {
		return err
	}
	if err := sanitizeCopiedToolCallResults(ctx, tx, tempIDsTable); err != nil {
		return err
	}
	return sanitizeCopiedToolResultEvents(ctx, tx, tempIDsTable)
}

type copiedTextUpdate struct {
	id      int64
	content string
	length  int
}

func sanitizeCopiedMessageContent(
	ctx context.Context,
	tx *sql.Tx,
	tempIDsTable string,
) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, content, content_length
		 FROM main.messages
		 WHERE session_id IN (SELECT id FROM `+tempIDsTable+`)`,
	)
	if err != nil {
		return fmt.Errorf("querying copied messages: %w", err)
	}
	defer rows.Close()

	var updates []copiedTextUpdate
	for rows.Next() {
		var row copiedTextUpdate
		var storedLength int
		if err := rows.Scan(&row.id, &row.content, &storedLength); err != nil {
			return fmt.Errorf("scanning copied message: %w", err)
		}
		sanitized := SanitizeUTF8(row.content)
		if sanitized == row.content {
			continue
		}
		row.length = sanitizedCopiedTextLength(
			row.content, sanitized, storedLength,
		)
		row.content = sanitized
		updates = append(updates, row)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating copied messages: %w", err)
	}
	for _, row := range updates {
		if _, err := tx.ExecContext(ctx,
			`UPDATE main.messages
			 SET content = ?, content_length = ?
			 WHERE id = ?`,
			row.content, row.length, row.id,
		); err != nil {
			return fmt.Errorf("updating copied message %d: %w", row.id, err)
		}
	}
	return nil
}

type copiedNullableTextUpdate struct {
	id      int64
	content any
	length  any
}

func sanitizeCopiedToolCallInputs(
	ctx context.Context,
	tx *sql.Tx,
	tempIDsTable string,
) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, input_json
		 FROM main.tool_calls
		 WHERE session_id IN (SELECT id FROM `+tempIDsTable+`)
		   AND input_json IS NOT NULL`,
	)
	if err != nil {
		return fmt.Errorf("querying copied tool call inputs: %w", err)
	}
	defer rows.Close()

	var updates []copiedNullableTextUpdate
	for rows.Next() {
		var row copiedNullableTextUpdate
		var content sql.NullString
		if err := rows.Scan(&row.id, &content); err != nil {
			return fmt.Errorf("scanning copied tool call input: %w", err)
		}
		if !content.Valid {
			continue
		}
		sanitized := SanitizeUTF8(content.String)
		if sanitized == content.String {
			continue
		}
		if sanitized == "" {
			row.content = nil
		} else {
			row.content = sanitized
		}
		updates = append(updates, row)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating copied tool call inputs: %w", err)
	}
	for _, row := range updates {
		if _, err := tx.ExecContext(ctx,
			`UPDATE main.tool_calls
			 SET input_json = ?
			 WHERE id = ?`,
			row.content, row.id,
		); err != nil {
			return fmt.Errorf("updating copied tool call input %d: %w", row.id, err)
		}
	}
	return nil
}

func sanitizeCopiedToolCallResults(
	ctx context.Context,
	tx *sql.Tx,
	tempIDsTable string,
) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, result_content, result_content_length
		 FROM main.tool_calls
		 WHERE session_id IN (SELECT id FROM `+tempIDsTable+`)
		   AND result_content IS NOT NULL`,
	)
	if err != nil {
		return fmt.Errorf("querying copied tool calls: %w", err)
	}
	defer rows.Close()

	var updates []copiedNullableTextUpdate
	for rows.Next() {
		var id int64
		var content sql.NullString
		var storedLength sql.NullInt64
		if err := rows.Scan(&id, &content, &storedLength); err != nil {
			return fmt.Errorf("scanning copied tool call: %w", err)
		}
		if !content.Valid {
			continue
		}
		sanitized := SanitizeUTF8(content.String)
		if sanitized == content.String {
			continue
		}
		update := copiedNullableTextUpdate{id: id}
		if sanitized == "" {
			update.content = nil
		} else {
			update.content = sanitized
		}
		update.length = sanitizedCopiedNullableTextLength(
			content.String, sanitized, storedLength,
		)
		updates = append(updates, update)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating copied tool calls: %w", err)
	}
	for _, row := range updates {
		if _, err := tx.ExecContext(ctx,
			`UPDATE main.tool_calls
			 SET result_content = ?, result_content_length = ?
			 WHERE id = ?`,
			row.content, row.length, row.id,
		); err != nil {
			return fmt.Errorf("updating copied tool call %d: %w", row.id, err)
		}
	}
	return nil
}

func sanitizeCopiedToolResultEvents(
	ctx context.Context,
	tx *sql.Tx,
	tempIDsTable string,
) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, content, content_length
		 FROM main.tool_result_events
		 WHERE session_id IN (SELECT id FROM `+tempIDsTable+`)`,
	)
	if err != nil {
		return fmt.Errorf("querying copied tool result events: %w", err)
	}
	defer rows.Close()

	var updates []copiedTextUpdate
	for rows.Next() {
		var row copiedTextUpdate
		var storedLength int
		if err := rows.Scan(&row.id, &row.content, &storedLength); err != nil {
			return fmt.Errorf("scanning copied tool result event: %w", err)
		}
		sanitized := SanitizeUTF8(row.content)
		if sanitized == row.content {
			continue
		}
		row.length = sanitizedCopiedTextLength(
			row.content, sanitized, storedLength,
		)
		row.content = sanitized
		updates = append(updates, row)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating copied tool result events: %w", err)
	}
	for _, row := range updates {
		if _, err := tx.ExecContext(ctx,
			`UPDATE main.tool_result_events
			 SET content = ?, content_length = ?
			 WHERE id = ?`,
			row.content, row.length, row.id,
		); err != nil {
			return fmt.Errorf(
				"updating copied tool result event %d: %w",
				row.id, err,
			)
		}
	}
	return nil
}

func sanitizedCopiedTextLength(
	original, sanitized string,
	storedLength int,
) int {
	removed := len(original) - len(sanitized)
	if removed > 0 {
		subtractRemovedBytes(&storedLength, removed)
	}
	return storedLength
}

func sanitizedCopiedNullableTextLength(
	original, sanitized string,
	storedLength sql.NullInt64,
) any {
	if !storedLength.Valid {
		return nil
	}
	length := int(storedLength.Int64)
	removed := len(original) - len(sanitized)
	if removed > 0 {
		subtractRemovedBytes(&length, removed)
	}
	return int64(length)
}

func copyPinnedMessagesForIDs(
	ctx context.Context,
	tx *sql.Tx,
	tempIDsTable string,
) error {
	if !oldDBHasTable(ctx, tx, "pinned_messages") {
		return nil
	}

	// Re-map old message IDs to the newly inserted message rows.
	// Prefer source_uuid when available because it survives ordinal
	// shifts, then fall back to the same (session_id, ordinal)
	// natural key used by tool call copying.
	if oldDBHasColumn(ctx, tx, "messages", "source_uuid") {
		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO main.pinned_messages
				(session_id, message_id, ordinal, note, created_at)
			SELECT
				op.session_id, new_m.id, new_m.ordinal,
				op.note, op.created_at
			FROM old_db.pinned_messages op
			JOIN old_db.messages old_m
				ON old_m.id = op.message_id
			JOIN main.messages new_m
				ON new_m.session_id = old_m.session_id
				AND new_m.source_uuid = old_m.source_uuid
			WHERE op.session_id IN (
				SELECT id FROM `+tempIDsTable+`
			)
			  AND old_m.source_uuid IS NOT NULL
			  AND old_m.source_uuid <> ''`,
		); err != nil {
			return fmt.Errorf(
				"copying pinned messages by source_uuid: %w", err,
			)
		}
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO main.pinned_messages
			(session_id, message_id, ordinal, note, created_at)
		SELECT
			op.session_id, new_m.id, new_m.ordinal,
			op.note, op.created_at
		FROM old_db.pinned_messages op
		JOIN old_db.messages old_m
			ON old_m.id = op.message_id
		JOIN main.messages new_m
			ON new_m.session_id = old_m.session_id
			AND new_m.ordinal = old_m.ordinal
		WHERE op.session_id IN (
			SELECT id FROM `+tempIDsTable+`
		)`,
	); err != nil {
		return fmt.Errorf("copying pinned messages by ordinal: %w", err)
	}
	return nil
}

// oldDBHasColumn checks if a column exists in an old_db table
// via PRAGMA table_info. Safe to call even if the table is missing.
func oldDBHasColumn(
	ctx context.Context, tx *sql.Tx, table, column string,
) bool {
	rows, err := tx.QueryContext(ctx,
		"PRAGMA old_db.table_info("+table+")")
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name string
		var typ, dflt sql.NullString
		var notNull, pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return false
		}
		if name == column {
			return true
		}
	}
	return false
}
