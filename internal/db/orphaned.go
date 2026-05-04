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

	// Snapshot orphaned session IDs before any inserts
	// change main.sessions. Exclude permanently deleted sessions
	// so they are not resurrected as orphans.
	if _, err := conn.ExecContext(ctx, `
		CREATE TEMP TABLE _orphaned_ids AS
		SELECT id FROM old_db.sessions
		WHERE id NOT IN (SELECT id FROM main.sessions)
		  AND id NOT IN (SELECT id FROM main.excluded_sessions)`,
	); err != nil {
		return 0, fmt.Errorf(
			"identifying orphaned sessions: %w", err,
		)
	}
	defer func() {
		_, _ = conn.ExecContext(
			ctx,
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
	if count == 0 {
		return 0, nil
	}

	t := time.Now()

	// Use a transaction so all three inserts are atomic.
	// Partial orphan copies would leave dangling sessions
	// without messages or tool_calls.
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin orphan tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := copySessionDataForIDs(ctx, tx, "_orphaned_ids"); err != nil {
		return 0, fmt.Errorf("copying orphaned data: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf(
			"committing orphaned data: %w", err,
		)
	}

	log.Printf(
		"resync: copied %d orphaned sessions in %s",
		count, time.Since(t).Round(time.Millisecond),
	)

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

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing trashed copy: %w", err)
	}
	return count, nil
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
	if tableExists != 1 {
		return nil
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
// This preserves display_name, deleted_at, starred_sessions,
// and pinned_messages across full DB rebuilds.
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

	// Copy display_name and deleted_at from the quiesced old DB.
	// These columns may be NULL (user cleared a rename or
	// restored a trashed session), so we copy the value as-is
	// rather than using COALESCE — a NULL in old_db is an
	// intentional clear that must be preserved.
	// Probe columns first so older source DBs that lack these
	// columns don't abort the migration.
	hasDisplayName := oldDBHasColumn(ctx, tx, "sessions", "display_name")
	hasDeletedAt := oldDBHasColumn(ctx, tx, "sessions", "deleted_at")

	if hasDisplayName && hasDeletedAt {
		if _, err := tx.ExecContext(ctx, `
			UPDATE main.sessions
			SET display_name = old_s.display_name,
			    deleted_at   = old_s.deleted_at
			FROM old_db.sessions old_s
			WHERE main.sessions.id = old_s.id`); err != nil {
			return fmt.Errorf("copying session metadata: %w", err)
		}
	} else if hasDisplayName {
		if _, err := tx.ExecContext(ctx, `
			UPDATE main.sessions
			SET display_name = old_s.display_name
			FROM old_db.sessions old_s
			WHERE main.sessions.id = old_s.id`); err != nil {
			return fmt.Errorf("copying display_name: %w", err)
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
	cols = append(cols,
		"started_at", "ended_at", "message_count",
		"user_message_count", "file_path", "file_size",
		"file_mtime", "file_hash", "parent_session_id",
		"relationship_type",
	)
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
		"cwd", "git_branch", "source_session_id",
		"source_version", "parser_malformed_lines",
		"is_truncated",
	} {
		if oldDBHasColumn(ctx, tx, "sessions", c) {
			cols = append(cols, c)
		}
	}
	return strings.Join(cols, ", ")
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
		)`,
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

	if err := copyPinnedMessagesForIDs(ctx, tx, tempIDsTable); err != nil {
		return err
	}
	return nil
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
