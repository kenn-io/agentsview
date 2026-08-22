package db

import (
	"context"
	"fmt"
)

// SecretFinding holds one redacted secret detection persisted per session.
// Natural coordinates (session_id + ordinal + match_index) are used so
// findings survive a full-resync orphan copy without needing row IDs.
type SecretFinding struct {
	SessionID      string `json:"session_id"`
	RuleName       string `json:"rule_name"`
	Confidence     string `json:"confidence"`
	LocationKind   string `json:"location_kind"` // message|tool_input|tool_result|tool_result_event
	MessageOrdinal int    `json:"message_ordinal"`
	CallIndex      *int   `json:"call_index,omitempty"`
	EventIndex     *int   `json:"event_index,omitempty"`
	MatchStart     int    `json:"match_start"`
	MatchEnd       int    `json:"match_end"`
	MatchIndex     int    `json:"match_index"`
	RedactedMatch  string `json:"redacted_match"`
	RulesVersion   string `json:"rules_version"`
}

// ReplaceSessionSecretFindings atomically replaces all secret findings for a
// session and updates the summary columns on the sessions row.
func (db *DB) ReplaceSessionSecretFindings(
	sessionID string, findings []SecretFinding, leakCount int, rulesVersion string,
) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.getWriter().Begin()
	if err != nil {
		return fmt.Errorf("beginning tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := replaceSecretFindingsTx(tx, sessionID, findings, leakCount, rulesVersion); err != nil {
		return err
	}
	return tx.Commit()
}

// replaceSecretFindingsTx deletes all existing findings for the session,
// inserts the new set, and updates the sessions summary columns. Caller owns
// the lock and transaction lifecycle.
func replaceSecretFindingsTx(
	tx transactionQueries,
	sessionID string,
	findings []SecretFinding,
	leakCount int,
	rulesVersion string,
) error {
	if _, err := tx.Exec(
		"DELETE FROM secret_findings WHERE session_id = ?",
		sessionID,
	); err != nil {
		return fmt.Errorf("deleting secret findings for %s: %w", sessionID, err)
	}

	for i := range findings {
		f := &findings[i]
		if _, err := tx.Exec(`
			INSERT INTO secret_findings (
				session_id, rule_name, confidence,
				location_kind, message_ordinal, call_index, event_index,
				match_start, match_end, match_index,
				redacted_match, rules_version
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sessionID, f.RuleName, f.Confidence,
			f.LocationKind, f.MessageOrdinal, f.CallIndex, f.EventIndex,
			f.MatchStart, f.MatchEnd, f.MatchIndex,
			f.RedactedMatch, rulesVersion,
		); err != nil {
			return fmt.Errorf("inserting secret finding: %w", err)
		}
	}

	if _, err := tx.Exec(`
		UPDATE sessions
		SET secret_leak_count = ?,
		    secrets_rules_version = ?,
		    local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE id = ?`,
		leakCount, rulesVersion, sessionID,
	); err != nil {
		return fmt.Errorf("updating session secret columns %s: %w", sessionID, err)
	}
	return nil
}

// SecretFindingSource returns the full source text a finding was detected in,
// reconstructed via GetAllMessages so the stored MatchStart/MatchEnd offsets
// remain valid. ok is false when the coordinates no longer resolve (the source
// was changed or removed), which --reveal treats as "source changed".
func (db *DB) SecretFindingSource(
	ctx context.Context, f SecretFinding,
) (string, bool, error) {
	msgs, err := db.GetAllMessages(ctx, f.SessionID)
	if err != nil {
		return "", false, err
	}
	text, ok := FindingSourceFromMessages(msgs, f)
	return text, ok, nil
}

// FindingSourceFromMessages returns the source text that finding f points at,
// reconstructed from msgs exactly as the scanner saw it. ok is false when the
// coordinates no longer resolve. Shared by the SQLite and PostgreSQL stores so
// the --reveal re-validation path stays identical.
func FindingSourceFromMessages(msgs []Message, f SecretFinding) (string, bool) {
	msg := findMessageByOrdinal(msgs, f.MessageOrdinal)
	if msg == nil {
		return "", false
	}
	if f.LocationKind == "message" {
		return msg.Content, true
	}
	text, ok := toolCallSource(msg.ToolCalls, f)
	return text, ok
}

func findMessageByOrdinal(msgs []Message, ordinal int) *Message {
	for i := range msgs {
		if msgs[i].Ordinal == ordinal {
			return &msgs[i]
		}
	}
	return nil
}

func toolCallAt(calls []ToolCall, idx *int) (ToolCall, bool) {
	if idx == nil || *idx < 0 || *idx >= len(calls) {
		return ToolCall{}, false
	}
	return calls[*idx], true
}

func toolCallSource(calls []ToolCall, f SecretFinding) (string, bool) {
	tc, ok := toolCallAt(calls, f.CallIndex)
	if !ok {
		return "", false
	}
	switch f.LocationKind {
	case "tool_input":
		return tc.InputJSON, true
	case "tool_result":
		if len(tc.ResultEvents) > 0 {
			return "", false
		}
		return tc.ResultContent, true
	case "tool_result_event":
		return resultEventContent(tc.ResultEvents, f.EventIndex)
	}
	return "", false
}

func resultEventContent(events []ToolResultEvent, idx *int) (string, bool) {
	if idx == nil {
		return "", false
	}
	for _, ev := range events {
		if ev.EventIndex == *idx {
			return ev.Content, true
		}
	}
	return "", false
}

// SessionSecretFindings returns all secret findings for a session ordered by
// natural position (ordinal, start offset, match index).
func (db *DB) SessionSecretFindings(
	ctx context.Context, sessionID string,
) ([]SecretFinding, error) {
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT session_id, rule_name, confidence,
		       location_kind, message_ordinal, call_index, event_index,
		       match_start, match_end, match_index,
		       redacted_match, rules_version
		FROM secret_findings
		WHERE session_id = ?
		ORDER BY message_ordinal, match_start, match_index`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying secret findings for %s: %w", sessionID, err)
	}
	defer rows.Close()

	out := make([]SecretFinding, 0, 8)
	for rows.Next() {
		var f SecretFinding
		if err := rows.Scan(
			&f.SessionID, &f.RuleName, &f.Confidence,
			&f.LocationKind, &f.MessageOrdinal, &f.CallIndex, &f.EventIndex,
			&f.MatchStart, &f.MatchEnd, &f.MatchIndex,
			&f.RedactedMatch, &f.RulesVersion,
		); err != nil {
			return nil, fmt.Errorf("scanning secret finding: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
