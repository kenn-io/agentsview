package db

import (
	"container/list"
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"sync"
)

type imageTextMemoEntry struct {
	key    [sha256.Size]byte
	images []RetainedImage
	bytes  int
}

type imageTextMemo struct {
	mu                              sync.Mutex
	entries                         map[[sha256.Size]byte]*list.Element
	order                           list.List
	imageCount, bytes               int
	maxEntries, maxImages, maxBytes int
}

// Refreshes revisit unchanged results. Keep parsed image metadata, never the
// potentially large source content, and bound both records and string bytes.
var retainedImageMemo = imageTextMemo{maxEntries: 1024, maxImages: 4096, maxBytes: 1 << 20}

// imageTextBatchTx defers pruning until every changed session has refreshed
// its occurrences, so a shared image can move between sessions in one write.
type imageTextBatchTx struct {
	transactionQueries
	oldHashes map[string]bool
}

func (tx *imageTextBatchTx) pruneImageText() error {
	return pruneImageTextHashes(tx.transactionQueries, tx.oldHashes)
}

func (m *imageTextMemo) images(content string) []RetainedImage {
	key := sha256.Sum256([]byte(content))
	m.mu.Lock()
	if entry := m.entries[key]; entry != nil {
		m.order.MoveToFront(entry)
		images := entry.Value.(imageTextMemoEntry).images
		m.mu.Unlock()
		return images
	}
	m.mu.Unlock()

	images := RetainedToolResultImages(content)
	if len(images) == 0 || len(images) > m.maxImages {
		return images
	}
	bytes := 0
	for _, image := range images {
		bytes += len(image.MediaType) + len(image.SHA256)
	}
	if bytes > m.maxBytes {
		return images
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if entry := m.entries[key]; entry != nil {
		m.order.MoveToFront(entry)
		return entry.Value.(imageTextMemoEntry).images
	}
	if m.entries == nil {
		m.entries = make(map[[sha256.Size]byte]*list.Element)
	}
	for m.order.Len() >= m.maxEntries || m.imageCount+len(images) > m.maxImages || m.bytes+bytes > m.maxBytes {
		last := m.order.Back()
		if last == nil {
			return images
		}
		old := last.Value.(imageTextMemoEntry)
		delete(m.entries, old.key)
		m.order.Remove(last)
		m.imageCount -= len(old.images)
		m.bytes -= old.bytes
	}
	m.entries[key] = m.order.PushFront(imageTextMemoEntry{key: key, images: images, bytes: bytes})
	m.imageCount += len(images)
	m.bytes += bytes
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
