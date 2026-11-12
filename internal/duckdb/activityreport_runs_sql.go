package duckdb

import (
	"context"
	"fmt"
	"time"

	"go.kenn.io/agentsview/internal/activity"
)

// activityRunOrder is the order a session's candidates are resolved in:
// by start, then by start ordinal, message pairs before tool candidates.
const activityRunOrder = "start_us, start_ordinal, kind, call_index, event_index"

// activitySessionRuns pairs the sessions' messages and tool events into
// interval candidates and resolves them in SQL into each session's runs over
// all time, which activity.SessionRuns clips to any report range.
//
// A message pairs with the next message that has a timestamp. A tool event
// pairs with whichever comes first of the session's next tool event and
// its first later message, by ordinal, with a later timestamp; the
// session's last message pairs with the first tool event after it. Each
// candidate is capped at the gap, trimmed to start where the session's
// earlier candidates reach, and dropped when they already cover it. Pieces
// that touch and share a model join into one run.
func (s *Store) activitySessionRuns(
	ctx context.Context, ids []string, gapCapSeconds float64,
) (map[string]activity.SessionRuns, error) {
	gapCap := time.Duration(gapCapSeconds) * time.Second
	sqlText, args := activityRunsSQL(ids, gapCap.Microseconds())
	rows, err := s.queryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("querying activity session runs: %w", err)
	}
	defer rows.Close()
	out := map[string]activity.SessionRuns{}
	builder := activity.NewSessionRunsBuilder(gapCapSeconds)
	var current string
	for rows.Next() {
		var session, model string
		var startUS, endUS int64
		var trimmed int64
		if err := rows.Scan(&session, &model, &startUS, &endUS, &trimmed); err != nil {
			return nil, fmt.Errorf("scanning activity session run piece: %w", err)
		}
		if session != current {
			if current != "" {
				out[current] = builder.Finish()
			}
			current = session
		}
		builder.AddPiece(model, startUS, endUS, trimmed == 1)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating activity session run pieces: %w", err)
	}
	if current != "" {
		out[current] = builder.Finish()
	}
	return out, nil
}

