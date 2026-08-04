package duckdb

import (
	"context"
	"fmt"
	"time"

	"go.kenn.io/agentsview/internal/activity"
)

func (s *Store) activityReportCandidateSource(
	ids []string, q activity.Query,
) activity.CandidateSource {
	return func(
		ctx context.Context,
		yield func(activity.IntervalCandidate) error,
	) error {
		if len(ids) == 0 {
			return nil
		}
		lower := q.RangeStart.Add(
			-time.Duration(q.GapCapSeconds) * time.Second,
		)
		query := `SELECT
			m.session_id, m.ordinal, successor.ordinal,
			m.timestamp, successor.timestamp,
			successor.role, successor.model,
			COALESCE((
				SELECT prior.model
				FROM messages prior
				WHERE prior.session_id = m.session_id
					AND prior.ordinal <= m.ordinal
					AND prior.role = 'assistant'
					AND prior.model != ''
					AND prior.timestamp IS NOT NULL
					AND prior.timestamp > (
						SELECT prior_previous.timestamp
						FROM messages prior_previous
						WHERE prior_previous.session_id = prior.session_id
							AND prior_previous.ordinal < prior.ordinal
							AND prior_previous.timestamp IS NOT NULL
						ORDER BY prior_previous.ordinal DESC
						LIMIT 1
					)
				ORDER BY prior.ordinal DESC
				LIMIT 1
			), 'unknown')
		FROM messages m
		JOIN messages successor
			ON successor.session_id = m.session_id
			AND successor.ordinal = (
				SELECT next.ordinal
				FROM messages next
				WHERE next.session_id = m.session_id
					AND next.ordinal > m.ordinal
					AND next.timestamp IS NOT NULL
				ORDER BY next.ordinal
				LIMIT 1
			)
		WHERE m.session_id IN (SELECT unnest(?))
			AND m.timestamp IS NOT NULL
			AND m.timestamp >= CAST(? AS TIMESTAMP)
			AND m.timestamp < CAST(? AS TIMESTAMP)
		ORDER BY m.timestamp, m.session_id, m.ordinal`
		rows, err := s.queryContext(
			ctx, query, ids,
			lower.UTC().Format(time.RFC3339Nano),
			q.EffectiveEnd.UTC().Format(time.RFC3339Nano),
		)
		if err != nil {
			return fmt.Errorf("querying duckdb activity report candidates: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			var candidate activity.IntervalCandidate
			var start, end any
			if err := rows.Scan(
				&candidate.SessionID, &candidate.StartOrdinal,
				&candidate.EndOrdinal, &start, &end,
				&candidate.ClosingRole, &candidate.ClosingModel,
				&candidate.PriorModel,
			); err != nil {
				return fmt.Errorf("scanning duckdb activity report candidate: %w", err)
			}
			startText, endText := formatDBTime(start), formatDBTime(end)
			candidate.Start, err = time.Parse(time.RFC3339Nano, startText)
			if err != nil {
				return fmt.Errorf("parsing duckdb activity candidate start: %w", err)
			}
			candidate.End, err = time.Parse(time.RFC3339Nano, endText)
			if err != nil {
				return fmt.Errorf("parsing duckdb activity candidate end: %w", err)
			}
			candidate.Start = candidate.Start.UTC()
			candidate.End = candidate.End.UTC()
			if err := yield(candidate); err != nil {
				return err
			}
		}
		return rows.Err()
	}
}

// ActivityReportCandidateSource exposes the backend's mechanical pairing
// stream for cross-backend contract tests. Activity semantics remain in the
// shared aggregator.
func (s *Store) ActivityReportCandidateSource(
	ids []string, q activity.Query,
) activity.CandidateSource {
	return s.activityReportCandidateSource(ids, q)
}

func formatDBTime(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case time.Time:
		return t.UTC().Format(time.RFC3339Nano)
	case string:
		return t
	case []byte:
		return string(t)
	default:
		return fmt.Sprint(t)
	}
}
