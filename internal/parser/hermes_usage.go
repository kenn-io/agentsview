package parser

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/agentsview/internal/money"
)

// Hermes accumulates each route under this composite key. Task-empty and
// voice_chat rows also contribute to sessions; auxiliary rows do not.
type hermesModelUsage struct {
	SessionID, Model, Provider, BaseURL, Mode, Task string
	Input, Output, CacheRead, CacheWrite, Reasoning int
	Estimate, Actual                                sql.NullFloat64
	Status, Source                                  string
}

func readHermesModelUsage(ctx context.Context, conn *sql.DB, sessionID string) (map[string][]hermesModelUsage, error) {
	columns, err := conn.QueryContext(ctx, `PRAGMA table_info(session_model_usage)`)
	if err != nil {
		return nil, fmt.Errorf("inspect hermes usage: %w", err)
	}
	defer columns.Close()
	present := map[string]bool{}
	for columns.Next() {
		var cid, notNull, pk int
		var name, kind string
		var defaultValue sql.NullString
		if err := columns.Scan(&cid, &name, &kind, &notNull, &defaultValue, &pk); err != nil {
			return nil, err
		}
		present[name] = true
	}
	err = columns.Err()
	columns.Close()
	if err != nil || len(present) == 0 {
		return nil, err
	}
	identity := []string{"task", "billing_provider", "billing_base_url", "billing_mode"}
	for i, name := range identity {
		if present[name] {
			identity[i] = "COALESCE(" + name + ", '')"
		} else {
			identity[i] = "''"
		}
	}
	query := fmt.Sprintf(`SELECT session_id, model, %s, %s, %s, %s,
		input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens,
		estimated_cost_usd, actual_cost_usd, COALESCE(cost_status, ''), COALESCE(cost_source, '')
		FROM session_model_usage`, identity[1], identity[2], identity[3], identity[0])
	var args []any
	if sessionID != "" {
		query += " WHERE session_id = ?"
		args = append(args, sessionID)
	}
	query += " ORDER BY 1, 2, 3, 4, 5, 6"
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query hermes usage: %w", err)
	}
	defer rows.Close()
	out := make(map[string][]hermesModelUsage)
	for rows.Next() {
		var row hermesModelUsage
		if err := rows.Scan(&row.SessionID, &row.Model, &row.Provider, &row.BaseURL, &row.Mode, &row.Task,
			&row.Input, &row.Output, &row.CacheRead, &row.CacheWrite, &row.Reasoning,
			&row.Estimate, &row.Actual, &row.Status, &row.Source); err != nil {
			return nil, fmt.Errorf("scan hermes usage: %w", err)
		}
		out[row.SessionID] = append(out[row.SessionID], row)
	}
	return out, rows.Err()
}

func hermesUsageEvents(ss hermesStateSession, sessionID string) []ParsedUsageEvent {
	if len(ss.modelUsage) == 0 {
		return hermesSummaryUsageEvents(ss, sessionID)
	}
	var events []ParsedUsageEvent
	var summaryCost *money.Money
	if summary := hermesSummaryUsageEvents(ss, sessionID); len(summary) > 0 {
		summaryCost = summary[0].Cost
	}
	for _, row := range ss.modelUsage {
		actual := row.Actual
		// Hermes stores default zero in the ledger even when cost is unknown.
		if actual.Float64 == 0 {
			actual.Valid = false
		}
		part := hermesStateSession{
			model: row.Model, inputTokens: row.Input, outputTokens: row.Output,
			cacheReadTokens: row.CacheRead, cacheWriteTokens: row.CacheWrite, reasoningTokens: row.Reasoning,
			estimatedCost: row.Estimate, actualCost: actual, costStatus: row.Status, costSource: row.Source,
			startedAt: ss.startedAt, endedAt: ss.endedAt,
		}
		parts := hermesSummaryUsageEvents(part, sessionID)
		if len(parts) == 0 {
			continue
		}
		event := parts[0]
		event.ProviderID = row.Provider
		event.DedupKey = fmt.Sprintf("ledger:%q:%q:%q:%q:%q:%q", sessionID, row.Model, row.Provider, row.BaseURL, row.Mode, row.Task)
		if row.Task == "" || row.Task == "voice_chat" {
			event.Cost = nil
			if summaryCost != nil {
				event.Cost = &money.Money{}
			}
			ss.inputTokens = max(ss.inputTokens-event.InputTokens, 0)
			ss.outputTokens = max(ss.outputTokens-event.OutputTokens, 0)
			ss.cacheReadTokens = max(ss.cacheReadTokens-event.CacheReadInputTokens, 0)
			ss.cacheWriteTokens = max(ss.cacheWriteTokens-event.CacheCreationInputTokens, 0)
			ss.reasoningTokens = max(ss.reasoningTokens-event.ReasoningTokens, 0)
		}
		events = append(events, event)
	}
	// Known main cost stays on the summary model, including a cost-only event.
	if ss.inputTokens > 0 || ss.outputTokens > 0 || ss.cacheReadTokens > 0 || ss.cacheWriteTokens > 0 || ss.reasoningTokens > 0 || summaryCost != nil {
		events = append(events, hermesSummaryUsageEvents(ss, sessionID)...)
	}
	return events
}
