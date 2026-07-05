package vector

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strconv"

	"go.kenn.io/agentsview/internal/db"
)

// refreshWatermarkKey is the vector_meta key holding the RFC3339 ended_at
// high-water mark of the most recent Refresh scan, used to restrict the
// next incremental (full=false) scan to newer sessions.
const refreshWatermarkKey = "refresh_watermark"

// MessageSource is the slice of the archive the mirror needs (implemented
// by *db.DB).
type MessageSource interface {
	ScanEmbeddableMessages(ctx context.Context, since string,
		fn func(db.EmbeddableMessage) error) (string, error)
}

// RefreshStats summarizes one Refresh call: Upserted counts mirror rows
// inserted or changed (new identity or content_hash changed), Unchanged
// counts rows rescanned with an identical content_hash (e.g. an
// ordinal-only shift), and Deleted counts mirror rows removed, whether by
// slot eviction (see DocKey) or, in full mode, by reconciliation against
// identities no longer present in the scan.
type RefreshStats struct {
	Upserted  int
	Deleted   int
	Unchanged int
}

// DocKey computes the mirror's document identity for a message: a
// source_uuid keeps the key stable across ordinal-shifting rewrites
// (compaction, resync); its absence falls back to a session+ordinal key.
func DocKey(sessionID, sourceUUID string, ordinal int) string {
	if sourceUUID != "" {
		return "u:" + sessionID + ":" + sourceUUID
	}
	return "o:" + sessionID + ":" + strconv.Itoa(ordinal)
}

// contentHash returns the mirror's content_hash for content: kit's
// sqlitevec store uses it as the revision column, so any change here
// invalidates the embedding stamp and marks the document pending.
func contentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// Refresh reconciles the vector_messages mirror against src. full=true
// scans the entire archive (since="") and additionally deletes mirror rows
// (and their vectors, via store.DeleteVectors) whose identity was not seen
// in the scan; full=false scans only sessions newer than the stored
// watermark (vector_meta key "refresh_watermark") and only upserts,
// leaving deletion reconciliation to a subsequent full refresh. The
// watermark is advanced to the scan's max ended_at afterwards.
func (ix *Index) Refresh(
	ctx context.Context, src MessageSource, full bool,
) (RefreshStats, error) {
	if err := ix.requireWritable(); err != nil {
		return RefreshStats{}, err
	}

	since := ""
	if !full {
		watermark, err := ix.refreshWatermark(ctx)
		if err != nil {
			return RefreshStats{}, err
		}
		since = watermark
	}

	var stats RefreshStats
	seen := make(map[string]struct{})
	maxEnded, err := src.ScanEmbeddableMessages(ctx, since, func(m db.EmbeddableMessage) error {
		key := DocKey(m.SessionID, m.SourceUUID, m.Ordinal)
		unchanged, deletedEvictions, err := ix.upsertMirrorRow(ctx, key, m)
		if err != nil {
			return fmt.Errorf("upserting mirror row %s: %w", key, err)
		}
		stats.Deleted += deletedEvictions
		if unchanged {
			stats.Unchanged++
		} else {
			stats.Upserted++
		}
		seen[key] = struct{}{}
		return nil
	})
	if err != nil {
		return RefreshStats{}, fmt.Errorf("scanning embeddable messages: %w", err)
	}

	if full {
		deleted, err := ix.reconcileDeletions(ctx, seen)
		if err != nil {
			return RefreshStats{}, err
		}
		stats.Deleted += deleted
	}

	if maxEnded != "" {
		if err := ix.setRefreshWatermark(ctx, maxEnded); err != nil {
			return RefreshStats{}, err
		}
	}

	return stats, nil
}

