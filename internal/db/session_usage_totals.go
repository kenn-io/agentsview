package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"strconv"
	"strings"

	"go.kenn.io/agentsview/internal/money"
)

// SessionUsageTotal is one session's row in session_usage_totals: a usage
// total the agent recorded for the whole session, such as Claude Code's
// cost-state. It overlaps the session's usage rows and, with SubagentScope
// "included", its subagents' rows, so it is never summed with usage. Reads
// only show it next to AgentsView's own totals.
type SessionUsageTotal struct {
	Source string `json:"source"`
	// SubagentScope is "included", "excluded", or "" when unknown; see the
	// parser.SubagentScope constants.
	SubagentScope string       `json:"subagent_scope,omitempty"`
	Cost          *money.Money `json:"cost,omitempty"`
	// Incomplete reports that the agent could not price every model.
	Incomplete bool                     `json:"incomplete,omitzero"`
	Models     []SessionUsageTotalModel `json:"models"`
}

// SessionUsageTotalModel is one model's share of a SessionUsageTotal.
// OutputTokens includes ReasoningTokens.
type SessionUsageTotalModel struct {
	Model                    string       `json:"model"`
	InputTokens              int64        `json:"input_tokens"`
	OutputTokens             int64        `json:"output_tokens"`
	ReasoningTokens          int64        `json:"reasoning_tokens,omitzero"`
	CacheCreationInputTokens int64        `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64        `json:"cache_read_input_tokens"`
	WebSearchRequests        int64        `json:"web_search_requests,omitzero"`
	Cost                     *money.Money `json:"cost,omitempty"`
}

// SessionUsageTotalColumns lists session_usage_totals' value columns, after
// session_id, in the order SessionUsageTotalValues and ScanSessionUsageTotal
// use. Every backend's copy of the table has these columns.
const SessionUsageTotalColumns = `source, subagent_scope, cost_microdollars,
	incomplete, models_json`

// SessionUsageTotalValues returns t's values in SessionUsageTotalColumns
// order. An absent cost is nil so every backend stores NULL.
func SessionUsageTotalValues(t *SessionUsageTotal) ([]any, error) {
	models := t.Models
	if models == nil {
		models = []SessionUsageTotalModel{}
	}
	modelsJSON, err := json.Marshal(models, json.Deterministic(true))
	if err != nil {
		return nil, fmt.Errorf("encoding session total models: %w", err)
	}
	var cost any
	if t.Cost != nil {
		cost = t.Cost.Microdollars
	}
	return []any{t.Source, t.SubagentScope, cost, t.Incomplete, string(modelsJSON)}, nil
}

// ScanSessionUsageTotal reads one row selected with SessionUsageTotalColumns
// through scan, such as a sql.Rows Scan method.
func ScanSessionUsageTotal(scan func(dest ...any) error) (*SessionUsageTotal, error) {
	var t SessionUsageTotal
	var cost sql.NullInt64
	var modelsJSON string
	if err := scan(&t.Source, &t.SubagentScope, &cost, &t.Incomplete, &modelsJSON); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(modelsJSON), &t.Models); err != nil {
		return nil, fmt.Errorf("decoding session total models: %w", err)
	}
	if t.Models == nil {
		t.Models = []SessionUsageTotalModel{}
	}
	if cost.Valid {
		t.Cost = &money.Money{Microdollars: cost.Int64}
	}
	return &t, nil
}

// SessionUsageTotalFingerprint renders t as one exact string, "" for nil.
// Push targets compare it to detect a changed total.
func SessionUsageTotalFingerprint(t *SessionUsageTotal) (string, error) {
	if t == nil {
		return "", nil
	}
	values, err := SessionUsageTotalValues(t)
	if err != nil {
		return "", err
	}
	cost := "-"
	if t.Cost != nil {
		cost = strconv.FormatInt(t.Cost.Microdollars, 10)
	}
	return strings.Join([]string{
		strconv.Quote(t.Source), strconv.Quote(t.SubagentScope), cost,
		strconv.FormatBool(t.Incomplete), values[4].(string),
	}, "|"), nil
}

// ReplaceSessionUsageTotal replaces one session's recorded total. A nil t
// removes it.
func (db *DB) ReplaceSessionUsageTotal(
	ctx context.Context, sessionID string, t *SessionUsageTotal,
) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.getWriter().Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning session total tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := replaceSessionUsageTotalTx(tx, sessionID, t, true); err != nil {
		return err
	}
	return tx.Commit()
}

// replaceSessionUsageTotalTx writes or removes the row and, when either
// changed the table, bumps local_modified_at so push targets re-select the
// session, as a plan usage replace does.
func replaceSessionUsageTotalTx(
	tx transactionQueries, sessionID string, t *SessionUsageTotal,
	enqueueArtifact bool,
) error {
	if t == nil {
		res, err := tx.Exec(
			`DELETE FROM session_usage_totals WHERE session_id = ?`, sessionID,
		)
		if err != nil {
			return fmt.Errorf("deleting session total for %s: %w", sessionID, err)
		}
		removed, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("counting removed session total for %s: %w", sessionID, err)
		}
		if removed == 0 {
			return nil
		}
		return planUsageChangedTx(tx, sessionID, enqueueArtifact)
	}
	values, err := SessionUsageTotalValues(t)
	if err != nil {
		return fmt.Errorf("writing session total for %s: %w", sessionID, err)
	}
	if _, err := tx.Exec(`
		INSERT OR REPLACE INTO session_usage_totals (session_id, `+SessionUsageTotalColumns+`)
		VALUES (?, ?, ?, ?, ?, ?)`,
		append([]any{sessionID}, values...)...,
	); err != nil {
		return fmt.Errorf("writing session total for %s: %w", sessionID, err)
	}
	return planUsageChangedTx(tx, sessionID, enqueueArtifact)
}

// GetSessionUsageTotal returns one session's recorded total, or nil when
// the session has none.
func (db *DB) GetSessionUsageTotal(
	ctx context.Context, sessionID string,
) (*SessionUsageTotal, error) {
	return sessionUsageTotalWithQuerier(ctx, db.getReader(), sessionID)
}

func sessionUsageTotalWithQuerier(
	ctx context.Context, q messageRowsQuerier, sessionID string,
) (*SessionUsageTotal, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT `+SessionUsageTotalColumns+` FROM session_usage_totals WHERE session_id = ?`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying session total for %s: %w", sessionID, err)
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	t, err := ScanSessionUsageTotal(rows.Scan)
	if err != nil {
		return nil, fmt.Errorf("scanning session total for %s: %w", sessionID, err)
	}
	return t, rows.Err()
}

