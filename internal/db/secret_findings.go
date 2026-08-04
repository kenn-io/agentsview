package db

import (
	"context"
	"database/sql"
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

// SecretFindingFilter narrows a findings listing. Empty fields do not filter.
type SecretFindingFilter struct {
	Project    string
	Agent      string
	DateFrom   string
	DateTo     string
	Rule       string
	Confidence string // definite | candidate | "" (all)
	// RulesVersions, when non-empty, limits rows to findings produced by one
	// of the currently accepted scanner versions. This lets service callers
	// hide stale findings after rule/fixture-deny changes before a backfill has
	// rewritten old rows.
	RulesVersions []string
	Limit         int
	Cursor        int
}

// SecretFindingRow is a finding enriched with its session's project and agent.
type SecretFindingRow struct {
	SecretFinding
	Project string `json:"project"`
	Agent   string `json:"agent"`
}

// SecretFindingPage is one offset-paginated page of findings.
type SecretFindingPage struct {
	Findings   []SecretFindingRow `json:"findings"`
	NextCursor int                `json:"next_cursor,omitempty"`
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
	tx *sql.Tx,
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

	return updateSessionSecretSummaryTx(tx, sessionID, leakCount, rulesVersion)
}

func updateSessionSecretSummaryTx(
	tx *sql.Tx, sessionID string, leakCount int, rulesVersion string,
) error {
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

// SessionSecretFindings returns all secret findings for a session ordered by
// natural position (ordinal, start offset, match index).
func (db *DB) SessionSecretFindings(
	ctx context.Context, sessionID string,
) ([]SecretFinding, error) {
	return sessionSecretFindingsWithQuerier(ctx, db.getReader(), sessionID)
}

func sessionSecretFindingsWithQuerier(
	ctx context.Context, q messageRowsQuerier, sessionID string,
) ([]SecretFinding, error) {
	rows, err := q.QueryContext(ctx, `
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
