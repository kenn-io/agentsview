package db

import (
	"context"
	"fmt"

	"go.kenn.io/agentsview/internal/parser"
)

// RecoverCodexSessionIdentity moves a retained alternative to the identity
// recovered from its returning source. Copy the archived transcript and its
// curation before deleting the alternative, so a later parse failure loses
// nothing. An occupied destination must belong to the same source and content.
func (db *DB) RecoverCodexSessionIdentity(ctx context.Context, oldID, id, path, agent string) error {
	if oldID == id || (agent != "codex" && agent != "traex" && agent != "augure-code") {
		return nil
	}
	if err := db.requireWritable(); err != nil {
		return err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	source, err := db.GetArtifactExportSession(ctx, oldID)
	if err != nil {
		return err
	}
	if source == nil || source.FilePath == nil || source.Agent != agent ||
		oldID != parser.AltSessionID(parser.BaseSessionID(oldID), *source.FilePath) {
		return nil
	}
	if source.DeletedAt != nil {
		return ErrSessionTrashed
	}
	target, err := db.GetArtifactExportSession(ctx, id)
	if err != nil {
		return err
	}
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	ctxTx := contextTransaction{ctx: ctx, tx: tx}
	var sameSource bool
	if err := tx.QueryRowContext(ctx, `SELECT file_path IN `+codexSourcePathsSQL("?1")+` FROM sessions WHERE id = ?2`, path, oldID).Scan(&sameSource); err != nil {
		return err
	}
	if !sameSource {
		return fmt.Errorf("retained Codex identity %s belongs to another source", oldID)
	}
	stored, err := sessionMessagesTx(ctx, tx, oldID)
	if err != nil {
		return err
	}
	pins, err := savePinsTx(ctxTx, oldID)
	if err != nil {
		return err
	}
	var pending recallEvidenceRevocationEvents
	if target == nil {
		write := sessionCopyWrite(*source, id, stored)
		write.Session.FilePath = &path
		write.DataVersion = source.DataVersion
		write.UsageEvents, err = usageEventsWithQuerier(ctx, tx, oldID, 0)
		if err != nil {
			return err
		}
		for i := range write.UsageEvents {
			write.UsageEvents[i].ID = 0
			write.UsageEvents[i].SessionID = id
		}
		write = db.storageSessionBatchWrite(write)
		if _, err := writeOneSessionBatchTx(ctx, tx, ctxTx, write, &pending, db.usageOnlyStorage(), nil); err != nil {
			return err
		}
	} else {
		if target.DeletedAt != nil {
			return ErrSessionTrashed
		}
		if target.Agent != agent {
			return fmt.Errorf("recovered Codex identity %s belongs to another agent", id)
		}
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(file_path IN `+codexSourcePathsSQL("?1")+`, 0) FROM sessions WHERE id = ?2`, path, id).Scan(&sameSource); err != nil {
			return err
		}
		destination, err := sessionMessagesTx(ctx, tx, id)
		if err != nil {
			return err
		}
		// Ignore the identity we are deliberately changing; compare the
		// archived message and tool content before merging curation.
		rekeyed := sessionCopyWrite(*source, id, stored).Messages
		for i := range destination {
			destination[i].ID = 0
			for j := range destination[i].ToolCalls {
				destination[i].ToolCalls[j].MessageID = 0
			}
		}
		if !sameSource || !transcriptMessagesEqual(rekeyed, destination) {
			return fmt.Errorf("recovered Codex identity %s already holds another transcript", id)
		}
	}
	if err := restorePinsTx(ctxTx, id, pins); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET display_name = COALESCE(display_name, ?),
        local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = ?`, source.DisplayName, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO starred_sessions(session_id,created_at)
        SELECT ?,created_at FROM starred_sessions WHERE session_id = ?`, id, oldID); err != nil {
		return err
	}
	if err := deleteSessionMessagesTx(ctxTx, oldID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, oldID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	db.notifyUsageSessions([]string{oldID, id})
	pending.flush()
	return nil
}
