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

// CopyOrphanedDataFrom copies sessions (and their messages
// and tool_calls) that exist in the source database but not
// in this database. This preserves archived sessions whose
// source files no longer exist on disk.
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
	// orphan copies would leave dangling sessions without messages or
	// tool_calls, while a revision update without the matching archive copy
	// could make a failed resync look complete.
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
		sourceVersion := copiedSourceDataVersion(ctx, tx)
		if err := removeGeneratedIdentitySnapshotsWithoutSource(
			ctx, tx, "_orphaned_ids", sourceVersion,
		); err != nil {
			return 0, fmt.Errorf("repairing orphan identity snapshots: %w", err)
		}
		if err := sanitizeCopiedSessionContent(
			ctx, tx, "_orphaned_ids", sourceVersion,
		); err != nil {
			return 0, fmt.Errorf("sanitizing orphaned data: %w", err)
		}
		if err := clearCopiedSelfParents(ctx, tx, "_orphaned_ids"); err != nil {
			return 0, err
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

	trashFilter := "deleted_at IS NOT NULL"
	if oldDBHasColumn(ctx, tx, "sessions", "deletion_cause") {
		trashFilter += " AND (deletion_cause IS NULL" +
			" OR deletion_cause <> '" + legacyDeletionCauseSourceMissing + "')"
	}
	if _, err := tx.ExecContext(ctx, `
		CREATE TEMP TABLE _trashed_ids AS
		SELECT id FROM old_db.sessions
		WHERE `+trashFilter+`
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
	sourceVersion := copiedSourceDataVersion(ctx, tx)
	if err := removeGeneratedIdentitySnapshotsWithoutSource(
		ctx, tx, "_trashed_ids", sourceVersion,
	); err != nil {
		return 0, fmt.Errorf("repairing trashed identity snapshots: %w", err)
	}
	if err := sanitizeCopiedSessionContent(
		ctx, tx, "_trashed_ids", sourceVersion,
	); err != nil {
		return 0, fmt.Errorf("sanitizing trashed data: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing trashed copy: %w", err)
	}
	return count, nil
}

// CopySyncStateFrom copies durable synchronization authority from the source
// database into the current database. Alongside selected pg_sync_state rows it
// preserves artifact publication work, publications, checkpoint heads, and
// floors across the temp-database resync swap. Transient bookkeeping such as
// last_sync_* timestamps is deliberately left behind so the rebuilt DB reports
// its own sync times.
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

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning sync state copy: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if oldDBHasTable(ctx, tx, "pg_sync_state") {
		if _, err := tx.ExecContext(ctx, `
			INSERT OR REPLACE INTO main.pg_sync_state (key, value)
			SELECT key, value FROM old_db.pg_sync_state
			WHERE key = 'pg_push_marker_id'
			   OR key LIKE 'artifact\_%' ESCAPE '\'
			   OR key = ?`, subagentParentRepairQueueStateKey); err != nil {
			return fmt.Errorf("copying sync state: %w", err)
		}
	}
	if oldDBHasTable(ctx, tx, "subagent_parent_repair_queue") {
		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO main.subagent_parent_repair_queue (session_id)
			SELECT session_id FROM old_db.subagent_parent_repair_queue`); err != nil {
			return fmt.Errorf("copying subagent parent repair queue: %w", err)
		}
	}
	if oldDBHasTable(ctx, tx, "subagent_parent_cleanup_queue") {
		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO main.subagent_parent_cleanup_queue (session_id)
			SELECT session_id FROM old_db.subagent_parent_cleanup_queue`); err != nil {
			return fmt.Errorf("copying subagent parent cleanup queue: %w", err)
		}
	}

	headRevisionExpr := "0"
	if oldDBHasColumn(ctx, tx, "artifact_checkpoint_heads", "publication_revision") {
		headRevisionExpr = "publication_revision"
	}
	headSizeExpr := "0"
	if oldDBHasColumn(ctx, tx, "artifact_checkpoint_heads", "checkpoint_size") {
		headSizeExpr = "checkpoint_size"
	}
	artifactCopies := []struct {
		table string
		sql   string
	}{
		{
			// A resync rebuilds the archive, so every copied session must be
			// re-verified by the exporter: copied rows are forced pending=1 on
			// both the fresh-insert and merge branches. Unchanged content is
			// cheap to skip because artifacts are content-addressed. Generation
			// still advances and never regresses so a stale source claim cannot
			// become valid in the rebuilt archive.
			"artifact_export_queue",
			`INSERT INTO main.artifact_export_queue(
				session_id, enqueued_at, generation, pending,
				rejected_generation, last_error, rejected_at)
			 SELECT session_id, enqueued_at, generation + 1, 1, NULL, '', NULL
			 FROM old_db.artifact_export_queue WHERE true
			 ON CONFLICT(session_id) DO UPDATE SET
				enqueued_at = CASE
					WHEN artifact_export_queue.pending = 1 AND excluded.pending = 1
						THEN min(artifact_export_queue.enqueued_at, excluded.enqueued_at)
					WHEN artifact_export_queue.pending = 1
						THEN artifact_export_queue.enqueued_at
					ELSE excluded.enqueued_at
				END,
				generation = max(artifact_export_queue.generation, excluded.generation) + 1,
				pending = 1,
				rejected_generation = NULL,
				last_error = '',
				rejected_at = NULL`,
		},
		{
			"artifact_publications",
			`INSERT OR REPLACE INTO main.artifact_publications(
				origin, session_id, manifest_hash, source_fingerprint)
			 SELECT origin, session_id, manifest_hash, source_fingerprint
			 FROM old_db.artifact_publications`,
		},
		{
			"artifact_publication_revisions",
			`INSERT INTO main.artifact_publication_revisions(origin, revision)
			 SELECT origin, revision FROM old_db.artifact_publication_revisions WHERE true
			 ON CONFLICT(origin) DO UPDATE SET
				revision = max(artifact_publication_revisions.revision, excluded.revision)`,
		},
		{
			"artifact_checkpoint_heads",
			`INSERT OR REPLACE INTO main.artifact_checkpoint_heads(
				origin, sequence, publication_revision, session_map_sha256,
				checkpoint_sha256, checkpoint_size)
			 SELECT origin, sequence, ` + headRevisionExpr + `, session_map_sha256,
				checkpoint_sha256, ` + headSizeExpr + `
			 FROM old_db.artifact_checkpoint_heads`,
		},
		{
			"artifact_checkpoint_floors",
			`INSERT INTO main.artifact_checkpoint_floors(origin, sequence)
			 SELECT origin, sequence FROM old_db.artifact_checkpoint_floors WHERE true
			 ON CONFLICT(origin) DO UPDATE SET
				sequence = max(artifact_checkpoint_floors.sequence, excluded.sequence)`,
		},
	}
	for _, copy := range artifactCopies {
		if !oldDBHasTable(ctx, tx, copy.table) {
			continue
		}
		if _, err := tx.ExecContext(ctx, copy.sql); err != nil {
			return fmt.Errorf("copying %s: %w", copy.table, err)
		}
	}
	if err := copyArtifactImportState(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO main.artifact_checkpoint_floors(origin, sequence)
		SELECT origin, sequence FROM main.artifact_checkpoint_heads WHERE true
		ON CONFLICT(origin) DO UPDATE SET
			sequence = max(
				artifact_checkpoint_floors.sequence,
				excluded.sequence
			)`); err != nil {
		return fmt.Errorf("advancing copied artifact checkpoint floors: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO main.artifact_export_queue(session_id)
		SELECT id FROM main.sessions
		WHERE (
			machine = 'local' OR machine = (
				SELECT value FROM main.pg_sync_state
				WHERE key = 'artifact_local_machine_name'
			)
		  )
		  AND deleted_at IS NULL
		  AND EXISTS (
			SELECT 1 FROM main.pg_sync_state
			WHERE key = 'artifact_origin_id'
		  )`); err != nil {
		return fmt.Errorf("queueing rebuilt sessions for artifact export: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing sync state copy: %w", err)
	}
	return nil
}

// clearCopiedSelfParents applies the self-parent repair to the sessions just
// copied from the source archive. The fresh archive's one-time
// repairLegacySelfParentedSessions pass usually runs before orphans are
// copied, so a self-parented row from an older source would otherwise
// survive the rebuild.
func clearCopiedSelfParents(
	ctx context.Context,
	tx *sql.Tx,
	tempIDsTable string,
) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE main.sessions
		SET parent_session_id = NULLIF(parser_parent_session_id, id),
		local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE id IN (SELECT id FROM `+tempIDsTable+`)
		  AND parent_session_id IS id`); err != nil {
		return fmt.Errorf("clearing copied self-parented sessions: %w", err)
	}
	return nil
}

