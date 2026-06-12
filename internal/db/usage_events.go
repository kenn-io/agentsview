package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// UsageEvent records token and cost accounting that does not belong
// to a single message row. Hermes session-level usage is the first
// source, but the shape intentionally mirrors usage query totals.
type UsageEvent struct {
	ID                       int64
	SessionID                string
	MessageOrdinal           *int
	Source                   string
	Model                    string
	InputTokens              int
	OutputTokens             int
	CacheCreationInputTokens int
	CacheReadInputTokens     int
	ReasoningTokens          int
	CostUSD                  *float64
	CostStatus               string
	CostSource               string
	OccurredAt               string
	DedupKey                 string
}

func (db *DB) ensureUsageEventsSchemaLocked(w *sql.DB) error {
	if _, err := w.Exec(`
		CREATE TABLE IF NOT EXISTS usage_events (
			id INTEGER PRIMARY KEY,
			session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
			message_ordinal INTEGER,
			source TEXT NOT NULL,
			model TEXT NOT NULL,
			input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0,
			cache_read_input_tokens INTEGER NOT NULL DEFAULT 0,
			reasoning_tokens INTEGER NOT NULL DEFAULT 0,
			cost_usd REAL,
			cost_status TEXT NOT NULL DEFAULT '',
			cost_source TEXT NOT NULL DEFAULT '',
			occurred_at TEXT,
			dedup_key TEXT NOT NULL DEFAULT ''
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_usage_events_dedup
			ON usage_events(session_id, source, dedup_key)
			WHERE dedup_key != '';
		CREATE INDEX IF NOT EXISTS idx_usage_events_session
			ON usage_events(session_id);
		CREATE INDEX IF NOT EXISTS idx_usage_events_occurred
			ON usage_events(occurred_at);
	`); err != nil {
		return fmt.Errorf("creating usage_events: %w", err)
	}
	return nil
}

// ReplaceSessionUsageEvents replaces all usage events for one session
// in a single transaction.
func (db *DB) ReplaceSessionUsageEvents(
	sessionID string, events []UsageEvent,
) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.getWriter().Begin()
	if err != nil {
		return fmt.Errorf("beginning usage events tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := replaceSessionUsageEventsTx(tx, sessionID, events); err != nil {
		return err
	}

	return tx.Commit()
}

