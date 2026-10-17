package db

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/parser"
)

// RecoverCodexSessionIdentity moves a retained alternative to the identity
// recovered from its returning source. Copy the archived transcript and its
// curation before deleting the alternative, so a later parse failure loses
// nothing. An occupied destination must belong to the same source and content.
func (db *DB) RecoverCodexSessionIdentity(ctx context.Context, oldID, id, path, agent, cwd string) error {
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
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO session_project_assignments
        (session_id, project, original_project, created_at, updated_at)
        SELECT ?, project, original_project, created_at, updated_at FROM session_project_assignments WHERE session_id = ?`, id, oldID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_labels(session_id,label,created_at)
        SELECT ?1,label,created_at FROM session_labels WHERE session_id = ?2
        AND NOT EXISTS (SELECT 1 FROM session_labels WHERE session_id = ?1)`, id, oldID); err != nil {
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
	snapshots, err := db.listSessionProjectIdentitySnapshotsFrom(ctx, tx, []string{oldID})
	if err != nil {
		return err
	}
	if snapshot, ok := snapshots[oldID]; ok && cwd != "" && snapshot.RootPath == cwd {
		// Reuse snapshot precedence: resolved destination evidence wins over
		// history, and foreign metadata must not supply the corrected project.
		snapshot.SessionID = id
		if err := upsertProjectIdentityObservationTxContext(ctx, tx, snapshot); err != nil {
			return err
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
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET project = COALESCE(
        (SELECT project FROM session_project_assignments WHERE session_id = ?1), project) WHERE id = ?1`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_project_assignments WHERE session_id = ?`, oldID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_labels WHERE session_id = ?`, oldID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO session_external_parents
        (session_id, parent_session_id, created_at, updated_at)
        SELECT ?,parent_session_id,created_at,updated_at FROM session_external_parents WHERE session_id = ?`, id, oldID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_external_parents WHERE session_id = ?`, oldID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE session_external_parents SET parent_session_id = ? WHERE parent_session_id = ?`, id, oldID); err != nil {
		return err
	}
	// The transcript is unchanged by this move. Keep reviewed entries and
	// verify their evidence against the recovered rows before removing the source.
	if _, err := tx.ExecContext(ctx, `UPDATE recall_entries SET source_session_id = ?,
        updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE source_session_id = ?`, id, oldID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE recall_evidence SET session_id = ? WHERE session_id = ?`, id, oldID); err != nil {
		return err
	}
	if err := reconcileRecallEvidenceForSessionTx(ctx, tx, id, &pending); err != nil {
		return err
	}
	if err := deleteSessionMessagesTx(ctxTx, oldID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, oldID); err != nil {
		return err
	}
	if _, err := linkSubagentSessionsForSessionsTx(ctx, tx, []string{id, oldID}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	db.notifyUsageSessions([]string{oldID, id})
	pending.flush()
	return nil
}

// CorrectCodexSourceExclusions releases a misfiled ID only after parsing its
// deleted source under a different identity. The deletion stays on that source.
func (db *DB) CorrectCodexSourceExclusions(ctx context.Context, id, path, agent string) error {
	if path == "" || (agent != "codex" && agent != "traex" && agent != "augure-code") {
		return nil
	}
	if err := db.requireWritable(); err != nil {
		return err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT id,file_path FROM excluded_sessions WHERE file_path IN `+codexSourcePathsSQL("?1")+`
        AND include_codex_pages = 0 AND id != ?2 AND (id GLOB ?3 OR id GLOB ?4)`, path, id, agent+":*", "*~"+agent+":*")
	if err != nil {
		return err
	}
	defer rows.Close()
	type exclusion struct{ id, path string }
	var exclusions []exclusion
	for rows.Next() {
		var e exclusion
		if err := rows.Scan(&e.id, &e.path); err != nil {
			return err
		}
		exclusions = append(exclusions, e)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, e := range exclusions {
		if err := retainCodexSourceExclusion(ctx, tx, e.id, id, e.path); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func retainCodexSourceExclusion(ctx context.Context, conn sqlContextExecer, oldID, parsedID, path string) error {
	oldKey := oldID[strings.LastIndex(oldID, ":")+1:]
	key := parsedID[strings.LastIndex(parsedID, ":")+1:]
	// Historical bare-rollout deletions still cover their canonical pages.
	if oldID == parsedID || (parser.CodexThreadIDFromSessionKey(key) != key && strings.HasSuffix(key, "_"+oldKey)) {
		return nil
	}
	retainedID := parser.AltSessionID(parser.BaseSessionID(oldID), path)
	if retainedID == oldID {
		return nil
	}
	if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO excluded_sessions(id,file_path,created_at,include_codex_pages)
        SELECT ?,file_path,created_at,0 FROM excluded_sessions WHERE id = ? AND file_path = ? AND include_codex_pages = 0`, retainedID, oldID, path); err != nil {
		return err
	}
	_, err := conn.ExecContext(ctx, `DELETE FROM excluded_sessions WHERE id = ? AND file_path = ? AND include_codex_pages = 0`, oldID, path)
	return err
}
