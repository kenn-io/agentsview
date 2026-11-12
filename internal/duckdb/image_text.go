package duckdb

import (
	"context"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
)

// insertImageText writes the rows the session's push fingerprint covered.
func insertImageText(ctx context.Context, exec duckMutationExecutor, rows []db.ImageTextRow) error {
	for _, r := range rows {
		if _, err := exec.ExecContext(ctx, `INSERT INTO image_text (session_id,source_kind,message_ordinal,call_index,event_index,image_index,image_hash,processor,provider,model,revision,text) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			r.SessionID, r.SourceKind, r.MessageOrdinal, r.CallIndex, r.EventIndex, r.ImageIndex, r.ImageHash, r.Processor, r.Provider, r.Model, r.Revision, r.Text); err != nil {
			return fmt.Errorf("inserting duckdb image_text %s: %w", r.SessionID, err)
		}
	}
	return nil
}