func replaceSessionUsageEventsTx(
	tx *sql.Tx, sessionID string, events []UsageEvent,
) error {
	if _, err := tx.Exec(
		`DELETE FROM usage_events WHERE session_id = ?`,
		sessionID,
	); err != nil {
		return fmt.Errorf("deleting usage events: %w", err)
	}

	for _, ev := range events {
		if ev.SessionID == "" {
			ev.SessionID = sessionID
		}
		if ev.SessionID != sessionID {
			return fmt.Errorf(
				"usage event session_id %q does not match %q",
				ev.SessionID, sessionID,
			)
		}
		var ordinal any
		if ev.MessageOrdinal != nil {
			ordinal = *ev.MessageOrdinal
		}
		var cost any
		if ev.CostUSD != nil {
			cost = *ev.CostUSD
		}
		var occurredAt any
		if ev.OccurredAt != "" {
			occurredAt = ev.OccurredAt
		}
		if _, err := tx.Exec(`
			INSERT INTO usage_events (
				session_id, message_ordinal, source, model,
				input_tokens, output_tokens,
				cache_creation_input_tokens, cache_read_input_tokens,
				reasoning_tokens, cost_usd, cost_status, cost_source,
				occurred_at, dedup_key
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			ev.SessionID, ordinal, ev.Source, ev.Model,
			ev.InputTokens, ev.OutputTokens,
			ev.CacheCreationInputTokens, ev.CacheReadInputTokens,
			ev.ReasoningTokens, cost, ev.CostStatus, ev.CostSource,
			occurredAt, ev.DedupKey,
		); err != nil {
			return fmt.Errorf("inserting usage event: %w", err)
		}
	}
	return nil
}

// GetUsageEvents returns usage events for one session in stable order.
// UsageEventFingerprints returns exact ordered fingerprints of
// stored usage events keyed by session ID. Used by PG push fast-paths
// to detect usage-only changes without N+1 queries.
func (db *DB) UsageEventFingerprints(
	sessionIDs []string,
) (map[string]string, error) {
	out := make(map[string]string, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return out, nil
	}
	for _, id := range sessionIDs {
		out[id] = ""
	}

	const batchSize = 900
	for start := 0; start < len(sessionIDs); start += batchSize {
		end := min(start+batchSize, len(sessionIDs))
		if err := db.appendUsageEventFingerprints(
			out, sessionIDs[start:end],
		); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (db *DB) appendUsageEventFingerprints(
	out map[string]string, sessionIDs []string,
) error {
	placeholders := make([]string, len(sessionIDs))
	args := make([]any, len(sessionIDs))
	for i, id := range sessionIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	rows, err := db.getReader().Query(`
		SELECT session_id, message_ordinal, source, model,
			input_tokens, output_tokens,
			cache_creation_input_tokens, cache_read_input_tokens,
			reasoning_tokens, cost_usd, cost_status, cost_source,
			occurred_at, dedup_key
		FROM usage_events
		WHERE session_id IN (`+strings.Join(placeholders, ",")+`)
		ORDER BY session_id, COALESCE(occurred_at, ''), id`,
		args...,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	builders := make(map[string]*strings.Builder)
	for rows.Next() {
		var sessionID string
		var ordinal sql.NullInt64
		var source, model, costStatus, costSource string
		var inputTokens, outputTokens int
		var cacheCreationInputTokens, cacheReadInputTokens int
		var reasoningTokens int
		var cost sql.NullFloat64
		var occurredAt, dedupKey sql.NullString
		if err := rows.Scan(
			&sessionID, &ordinal, &source, &model,
			&inputTokens, &outputTokens,
			&cacheCreationInputTokens, &cacheReadInputTokens,
			&reasoningTokens, &cost, &costStatus, &costSource,
			&occurredAt, &dedupKey,
		); err != nil {
			return err
		}
		b := builders[sessionID]
		if b == nil {
			b = &strings.Builder{}
			builders[sessionID] = b
		}
		occurred := ""
		if occurredAt.Valid {
			occurred = occurredAt.String
		}
		// Sanitize before measuring so fingerprints match the
		// PG-readback fingerprint, whose values were sanitized
		// at insert time (see SanitizeUTF8).
		source = SanitizeUTF8(source)
		model = SanitizeUTF8(model)
		costStatus = SanitizeUTF8(costStatus)
		costSource = SanitizeUTF8(costSource)
		dedupKey.String = SanitizeUTF8(dedupKey.String)
		fmt.Fprintf(b,
			"%t|%d|%d:%s|%d:%s|%d|%d|%d|%d|%d|%t|%g|%d:%s|%d:%s|%d:%s|%d:%s;",
			ordinal.Valid,
			ordinal.Int64,
			len(source), source,
			len(model), model,
			inputTokens,
			outputTokens,
			cacheCreationInputTokens,
			cacheReadInputTokens,
			reasoningTokens,
			cost.Valid,
			cost.Float64,
			len(costStatus), costStatus,
			len(costSource), costSource,
			len(occurred), occurred,
			len(dedupKey.String), dedupKey.String,
		)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for id, b := range builders {
		out[id] += b.String()
	}
	return nil
}

// UsageEventFingerprint returns exact ordered fingerprint for one session.
func (db *DB) UsageEventFingerprint(sessionID string) (string, error) {
	fps, err := db.UsageEventFingerprints([]string{sessionID})
	if err != nil {
		return "", err
	}
	return fps[sessionID], nil
}

func (db *DB) GetUsageEvents(
	ctx context.Context, sessionID string,
) ([]UsageEvent, error) {
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT id, session_id, message_ordinal, source, model,
			input_tokens, output_tokens,
			cache_creation_input_tokens, cache_read_input_tokens,
			reasoning_tokens, cost_usd, cost_status, cost_source,
			occurred_at, dedup_key
		FROM usage_events
		WHERE session_id = ?
		ORDER BY COALESCE(occurred_at, ''), id`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying usage events: %w", err)
	}
	defer rows.Close()

	var out []UsageEvent
	for rows.Next() {
		var ev UsageEvent
		var ordinal sql.NullInt64
		var cost sql.NullFloat64
		var occurred sql.NullString
		if err := rows.Scan(
			&ev.ID, &ev.SessionID, &ordinal, &ev.Source, &ev.Model,
			&ev.InputTokens, &ev.OutputTokens,
			&ev.CacheCreationInputTokens, &ev.CacheReadInputTokens,
			&ev.ReasoningTokens, &cost, &ev.CostStatus,
			&ev.CostSource, &occurred, &ev.DedupKey,
		); err != nil {
			return nil, fmt.Errorf("scanning usage event: %w", err)
		}
		if ordinal.Valid {
			v := int(ordinal.Int64)
			ev.MessageOrdinal = &v
		}
		if cost.Valid {
			v := cost.Float64
			ev.CostUSD = &v
		}
		if occurred.Valid {
			ev.OccurredAt = occurred.String
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating usage events: %w", err)
	}
	return out, nil
}
