package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/wesm/agentsview/internal/db"
)

// GetSessionTiming computes the per-session timing summary on the PG
// read store. Mirrors the SQLite implementation in internal/db/timing.go;
// only the dialect-specific SQL changes. The shared AssembleTiming
// helper does the stitching, attribution, and aggregation.
//
// Because PG messages are keyed by (session_id, ordinal) — there is no
// id column — we synthesize TurnRow.MessageID = int64(ordinal) so that
// AssembleTiming's callsByMsg lookup matches between turns and calls.
// CallRow.MessageID is set to message_ordinal for the same reason.
func (s *Store) GetSessionTiming(
	ctx context.Context, sessionID string,
) (*db.SessionTiming, error) {
	sess, err := s.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if sess == nil {
		return nil, nil
	}

	turnRows, err := s.queryTurnRows(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	callRows, err := s.queryCallRows(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return db.AssembleTiming(sess, turnRows, callRows, time.Now().UTC()), nil
}

func (s *Store) queryTurnRows(
	ctx context.Context, sessionID string,
) ([]db.TurnRow, error) {
	rows, err := s.pg.QueryContext(ctx, `
		SELECT
		  m2.ordinal, m2.timestamp, m2.has_tool_use,
		  CASE
		    WHEN NOT m2.has_tool_use THEN NULL
		    WHEN m2.delta_ms < 0    THEN NULL
		    ELSE m2.delta_ms
		  END AS turn_duration_ms
		FROM (
		  SELECT
		    m.session_id, m.ordinal, m.timestamp, m.has_tool_use,
		    (round(EXTRACT(EPOCH FROM (
		      COALESCE(
		        LEAD(m.timestamp) OVER (ORDER BY m.ordinal),
		        s.ended_at
		      ) - m.timestamp
		    )) * 1000))::bigint AS delta_ms
		  FROM messages m
		  LEFT JOIN sessions s ON s.id = m.session_id
		  WHERE m.session_id = $1
		) m2
		ORDER BY m2.ordinal
	`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("querying timing turns: %w", err)
	}
	defer rows.Close()

	var out []db.TurnRow
	for rows.Next() {
		var ordinal int
		var ts *time.Time
		var hasToolUse bool
		var dur sql.NullInt64
		if err := rows.Scan(&ordinal, &ts, &hasToolUse, &dur); err != nil {
			return nil, fmt.Errorf("scanning timing turn: %w", err)
		}
		r := db.TurnRow{
			MessageID:  int64(ordinal),
			Ordinal:    int64(ordinal),
			HasToolUse: hasToolUse,
		}
		if ts != nil {
			r.Timestamp = FormatISO8601(*ts)
		}
		if dur.Valid {
			v := dur.Int64
			r.DurationMs = &v
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) queryCallRows(
	ctx context.Context, sessionID string,
) ([]db.CallRow, error) {
	now := time.Now().UTC()
	rows, err := s.pg.QueryContext(ctx, `
		SELECT
		  tc.message_ordinal,
		  tc.tool_use_id,
		  tc.tool_name,
		  tc.category,
		  tc.skill_name,
		  tc.subagent_session_id,
		  tc.input_json,
		  CASE
		    WHEN tc.subagent_session_id IS NOT NULL
		         AND s_sub.started_at IS NOT NULL THEN
		      (round(EXTRACT(EPOCH FROM (
		        COALESCE(s_sub.ended_at, $1::timestamptz) - s_sub.started_at
		      )) * 1000))::bigint
		    ELSE NULL
		  END AS subagent_duration_ms
		FROM tool_calls tc
		LEFT JOIN sessions s_sub
		  ON s_sub.id = tc.subagent_session_id
		WHERE tc.session_id = $2
		ORDER BY tc.message_ordinal, tc.id
	`, now, sessionID)
	if err != nil {
		return nil, fmt.Errorf("querying timing calls: %w", err)
	}
	defer rows.Close()

	var out []db.CallRow
	for rows.Next() {
		var msgOrdinal int
		var toolUseID, inputJSON, skill, sub sql.NullString
		var toolName, category string
		var subDur sql.NullInt64
		if err := rows.Scan(
			&msgOrdinal, &toolUseID, &toolName, &category,
			&skill, &sub, &inputJSON, &subDur,
		); err != nil {
			return nil, fmt.Errorf("scanning timing call: %w", err)
		}
		r := db.CallRow{
			MessageID: int64(msgOrdinal),
			ToolName:  toolName,
			Category:  category,
		}
		if toolUseID.Valid {
			r.ToolUseID = toolUseID.String
		}
		if skill.Valid {
			v := skill.String
			r.SkillName = &v
		}
		if sub.Valid {
			v := sub.String
			r.SubagentSessionID = &v
		}
		if inputJSON.Valid {
			r.InputJSON = inputJSON.String
		}
		if subDur.Valid {
			v := subDur.Int64
			r.DurationMs = &v
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
