package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"log"

	"go.kenn.io/agentsview/internal/usagefacts"
)

// messageUsageCounters returns the two counters usage reads take from a
// message's stored token JSON: its output tokens, clamped as SQLite reads
// them, and the web searches it billed. Push stores them in
// messages.usage_output_tokens and messages.usage_web_search_requests so
// usage reads never parse token_usage in SQL.
func messageUsageCounters(tokenUsage string) (outputTokens, webSearchRequests int64) {
	parsed := usagefacts.ParseTokenUsage(tokenUsage)
	return parsed.OutputTokens, parsed.WebSearchRequests
}

// messageUsageCounterBackfillBatch bounds each backfill statement so a large
// archive never holds one long transaction on CockroachDB.
const messageUsageCounterBackfillBatch = 5000

// backfillMessageUsageCountersPG fills the usage counter columns on messages a
// binary without them wrote: rows from before the upgrade, and rows an older
// binary pushes afterwards. Until this runs, usage reads bill no web searches
// for such a row and rank its Claude output snapshot as having no output
// tokens. idx_messages_usage_counters_missing keeps the check cheap once no
// row is left, so every push can run it.
//
// Each batch also bumps the filled sessions' updated_at, the change every
// publisher makes alongside its message writes, so usage caches and the
// usage change journal see the new counters.
func backfillMessageUsageCountersPG(ctx context.Context, pg storeExecutor) (int, error) {
	total := 0
	for {
		n, err := backfillMessageUsageCounterBatchPG(ctx, pg)
		total += n
		if err != nil || n < messageUsageCounterBackfillBatch {
			return total, err
		}
	}
}

// fillMessageUsageCountersPG runs backfillMessageUsageCountersPG during a
// push's schema check. A publisher locks a session before its messages and the
// fill locks messages first, so the two can deadlock when another machine
// pushes the same session. Each batch is its own statement, so the database
// rolled back only the batch it aborted; the fill logs the conflict and the
// next push fills those rows. Callers inside an explicit transaction cannot go
// on after an abort and call backfillMessageUsageCountersPG directly.
func fillMessageUsageCountersPG(ctx context.Context, db *sql.DB) (int, error) {
	filled, err := backfillMessageUsageCountersPG(ctx, db)
	if isTransactionConflict(err) {
		log.Printf("pg schema: message usage counter fill deferred: %v", err)
		return filled, nil
	}
	return filled, err
}

type messageUsageCounterBatch struct {
	sessionIDs, tokenUsages                   []string
	ordinals, outputTokens, webSearchRequests []int64
}

func backfillMessageUsageCounterBatchPG(ctx context.Context, pg storeExecutor) (int, error) {
	batch, err := readMessagesWithoutUsageCounters(ctx, pg)
	if err != nil || len(batch.sessionIDs) == 0 {
		return 0, err
	}
	// Matching token_usage skips a message an older binary replaced after the
	// read: it keeps NULL counters, and a later batch parses its new JSON.
	if _, err := pg.ExecContext(ctx, `
		WITH v AS (
			SELECT unnest($1::text[]) AS session_id,
				unnest($2::bigint[]) AS ordinal,
				unnest($3::text[]) AS token_usage,
				unnest($4::bigint[]) AS output_tokens,
				unnest($5::bigint[]) AS web_search_requests
		), filled AS (
			UPDATE messages m
			SET usage_output_tokens = v.output_tokens,
				usage_web_search_requests = v.web_search_requests
			FROM v
			WHERE m.session_id = v.session_id AND m.ordinal = v.ordinal
			  AND m.token_usage = v.token_usage
			  AND m.usage_output_tokens IS NULL
			RETURNING m.session_id
		)
		UPDATE sessions SET updated_at = NOW()
		WHERE id IN (SELECT session_id FROM filled)`,
		batch.sessionIDs, batch.ordinals, batch.tokenUsages,
		batch.outputTokens, batch.webSearchRequests,
	); err != nil {
		return 0, fmt.Errorf("storing message usage counters: %w", err)
	}
	return len(batch.sessionIDs), nil
}

// readMessagesWithoutUsageCounters returns before the caller's UPDATE runs,
// so its rows close first: a transaction runs one statement at a time.
func readMessagesWithoutUsageCounters(
	ctx context.Context, pg pgSessionQueryer,
) (messageUsageCounterBatch, error) {
	var batch messageUsageCounterBatch
	rows, err := pg.QueryContext(ctx,
		`SELECT session_id, ordinal, token_usage FROM messages
		 WHERE usage_output_tokens IS NULL LIMIT $1`,
		messageUsageCounterBackfillBatch)
	if err != nil {
		return batch, fmt.Errorf("querying messages without usage counters: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sessionID, tokenUsage string
		var ordinal int64
		if err := rows.Scan(&sessionID, &ordinal, &tokenUsage); err != nil {
			return batch, fmt.Errorf("scanning message without usage counters: %w", err)
		}
		output, searches := messageUsageCounters(tokenUsage)
		batch.sessionIDs = append(batch.sessionIDs, sessionID)
		batch.tokenUsages = append(batch.tokenUsages, tokenUsage)
		batch.ordinals = append(batch.ordinals, ordinal)
		batch.outputTokens = append(batch.outputTokens, output)
		batch.webSearchRequests = append(batch.webSearchRequests, searches)
	}
	if err := rows.Err(); err != nil {
		return batch, fmt.Errorf("reading messages without usage counters: %w", err)
	}
	return batch, nil
}
