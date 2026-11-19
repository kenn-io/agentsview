package db

import (
	"context"
	"fmt"
	"log"

	"go.kenn.io/agentsview/internal/usagefacts"
)

// storedMessageUsageCounters returns the two counters usage reads take from a
// message's token JSON: its clamped output tokens and the web searches it
// billed. Writes store them in messages.usage_output_tokens and
// messages.usage_web_search_requests so usage SQL never parses token_usage.
// The SQL functions agentsview_usage_output_tokens and
// agentsview_usage_web_search_requests compute the same values for copies
// made in SQL.
func storedMessageUsageCounters(tokenUsage string) (outputTokens, webSearchRequests int64) {
	parsed := usagefacts.ParseTokenUsage(tokenUsage)
	return parsed.OutputTokens, parsed.WebSearchRequests
}

// addMessageUsageCounters adds the stored usage counter columns and fills them
// for every existing message. Both happen in one transaction, so the columns'
// presence means the fill finished. No trigger watches these columns, so the
// UPDATE leaves the full-text indexes and usage generations alone.
func addMessageUsageCounters(ctx context.Context, w *writerHandle, progress OpenProgressFunc) error {
	var present bool
	if err := w.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pragma_table_info('messages')
		WHERE name = 'usage_web_search_requests')`).Scan(&present); err != nil {
		return fmt.Errorf("probing messages.usage_web_search_requests: %w", err)
	}
	if present {
		return nil
	}
	progress.report("Storing message usage counters")
	tx, err := w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting message usage counter migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, statement := range []string{
		"ALTER TABLE messages ADD COLUMN usage_output_tokens INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE messages ADD COLUMN usage_web_search_requests INTEGER NOT NULL DEFAULT 0",
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("adding message usage counters: %w", err)
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE messages
		SET usage_output_tokens = agentsview_usage_output_tokens(token_usage),
			usage_web_search_requests = agentsview_usage_web_search_requests(token_usage)
		WHERE token_usage != ''`)
	if err != nil {
		return fmt.Errorf("filling message usage counters: %w", err)
	}
	filled, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("counting filled message usage counters: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing message usage counter migration: %w", err)
	}
	log.Printf("migration: added message usage counter columns; filled %d messages", filled)
	return nil
}
