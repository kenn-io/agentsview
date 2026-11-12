package db

import (
	"context"
	"fmt"
)

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
	ids, err := scanStrings(rows)
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
	return refreshImageTextOccurrencesWhereTx(tx, sessionID, "", "")
}

func refreshMessageDiffImageTextTx(tx transactionQueries, sessionID string, plan messageDiffPlan, useDiff bool) error {
	if !useDiff {
		return refreshImageTextOccurrencesTx(tx, sessionID)
	}
	msgs := append([]Message(nil), plan.inserts...)
	for _, update := range plan.updates {
		msgs = append(msgs, update.msg)
	}
	return refreshIncrementalImageTextOccurrencesTx(tx, sessionID, msgs, nil)
}

// refreshIncrementalImageTextOccurrencesTx refreshes only changed tool results.
func refreshIncrementalImageTextOccurrencesTx(tx transactionQueries, sessionID string, msgs []Message, updated []string) error {
	// No matching rows: remove occurrences whose source row disappeared.
	if err := refreshImageTextOccurrencesWhereTx(tx, sessionID, " AND 0", " AND 0"); err != nil {
		return err
	}
	ids := make(map[string]bool)
	ordinals := make(map[int]bool)
	for _, msg := range msgs {
		for _, call := range msg.ToolCalls {
			if call.ToolUseID != "" {
				ids[call.ToolUseID] = true
			} else {
				ordinals[msg.Ordinal] = true
			}
			for _, event := range call.ResultEvents {
				ids[event.ToolUseID] = true
			}
		}
	}
	for _, id := range updated {
		ids[id] = true
	}
	delete(ids, "")
	for id := range ids {
		if err := refreshImageTextOccurrencesWhereTx(tx, sessionID, ` AND tc.tool_use_id=?`, ` AND e.tool_use_id=?`, id); err != nil {
			return err
		}
	}
	for ordinal := range ordinals {
		if err := refreshImageTextOccurrencesWhereTx(tx, sessionID, ` AND tc.message_id IN (SELECT id FROM messages WHERE session_id=tc.session_id AND ordinal=?)`, ` AND e.tool_call_message_ordinal=?`, ordinal); err != nil {
			return err
		}
	}
	return nil
}

// refreshImageTextOccurrencesWhereTx replaces the matching tool results' occurrences and drops those of removed results.
func refreshImageTextOccurrencesWhereTx(tx transactionQueries, sessionID, callFilter, eventFilter string, args ...any) error {
	var enabled int
	if err := tx.QueryRow(`SELECT count(*) FROM stats WHERE key=? AND value!=0`, imageTextBackfillKey).Scan(&enabled); err != nil {
		return err
	}
	if enabled == 0 {
		return nil
	}
	occurrences, err := imageTextOccurrencesQuery(tx, sessionID, callFilter, eventFilter, args)
	if err != nil {
		return err
	}
	deleteArgs := append(append([]any{sessionID, sessionID}, args...), sessionID)
	if _, err := tx.Exec(`DELETE FROM image_text_occurrences WHERE session_id=? AND ((call_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM tool_calls WHERE id=call_id)) OR (event_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM tool_result_events WHERE id=event_id))
		OR call_id IN (SELECT tc.id FROM tool_calls tc WHERE tc.session_id=? `+callFilter+`) OR event_id IN (SELECT e.id FROM tool_result_events e WHERE e.session_id=? `+eventFilter+`))`, append(deleteArgs, args...)...); err != nil {
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

func imageTextOccurrencesQuery(tx transactionQueries, sessionID, callFilter, eventFilter string, args []any) ([]ImageTextOccurrence, error) {
	queryArgs := append([]any{sessionID}, args...)
	queryArgs = append(queryArgs, sessionID)
	queryArgs = append(queryArgs, args...)
	rows, err := tx.Query(`SELECT tc.id, 0, COALESCE(tc.result_content, '') FROM tool_calls tc
		WHERE tc.session_id=? AND (tc.result_content LIKE '%image%' OR tc.result_content LIKE '%data:%') AND NOT EXISTS (SELECT 1 FROM tool_result_events e WHERE e.session_id=tc.session_id AND e.tool_use_id=tc.tool_use_id AND tc.tool_use_id<>'') `+callFilter+`
		UNION ALL SELECT 0, e.id, e.content FROM tool_result_events e WHERE e.session_id=? AND (e.content LIKE '%image%' OR e.content LIKE '%data:%') `+eventFilter, queryArgs...)
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
		for _, image := range RetainedToolResultImages(content) {
			if image.SHA256 != "" {
				occurrences = append(occurrences, ImageTextOccurrence{CallID: callID, EventID: eventID, Index: image.Index, Hash: image.SHA256})
			}
		}
	}
	return occurrences, rows.Err()
}

// CopyImageTextWithOccurrencesFrom carries the old image cache across a resync and enables occurrences only when the old archive had.
func (db *DB) CopyImageTextWithOccurrencesFrom(ctx context.Context, sourcePath string) error {
	enabled, err := db.CopyImageTextFrom(ctx, sourcePath)
	if err != nil || !enabled {
		return err
	}
	return db.BackfillImageTextOccurrences(ctx)
}