// upsertMirrorRow evicts any row occupying the same (session_id, ordinal)
// slot under a different doc_key, then upserts key's row. It returns
// whether the row's content_hash was unchanged (a no-op update, e.g. an
// ordinal-only shift) and how many rows the slot eviction deleted (0 or 1).
func (ix *Index) upsertMirrorRow(
	ctx context.Context, key string, m db.EmbeddableMessage,
) (unchanged bool, evicted int, err error) {
	evicted, err = ix.evictSlotOccupant(ctx, key, m.SessionID, m.Ordinal)
	if err != nil {
		return false, 0, err
	}

	var existingHash sql.NullString
	err = ix.db.QueryRowContext(ctx,
		`SELECT content_hash FROM vector_messages WHERE doc_key = ?`, key,
	).Scan(&existingHash)
	if err != nil && err != sql.ErrNoRows {
		return false, evicted, fmt.Errorf("reading existing content hash: %w", err)
	}

	hash := contentHash(m.Content)
	unchanged = existingHash.Valid && existingHash.String == hash

	if _, err := ix.db.ExecContext(ctx, `
INSERT INTO vector_messages (doc_key, session_id, source_uuid, ordinal, content, content_hash)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(doc_key) DO UPDATE SET
    session_id = excluded.session_id,
    ordinal = excluded.ordinal,
    content = excluded.content,
    content_hash = excluded.content_hash`,
		key, m.SessionID, m.SourceUUID, m.Ordinal, m.Content, hash,
	); err != nil {
		return false, evicted, fmt.Errorf("upserting row: %w", err)
	}
	return unchanged, evicted, nil
}

// evictSlotOccupant deletes any row occupying (sessionID, ordinal) under a
// doc_key other than key, guarding the mirror's unique index before an
// upsert lands on that slot. Each evicted doc_key's vectors are deleted
// first via store.DeleteVectors — the same order full-mode reconciliation
// uses — because reconcileDeletions can never sweep an evicted key: its
// mirror row is already gone before reconciliation runs, and kit's store
// keeps orphaned vectors occupying KNN LIMIT slots even though
// QueryGeneration filters them from hits.
func (ix *Index) evictSlotOccupant(
	ctx context.Context, key, sessionID string, ordinal int,
) (int, error) {
	rows, err := ix.db.QueryContext(ctx,
		`SELECT doc_key FROM vector_messages
		 WHERE session_id = ? AND ordinal = ? AND doc_key != ?`,
		sessionID, ordinal, key)
	if err != nil {
		return 0, fmt.Errorf("finding slot occupant: %w", err)
	}
	var evictKeys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scanning slot occupant: %w", err)
		}
		evictKeys = append(evictKeys, k)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("iterating slot occupants: %w", err)
	}
	rows.Close()

	for _, k := range evictKeys {
		if err := ix.store.DeleteVectors(ctx, k); err != nil {
			return 0, fmt.Errorf("deleting evicted vectors for %s: %w", k, err)
		}
		if _, err := ix.db.ExecContext(ctx,
			`DELETE FROM vector_messages WHERE doc_key = ?`, k,
		); err != nil {
			return 0, fmt.Errorf("evicting slot occupant %s: %w", k, err)
		}
	}
	return len(evictKeys), nil
}

// reconcileDeletions deletes every mirror row (and its vectors) whose
// doc_key was not seen in a full scan.
func (ix *Index) reconcileDeletions(
	ctx context.Context, seen map[string]struct{},
) (int, error) {
	rows, err := ix.db.QueryContext(ctx, `SELECT doc_key FROM vector_messages`)
	if err != nil {
		return 0, fmt.Errorf("listing mirror doc_keys: %w", err)
	}
	var vanished []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scanning mirror doc_key: %w", err)
		}
		if _, ok := seen[key]; !ok {
			vanished = append(vanished, key)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("iterating mirror doc_keys: %w", err)
	}
	rows.Close()

	for _, key := range vanished {
		if err := ix.store.DeleteVectors(ctx, key); err != nil {
			return 0, fmt.Errorf("deleting vectors for %s: %w", key, err)
		}
		if _, err := ix.db.ExecContext(ctx,
			`DELETE FROM vector_messages WHERE doc_key = ?`, key,
		); err != nil {
			return 0, fmt.Errorf("deleting mirror row %s: %w", key, err)
		}
	}
	return len(vanished), nil
}

// refreshWatermark reads the stored refresh watermark, returning "" when
// none has been recorded yet.
func (ix *Index) refreshWatermark(ctx context.Context) (string, error) {
	var value string
	err := ix.db.QueryRowContext(ctx,
		`SELECT value FROM vector_meta WHERE key = ?`, refreshWatermarkKey,
	).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading refresh watermark: %w", err)
	}
	return value, nil
}

// setRefreshWatermark advances the stored refresh watermark to value.
func (ix *Index) setRefreshWatermark(ctx context.Context, value string) error {
	if _, err := ix.db.ExecContext(ctx, `
INSERT INTO vector_meta (key, value) VALUES (?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		refreshWatermarkKey, value,
	); err != nil {
		return fmt.Errorf("advancing refresh watermark: %w", err)
	}
	return nil
}
