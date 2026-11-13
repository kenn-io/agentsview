package postgres

import (
	"context"
	"fmt"

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

type pgQueryExecer interface {
	pgSessionQueryer
	pgSessionExecer
}

// backfillMessageUsageCountersPG fills the usage counter columns on messages a
// binary without them wrote: rows from before the upgrade, and rows an older
// binary pushes afterwards. Usage reads take these counters only from the
// columns, so such a row counts no output tokens or web searches until this
// runs. idx_messages_usage_counters_missing keeps the check cheap once no row
// is left.
func backfillMessageUsageCountersPG(ctx context.Context, pg pgQueryExecer) (int, error) {
	total := 0
	for {
		n, err := backfillMessageUsageCounterBatchPG(ctx, pg)
		total += n
		if err != nil || n < messageUsageCounterBackfillBatch {
			return total, err
		}
	}
}

type messageUsageCounterBatch struct {
	sessionIDs                                []string
	ordinals, outputTokens, webSearchRequests []int64
}

func backfillMessageUsageCounterBatchPG(ctx context.Context, pg pgQueryExecer) (int, error) {
	batch, err := readMessagesWithoutUsageCounters(ctx, pg)
	if err != nil || len(batch.sessionIDs) == 0 {
		return 0, err
	}
	if _, err := pg.ExecContext(ctx, `
		UPDATE messages m
		SET usage_output_tokens = v.output_tokens,
			usage_web_search_requests = v.web_search_requests
		FROM (SELECT unnest($1::text[]) AS session_id,
			unnest($2::bigint[]) AS ordinal,
			unnest($3::bigint[]) AS output_tokens,
			unnest($4::bigint[]) AS web_search_requests) v
		WHERE m.session_id = v.session_id AND m.ordinal = v.ordinal
		  AND m.usage_output_tokens IS NULL`,
		batch.sessionIDs, batch.ordinals, batch.outputTokens, batch.webSearchRequests,
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
		batch.ordinals = append(batch.ordinals, ordinal)
		batch.outputTokens = append(batch.outputTokens, output)
		batch.webSearchRequests = append(batch.webSearchRequests, searches)
	}
	if err := rows.Err(); err != nil {
		return batch, fmt.Errorf("reading messages without usage counters: %w", err)
	}
	return batch, nil
}
