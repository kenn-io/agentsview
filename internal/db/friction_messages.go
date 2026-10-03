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

// GetFrictionMessages loads a complete transcript only when it fits the
// friction recompute budget: 2,000 message/call/event rows and 4 MiB of text.
// Oversized sessions return ok=false; callers must leave their findings stale.
func (db *DB) GetFrictionMessages(ctx context.Context, sessionID string) (msgs []Message, ok bool, err error) {
	tx, err := db.getReader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback() }()
	// ponytail: bounded full recompute; larger sessions need incremental detector state.
	// Check and hydrate the same snapshot so concurrent appends cannot exceed the budget.
	ok, err = frictionInputWithinBudget(ctx, tx, sessionID)
	if err != nil || !ok {
		return nil, false, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT "+selectMessageCols+" FROM messages WHERE session_id = ? ORDER BY ordinal", sessionID)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	msgs, err = scanMessages(rows)
	if err != nil {
		return nil, false, err
	}
	if err := attachToolCallsWithQuerier(ctx, tx, msgs); err != nil {
		return nil, false, err
	}
	return msgs, true, nil
}

// FrictionInputWithinBudget checks a transcript's size without loading its text.
// A different revision returns false so the caller cannot apply a smaller
// replacement's budget to an older, larger transcript it already loaded.
func (db *DB) FrictionInputWithinBudget(ctx context.Context, sessionID, revision string) (bool, error) {
	tx, err := db.getReader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var matches bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM sessions WHERE id = ? AND transcript_revision = ?)`, sessionID, revision,
	).Scan(&matches); err != nil {
		return false, err
	}
	if !matches {
		return false, nil
	}
	return frictionInputWithinBudget(ctx, tx, sessionID)
}

func frictionInputWithinBudget(ctx context.Context, tx *sql.Tx, sessionID string) (bool, error) {
	remainingRows, remainingBytes := frictionMaxRows, frictionMaxBytes
	for _, table := range []struct{ name, columns string }{
		{"messages", "session_id role content thinking_text timestamp model reasoning_effort token_usage provider_id claude_message_id claude_request_id source_type source_subtype prompt_source source_uuid source_parent_uuid"},
		{"tool_calls", "session_id tool_name category tool_use_id input_json skill_name result_content subagent_session_id file_path"},
		{"tool_result_events", "session_id tool_use_id agent_id subagent_session_id source status content timestamp raw_content_digest"},
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
