package db

import (
	"container/list"
	"crypto/sha256"
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
var retainedImageMemo = imageTextMemo{maxEntries: 256, maxImages: 4096, maxBytes: 1 << 20}

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

// refreshImageTextOccurrencesTx records image locations after the final tool-result projection.
func refreshImageTextOccurrencesTx(tx transactionQueries, sessionID string) error {
	oldRows, err := tx.Query(`SELECT COALESCE(call_id,0),COALESCE(event_id,0),image_index,image_hash,media_type FROM image_text_occurrences WHERE session_id=?`, sessionID)
	if err != nil {
		return err
	}
	var oldHashes []string
	oldOccurrences := make(map[ImageTextOccurrence]bool)
	for oldRows.Next() {
		var old ImageTextOccurrence
		if err := oldRows.Scan(&old.CallID, &old.EventID, &old.Index, &old.Hash, &old.MediaType); err != nil {
			oldRows.Close()
			return err
		}
		oldHashes = append(oldHashes, old.Hash)
		oldOccurrences[old] = true
	}
	if err := oldRows.Err(); err != nil {
		oldRows.Close()
		return err
	}
	if err := oldRows.Close(); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT tc.id, 0, COALESCE(tc.result_content, '') FROM tool_calls tc
		WHERE tc.session_id=? AND tc.result_content LIKE '%image%' AND NOT EXISTS (SELECT 1 FROM tool_result_events e WHERE e.session_id=tc.session_id AND e.tool_use_id=tc.tool_use_id AND tc.tool_use_id<>'')
		UNION ALL SELECT 0, e.id, e.content FROM tool_result_events e WHERE e.session_id=? AND e.content LIKE '%image%'`, sessionID, sessionID)
	if err != nil {
		return err
	}
	var occurrences []ImageTextOccurrence
	for rows.Next() {
		var row ImageTextRow
		if err := rows.Scan(&row.CallID, &row.EventID, &row.Content); err != nil {
			rows.Close()
			return err
		}
		for _, image := range retainedImageMemo.images(row.Content) {
			if image.SHA256 != "" {
				occurrences = append(occurrences, ImageTextOccurrence{CallID: row.CallID, EventID: row.EventID, Index: image.Index, Hash: image.SHA256, MediaType: image.MediaType})
			}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
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
		if _, err := tx.Exec(`INSERT INTO image_text_occurrences(session_id,call_id,event_id,image_index,image_hash,media_type) VALUES(?,?,?,?,?,?)`, sessionID, call, event, o.Index, o.Hash, o.MediaType); err != nil {
			return err
		}
		if !oldOccurrences[o] {
			if _, err := tx.Exec(`DELETE FROM image_text_cache WHERE image_hash=? AND succeeded=0 AND retry_at='9999-12-31T23:59:59Z'`, o.Hash); err != nil {
				return err
			}
		}
	}
	for _, hash := range oldHashes {
		if _, err = tx.Exec(`DELETE FROM image_text_cache WHERE image_hash=? AND NOT EXISTS (SELECT 1 FROM image_text_occurrences o WHERE o.image_hash=?)`, hash, hash); err != nil {
			return err
		}
	}
	return nil
}
