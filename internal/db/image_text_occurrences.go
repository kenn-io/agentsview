package db

// refreshImageTextOccurrencesTx records the current representative of each
// image after all tool-result projections for the session have been written.
func refreshImageTextOccurrencesTx(tx transactionQueries, sessionID string) error {
	rows, err := tx.Query(`SELECT tc.id, 0, COALESCE(tc.result_content, '') FROM tool_calls tc
		WHERE tc.session_id=? AND NOT EXISTS (SELECT 1 FROM tool_result_events e WHERE e.session_id=tc.session_id AND e.tool_use_id=tc.tool_use_id AND tc.tool_use_id<>'')
		UNION ALL SELECT 0, e.id, e.content FROM tool_result_events e WHERE e.session_id=?`, sessionID, sessionID)
	if err != nil {
		return err
	}
	var occurrences []ImageTextOccurrence
	seen := make(map[string]bool)
	for rows.Next() {
		var row ImageTextRow
		if err := rows.Scan(&row.CallID, &row.EventID, &row.Content); err != nil {
			rows.Close()
			return err
		}
		for _, image := range RetainedToolResultImages(row.Content) {
			if image.SHA256 != "" && !seen[image.SHA256] {
				seen[image.SHA256] = true
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
	}
	_, err = tx.Exec(`DELETE FROM image_text_cache WHERE NOT EXISTS (SELECT 1 FROM image_text_occurrences o WHERE o.image_hash=image_text_cache.image_hash)`)
	return err
}