func copyArtifactImportState(ctx context.Context, tx *sql.Tx) error {
	if oldDBHasTable(ctx, tx, "artifact_import_queue") {
		quarantinePending := "0"
		if oldDBHasColumn(
			ctx, tx, "artifact_import_queue", "quarantine_pending",
		) {
			quarantinePending = "quarantine_pending"
		}
		var conflicts int
		err := tx.QueryRowContext(ctx, `
			SELECT count(*)
			FROM old_db.artifact_import_queue old
			JOIN main.artifact_import_queue current
			  ON current.origin = old.origin
			 AND current.kind = old.kind
			 AND current.name = old.name
			WHERE current.sha256 <> old.sha256 OR current.size <> old.size`,
		).Scan(&conflicts)
		if err != nil {
			return fmt.Errorf("checking artifact import queue conflicts: %w", err)
		}
		if conflicts > 0 {
			return fmt.Errorf(
				"%w: copied artifact import queue identity changed",
				ErrArtifactImportConflict,
			)
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO main.artifact_import_queue (
				origin, kind, name, sha256, size,
				required_checkpoint_version,
				required_manifest_version,
				required_segment_version,
				attempt_generation, quarantine_pending, enqueued_at
			)
			SELECT
				origin, kind, name, sha256, size,
				required_checkpoint_version,
				required_manifest_version,
				required_segment_version,
				attempt_generation, `+quarantinePending+`, enqueued_at
			FROM old_db.artifact_import_queue WHERE true
			ON CONFLICT(origin, kind, name) DO UPDATE SET
				required_checkpoint_version = max(
					artifact_import_queue.required_checkpoint_version,
					excluded.required_checkpoint_version
				),
				required_manifest_version = max(
					artifact_import_queue.required_manifest_version,
					excluded.required_manifest_version
				),
				required_segment_version = max(
					artifact_import_queue.required_segment_version,
					excluded.required_segment_version
				),
				attempt_generation = max(
					artifact_import_queue.attempt_generation,
					excluded.attempt_generation
				),
				quarantine_pending = max(
					artifact_import_queue.quarantine_pending,
					excluded.quarantine_pending
				),
				enqueued_at = min(
					artifact_import_queue.enqueued_at,
					excluded.enqueued_at
				)`)
		if err != nil {
			return fmt.Errorf("copying artifact import queue: %w", err)
		}
		_, err = tx.ExecContext(ctx, `
			DELETE FROM main.artifact_import_queue
			WHERE kind = 'checkpoints'
			  AND EXISTS (
				SELECT 1
				FROM main.artifact_import_queue newer
				WHERE newer.origin = artifact_import_queue.origin
				  AND newer.kind = artifact_import_queue.kind
				  AND newer.name > artifact_import_queue.name
			  )`)
		if err != nil {
			return fmt.Errorf("pruning copied artifact import queue: %w", err)
		}
	}
	if oldDBHasTable(ctx, tx, "artifact_import_attempt_generations") {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO main.artifact_import_attempt_generations (
				singleton, generation
			)
			SELECT singleton, generation
			FROM old_db.artifact_import_attempt_generations WHERE true
			ON CONFLICT(singleton) DO UPDATE SET
				generation = max(
					artifact_import_attempt_generations.generation,
					excluded.generation
				)`)
		if err != nil {
			return fmt.Errorf("copying artifact import generations: %w", err)
		}
	}
	if err := copyArtifactPeerHeads(ctx, tx); err != nil {
		return err
	}
	if err := copyArtifactCheckpointStages(ctx, tx); err != nil {
		return err
	}
	if err := copyArtifactCheckpointLandings(ctx, tx); err != nil {
		return err
	}
	if oldDBHasTable(ctx, tx, "artifact_imported_sessions") {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO main.artifact_imported_sessions (
				origin, gid, manifest_hash, imported_session_id, imported_at
			)
			SELECT
				origin, gid, manifest_hash, imported_session_id, imported_at
			FROM old_db.artifact_imported_sessions WHERE true
			ON CONFLICT(origin, gid) DO UPDATE SET
				manifest_hash = excluded.manifest_hash,
				imported_session_id = excluded.imported_session_id,
				imported_at = excluded.imported_at
			WHERE excluded.imported_at >= artifact_imported_sessions.imported_at`)
		if err != nil {
			return fmt.Errorf("copying artifact imported-session provenance: %w", err)
		}
	}
	return nil
}

func copyArtifactCheckpointStages(ctx context.Context, tx *sql.Tx) error {
	if !oldDBHasTable(ctx, tx, "artifact_checkpoint_stages") {
		return nil
	}
	var conflicts int
	err := tx.QueryRowContext(ctx, `
		SELECT count(*)
		FROM old_db.artifact_checkpoint_stages old
		JOIN main.artifact_checkpoint_stages current
		  ON current.origin = old.origin
		 AND current.sequence = old.sequence
		WHERE current.checkpoint_sha256 <> old.checkpoint_sha256
		   OR current.checkpoint_size <> old.checkpoint_size
		   OR current.decoder_version <> old.decoder_version
		   OR (
				current.complete = 1 AND old.complete = 1
				AND current.session_count <> old.session_count
		   )`,
	).Scan(&conflicts)
	if err != nil {
		return fmt.Errorf("checking copied artifact checkpoint stages: %w", err)
	}
	if conflicts > 0 {
		return fmt.Errorf(
			"%w: copied artifact checkpoint stage changed",
			ErrArtifactImportConflict,
		)
	}
	oldHasStageSessions := oldDBHasTable(
		ctx, tx, "artifact_checkpoint_stage_sessions",
	)
	if !oldHasStageSessions {
		var stages int
		err = tx.QueryRowContext(ctx, `
			SELECT count(*) FROM old_db.artifact_checkpoint_stages`,
		).Scan(&stages)
		if err != nil {
			return fmt.Errorf(
				"checking copied artifact checkpoint stage maps: %w", err,
			)
		}
		if stages > 0 {
			return fmt.Errorf(
				"%w: copied artifact checkpoint stage map is unavailable",
				ErrArtifactImportConflict,
			)
		}
	}
	if oldHasStageSessions {
		err = validateArtifactCheckpointStageMerges(ctx, tx)
		if err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO main.artifact_checkpoint_stages (
			origin, sequence, checkpoint_sha256, checkpoint_size,
			complete, session_count, pending_count,
			decoded_count, decode_offset, decoder_version
		)
		SELECT
			origin, sequence, checkpoint_sha256, checkpoint_size,
			complete, session_count, pending_count,
			decoded_count, decode_offset, decoder_version
		FROM old_db.artifact_checkpoint_stages WHERE true
		ON CONFLICT(origin, sequence) DO UPDATE SET
			complete = max(artifact_checkpoint_stages.complete, excluded.complete),
			session_count = max(
				artifact_checkpoint_stages.session_count,
				excluded.session_count
			),
			decoded_count = max(
				artifact_checkpoint_stages.decoded_count,
				excluded.decoded_count
			),
			decode_offset = max(
				artifact_checkpoint_stages.decode_offset,
				excluded.decode_offset
			)`)
	if err != nil {
		return fmt.Errorf("copying artifact checkpoint stages: %w", err)
	}
	if !oldHasStageSessions {
		return nil
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO main.artifact_checkpoint_stage_sessions (
			origin, sequence, gid, manifest_hash, attempt_generation, satisfied
		)
		SELECT
			origin, sequence, gid, manifest_hash, attempt_generation, satisfied
		FROM old_db.artifact_checkpoint_stage_sessions WHERE true
		ON CONFLICT(origin, sequence, gid) DO UPDATE SET
			attempt_generation = max(
				artifact_checkpoint_stage_sessions.attempt_generation,
				excluded.attempt_generation
			),
			satisfied = max(
				artifact_checkpoint_stage_sessions.satisfied,
				excluded.satisfied
			)`)
	if err != nil {
		return fmt.Errorf("copying artifact checkpoint stage sessions: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE main.artifact_checkpoint_stages AS stage
		SET pending_count = (
			SELECT count(*)
			FROM main.artifact_checkpoint_stage_sessions sessions
			WHERE sessions.origin = stage.origin
			  AND sessions.sequence = stage.sequence
			  AND sessions.satisfied = 0
		)
		WHERE EXISTS (
			SELECT 1
			FROM old_db.artifact_checkpoint_stages old
			WHERE old.origin = stage.origin
			  AND old.sequence = stage.sequence
		)`)
	if err != nil {
		return fmt.Errorf("recounting copied artifact checkpoint stages: %w", err)
	}
	return nil
}