// activityRunsSQL reads the sessions' messages, with timestamps as
// microseconds since the epoch, and their completed or errored
// tool_execution events with a timestamp.
func activityRunsSQL(ids []string, gapUS int64) (string, []any) {
	query := `WITH
		activity_run_messages AS (
			SELECT session_id, ordinal, epoch_us(timestamp) AS ts_us, role, model
			FROM messages WHERE session_id IN (SELECT unnest(?))
		),
		activity_run_terminals AS (
			SELECT session_id, tool_call_message_ordinal AS ordinal, call_index, event_index,
				epoch_us(timestamp) AS ts_us
			FROM tool_result_events
			WHERE session_id IN (SELECT unnest(?)) AND source = 'tool_execution'
				AND status IN ('completed', 'errored') AND timestamp IS NOT NULL
		),
		activity_run_stamped AS (
			SELECT session_id, ordinal, ts_us, role, model,
				lead(ordinal) OVER w AS next_ordinal,
				lead(ts_us) OVER w AS next_us,
				lead(role) OVER w AS next_role,
				lead(model) OVER w AS next_model,
				lag(ts_us) OVER w AS previous_us
			FROM activity_run_messages
			WHERE ts_us IS NOT NULL
			WINDOW w AS (PARTITION BY session_id ORDER BY ordinal)
		),
		activity_run_paired AS (
			SELECT *,
				last_value(CASE WHEN role = 'assistant' AND model != '' AND ts_us > previous_us THEN model END IGNORE NULLS)
					OVER (PARTITION BY session_id ORDER BY ordinal ROWS UNBOUNDED PRECEDING) AS prior_model
			FROM activity_run_stamped
		),
		activity_run_tool_stream AS (
			SELECT session_id, ordinal, 0 AS is_terminal, ts_us, role, model,
				CAST(0 AS BIGINT) AS call_index, CAST(0 AS BIGINT) AS event_index
			FROM activity_run_messages
			WHERE session_id IN (SELECT session_id FROM activity_run_terminals)
			UNION ALL
			SELECT session_id, ordinal, 1, ts_us, '', '', call_index, event_index
			FROM activity_run_terminals
		),
		activity_run_tool_ordered AS (
			SELECT *,
				max(CASE WHEN is_terminal = 0 THEN ts_us END) OVER (PARTITION BY session_id
					ORDER BY ordinal, is_terminal ROWS UNBOUNDED PRECEDING) AS reached_us,
				max(CASE WHEN is_terminal = 0 THEN ts_us END) OVER (PARTITION BY session_id
					ORDER BY ordinal, is_terminal ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING) AS earlier_us,
				last_value(CASE WHEN is_terminal = 0 AND role = 'assistant' AND model != '' THEN model END IGNORE NULLS)
					OVER (PARTITION BY session_id ORDER BY ordinal, is_terminal ROWS UNBOUNDED PRECEDING) AS prior_model
			FROM activity_run_tool_stream
		),
		activity_run_tool_timed AS (
			SELECT *,
				last_value(CASE WHEN is_record = 1 THEN ordinal END IGNORE NULLS) OVER r AS record_ordinal,
				last_value(CASE WHEN is_record = 1 THEN ts_us END IGNORE NULLS) OVER r AS record_us,
				last_value(CASE WHEN is_record = 1 THEN role END IGNORE NULLS) OVER r AS record_role,
				last_value(CASE WHEN is_record = 1 THEN model END IGNORE NULLS) OVER r AS record_model,
				lead(ts_us) OVER t AS next_terminal_us,
				lag(ts_us) OVER t AS previous_terminal_us,
				last_value(CASE WHEN is_terminal = 0 AND ts_us IS NOT NULL THEN ts_us END IGNORE NULLS) OVER s AS last_us,
				last_value(CASE WHEN is_terminal = 0 AND ts_us IS NOT NULL THEN ordinal END IGNORE NULLS) OVER s AS last_ordinal,
				last_value(CASE WHEN is_terminal = 0 AND ts_us IS NOT NULL THEN COALESCE(prior_model, '') END IGNORE NULLS)
					OVER s AS last_prior
			FROM (
				SELECT *,
					CASE WHEN is_terminal = 0 AND ts_us IS NOT NULL AND (earlier_us IS NULL OR ts_us > earlier_us)
						THEN 1 ELSE 0 END AS is_record
				FROM activity_run_tool_ordered
			) o
			WINDOW r AS (PARTITION BY session_id ORDER BY ts_us DESC NULLS LAST, is_terminal DESC
					ROWS UNBOUNDED PRECEDING),
				t AS (PARTITION BY session_id, is_terminal ORDER BY ts_us, call_index, event_index),
				s AS (PARTITION BY session_id ORDER BY ordinal, is_terminal
					ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING)
		),
		activity_run_reversed_next AS (
			SELECT session_id, ordinal, ts_us, call_index, event_index, fallback_ordinal, fallback_us,
				fallback_role, fallback_model, 1 AS fallback_hit
			FROM (
				SELECT t.session_id, t.ordinal, t.ts_us, t.call_index, t.event_index,
					m.ordinal AS fallback_ordinal, m.ts_us AS fallback_us, m.role AS fallback_role,
					m.model AS fallback_model,
					row_number() OVER (PARTITION BY t.session_id, t.ordinal, t.ts_us, t.call_index, t.event_index
						ORDER BY m.ordinal) AS fallback_rank
				FROM (
					SELECT session_id, ordinal, ts_us, call_index, event_index
					FROM activity_run_tool_ordered
					WHERE is_terminal = 1 AND reached_us > ts_us
				) t
				JOIN (
					SELECT session_id, ordinal, ts_us, role, model
					FROM activity_run_tool_ordered
					WHERE is_terminal = 0 AND ts_us IS NOT NULL
				) m ON m.session_id = t.session_id
				WHERE m.ordinal > t.ordinal AND m.ts_us > t.ts_us
			) ranked
			WHERE fallback_rank = 1
		),
		activity_run_tool_rows AS (
			SELECT r.*,
				CASE WHEN r.reached_us IS NULL OR r.reached_us <= r.ts_us THEN r.record_us
					WHEN f.fallback_hit = 1 THEN f.fallback_us END AS message_us,
				CASE WHEN r.reached_us IS NULL OR r.reached_us <= r.ts_us THEN r.record_role
					WHEN f.fallback_hit = 1 THEN f.fallback_role END AS message_role,
				CASE WHEN r.reached_us IS NULL OR r.reached_us <= r.ts_us THEN r.record_model
					WHEN f.fallback_hit = 1 THEN f.fallback_model END AS message_model
			FROM (SELECT * FROM activity_run_tool_timed WHERE is_terminal = 1) r
			LEFT JOIN activity_run_reversed_next f
				ON f.session_id = r.session_id AND f.ordinal = r.ordinal AND f.ts_us = r.ts_us
				AND f.call_index = r.call_index AND f.event_index = r.event_index
		),
		activity_run_tool_closed AS (
			SELECT *,
				CASE WHEN next_terminal_us IS NOT NULL AND (message_us IS NULL OR next_terminal_us < message_us)
					THEN 1 ELSE 0 END AS closed_by_tool
			FROM activity_run_tool_rows
		),
		activity_run_tool_priors AS (
			SELECT c.session_id,
				CASE WHEN v.variant = 1 THEN c.ordinal ELSE c.last_ordinal END AS start_ordinal,
				CASE WHEN v.variant = 1 THEN c.ts_us ELSE c.last_us END AS start_us,
				CASE WHEN v.variant = 2 THEN c.ts_us WHEN c.closed_by_tool = 1 THEN c.next_terminal_us
					ELSE c.message_us END AS end_us,
				c.call_index, c.event_index,
				CASE WHEN v.variant = 1 AND c.closed_by_tool = 0 THEN c.message_role ELSE 'tool' END AS closing_role,
				CASE WHEN v.variant = 1 AND c.closed_by_tool = 0 THEN c.message_model ELSE '' END AS closing_model,
				CASE WHEN v.variant = 1 THEN c.prior_model ELSE c.last_prior END AS prior_model
			FROM activity_run_tool_closed c
			CROSS JOIN (SELECT 1 AS variant UNION ALL SELECT 2 AS variant) v
			WHERE (v.variant = 1 AND (c.closed_by_tool = 1 OR c.message_us IS NOT NULL))
				OR (v.variant = 2 AND c.last_us IS NOT NULL AND c.ts_us > c.last_us
					AND (c.previous_terminal_us IS NULL OR c.previous_terminal_us <= c.last_us))
		),
		activity_run_candidates AS (
			SELECT session_id, ordinal AS start_ordinal, ts_us AS start_us, next_us AS end_us,
				0 AS kind, 0 AS call_index, 0 AS event_index,
				CASE WHEN next_role = 'assistant' AND next_model != '' THEN next_model
					WHEN COALESCE(prior_model, '') != '' THEN prior_model ELSE 'unknown' END AS model
			FROM activity_run_paired
			WHERE next_us IS NOT NULL
			UNION ALL
			SELECT session_id, start_ordinal, start_us, end_us, 1, call_index, event_index,
				CASE WHEN closing_role = 'assistant' AND closing_model != '' THEN closing_model
					WHEN COALESCE(prior_model, '') != '' THEN prior_model ELSE 'unknown' END
			FROM activity_run_tool_priors
		),
		activity_run_capped AS (
			SELECT session_id, start_ordinal, start_us, kind, call_index, event_index, model,
				CASE WHEN end_us < start_us + ? THEN end_us ELSE start_us + ? END AS end_us
			FROM activity_run_candidates
			WHERE end_us > start_us
		),
		activity_run_reached AS (
			SELECT *,
				max(end_us) OVER (PARTITION BY session_id ORDER BY ` + activityRunOrder + `
					ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING) AS reach_us
			FROM activity_run_capped
		),
		activity_run_pieces AS (
			SELECT session_id, model, end_us AS piece_end,
				CASE WHEN reach_us > start_us THEN reach_us ELSE start_us END AS piece_start,
				CASE WHEN reach_us > start_us THEN 1 ELSE 0 END AS trimmed
			FROM activity_run_reached
			WHERE reach_us IS NULL OR end_us > reach_us
		)
		SELECT session_id, model, CAST(piece_start AS BIGINT), CAST(piece_end AS BIGINT), CAST(trimmed AS BIGINT)
		FROM activity_run_pieces
		ORDER BY session_id, piece_start`
	return query, []any{ids, ids, gapUS, gapUS}
}