// SessionUsageTotalFingerprints returns each session's recorded total as one
// exact string, "" for a session without one. Push targets compare it as
// they do plan usage fingerprints.
func (db *DB) SessionUsageTotalFingerprints(
	ctx context.Context, sessionIDs []string,
) (map[string]string, error) {
	out := make(map[string]string, len(sessionIDs))
	for _, id := range sessionIDs {
		out[id] = ""
	}
	const batchSize = 900
	for start := 0; start < len(sessionIDs); start += batchSize {
		batch := sessionIDs[start:min(start+batchSize, len(sessionIDs))]
		if err := db.appendSessionUsageTotalFingerprints(ctx, out, batch); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (db *DB) appendSessionUsageTotalFingerprints(
	ctx context.Context, out map[string]string, sessionIDs []string,
) error {
	args := make([]any, len(sessionIDs))
	for i, id := range sessionIDs {
		args[i] = id
	}
	rows, err := db.getReader().QueryContext(ctx,
		`SELECT session_id, `+SessionUsageTotalColumns+` FROM session_usage_totals
		WHERE session_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")+`)`,
		args...,
	)
	if err != nil {
		return fmt.Errorf("querying session total fingerprints: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sessionID string
		t, err := ScanSessionUsageTotal(scanAfter(rows.Scan, &sessionID))
		if err != nil {
			return fmt.Errorf("scanning session total fingerprint: %w", err)
		}
		if out[sessionID], err = SessionUsageTotalFingerprint(t); err != nil {
			return err
		}
	}
	return rows.Err()
}

// scanAfter adapts scan to read leading into dest before the columns the
// returned function is asked for.
func scanAfter(scan func(dest ...any) error, leading ...any) func(dest ...any) error {
	return func(dest ...any) error {
		return scan(append(append([]any{}, leading...), dest...)...)
	}
}
