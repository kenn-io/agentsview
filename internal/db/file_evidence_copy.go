package db

import (
	"context"
	"database/sql"
	"strings"

	"go.kenn.io/agentsview/internal/config"
)

// The destination identity comes from session/ordinal, never the source rowid.
func copyFileEvidenceTx(ctx context.Context, tx *sql.Tx, ids string) error {
	if !oldDBHasTable(ctx, tx, "message_file_evidence") {
		return nil
	}
	cols := "e." + strings.ReplaceAll(evidenceCols, ",", ",e.")
	if _, err := tx.ExecContext(ctx, "INSERT OR REPLACE INTO main.message_file_evidence(message_id,"+evidenceCols+") SELECT dest.id,"+cols+" FROM old_db.message_file_evidence e JOIN old_db.messages src ON src.id=e.message_id JOIN "+ids+" copied ON copied.source_id=src.session_id JOIN main.messages dest ON dest.session_id=copied.id AND dest.ordinal=src.ordinal"); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "UPDATE main.sessions AS dest SET file_evidence_version=src.file_evidence_version,file_evidence_coverage=src.file_evidence_coverage FROM old_db.sessions src JOIN "+ids+" copied ON copied.source_id=src.id WHERE dest.id=copied.id")
	return err
}

func copiedFileEvidencePolicyTx(ctx context.Context, tx *sql.Tx, ids string, policy config.ArchiveContent, images ...config.ToolResultImages) error {
	drop := len(images) > 0 && images[0] == config.ToolResultImagesDrop
	if !policy.OmitsToolContent() && !drop {
		return nil
	}
	where := "m.session_id IN (SELECT id FROM " + ids + ")"
	removed := "1"
	if !policy.UsageOnly() {
		removed = "e.support_kind<>'message'"
		if !policy.OmitsToolContent() {
			removed = "0"
		}
		if drop {
			removed = "(" + removed + " OR e.kind IN ('image','document'))"
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE sessions SET file_evidence_coverage='withheld' WHERE id IN (SELECT m.session_id FROM messages m JOIN message_file_evidence e ON e.message_id=m.id WHERE "+where+" AND "+removed+")"); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "DELETE FROM message_file_evidence WHERE (message_id,slot) IN (SELECT e.message_id,e.slot FROM messages m JOIN message_file_evidence e ON e.message_id=m.id WHERE "+where+" AND "+removed+")")
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE sessions SET file_evidence_coverage='complete' WHERE id IN(SELECT id FROM "+ids+") AND EXISTS(SELECT 1 FROM messages m JOIN message_file_evidence e ON e.message_id=m.id WHERE m.session_id=sessions.id)")
	return err
}

func reconcileFileEvidenceRevisionsTx(ctx context.Context, tx *sql.Tx) error {
	if !oldDBHasTable(ctx, tx, "message_file_evidence") {
		_, err := tx.ExecContext(ctx, "UPDATE main.sessions AS s SET transcript_revision=(SELECT CAST(CAST(old.transcript_revision AS INTEGER)+1 AS TEXT) FROM old_db.sessions old WHERE old.id=s.id) WHERE s.file_evidence_version>0 AND EXISTS(SELECT 1 FROM old_db.sessions old WHERE old.id=s.id)")
		return err
	}
	// Compare ordered coordinates and metadata, excluding database row IDs.
	cols := "m.ordinal,e." + strings.ReplaceAll(evidenceCols, ",", ",e.")
	current := "SELECT " + cols + " FROM main.messages m JOIN main.message_file_evidence e ON e.message_id=m.id WHERE m.session_id=s.id"
	old := strings.ReplaceAll(current, "main.", "old_db.")
	_, err := tx.ExecContext(ctx, "UPDATE main.sessions AS s SET transcript_revision=(SELECT CAST(CAST(old.transcript_revision AS INTEGER)+1 AS TEXT) FROM old_db.sessions old WHERE old.id=s.id) WHERE EXISTS(SELECT 1 FROM old_db.sessions old WHERE old.id=s.id AND (s.file_evidence_version<>old.file_evidence_version OR s.file_evidence_coverage<>old.file_evidence_coverage OR EXISTS("+current+" EXCEPT "+old+") OR EXISTS("+old+" EXCEPT "+current+")))")
	return err
}
