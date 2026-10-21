package db

import (
	"context"
	"database/sql"
	"fmt"
)

// CopyAttachedArchiveStateFrom supplies identity and completion state before
// replacement sessions are parsed and reserves ownership until rows are restored.
func (d *DB) CopyAttachedArchiveStateFrom(ctx context.Context, source string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	conn, err := d.getWriter().Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "ATTACH DATABASE ? AS old_db", source); err != nil {
		return err
	}
	defer func() { _, _ = execWithoutCancel(ctx, conn, "DETACH DATABASE old_db") }()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := copyAttachedArchiveStateTx(ctx, tx); err != nil {
		return err
	}
	owners := make(map[string]ArchiveSessionProvenance)
	if oldDBHasTable(ctx, tx, "archive_session_provenance") {
		rows, err := tx.QueryContext(ctx, `SELECT session_id,archive_id,device_id,root_id,source_key,manifest_id,native_session_id FROM old_db.archive_session_provenance`)
		if err != nil {
			return fmt.Errorf("reading attached archive ownership: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var owner ArchiveSessionProvenance
			source := &owner.Source
			if err := rows.Scan(&id, &source.ArchiveID, &source.DeviceID, &source.RootID, &source.SourceKey, &owner.ManifestID, &owner.NativeSessionID); err != nil {
				return fmt.Errorf("reading attached archive owner: %w", err)
			}
			owners[id] = owner
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	d.archiveRebuildProvenance.Store(&owners)
	return nil
}

func copyAttachedArchiveStateTx(ctx context.Context, tx *sql.Tx) error {
	for _, item := range []struct{ table, columns string }{
		{"archive_attachments", "repository_id,snapshot_id,archive_id,target,pending_count"},
		{"archive_source_bindings", "archive_id,device_id,root_id,source_key,manifest_id"},
		{"archive_attachment_sources", "repository_id,snapshot_id,archive_id,device_id,root_id,source_key,target,completed_target,last_error"},
	} {
		if !oldDBHasTable(ctx, tx, item.table) {
			continue
		}
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO main."+item.table+" ("+item.columns+") SELECT "+item.columns+" FROM old_db."+item.table); err != nil {
			return fmt.Errorf("copying attached archive state: %w", err)
		}
	}
	if oldDBHasTable(ctx, tx, "pg_sync_state") {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO main.pg_sync_state(key,value) SELECT key,value FROM old_db.pg_sync_state
   WHERE key IN ('artifact_local_installation_id','artifact_local_machine_name') OR key LIKE 'machine\_alias:%' ESCAPE '\' OR key LIKE 'machine\_label:%' ESCAPE '\'`); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE archive_attachments SET pending_count=(SELECT count(*) FROM archive_attachment_sources s
  WHERE s.repository_id=archive_attachments.repository_id AND s.snapshot_id=archive_attachments.snapshot_id AND s.completed_target!=s.target)`)
	return err
}

func (d *DB) AttachedArchiveIDPrefixes(ctx context.Context) ([]string, error) {
	rows, err := d.getReader().QueryContext(ctx, `SELECT device_id FROM archive_source_bindings GROUP BY device_id ORDER BY device_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var prefixes []string
	for rows.Next() {
		var device string
		if err := rows.Scan(&device); err != nil {
			return nil, err
		}
		prefixes = append(prefixes, device+"~")
	}
	return prefixes, rows.Err()
}
