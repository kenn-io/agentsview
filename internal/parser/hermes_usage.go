package parser

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strconv"

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
		COALESCE(input_tokens, 0), COALESCE(output_tokens, 0), COALESCE(cache_read_tokens, 0), COALESCE(cache_write_tokens, 0), COALESCE(reasoning_tokens, 0),
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
		var tokens [5]any
		var estimate, actual any
		if err := rows.Scan(&row.SessionID, &row.Model, &row.Provider, &row.BaseURL, &row.Mode, &row.Task,
			&tokens[0], &tokens[1], &tokens[2], &tokens[3], &tokens[4],
			&estimate, &actual, &row.Status, &row.Source); err != nil {
			return nil, fmt.Errorf("scan hermes usage: %w", err)
		}
		var numericErr error
		for i, target := range []*int{&row.Input, &row.Output, &row.CacheRead, &row.CacheWrite, &row.Reasoning} {
			*target, numericErr = strconv.Atoi(fmt.Sprint(tokens[i]))
			if numericErr != nil {
				break
			}
		}
		if numericErr == nil {
			numericErr = row.Estimate.Scan(estimate)
		}
		if numericErr == nil {
			numericErr = row.Actual.Scan(actual)
		}
		if numericErr != nil {
			log.Printf("hermes: invalid ledger number for session %q model %q task %q: %v; skipping row", row.SessionID, row.Model, row.Task, numericErr)
			continue
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
	var remainder int64
	if summary := hermesSummaryUsageEvents(ss, sessionID); len(summary) > 0 {
		summaryCost = summary[0].Cost
		if summaryCost != nil {
			remainder = summaryCost.Microdollars
		}
	}
	for _, row := range ss.modelUsage {
		actual := row.Actual
		// Hermes stores default zero in the ledger even when cost is unknown.
		if actual.Float64 == 0 && (row.Status != "actual" || !hermesHasCostSource(row.Source)) {
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
			if summaryCost != nil {
				// Delta writes update both stores with the same cost basis.
				// Gateway reports can later replace only the session summary.
				part.actualCost = row.Actual
				part.actualCost.Valid = ss.actualCost.Valid
				part.costStatus, part.costSource = ss.costStatus, ss.costSource
				event.Cost = &money.Money{}
				if priced := hermesSummaryUsageEvents(part, sessionID); len(priced) > 0 && priced[0].Cost != nil {
					event.Cost = priced[0].Cost
				}
				// Keep ledger charges even when a gateway lowers the summary cost.
				remainder = max(remainder-event.Cost.Microdollars, 0)
			}
			ss.inputTokens = max(ss.inputTokens-event.InputTokens, 0)
			ss.outputTokens = max(ss.outputTokens-event.OutputTokens, 0)
			ss.cacheReadTokens = max(ss.cacheReadTokens-event.CacheReadInputTokens, 0)
			ss.cacheWriteTokens = max(ss.cacheWriteTokens-event.CacheCreationInputTokens, 0)
			ss.reasoningTokens = max(ss.reasoningTokens-event.ReasoningTokens, 0)
		}
		events = append(events, event)
	}
	if ss.inputTokens > 0 || ss.outputTokens > 0 || ss.cacheReadTokens > 0 || ss.cacheWriteTokens > 0 || ss.reasoningTokens > 0 || remainder > 0 {
		summary := hermesSummaryUsageEvents(ss, sessionID)
		if len(summary) > 0 && summaryCost != nil {
			summary[0].Cost = &money.Money{Microdollars: remainder}
		}
		events = append(events, summary...)
	}
	return events
}
