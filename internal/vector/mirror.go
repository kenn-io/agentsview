package vector

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

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
// inserted or changed (new identity or content_hash changed; this includes
// a doc_key reinserted after a same-scan slot eviction, see Refresh),
// Unchanged counts rows rescanned with an identical content_hash (e.g. an
// ordinal-only shift with no eviction involved), and Deleted counts mirror
// rows genuinely removed — a slot-evicted doc_key not reinserted anywhere
// else in the same scan, or, in full mode, an identity not seen in the scan
// at all.
type RefreshStats struct {
	Upserted  int
	Deleted   int
	Unchanged int
}

// DocKey computes the mirror's document identity for a message: a
// source_uuid keeps the key stable across ordinal-shifting rewrites
// (compaction, resync); its absence falls back to a session+ordinal key.
//
// The messages schema permits more than one message in a session to share a
// non-empty source_uuid, so occurrence disambiguates them: it is the 1-based
// count of how many times (sessionID, sourceUUID) has been seen so far in
// scan order. The first occurrence keeps the plain "u:<session>:<uuid>"
// key; later occurrences append "#<occurrence>". Since the scan is ordered
// by (session_id, ordinal), occurrence assignment is deterministic and
// stable across resyncs. occurrence is ignored when sourceUUID is empty.
//
// sessionID and sourceUUID are percent-escaped (escapeDocKeyComponent)
// before joining so the ":" and "#" delimiters, and any literal "%", inside
// either component cannot be confused with the key's own structure — e.g.
// source_uuid "dup#2" at its first occurrence would otherwise collide with
// source_uuid "dup" at its second occurrence.
func DocKey(sessionID, sourceUUID string, ordinal, occurrence int) string {
	session := escapeDocKeyComponent(sessionID)
	if sourceUUID != "" {
		uuid := escapeDocKeyComponent(sourceUUID)
		if occurrence > 1 {
			return "u:" + session + ":" + uuid + "#" + strconv.Itoa(occurrence)
		}
		return "u:" + session + ":" + uuid
	}
	return "o:" + session + ":" + strconv.Itoa(ordinal)
}