func validateArtifactCheckpointStageMerges(
	ctx context.Context,
	tx *sql.Tx,
) error {
	var conflicts int
	err := tx.QueryRowContext(ctx, `
		WITH overlapping AS (
			SELECT
				old_stage.complete AS old_complete,
				current_stage.complete AS current_complete,
				old_stage.session_count AS old_session_count,
				current_stage.session_count AS current_session_count,
				old_stage.decoded_count AS old_decoded_count,
				current_stage.decoded_count AS current_decoded_count,
				old_stage.decode_offset AS old_decode_offset,
				current_stage.decode_offset AS current_decode_offset,
				EXISTS (
					SELECT gid, manifest_hash
					FROM old_db.artifact_checkpoint_stage_sessions
					WHERE origin = old_stage.origin
					  AND sequence = old_stage.sequence
					EXCEPT
					SELECT gid, manifest_hash
					FROM main.artifact_checkpoint_stage_sessions
					WHERE origin = old_stage.origin
					  AND sequence = old_stage.sequence
				) AS old_only,
				EXISTS (
					SELECT gid, manifest_hash
					FROM main.artifact_checkpoint_stage_sessions
					WHERE origin = old_stage.origin
					  AND sequence = old_stage.sequence
					EXCEPT
					SELECT gid, manifest_hash
					FROM old_db.artifact_checkpoint_stage_sessions
					WHERE origin = old_stage.origin
					  AND sequence = old_stage.sequence
				) AS current_only
			FROM old_db.artifact_checkpoint_stages old_stage
			JOIN main.artifact_checkpoint_stages current_stage
			  ON current_stage.origin = old_stage.origin
			 AND current_stage.sequence = old_stage.sequence
			 AND current_stage.checkpoint_sha256 = old_stage.checkpoint_sha256
			 AND current_stage.checkpoint_size = old_stage.checkpoint_size
		)
		SELECT count(*) FROM overlapping
		WHERE (
			old_complete = 1 AND current_complete = 1
			AND (old_only OR current_only)
		)
		OR (
			old_complete = 0 AND current_complete = 0
			AND (
				(
					old_decoded_count < current_decoded_count
					AND old_decode_offset > current_decode_offset
				)
				OR (
					old_decoded_count > current_decoded_count
					AND old_decode_offset < current_decode_offset
				)
				OR (
					old_decoded_count <= current_decoded_count
					AND old_only
				)
				OR (
					current_decoded_count <= old_decoded_count
					AND current_only
				)
			)
		)
		OR (
			old_complete = 1 AND current_complete = 0
			AND (
				current_decoded_count > old_session_count
				OR current_only
			)
		)
		OR (
			old_complete = 0 AND current_complete = 1
			AND (
				old_decoded_count > current_session_count
				OR old_only
			)
		)`,
	).Scan(&conflicts)
	if err != nil {
		return fmt.Errorf(
			"checking copied artifact checkpoint stage maps: %w", err,
		)
	}
	if conflicts > 0 {
		return fmt.Errorf(
			"%w: copied artifact checkpoint stage map changed",
			ErrArtifactImportConflict,
		)
	}
	return nil
}

