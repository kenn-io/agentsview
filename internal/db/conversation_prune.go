// ABOUTME: Removes conversation export tombstones and records the revision
// ABOUTME: floor below which change cursors and checkpoints must restart.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// conversationPrunedRevisionKey holds the highest publication revision of a
// pruned tombstone. A change cursor or export checkpoint below it may have
// missed a deletion and must restart from a full walk.
const conversationPrunedRevisionKey = "conversation_pruned_revision"

// conversationPruneMarkKey records "<revision> <unix seconds>": the
// conversation revision head when the retention clock last started.
const conversationPruneMarkKey = "conversation_prune_mark"

// ConversationTombstoneRetention is how long a deletion stays readable by
// change-stream and export readers before automatic pruning may remove it.
// Each tombstone survives between one and two retention periods.
const ConversationTombstoneRetention = 7 * 24 * time.Hour

// conversationPruneRowidBatch bounds each delete transaction, so pruning an
// archive with tens of millions of tombstones keeps the WAL small and lets
// other writes interleave.
const conversationPruneRowidBatch = 100_000

// PruneConversationTombstones deletes every removed row of the conversation
// index that has no native source ID and returns how many it deleted.
func (db *DB) PruneConversationTombstones(ctx context.Context) (int64, error) {
	return db.pruneConversationTombstonesThrough(ctx, math.MaxInt64)
}

// PruneExpiredConversationTombstones deletes tombstones published before the
// retention clock last started, once that start is at least retention old,
// and then restarts the clock at the current revision head. A reader that
// reads at least once per retention period never needs to restart.
func (db *DB) PruneExpiredConversationTombstones(ctx context.Context, now time.Time, retention time.Duration) (int64, error) {
	active, err := db.ConversationExportActive(ctx)
	if err != nil || !active {
		return 0, err
	}
	var mark, head int64
	var since time.Time
	var marked bool
	if err := db.Update(ctx, func(tx *sql.Tx) error {
		var raw string
		err := tx.QueryRowContext(ctx, `SELECT value FROM archive_metadata WHERE key = ?`, conversationPruneMarkKey).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return err
		}
		revision, seconds, ok := strings.Cut(raw, " ")
		if mark, err = strconv.ParseInt(revision, 10, 64); err != nil || !ok {
			return fmt.Errorf("invalid conversation prune mark %q", raw)
		}
		unix, err := strconv.ParseInt(seconds, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid conversation prune mark %q", raw)
		}
		since, marked = time.Unix(unix, 0), true
		return nil
	}); err != nil {
		return 0, err
	}
	if marked && now.Sub(since) < retention {
		return 0, nil
	}
	var pruned int64
	if marked {
		if pruned, err = db.pruneConversationTombstonesThrough(ctx, mark); err != nil {
			return pruned, err
		}
	}
	err = db.Update(ctx, func(tx *sql.Tx) error {
		var err error
		if head, err = archiveJournalRevision(ctx, tx, conversationRevisionKey, "conversation publication"); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO archive_metadata(key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			conversationPruneMarkKey, fmt.Sprintf("%d %d", head, now.Unix()))
		return err
	})
	return pruned, err
}

// pruneConversationTombstonesThrough deletes removed rows without a native
// source ID published at or before through. Tombstones with a source ID stay:
// they map a returning message back to its export ID. It first raises the
// pruned revision floor to the newest such tombstone, so every reader whose
// position is older restarts before any row disappears. Tombstones written
// while it runs have a newer revision and are kept.
func (db *DB) pruneConversationTombstonesThrough(ctx context.Context, through int64) (int64, error) {
	// The scan reads the whole index, so it runs on a reader instead of
	// holding the writer. Tombstones it misses are newer and stay.
	var floor, maxRowID int64
	if err := db.getReader().QueryRowContext(ctx, `SELECT COALESCE(MAX(revision), 0), COALESCE(MAX(rowid), 0)
	 FROM conversation_messages WHERE removed = 1 AND source_id = '' AND revision <= ?`, through).Scan(&floor, &maxRowID); err != nil {
		return 0, fmt.Errorf("find: %w", err)
	}
	if floor == 0 {
		return 0, nil
	}
	if err := db.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO archive_metadata(key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = CAST(MAX(CAST(value AS INTEGER), CAST(excluded.value AS INTEGER)) AS TEXT)`,
			conversationPrunedRevisionKey, floor)
		return err
	}); err != nil {
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