// escapeDocKeyComponent percent-encodes the characters DocKey uses as
// delimiters — ':', '#', and '%' itself — so a session_id or source_uuid
// containing them cannot be mistaken for key structure, keeping DocKey
// injective.
func escapeDocKeyComponent(s string) string {
	if !strings.ContainsAny(s, "%:#") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '%', ':', '#':
			fmt.Fprintf(&b, "%%%02X", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
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
// leaving that reconciliation to a subsequent full refresh. Either mode
// also resolves same-scan slot evictions (see evictSlotOccupant) once the
// scan completes: a UUID-keyed doc_key evicted from a (session_id, ordinal)
// slot it no longer occupies is deleted via store.DeleteVectors only if it
// was not reinserted elsewhere in the same scan, so a row that merely
// shifted (or was displaced in a shift cascade) keeps its embeddings. The
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
	occurrences := make(map[string]int)
	evicted := make(map[string]struct{})
	sentinel := 0
	maxEnded, err := src.ScanEmbeddableMessages(ctx, since, func(m db.EmbeddableMessage) error {
		occurrence := 1
		if m.SourceUUID != "" {
			occKey := m.SessionID + "\x00" + m.SourceUUID
			occurrences[occKey]++
			occurrence = occurrences[occKey]
		}
		key := DocKey(m.SessionID, m.SourceUUID, m.Ordinal, occurrence)
		unchanged, evictedKeys, err := ix.upsertMirrorRow(ctx, key, m, &sentinel)
		if err != nil {
			return fmt.Errorf("upserting mirror row %s: %w", key, err)
		}
		for _, k := range evictedKeys {
			evicted[k] = struct{}{}
		}
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

	finalized, err := ix.finalizeEvictions(ctx, evicted)
	if err != nil {
		return RefreshStats{}, err
	}
	stats.Deleted += finalized

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
// ordinal-only shift) and the doc_key(s) the slot eviction displaced (0 or
// 1), for the caller to reconcile once the whole scan completes. sentinel
// is a per-Refresh-call counter evictSlotOccupant uses to park a displaced
// row at a unique negative ordinal; see evictSlotOccupant.
func (ix *Index) upsertMirrorRow(
	ctx context.Context, key string, m db.EmbeddableMessage, sentinel *int,
) (unchanged bool, evicted []string, err error) {
	evicted, err = ix.evictSlotOccupant(ctx, key, m.SessionID, m.Ordinal, sentinel)
	if err != nil {
		return false, nil, err
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

// evictSlotOccupant parks the mirror row of any doc_key occupying
// (sessionID, ordinal) under a key other than key at a unique negative
// ordinal, guarding the mirror's unique index before an upsert lands on
// that slot without deleting the row outright. Message ordinals are always
// >= 0, so a negative ordinal can never collide with a real one or with
// another parked row: sentinel is a counter shared across one Refresh
// scan, decremented per eviction to keep every parked ordinal distinct.
//
// The row is left in place, not deleted, so that if the same doc_key is
// reinserted later in the same scan (a stable UUID-keyed identity that
// merely shifted position in a cascade), upsertMirrorRow's ON CONFLICT(doc_key)
// path updates it in place and never touches the embed_gen column kit's
// SaveVectors stamped it with — a fresh INSERT would reset embed_gen to
// NULL and silently uncover the document. Whether an evicted key is
// genuinely gone or was reinserted is not decidable until the whole scan
// finishes, so store.DeleteVectors and the row's actual removal are
// deferred to Refresh's post-scan finalizeEvictions pass.
func (ix *Index) evictSlotOccupant(
	ctx context.Context, key, sessionID string, ordinal int, sentinel *int,
) ([]string, error) {
	rows, err := ix.db.QueryContext(ctx,
		`SELECT doc_key FROM vector_messages
		 WHERE session_id = ? AND ordinal = ? AND doc_key != ?`,
		sessionID, ordinal, key)
	if err != nil {
		return nil, fmt.Errorf("finding slot occupant: %w", err)
	}
	var evictKeys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scanning slot occupant: %w", err)
		}
		evictKeys = append(evictKeys, k)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterating slot occupants: %w", err)
	}
	rows.Close()

	for _, k := range evictKeys {
		*sentinel--
		if _, err := ix.db.ExecContext(ctx,
			`UPDATE vector_messages SET ordinal = ? WHERE doc_key = ?`, *sentinel, k,
		); err != nil {
			return nil, fmt.Errorf("evicting slot occupant %s: %w", k, err)
		}
	}
	return evictKeys, nil
}

// finalizeEvictions resolves every doc_key evictSlotOccupant displaced
// during one Refresh scan: a key whose ordinal is still negative (the
// sentinel evictSlotOccupant parked it at) once the scan is done was never
// reinserted, so it is genuinely gone — its vectors and stamps are deleted
// via store.DeleteVectors, and its mirror row is finally removed. This
// cleanup mirrors what full-mode reconcileDeletions does for identities
// missing from the scan entirely, needed here because reconcileDeletions
// can never see an evicted key parked at a sentinel ordinal (it looks
// vanished until the deletion actually happens), and kit's store keeps
// orphaned vectors occupying KNN LIMIT slots even though QueryGeneration
// filters them from hits. A key whose ordinal was overwritten back to a
// real (non-negative) value was reinserted under its own doc_key later in
// the same scan — it merely shifted position and keeps its row and
// embeddings untouched.
func (ix *Index) finalizeEvictions(ctx context.Context, evicted map[string]struct{}) (int, error) {
	if len(evicted) == 0 {
		return 0, nil
	}
	keys := make([]string, 0, len(evicted))
	for k := range evicted {
		keys = append(keys, k)
	}
	ordinals, err := ix.currentOrdinals(ctx, keys)
	if err != nil {
		return 0, err
	}

	var deleted int
	for _, key := range keys {
		if ordinal, ok := ordinals[key]; ok && ordinal >= 0 {
			continue
		}
		if err := ix.store.DeleteVectors(ctx, key); err != nil {
			return deleted, fmt.Errorf("deleting evicted vectors for %s: %w", key, err)
		}
		if _, err := ix.db.ExecContext(ctx,
			`DELETE FROM vector_messages WHERE doc_key = ?`, key,
		); err != nil {
			return deleted, fmt.Errorf("deleting evicted mirror row %s: %w", key, err)
		}
		deleted++
	}
	return deleted, nil
}

// currentOrdinals returns the current ordinal of each of keys that is still
// present in vector_messages; a key absent from the result was somehow
// already removed from the mirror.
func (ix *Index) currentOrdinals(ctx context.Context, keys []string) (map[string]int, error) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
	args := make([]any, len(keys))
	for i, k := range keys {
		args[i] = k
	}

	rows, err := ix.db.QueryContext(ctx,
		`SELECT doc_key, ordinal FROM vector_messages WHERE doc_key IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("checking evicted doc_key ordinals: %w", err)
	}
	defer rows.Close()

	ordinals := make(map[string]int, len(keys))
	for rows.Next() {
		var k string
		var ordinal int
		if err := rows.Scan(&k, &ordinal); err != nil {
			return nil, fmt.Errorf("scanning evicted doc_key ordinal: %w", err)
		}
		ordinals[k] = ordinal
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("checking evicted doc_key ordinals: %w", err)
	}
	return ordinals, nil
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
