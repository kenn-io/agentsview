package db

// refreshImageTextOccurrencesTx records image locations after the final tool-result projection.
func refreshImageTextOccurrencesTx(tx transactionQueries, sessionID string) error {
	oldRows, err := tx.Query(`SELECT COALESCE(call_id,0),COALESCE(event_id,0),image_index,image_hash FROM image_text_occurrences WHERE session_id=?`, sessionID)
	if err != nil {
		return err
	}
	var oldHashes []string
	oldOccurrences := make(map[ImageTextOccurrence]bool)
	for oldRows.Next() {
		var old ImageTextOccurrence
		if err := oldRows.Scan(&old.CallID, &old.EventID, &old.Index, &old.Hash); err != nil {
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
		for _, image := range RetainedToolResultImages(row.Content) {
			if image.SHA256 != "" {
				occurrences = append(occurrences, ImageTextOccurrence{CallID: row.CallID, EventID: row.EventID, Index: image.Index, Hash: image.SHA256})
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
		if _, err := tx.Exec(`INSERT INTO image_text_occurrences(session_id,call_id,event_id,image_index,image_hash) VALUES(?,?,?,?,?)`, sessionID, call, event, o.Index, o.Hash); err != nil {
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
