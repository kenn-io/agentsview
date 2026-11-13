// ABOUTME: Removes conversation export tombstones and records the revision
// ABOUTME: floor below which change cursors and checkpoints must restart.
package db

import (
	"context"
	"database/sql"
	"fmt"
)

// conversationPrunedRevisionKey holds the highest publication revision of a
// pruned tombstone. A change cursor or export checkpoint below it may have
// missed a deletion and must restart from a full walk.
const conversationPrunedRevisionKey = "conversation_pruned_revision"

// conversationPruneRowidBatch bounds each delete transaction, so pruning an
// archive with tens of millions of tombstones keeps the WAL small and lets
// other writes interleave.
const conversationPruneRowidBatch = 100_000

// PruneConversationTombstones deletes the removed rows of the conversation
// index that have no native source ID and returns how many it deleted.
// Tombstones with a source ID stay: they map a returning message back to its
// export ID. It first raises the pruned revision
// floor to the newest tombstone, so every reader whose position is older
// restarts before any row disappears. Tombstones written while it runs have a
// newer revision and are kept.
func (db *DB) PruneConversationTombstones(ctx context.Context) (int64, error) {
	var floor, maxRowID int64
	if err := db.Update(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision), 0), COALESCE(MAX(rowid), 0)
		 FROM conversation_messages WHERE removed = 1 AND source_id = ''`).Scan(&floor, &maxRowID); err != nil {
			return fmt.Errorf("find: %w", err)
		}
		if floor == 0 {
			return nil
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO archive_metadata(key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = CAST(MAX(CAST(value AS INTEGER), CAST(excluded.value AS INTEGER)) AS TEXT)`,
			conversationPrunedRevisionKey, floor)
		return err
	}); err != nil || floor == 0 {
		return 0, err
	}
	var pruned int64
	for low := int64(0); low < maxRowID; low += conversationPruneRowidBatch {
		if err := db.Update(ctx, func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx, `DELETE FROM conversation_messages
			 WHERE rowid > ? AND rowid <= ? AND removed = 1 AND source_id = '' AND revision <= ?`,
				low, low+conversationPruneRowidBatch, floor)
			if err != nil {
				return fmt.Errorf("delete: %w", err)
			}
			n, err := res.RowsAffected()
			pruned += n
			return err
		}); err != nil {
			return pruned, err
		}
	}
	return pruned, nil
}
