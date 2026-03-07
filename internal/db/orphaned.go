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

	// Copy session rows. Build column list dynamically so
	// older source DBs missing display_name/deleted_at don't
	// abort the migration.
	orphanCols := orphanSessionCols(ctx, tx)

	if _, err := tx.ExecContext(ctx,
		"INSERT OR IGNORE INTO sessions ("+orphanCols+") "+
			"SELECT "+orphanCols+" FROM old_db.sessions "+
			"WHERE id IN (SELECT id FROM _orphaned_ids)",
	); err != nil {
		return 0, fmt.Errorf(
			"copying orphaned sessions: %w", err,
		)
	}

	// Copy messages. Omit id to let auto-increment assign
	// new IDs (old IDs may collide with freshly synced
	// messages).
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO messages
			(session_id, ordinal, role, content,
			 timestamp, has_thinking, has_tool_use,
			 content_length)
		SELECT
			session_id, ordinal, role, content,
			timestamp, has_thinking, has_tool_use,
			content_length
		FROM old_db.messages
		WHERE session_id IN (
			SELECT id FROM _orphaned_ids
		)`,
	); err != nil {
		return 0, fmt.Errorf(
			"copying orphaned messages: %w", err,
		)
	}

	// Copy tool_calls. Map old message_id to new
	// message_id via the (session_id, ordinal) natural key.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tool_calls
			(message_id, session_id, tool_name, category,
			 tool_use_id, input_json, skill_name,
			 result_content_length, subagent_session_id)
		SELECT
			new_m.id, otc.session_id, otc.tool_name,
			otc.category, otc.tool_use_id, otc.input_json,
			otc.skill_name, otc.result_content_length,
			otc.subagent_session_id
		FROM old_db.tool_calls otc
		JOIN old_db.messages old_m
			ON old_m.id = otc.message_id
		JOIN main.messages new_m
			ON new_m.session_id = old_m.session_id
			AND new_m.ordinal = old_m.ordinal
		WHERE otc.session_id IN (
			SELECT id FROM _orphaned_ids
		)`,
	); err != nil {
		return 0, fmt.Errorf(
			"copying orphaned tool_calls: %w", err,
		)
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

	// Merge display_name and deleted_at for sessions that
	// exist in both DBs.  Probe columns first so older source
	// DBs that lack these columns don't abort the migration.
	hasDisplayName := oldDBHasColumn(ctx, tx, "sessions", "display_name")
	hasDeletedAt := oldDBHasColumn(ctx, tx, "sessions", "deleted_at")

	// Use COALESCE so that any value already in newDB (from a
	// concurrent user action during resync) is preserved.  Old
	// values only fill in NULLs, preventing stale metadata from
	// overwriting newer user actions.
	if hasDisplayName && hasDeletedAt {
		if _, err := tx.ExecContext(ctx, `
			UPDATE main.sessions
			SET display_name = COALESCE(main.sessions.display_name, old_s.display_name),
			    deleted_at   = COALESCE(main.sessions.deleted_at,   old_s.deleted_at)
			FROM old_db.sessions old_s
			WHERE main.sessions.id = old_s.id
			  AND (old_s.display_name IS NOT NULL
			       OR old_s.deleted_at IS NOT NULL)`); err != nil {
			return fmt.Errorf("copying session metadata: %w", err)
		}
	} else if hasDisplayName {
		if _, err := tx.ExecContext(ctx, `
			UPDATE main.sessions
			SET display_name = COALESCE(main.sessions.display_name, old_s.display_name)
			FROM old_db.sessions old_s
			WHERE main.sessions.id = old_s.id
			  AND old_s.display_name IS NOT NULL`); err != nil {
			return fmt.Errorf("copying display_name: %w", err)
		}
	} else if hasDeletedAt {
		if _, err := tx.ExecContext(ctx, `
			UPDATE main.sessions
			SET deleted_at = COALESCE(main.sessions.deleted_at, old_s.deleted_at)
			FROM old_db.sessions old_s
			WHERE main.sessions.id = old_s.id
			  AND old_s.deleted_at IS NOT NULL`); err != nil {
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
	return strings.Join(cols, ", ")
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
