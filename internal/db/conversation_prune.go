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

// errConversationGenerationChanged stops a pruning pass whose archive was
// replaced underneath it; its revisions and rowids name the old generation.
var errConversationGenerationChanged = errors.New("archive replaced during conversation pruning")

// PruneConversationTombstones deletes every removed row of the conversation
// index that has no native source ID and returns how many it deleted.
func (db *DB) PruneConversationTombstones(ctx context.Context) (int64, error) {
	return db.pruneConversationTombstonesThrough(ctx, math.MaxInt64, "")
}

// PruneExpiredConversationTombstones deletes tombstones published before the
// retention clock last started, once that start is at least retention old,
// and then restarts the clock at the revision head read with the old mark.
// Deletions committed while it runs are newer than that head, so they wait a
// full period. A reader that reads at least once per retention period never
// needs to restart.
func (db *DB) PruneExpiredConversationTombstones(ctx context.Context, now time.Time, retention time.Duration) (int64, error) {
	active, err := db.ConversationExportActive(ctx)
	if err != nil || !active {
		return 0, err
	}
	var mark, head int64
	var generation string
	expired := false
	if err := db.Update(ctx, func(tx *sql.Tx) error {
		var err error
		if generation, err = sessionExportMetadataValue(ctx, tx, archiveMetadataDatabaseIDKey, ErrDatabaseIDMissing, "database id"); err != nil {
			return err
		}
		if head, err = archiveJournalRevision(ctx, tx, conversationRevisionKey, "conversation publication"); err != nil {
			return err
		}
		var raw string
		err = tx.QueryRowContext(ctx, `SELECT value FROM archive_metadata WHERE key = ?`, conversationPruneMarkKey).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return putConversationPruneMarkTx(ctx, tx, head, now)
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
		expired = now.Sub(time.Unix(unix, 0)) >= retention
		return nil
	}); err != nil || !expired {
		return 0, err
	}
	pruned, err := db.pruneConversationTombstonesThrough(ctx, mark, generation)
	if err != nil {
		return pruned, err
	}
	err = db.Update(ctx, func(tx *sql.Tx) error {
		if err := requireConversationGenerationTx(ctx, tx, generation); err != nil {
			return err
		}
		return putConversationPruneMarkTx(ctx, tx, head, now)
	})
	return pruned, err
}

func putConversationPruneMarkTx(ctx context.Context, tx *sql.Tx, head int64, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO archive_metadata(key, value) VALUES (?, ?)
	 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		conversationPruneMarkKey, fmt.Sprintf("%d %d", head, now.Unix()))
	return err
}

func requireConversationGenerationTx(ctx context.Context, tx sessionExportQuerier, generation string) error {
	current, err := sessionExportMetadataValue(ctx, tx, archiveMetadataDatabaseIDKey, ErrDatabaseIDMissing, "database id")
	if err != nil {
		return err
	}
	if current != generation {
		return errConversationGenerationChanged
	}
	return nil
}

// pruneConversationTombstonesThrough deletes removed rows without a native
// source ID published at or before through. Tombstones with a source ID stay:
// they map a returning message back to its export ID. It first raises the
// pruned revision floor to the newest such tombstone, so every reader whose
// position is older restarts before any row disappears. Tombstones written
// while it runs have a newer revision and are kept. Every step checks the
// database generation, which an empty generation takes from the scan, and
// stops if a resync replaced the archive.
func (db *DB) pruneConversationTombstonesThrough(ctx context.Context, through int64, generation string) (int64, error) {
	floor, maxRowID, generation, err := db.scanConversationTombstones(ctx, through, generation)
	if err != nil || floor == 0 {
		return 0, err
	}
	if err := db.Update(ctx, func(tx *sql.Tx) error {
		if err := requireConversationGenerationTx(ctx, tx, generation); err != nil {
			return err
		}
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
			if err := requireConversationGenerationTx(ctx, tx, generation); err != nil {
				return err
			}
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

// scanConversationTombstones finds the newest prunable revision and the
// highest rowid to visit. The scan reads the whole index, so it waits in the
// bulk-read lane instead of holding the writer or a quick reader.
func (db *DB) scanConversationTombstones(ctx context.Context, through int64, generation string) (floor, maxRowID int64, current string, err error) {
	release, err := db.acquireBulkRead(ctx)
	if err != nil {
		return 0, 0, "", err
	}
	defer release()
	tx, err := db.getReader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, 0, "", err
	}
	defer func() { _ = tx.Rollback() }()
	if current, err = sessionExportMetadataValue(ctx, tx, archiveMetadataDatabaseIDKey, ErrDatabaseIDMissing, "database id"); err != nil {
		return 0, 0, "", err
	}
	if generation != "" && current != generation {
		return 0, 0, "", errConversationGenerationChanged
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision), 0), COALESCE(MAX(rowid), 0)
	 FROM conversation_messages WHERE removed = 1 AND source_id = '' AND revision <= ?`, through).Scan(&floor, &maxRowID); err != nil {
		return 0, 0, "", fmt.Errorf("find: %w", err)
	}
	return floor, maxRowID, current, tx.Commit()
}