func copyArtifactPeerHeads(ctx context.Context, tx *sql.Tx) error {
	if !oldDBHasTable(ctx, tx, "artifact_peer_checkpoint_heads") {
		return nil
	}
	var conflicts int
	err := tx.QueryRowContext(ctx, `
		SELECT count(*)
		FROM old_db.artifact_peer_checkpoint_heads old
		JOIN main.artifact_peer_checkpoint_heads current
		  ON current.origin = old.origin
		 AND current.sequence = old.sequence
		WHERE current.checkpoint_sha256 <> old.checkpoint_sha256
		   OR current.checkpoint_size <> old.checkpoint_size`,
	).Scan(&conflicts)
	if err != nil {
		return fmt.Errorf("checking copied artifact peer heads: %w", err)
	}
	if conflicts > 0 {
		return fmt.Errorf(
			"%w: copied artifact peer head identity changed",
			ErrArtifactImportConflict,
		)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO main.artifact_peer_checkpoint_heads (
			origin, sequence, checkpoint_sha256, checkpoint_size
		)
		SELECT origin, sequence, checkpoint_sha256, checkpoint_size
		FROM old_db.artifact_peer_checkpoint_heads WHERE true
		ON CONFLICT(origin) DO UPDATE SET
			sequence = excluded.sequence,
			checkpoint_sha256 = excluded.checkpoint_sha256,
			checkpoint_size = excluded.checkpoint_size
		WHERE excluded.sequence > artifact_peer_checkpoint_heads.sequence`)
	if err != nil {
		return fmt.Errorf("copying artifact peer heads: %w", err)
	}
	return nil
}

func copyArtifactCheckpointLandings(ctx context.Context, tx *sql.Tx) error {
	if !oldDBHasTable(ctx, tx, "artifact_checkpoint_landings") {
		return nil
	}
	oldHasLandingSessions := oldDBHasTable(
		ctx, tx, "artifact_checkpoint_landing_sessions",
	)
	if !oldHasLandingSessions {
		var landings int
		if err := tx.QueryRowContext(ctx, `
			SELECT count(*) FROM old_db.artifact_checkpoint_landings`,
		).Scan(&landings); err != nil {
			return fmt.Errorf(
				"checking copied artifact checkpoint landing maps: %w", err,
			)
		}
		if landings > 0 {
			return fmt.Errorf(
				"%w: copied artifact checkpoint landing map is unavailable",
				ErrArtifactImportConflict,
			)
		}
	}
	var conflicts int
	err := tx.QueryRowContext(ctx, `
		SELECT count(*)
		FROM old_db.artifact_checkpoint_landings old
		JOIN main.artifact_checkpoint_landings current
		  ON current.origin = old.origin
		 AND current.sequence = old.sequence
		WHERE current.checkpoint_sha256 <> old.checkpoint_sha256
		   OR current.checkpoint_size <> old.checkpoint_size`,
	).Scan(&conflicts)
	if err != nil {
		return fmt.Errorf("checking copied artifact checkpoint landings: %w", err)
	}
	if conflicts > 0 {
		return fmt.Errorf(
			"%w: copied artifact checkpoint landing identity changed",
			ErrArtifactImportConflict,
		)
	}
	if oldHasLandingSessions {
		err = tx.QueryRowContext(ctx, `
			SELECT count(*)
			FROM old_db.artifact_checkpoint_landings old
			JOIN main.artifact_checkpoint_landings current
			  ON current.origin = old.origin
			 AND current.sequence = old.sequence
			 AND current.checkpoint_sha256 = old.checkpoint_sha256
			 AND current.checkpoint_size = old.checkpoint_size
			WHERE EXISTS (
				SELECT gid, manifest_hash
				FROM old_db.artifact_checkpoint_landing_sessions
				WHERE origin = old.origin
				EXCEPT
				SELECT gid, manifest_hash
				FROM main.artifact_checkpoint_landing_sessions
				WHERE origin = old.origin
			)
			OR EXISTS (
				SELECT gid, manifest_hash
				FROM main.artifact_checkpoint_landing_sessions
				WHERE origin = old.origin
				EXCEPT
				SELECT gid, manifest_hash
				FROM old_db.artifact_checkpoint_landing_sessions
				WHERE origin = old.origin
			)`,
		).Scan(&conflicts)
		if err != nil {
			return fmt.Errorf(
				"checking copied artifact checkpoint landing maps: %w", err,
			)
		}
		if conflicts > 0 {
			return fmt.Errorf(
				"%w: copied artifact checkpoint landing map changed",
				ErrArtifactImportConflict,
			)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		CREATE TEMP TABLE _artifact_import_replaced_landings AS
		SELECT old.origin
		FROM old_db.artifact_checkpoint_landings old
		LEFT JOIN main.artifact_checkpoint_landings current
		  ON current.origin = old.origin
		WHERE current.origin IS NULL OR old.sequence > current.sequence`,
	); err != nil {
		return fmt.Errorf("selecting copied artifact checkpoint landings: %w", err)
	}
	defer func() {
		_, _ = tx.ExecContext(
			context.WithoutCancel(ctx),
			"DROP TABLE IF EXISTS _artifact_import_replaced_landings",
		)
	}()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO main.artifact_checkpoint_landings (
			origin, sequence, checkpoint_sha256, checkpoint_size
		)
		SELECT origin, sequence, checkpoint_sha256, checkpoint_size
		FROM old_db.artifact_checkpoint_landings WHERE true
		ON CONFLICT(origin) DO UPDATE SET
			sequence = excluded.sequence,
			checkpoint_sha256 = excluded.checkpoint_sha256,
			checkpoint_size = excluded.checkpoint_size
		WHERE excluded.sequence > artifact_checkpoint_landings.sequence`)
	if err != nil {
		return fmt.Errorf("copying artifact checkpoint landings: %w", err)
	}
	if oldHasLandingSessions {
		_, err = tx.ExecContext(ctx, `
			DELETE FROM main.artifact_checkpoint_landing_sessions
			WHERE origin IN (
				SELECT origin FROM _artifact_import_replaced_landings
			)`)
		if err != nil {
			return fmt.Errorf("clearing copied artifact landing maps: %w", err)
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO main.artifact_checkpoint_landing_sessions (
				origin, gid, manifest_hash
			)
			SELECT sessions.origin, sessions.gid, sessions.manifest_hash
			FROM old_db.artifact_checkpoint_landing_sessions sessions
			JOIN _artifact_import_replaced_landings replaced
			  ON replaced.origin = sessions.origin`)
		if err != nil {
			return fmt.Errorf("copying artifact checkpoint landing maps: %w", err)
		}
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
// archive metadata, project identity observations, worktree project mappings,
// and explicit session project assignments across full DB rebuilds. Immutable
// project snapshots are restored only from source versions that recorded
// parser-source labels reliably.
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

	// Copy user-managed metadata from the quiesced old DB. User-owned
	// deleted_at is copied for all rows. Legacy source_missing deletion state
	// must not re-hide a row that the fresh sync revived after its source
	// reappeared. display_name is overlaid ONLY for
	// user-owned rows: the fresh DB already holds re-parsed session_name
	// values, so agent-owned and cleared rows must keep the fresh value.
	// Probe columns first so older source DBs don't abort.
	hasDisplayName := oldDBHasColumn(ctx, tx, "sessions", "display_name")
	hasDeletedAt := oldDBHasColumn(ctx, tx, "sessions", "deleted_at")
	hasDeletionCause := oldDBHasColumn(ctx, tx, "sessions", "deletion_cause")

	if hasDeletedAt && hasDeletionCause {
		if _, err := tx.ExecContext(ctx, `
			UPDATE main.sessions
			SET deleted_at = CASE
					WHEN old_s.deletion_cause = '`+legacyDeletionCauseSourceMissing+`'
					THEN main.sessions.deleted_at
					ELSE old_s.deleted_at
				END,
				deletion_cause = CASE
					WHEN old_s.deletion_cause = '`+legacyDeletionCauseSourceMissing+`'
					THEN main.sessions.deletion_cause
					ELSE old_s.deletion_cause
				END
			FROM old_db.sessions old_s
			WHERE main.sessions.id = old_s.id`); err != nil {
			return fmt.Errorf("copying deletion state: %w", err)
		}
	} else if hasDeletedAt {
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
	// Auto-increment message IDs differ between DBs, so old
	// message_id must be re-resolved against the fresh rows.
	// Prefer the source_uuid natural key: a re-parse can insert or
	// drop rows (e.g. the v88 IDE-envelope split), shifting ordinals
	// so that the old (session_id, ordinal) key lands on an unrelated
	// row. The uuid must be unique on BOTH sides: a duplicate in the
	// old DB means the uuid does not identify which message the pin
	// was on, so transferring it to a lone same-uuid survivor could
	// misattach a pin whose real target was removed by the re-parse.
	// Duplicated uuids fall back to the pin's occurrence rank inside
	// its (uuid, role, content) group, requiring the group to keep its
	// size on both sides: rank follows the pinned occurrence across
	// ordinal shifts, while a changed group size means the rank no
	// longer identifies an occurrence. Legacy pins whose source row
	// has no source_uuid fall back the same way over the visible
	// (role, content) group. A nonempty uuid with no safe
	// match means the pinned message is gone: the pin is dropped rather
	// than silently attached to whatever now occupies its ordinal.
	if oldDBHasTable(ctx, tx, "pinned_messages") {
		hasSourceUUID := oldDBHasColumn(
			ctx, tx, "messages", "source_uuid",
		)
		if hasSourceUUID {
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
					SELECT id FROM main.sessions
				)
				AND old_m.source_uuid != ''
				AND (
					SELECT COUNT(*) FROM main.messages x
					WHERE x.session_id = old_m.session_id
					AND x.source_uuid = old_m.source_uuid
				) = 1
				AND (
					SELECT COUNT(*) FROM old_db.messages y
					WHERE y.session_id = old_m.session_id
					AND y.source_uuid = old_m.source_uuid
				) = 1`); err != nil {
				return fmt.Errorf(
					"copying pinned messages by source uuid: %w", err,
				)
			}
		}
		// Rank fallback for duplicated uuids: identical (uuid, role,
		// content) rows are distinguishable only by position, so a pin
		// transfers to the row holding the same occurrence rank inside
		// its identity group, provided the group kept its size on both
		// sides. Rank, unlike the old ordinal, follows the pinned
		// occurrence across shifts caused by rows inserted before the
		// group; a changed group size means the rank no longer
		// identifies an occurrence and the pin is dropped. When the
		// uuid was unique the source_uuid pass already restored the
		// same row and INSERT OR IGNORE dedupes.
		if hasSourceUUID {
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
					AND new_m.role = old_m.role
					AND new_m.content = old_m.content
				WHERE op.session_id IN (
					SELECT id FROM main.sessions
				)
				AND old_m.source_uuid != ''
				AND (
					SELECT COUNT(*) FROM old_db.messages y
					WHERE y.session_id = old_m.session_id
					AND y.source_uuid = old_m.source_uuid
					AND y.role = old_m.role
					AND y.content = old_m.content
				) = (
					SELECT COUNT(*) FROM main.messages x
					WHERE x.session_id = old_m.session_id
					AND x.source_uuid = old_m.source_uuid
					AND x.role = old_m.role
					AND x.content = old_m.content
				)
				AND (
					SELECT COUNT(*) FROM old_db.messages y2
					WHERE y2.session_id = old_m.session_id
					AND y2.source_uuid = old_m.source_uuid
					AND y2.role = old_m.role
					AND y2.content = old_m.content
					AND y2.ordinal <= old_m.ordinal
				) = (
					SELECT COUNT(*) FROM main.messages x2
					WHERE x2.session_id = old_m.session_id
					AND x2.source_uuid = old_m.source_uuid
					AND x2.role = old_m.role
					AND x2.content = old_m.content
					AND x2.ordinal <= new_m.ordinal
				)`); err != nil {
				return fmt.Errorf(
					"copying duplicated-uuid pinned messages: %w", err,
				)
			}
		}
		// Rank fallback for legacy pins without a uuid, mirroring
		// restoreLegacyPinByRankTx: the pin transfers to the visible
		// row holding its role, content, and occurrence rank within
		// the visible (role, content) group, provided the group kept
		// its size on both sides. Rank follows the pinned occurrence
		// across shifts from rows the re-parse inserted (e.g. hidden
		// IDE-envelope rows); a changed group size means the rank no
		// longer identifies an occurrence and the pin is dropped. A
		// legacy row may gain a provider uuid in the fresh DB while
		// retaining this fallback identity. Old archives may predate
		// the is_system column; without it every old row counts as
		// visible.
		legacyOnly := ""
		if hasSourceUUID {
			legacyOnly = `
			AND (old_m.source_uuid IS NULL
				OR old_m.source_uuid = '')`
		}
		oldMVisible, oldYVisible, oldY2Visible := "", "", ""
		if oldDBHasColumn(ctx, tx, "messages", "is_system") {
			oldMVisible = `
			AND old_m.is_system = 0`
			oldYVisible = `
				AND y.is_system = 0`
			oldY2Visible = `
				AND y2.is_system = 0`
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
				AND new_m.role = old_m.role
				AND new_m.content = old_m.content
				AND new_m.is_system = 0
			WHERE op.session_id IN (
				SELECT id FROM main.sessions
			)`+legacyOnly+oldMVisible+`
			AND (
				SELECT COUNT(*) FROM old_db.messages y
				WHERE y.session_id = old_m.session_id
				AND y.role = old_m.role
				AND y.content = old_m.content`+oldYVisible+`
			) = (
				SELECT COUNT(*) FROM main.messages x
				WHERE x.session_id = old_m.session_id
				AND x.role = old_m.role
				AND x.content = old_m.content
				AND x.is_system = 0
			)
			AND (
				SELECT COUNT(*) FROM old_db.messages y2
				WHERE y2.session_id = old_m.session_id
				AND y2.role = old_m.role
				AND y2.content = old_m.content
				AND y2.ordinal <= old_m.ordinal`+oldY2Visible+`
			) = (
				SELECT COUNT(*) FROM main.messages x2
				WHERE x2.session_id = old_m.session_id
				AND x2.role = old_m.role
				AND x2.content = old_m.content
				AND x2.is_system = 0
				AND x2.ordinal <= new_m.ordinal
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
				charged_microdollars, cursor_token_fee_microdollars,
				user_id, user_email, is_headless, dedup_key
			)
			SELECT
				occurred_at, model, kind,
				input_tokens, output_tokens,
				cache_write_tokens, cache_read_tokens,
				charged_microdollars, cursor_token_fee_microdollars,
				user_id, user_email, is_headless, dedup_key
			FROM old_db.cursor_usage_events
			ORDER BY occurred_at, id`); err != nil {
			return fmt.Errorf("copying cursor usage events: %w", err)
		}
	}

	// database_id identifies the new physical generation and is never
	// copied: every mirror push keys its incremental cursors to it, so the
	// first push after a resync always full-rebuilds. That rebuild also
	// makes journal continuity across the swap worthless, which is why the
	// publication-revision counters are not copied either — the fresh
	// database's own trigger-maintained counters stand, and the fresh
	// journal rows they stamp are only ever consumed relative to them. Remote
	// import data versions also identify the physical database generation; a
	// remote contributor must establish them again in the replacement.
	if oldDBHasTable(ctx, tx, "archive_metadata") {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO main.archive_metadata (key, value, created_at, updated_at)
			SELECT key, value, created_at, updated_at
			FROM old_db.archive_metadata
			WHERE key NOT IN (
				'database_id',
				'project_identity_publication_revision',
				'session_deletion_publication_revision',
				'worktree_mapping_publication_revision'
			)
			AND key NOT GLOB 'remote_import_data_version:*'
			ON CONFLICT(key) DO UPDATE SET
				value = excluded.value,
				created_at = excluded.created_at,
				updated_at = excluded.updated_at`); err != nil {
			return fmt.Errorf("copying archive metadata: %w", err)
		}
	}

	// The session_deletion_changes journal is deliberately NOT copied from
	// the source: its only consumers (the DuckDB mirror and internal/db)
	// full-rebuild after every resync because the database_id changed, so
	// journal continuity across the swap has no consumer. The fresh
	// database's journal starts over with its own counter.

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

	sourceVersion := copiedSourceDataVersion(ctx, tx)
	if sourceVersion >= projectIdentitySourceSnapshotDataVersion &&
		oldDBHasTable(ctx, tx, "session_project_identity_snapshots") {
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

	// Session assignments are user-owned metadata. Restore them only for
	// sessions that survived the rebuild, then reapply the effective project
	// selected by the user instead of the parser-derived label.
	if oldDBHasTable(ctx, tx, "session_project_assignments") {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO main.session_project_assignments
				(session_id, project, created_at, updated_at)
			SELECT session_id, project, created_at, updated_at
			FROM old_db.session_project_assignments
			WHERE session_id IN (SELECT id FROM main.sessions)
			ON CONFLICT(session_id) DO UPDATE SET
				project = excluded.project,
				created_at = excluded.created_at,
				updated_at = excluded.updated_at`); err != nil {
			return fmt.Errorf("copying session project assignments: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE main.sessions
			SET project = assignment.project
			FROM main.session_project_assignments assignment
			WHERE main.sessions.id = assignment.session_id`); err != nil {
			return fmt.Errorf("applying copied session project assignments: %w", err)
		}
	}

	if oldDBHasTable(ctx, tx, "sessions") {
		rows, err := tx.QueryContext(ctx, `
			SELECT current.id, previous.project, current.project
			FROM main.sessions current
			JOIN old_db.sessions previous ON previous.id = current.id
			WHERE previous.project != current.project
			ORDER BY current.id`)
		if err != nil {
			return fmt.Errorf("listing reparsed session project changes: %w", err)
		}
		type copiedProjectChange struct {
			sessionID       string
			previousProject string
			currentProject  string
		}
		var projectChanges []copiedProjectChange
		for rows.Next() {
			var change copiedProjectChange
			if err := rows.Scan(
				&change.sessionID,
				&change.previousProject,
				&change.currentProject,
			); err != nil {
				rows.Close()
				return fmt.Errorf("scanning reparsed session project change: %w", err)
			}
			projectChanges = append(projectChanges, change)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("iterating reparsed session project changes: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("closing reparsed session project changes: %w", err)
		}
		for _, change := range projectChanges {
			if err := reconcileSessionProjectIdentityAggregatesTx(
				ctx, tx, change.sessionID,
				[]string{change.previousProject, change.currentProject},
			); err != nil {
				return fmt.Errorf(
					"reconciling reparsed session project change %s: %w",
					change.sessionID, err,
				)
			}
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
		originalProjectSelect := "''"
		if oldDBHasColumn(ctx, tx, "worktree_project_mappings", "original_project") {
			originalProjectSelect = "original_project"
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM main.worktree_project_mappings
			WHERE NOT EXISTS (
				SELECT 1
				FROM old_db.worktree_project_mappings old_m
				WHERE old_m.machine = main.worktree_project_mappings.machine
				  AND replace(old_m.path_prefix, char(92), '/') =
					main.worktree_project_mappings.path_prefix
			)`); err != nil {
			return fmt.Errorf("reconciling worktree project mappings: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO main.worktree_project_mappings
				(machine, path_prefix, layout, project, original_project,
				 enabled, created_at, updated_at)
			SELECT machine, replace(path_prefix, char(92), '/'),
				`+layoutSelect+`, project,
				`+originalProjectSelect+`, enabled, created_at, updated_at
			FROM old_db.worktree_project_mappings
			WHERE true
			ON CONFLICT(machine, path_prefix) DO UPDATE SET
				layout = excluded.layout,
				project = excluded.project,
				original_project = CASE
					WHEN worktree_project_mappings.original_project = ''
						THEN excluded.original_project
					ELSE worktree_project_mappings.original_project
				END,
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
// deletion state only when the source schema has it.
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
	if oldDBHasColumn(ctx, tx, "sessions", "parser_parent_session_id") {
		cols = append(cols, "parser_parent_session_id")
	}
	for _, c := range []string{"agent_label", "entrypoint", "session_kind"} {
		if oldDBHasColumn(ctx, tx, "sessions", c) {
			cols = append(cols, c)
		}
	}
	if oldDBHasColumn(ctx, tx, "sessions", "deleted_at") {
		cols = append(cols, "deleted_at")
	}
	if oldDBHasColumn(ctx, tx, "sessions", "deletion_cause") {
		cols = append(cols, "deletion_cause")
	}
	if oldDBHasColumn(ctx, tx, "sessions", "source_missing_at") {
		cols = append(cols, "source_missing_at")
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
// The comparison covers the same persisted transcript identity used by the
// incremental message-diff path, including usage and provider dedup identities.
// Session metadata and parser-only source bookkeeping remain excluded.
func reconcileTranscriptRevisionsTx(
	ctx context.Context, tx *sql.Tx,
) error {
	if !oldDBHasColumn(ctx, tx, "sessions", "transcript_revision") {
		return nil
	}
	for table, columns := range map[string][]string{
		"messages": {
			"thinking_text", "is_system", "model",
			"token_usage", "claude_message_id", "claude_request_id",
			"source_uuid",
			"context_tokens", "output_tokens",
			"has_context_tokens", "has_output_tokens",
			"source_subtype", "prompt_source", "is_compact_boundary",
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
						has_thinking, has_tool_use, is_system, model, token_usage,
						claude_message_id, claude_request_id, source_uuid,
						context_tokens, output_tokens, has_context_tokens,
						has_output_tokens, source_subtype, prompt_source,
						is_compact_boundary
					FROM main.messages WHERE session_id = current.id
					EXCEPT
					SELECT ordinal, role, content, thinking_text, timestamp,
						has_thinking, has_tool_use, is_system, model, token_usage,
						claude_message_id, claude_request_id, source_uuid,
						context_tokens, output_tokens, has_context_tokens,
						has_output_tokens, source_subtype, prompt_source,
						is_compact_boundary
					FROM old_db.messages WHERE session_id = current.id
				)
				AND NOT EXISTS (
					SELECT ordinal, role, content, thinking_text, timestamp,
						has_thinking, has_tool_use, is_system, model, token_usage,
						claude_message_id, claude_request_id, source_uuid,
						context_tokens, output_tokens, has_context_tokens,
						has_output_tokens, source_subtype, prompt_source,
						is_compact_boundary
					FROM old_db.messages WHERE session_id = current.id
					EXCEPT
					SELECT ordinal, role, content, thinking_text, timestamp,
						has_thinking, has_tool_use, is_system, model, token_usage,
						claude_message_id, claude_request_id, source_uuid,
						context_tokens, output_tokens, has_context_tokens,
						has_output_tokens, source_subtype, prompt_source,
						is_compact_boundary
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
		"output_tokens", "provider_id", "has_context_tokens",
		"has_output_tokens",
		"claude_message_id", "claude_request_id",
		"source_type", "source_subtype", "prompt_source",
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

	if oldDBHasTable(ctx, tx, "usage_events") {
		usageEventCols := "session_id, message_ordinal, source, model"
		usageEventSelect := usageEventCols
		if oldDBHasColumn(ctx, tx, "usage_events", "provider_id") {
			usageEventCols += ", provider_id"
			usageEventSelect += ", provider_id"
		}
		usageEventCols += `,
				input_tokens, output_tokens,
				cache_creation_input_tokens, cache_read_input_tokens,
				reasoning_tokens, cost_microdollars, cost_status, cost_source,
				occurred_at, dedup_key`
		usageEventSelect += `,
				input_tokens, output_tokens,
				cache_creation_input_tokens, cache_read_input_tokens,
				reasoning_tokens, cost_microdollars, cost_status, cost_source,
				occurred_at, dedup_key`
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO usage_events (`+usageEventCols+`)
			SELECT `+usageEventSelect+`
			FROM old_db.usage_events
			WHERE session_id IN (
				SELECT id FROM `+tempIDsTable+`
			)`,
		); err != nil {
			return fmt.Errorf("copying usage_events: %w", err)
		}
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

// removeGeneratedIdentitySnapshotsWithoutSource removes placeholder snapshots
// created by the session-insert trigger for the current copy batch. Sources
// predating parser-source snapshots cannot provide trustworthy replacements,
// so every generated snapshot in that batch is removed. Newer sources retain
// placeholders only when CopySessionMetadataFrom will overlay real evidence.
// The temporary ID table and both snapshot primary keys keep the work
// proportional to copied rows rather than total archive size.
func removeGeneratedIdentitySnapshotsWithoutSource(
	ctx context.Context,
	tx *sql.Tx,
	tempIDsTable string,
	sourceVersion int,
) error {
	missingSourceSnapshot := "true"
	if sourceVersion >= projectIdentitySourceSnapshotDataVersion &&
		oldDBHasTable(ctx, tx, "session_project_identity_snapshots") {
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

// projectIdentitySourceSnapshotDataVersion is the first archive version whose
// full reparse rebuilt immutable snapshots with the parser-source project
// rather than a worktree mapping target. Older snapshots must not cross a
// full-resync copy.
const projectIdentitySourceSnapshotDataVersion = 77

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

	// Re-map old message IDs to the newly inserted message rows. These
	// orphaned messages were copied verbatim above, so their ordinals
	// cannot shift. source_uuid is not a safe key here because providers
	// can duplicate it across messages; joining on it would turn one pin
	// into one pin for every matching row.
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
		return fmt.Errorf("copying pinned messages: %w", err)
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
