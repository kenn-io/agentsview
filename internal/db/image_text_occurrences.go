package db

import (
	"context"
	"crypto/sha256"
	"fmt"
	"unsafe"

	"github.com/jellydator/ttlcache/v3"
)

// Refreshes revisit unchanged results. Keep parsed image metadata, never the
// potentially large source content, and bound both records and their bytes.
var retainedImageMemo = newImageTextMemo(1024, 1<<20)

type imageTextMemo struct {
	items    *ttlcache.Cache[[sha256.Size]byte, []RetainedImage]
	maxBytes uint64
}

func newImageTextMemo(maxEntries, maxBytes uint64) imageTextMemo {
	cost := func(item ttlcache.CostItem[[sha256.Size]byte, []RetainedImage]) uint64 {
		return retainedImagesCost(item.Value)
	}
	return imageTextMemo{items: ttlcache.New(
		ttlcache.WithCapacity[[sha256.Size]byte, []RetainedImage](maxEntries),
		ttlcache.WithMaxCost(maxBytes, cost),
	), maxBytes: maxBytes}
}

func retainedImagesCost(images []RetainedImage) uint64 {
	var cost uint64
	for _, image := range images {
		cost += uint64(unsafe.Sizeof(image)) + uint64(len(image.MediaType)+len(image.SHA256))
	}
	return cost
}

func (m imageTextMemo) images(content string) []RetainedImage {
	key := sha256.Sum256([]byte(content))
	if item := m.items.Get(key); item != nil {
		return item.Value()
	}
	images := RetainedToolResultImages(content)
	if len(images) > 0 && retainedImagesCost(images) <= m.maxBytes {
		m.items.Set(key, images, ttlcache.NoTTL)
	}
	return images
}

const imageTextBackfillKey = "image_text_occurrences_backfill_v2"

func (db *DB) BackfillImageTextOccurrences(ctx context.Context) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var done int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM stats WHERE key=? AND value!=0`, imageTextBackfillKey).Scan(&done); err != nil {
		return err
	}
	if done != 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM image_text_cache WHERE NOT EXISTS (SELECT 1 FROM image_text_occurrences o WHERE o.image_hash=image_text_cache.image_hash)`); err != nil {
			return err
		}
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO stats(key,value) VALUES (?,1)`, imageTextBackfillKey); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT session_id FROM tool_calls WHERE result_content LIKE '%image%' OR result_content LIKE '%data:%' UNION SELECT session_id FROM tool_result_events WHERE content LIKE '%image%' OR content LIKE '%data:%'`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := refreshImageTextOccurrencesTx(tx, id); err != nil {
			return fmt.Errorf("backfilling image text occurrences: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET sync_marker=MAX(COALESCE(sync_marker,''),strftime('%Y-%m-%dT%H:%M:%fZ','now')) WHERE id IN (SELECT o.session_id FROM image_text_occurrences o JOIN image_text_cache c ON c.image_hash=o.image_hash WHERE c.succeeded=1 AND c.text<>'')`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM image_text_cache WHERE NOT EXISTS (SELECT 1 FROM image_text_occurrences o WHERE o.image_hash=image_text_cache.image_hash)`); err != nil {
		return err
	}
	return tx.Commit()
}

// refreshImageTextOccurrencesTx records image locations after the final tool-result projection.
func refreshImageTextOccurrencesTx(tx transactionQueries, sessionID string) error {
	var enabled int
	if err := tx.QueryRow(`SELECT count(*) FROM stats WHERE key=? AND value!=0`, imageTextBackfillKey).Scan(&enabled); err != nil {
		return err
	}
	if enabled == 0 {
		return nil
	}
	occurrences, err := sessionImageTextOccurrences(tx, sessionID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM image_text_occurrences WHERE session_id=?`, sessionID); err != nil {
		return err
	}
	for _, o := range occurrences {
		var call, event any
		if o.CallID != 0 {
			call = o.CallID
		}
		if o.EventID != 0 {
			event = o.EventID
		}
		if _, err := tx.Exec(`INSERT INTO image_text_occurrences(session_id,call_id,event_id,image_index,image_hash) VALUES(?,?,?,?,?)`, sessionID, call, event, o.Index, o.Hash); err != nil {
			return err
		}
	}
	return nil
}

func sessionImageTextOccurrences(tx transactionQueries, sessionID string) ([]ImageTextOccurrence, error) {
	rows, err := tx.Query(`SELECT tc.id, 0, COALESCE(tc.result_content, '') FROM tool_calls tc
		WHERE tc.session_id=? AND (tc.result_content LIKE '%image%' OR tc.result_content LIKE '%data:%') AND NOT EXISTS (SELECT 1 FROM tool_result_events e WHERE e.session_id=tc.session_id AND e.tool_use_id=tc.tool_use_id AND tc.tool_use_id<>'')
		UNION ALL SELECT 0, e.id, e.content FROM tool_result_events e WHERE e.session_id=? AND (e.content LIKE '%image%' OR e.content LIKE '%data:%')`, sessionID, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var occurrences []ImageTextOccurrence
	for rows.Next() {
		var callID, eventID int64
		var content string
		if err := rows.Scan(&callID, &eventID, &content); err != nil {
			return nil, err
		}
		for _, image := range retainedImageMemo.images(content) {
			if image.SHA256 != "" {
				occurrences = append(occurrences, ImageTextOccurrence{CallID: callID, EventID: eventID, Index: image.Index, Hash: image.SHA256})
			}
		}
	}
	return occurrences, rows.Err()
}
