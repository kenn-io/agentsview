package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
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

// imageTextBatchTx defers pruning until every changed session has refreshed
// its occurrences, so a shared image can move between sessions in one write.
type imageTextBatchTx struct {
	transactionQueries
	oldHashes map[string]bool
}

func (tx *imageTextBatchTx) pruneImageText() error {
	return pruneImageTextHashes(tx.transactionQueries, tx.oldHashes)
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

// backfillImageTextOccurrencesTx projects retained rows without reparsing sources.
func backfillImageTextOccurrencesTx(ctx context.Context, tx *sql.Tx) error {
	batch := &imageTextBatchTx{transactionQueries: tx, oldHashes: make(map[string]bool)}
	last := ""
	for {
		rows, err := tx.QueryContext(ctx, `SELECT session_id FROM (
			SELECT session_id FROM tool_calls WHERE result_content LIKE '%image%' AND session_id>?
			UNION SELECT session_id FROM tool_result_events WHERE content LIKE '%image%' AND session_id>?
		) ORDER BY session_id LIMIT 100`, last, last)
		if err != nil {
			return fmt.Errorf("finding retained images: %w", err)
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
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
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := refreshImageTextOccurrencesTx(batch, id); err != nil {
				return fmt.Errorf("projecting retained images: %w", err)
			}
		}
		if len(ids) < 100 {
			return batch.pruneImageText()
		}
		last = ids[len(ids)-1]
	}
}

// refreshImageTextOccurrencesTx records image locations after the final tool-result projection.
func refreshImageTextOccurrencesTx(tx transactionQueries, sessionID string) error {
	oldHashes, err := sessionImageTextHashes(tx, sessionID)
	if err != nil {
		return err
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
	if batch, ok := tx.(*imageTextBatchTx); ok {
		for hash := range oldHashes {
			batch.oldHashes[hash] = true
		}
		return nil
	}
	return pruneImageTextHashes(tx, oldHashes)
}

func pruneImageTextHashes(tx transactionQueries, hashes map[string]bool) error {
	for hash := range hashes {
		if _, err := tx.Exec(`DELETE FROM image_text_cache WHERE image_hash=? AND NOT EXISTS (SELECT 1 FROM image_text_occurrences o WHERE o.image_hash=?)`, hash, hash); err != nil {
			return err
		}
	}
	return nil
}

func sessionImageTextHashes(tx transactionQueries, sessionID string) (map[string]bool, error) {
	rows, err := tx.Query(`SELECT DISTINCT image_hash FROM image_text_occurrences WHERE session_id=?`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	hashes := make(map[string]bool)
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, err
		}
		hashes[hash] = true
	}
	return hashes, rows.Err()
}

func sessionImageTextOccurrences(tx transactionQueries, sessionID string) ([]ImageTextOccurrence, error) {
	rows, err := tx.Query(`SELECT tc.id, 0, COALESCE(tc.result_content, '') FROM tool_calls tc
		WHERE tc.session_id=? AND tc.result_content LIKE '%image%' AND NOT EXISTS (SELECT 1 FROM tool_result_events e WHERE e.session_id=tc.session_id AND e.tool_use_id=tc.tool_use_id AND tc.tool_use_id<>'')
		UNION ALL SELECT 0, e.id, e.content FROM tool_result_events e WHERE e.session_id=? AND e.content LIKE '%image%'`, sessionID, sessionID)
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
