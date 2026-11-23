package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

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

// addMessageUsageCounters adds the columns and records any deferred fill.
func addMessageUsageCounters(ctx context.Context, w *writerHandle, progress OpenProgressFunc, deferFill bool) (bool, error) {
	var present, pending bool
	if err := w.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pragma_table_info('messages') WHERE name = 'usage_web_search_requests')`).Scan(&present); err != nil {
		return false, fmt.Errorf("probing message usage counters: %w", err)
	}
	if err := w.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM stats WHERE key = 'message_usage_counters_pending')`).Scan(&pending); err != nil {
		return false, err
	}
	if present && !pending {
		return false, nil
	}
	progress.report("Storing message usage counters")
	tx, err := w.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if !present {
		for _, statement := range []string{
			"ALTER TABLE messages ADD COLUMN usage_output_tokens INTEGER NOT NULL DEFAULT 0",
			"ALTER TABLE messages ADD COLUMN usage_web_search_requests INTEGER NOT NULL DEFAULT 0",
		} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return false, fmt.Errorf("adding message usage counters: %w", err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO stats(key, value) VALUES ('message_usage_counters_pending', 1)`); err != nil {
		return false, err
	}
	if !deferFill {
		if err := fillMessageUsageCounters(ctx, tx); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return deferFill, nil
}

func fillMessageUsageCounters(ctx context.Context, tx *sql.Tx) error {
	result, err := tx.ExecContext(ctx, `DELETE FROM stats WHERE key = 'message_usage_counters_pending'`)
	if err != nil {
		return err
	}
	deleted, err := result.RowsAffected()
	if err != nil || deleted == 0 {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages
		SET usage_output_tokens = agentsview_usage_output_tokens(token_usage),
			usage_web_search_requests = agentsview_usage_web_search_requests(token_usage)
		WHERE token_usage != ''`); err != nil {
		return fmt.Errorf("filling message usage counters: %w", err)
	}
	return nil
}

// ErrMessageUsageCountersPending asks read-only callers to wait for the upgrade.
var ErrMessageUsageCountersPending = errors.New("archive upgrade is still running; retry after the daemon finishes")

// finishUsageCounters fills the deferred counters before widening the usage index.
func (db *DB) finishUsageCounters(ctx context.Context) error {
	if !db.usageCountersPending.Load() {
		return nil
	}
	_, err := RetryAcrossHandoff(ctx, db, func() (struct{}, error) {
		if err := db.lockWriterCtx(ctx); err != nil {
			return struct{}{}, err
		}
		defer db.mu.Unlock()
		if db.writerClosed.Load() {
			return struct{}{}, ErrWriterClosed
		}
		w := db.getWriter()
		tx, err := w.BeginTx(ctx, nil)
		if err != nil {
			return struct{}{}, err
		}
		defer func() { _ = tx.Rollback() }()
		if err := fillMessageUsageCounters(ctx, tx); err != nil {
			return struct{}{}, err
		}
		if err := tx.Commit(); err != nil {
			return struct{}{}, err
		}
		if err := ensureUsageIndexesLocked(ctx, w); err != nil {
			return struct{}{}, err
		}
		db.usageCountersPending.Store(false)
		return struct{}{}, nil
	})
	return err
}
