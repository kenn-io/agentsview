package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

const (
	frictionMaxRows  = 2000
	frictionMaxBytes = 4 << 20
)

// FrictionInputFits applies the friction review budget to loaded rows:
// 2,000 message, tool-call and result-event rows and 4 MiB of their text.
// Every friction path checks it, so a session is reviewed or settled empty
// the same way whether sync, backfill or hosted ingest sees it.
func FrictionInputFits(msgs []Message) bool {
	rows, bytes := 0, 0
	for _, m := range msgs {
		rows++
		bytes += len(m.Content) + len(m.ThinkingText)
		for _, tc := range m.ToolCalls {
			rows++
			// Count the summary as storage keeps it, so a restored copy of a
			// sole result event is not counted twice.
			bytes += len(tc.InputJSON) + len(DedupToolCallResultSummary(tc.ResultContent, tc.ResultEvents))
			for _, ev := range tc.ResultEvents {
				rows++
				bytes += len(ev.Content)
			}
		}
		if rows > frictionMaxRows || bytes > frictionMaxBytes {
			return false
		}
	}
	return true
}

// GetFrictionMessages loads a complete transcript only when it fits the
// friction budget. Oversized sessions return ok=false without loading text.
func (db *DB) GetFrictionMessages(ctx context.Context, sessionID string) (msgs []Message, ok bool, err error) {
	tx, err := db.getReader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback() }()
	// Check and hydrate the same snapshot so concurrent appends cannot exceed the budget.
	ok, err = frictionInputWithinBudget(ctx, tx, sessionID)
	if err != nil || !ok {
		return nil, false, err
	}
	db.messagesLoadCount.Add(1)
	msgs, err = allMessagesWithQuerier(ctx, tx, sessionID)
	if err != nil {
		return nil, false, err
	}
	return msgs, true, nil
}

// frictionInputWithinBudget is FrictionInputFits measured in SQL, so an
// oversized transcript is rejected without loading its text.
func frictionInputWithinBudget(ctx context.Context, tx *sql.Tx, sessionID string) (bool, error) {
	remainingRows, remainingBytes := frictionMaxRows, frictionMaxBytes
	for _, table := range []struct{ name, columns string }{
		{"messages", "content thinking_text"},
		{"tool_calls", "input_json result_content"},
		{"tool_result_events", "content"},
	} {
		// octet_length reads SQLite record metadata without loading TEXT payloads.
		var lengths []string
		for column := range strings.FieldsSeq(table.columns) {
			lengths = append(lengths, "COALESCE(octet_length("+column+"), 0)")
		}
		var rows, bytes int
		query := "SELECT count(*), COALESCE(sum(n), 0) FROM (SELECT " + strings.Join(lengths, " + ") + " AS n FROM " + table.name + " WHERE session_id = ? LIMIT ?)"
		if err := tx.QueryRowContext(ctx, query, sessionID, remainingRows+1).Scan(&rows, &bytes); err != nil {
			return false, fmt.Errorf("checking friction input size: %w", err)
		}
		remainingRows -= rows
		remainingBytes -= bytes
		if remainingRows < 0 || remainingBytes < 0 {
			return false, nil
		}
	}
	return true, nil
}
