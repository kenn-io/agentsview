package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/stringutil"
)

// SessionTiming is the payload of GET /api/v1/sessions/{id}/timing.
// All durations are in milliseconds. *int64 fields are null when the
// underlying execution interval is unknown or incomplete.
type SessionTiming struct {
	SessionID       string          `json:"session_id"`
	TotalDurationMs int64           `json:"total_duration_ms"`
	ToolDurationMs  int64           `json:"tool_duration_ms"`
	TurnCount       int             `json:"turn_count"`
	ToolCallCount   int             `json:"tool_call_count"`
	SubagentCount   int             `json:"subagent_count"`
	SlowestCall     *CallTiming     `json:"slowest_call"`
	ByCategory      []CategoryTotal `json:"by_category"`
	Turns           []TurnTiming    `json:"turns"`
	Running         bool            `json:"running"`
	Activity        []TurnActivity  `json:"activity"`
	ActivityTotals  ActivityTotals  `json:"activity_totals"`
}

type CategoryTotal struct {
	Category   string `json:"category"`
	DurationMs int64  `json:"duration_ms"`
	CallCount  int    `json:"call_count"`
}

type TurnTiming struct {
	MessageID       int64        `json:"message_id"`
	Ordinal         int          `json:"ordinal"` // for ui.scrollToOrdinal
	StartedAt       string       `json:"started_at"`
	DurationMs      *int64       `json:"duration_ms"`
	PrimaryCategory string       `json:"primary_category"`
	Calls           []CallTiming `json:"calls"`
}

type CallTiming struct {
	ToolUseID         string  `json:"tool_use_id"`
	ToolName          string  `json:"tool_name"`
	Category          string  `json:"category"`
	SkillName         *string `json:"skill_name,omitempty"`
	SubagentSessionID *string `json:"subagent_session_id,omitempty"`
	DurationMs        *int64  `json:"duration_ms"`
	IsParallel        bool    `json:"is_parallel"`
	InputPreview      string  `json:"input_preview"`
}

// TurnRow is the per-message timing row returned by the per-turn SQL
// query. Both SQLite and PG mirrors scan into this shape and pass slices
// to AssembleTiming.
type TurnRow struct {
	MessageID        int64
	Ordinal          int64
	Timestamp        string
	HasToolUse       bool
	DurationMs       *int64
	Role             string
	IsSystem         bool
	IsSystemPrefixed bool
	SourceSubtype    string
	ContentLength    int
}

// CallRow is the per-tool_call row returned by the per-call SQL query.
// All backends pass raw execution evidence to AssembleTiming.
type CallRow struct {
	MessageID         int64
	ToolUseID         string
	ToolName          string
	Category          string
	SkillName         *string
	SubagentSessionID *string
	InputJSON         string
	ExecutionStart    string
	ExecutionEnd      string
	SubagentStart     string
	SubagentEnd       string
}

// GetSessionTiming computes the per-session timing summary. Returns
// (nil, nil) when the session does not exist (mirrors GetSession's
// contract; the HTTP handler turns this into a 404).
func (db *DB) GetSessionTiming(
	ctx context.Context, sessionID string,
) (*SessionTiming, error) {
	sess, err := db.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if sess == nil {
		return nil, nil
	}

	turnRows, err := db.queryTurnRows(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	callRows, err := db.queryCallRows(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return AssembleTiming(sess, turnRows, callRows, time.Now().UTC()), nil
}

func (db *DB) queryTurnRows(
	ctx context.Context, sessionID string,
) ([]TurnRow, error) {
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT
		  m2.id, m2.ordinal, m2.timestamp, m2.has_tool_use,
		  CASE
		    WHEN m2.has_tool_use = 0 THEN NULL
		    WHEN m2.delta_ms < 0    THEN NULL
		    ELSE m2.delta_ms
		  END AS turn_duration_ms,
		  m2.role, m2.is_system, m2.is_system_prefixed,
		  COALESCE(m2.source_subtype, ''), m2.content_length
		FROM (
		  SELECT
		    m.*,
		    CASE WHEN `+SystemPrefixSQL("m.content", "m.role")+` THEN 0 ELSE 1 END AS is_system_prefixed,
		    CAST(
		      ROUND(
		        (julianday(
		          COALESCE(
		            LEAD(m.timestamp) OVER (ORDER BY m.ordinal),
		            s.ended_at
		          )
		        ) - julianday(m.timestamp)) * 86400000
		      ) AS INTEGER
		    ) AS delta_ms
		  FROM messages m
		  LEFT JOIN sessions s ON s.id = m.session_id
		  WHERE m.session_id = ?
		) m2
		ORDER BY m2.ordinal
	`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TurnRow
	for rows.Next() {
		var r TurnRow
		var ts sql.NullString
		var hasFlag int
		var dur sql.NullInt64
		if err := rows.Scan(
			&r.MessageID, &r.Ordinal, &ts, &hasFlag, &dur,
			&r.Role, &r.IsSystem, &r.IsSystemPrefixed, &r.SourceSubtype, &r.ContentLength,
		); err != nil {
			return nil, err
		}
		if ts.Valid {
			r.Timestamp = ts.String
		}
		r.HasToolUse = hasFlag == 1
		if dur.Valid {
			v := dur.Int64
			r.DurationMs = &v
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (db *DB) queryCallRows(
	ctx context.Context, sessionID string,
) ([]CallRow, error) {
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT
		  tc.message_id,
		  tc.tool_use_id,
		  tc.tool_name,
		  tc.category,
		  tc.skill_name,
		  tc.subagent_session_id,
		  tc.input_json,
		  (
		    SELECT tre.timestamp
		    FROM tool_result_events tre
		    WHERE tre.session_id = tc.session_id
		      AND tre.tool_call_message_ordinal = m.ordinal
		      AND tre.call_index = tc.call_index
		      AND tre.source = 'tool_execution'
		      AND tre.status = 'started'
		      AND NULLIF(tre.timestamp, '') IS NOT NULL
		    ORDER BY tre.event_index ASC
		    LIMIT 1
		  ) AS execution_started_at,
		  (
		    SELECT tre.timestamp
		    FROM tool_result_events tre
		    WHERE tre.session_id = tc.session_id
		      AND tre.tool_call_message_ordinal = m.ordinal
		      AND tre.call_index = tc.call_index
		      AND tre.source = 'tool_execution'
		      AND tre.status IN ('completed', 'errored')
		      AND NULLIF(tre.timestamp, '') IS NOT NULL
		    ORDER BY tre.event_index DESC
		    LIMIT 1
		  ) AS execution_completed_at
		  ,s_sub.started_at
		  ,s_sub.ended_at
		FROM tool_calls tc
		JOIN messages m ON m.id = tc.message_id
		LEFT JOIN sessions s_sub ON s_sub.id = tc.subagent_session_id
		WHERE tc.session_id = ?
		ORDER BY tc.message_id, tc.id
	`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CallRow
	for rows.Next() {
		var r CallRow
		var toolUseID, inputJSON sql.NullString
		var skill, sub, executionStarted, executionCompleted, subagentStarted, subagentEnded sql.NullString
		if err := rows.Scan(
			&r.MessageID, &toolUseID, &r.ToolName, &r.Category,
			&skill, &sub, &inputJSON, &executionStarted, &executionCompleted,
			&subagentStarted, &subagentEnded,
		); err != nil {
			return nil, err
		}
		if toolUseID.Valid {
			r.ToolUseID = toolUseID.String
		}
		if skill.Valid {
			s := skill.String
			r.SkillName = &s
		}
		if sub.Valid {
			s := sub.String
			r.SubagentSessionID = &s
		}
		if inputJSON.Valid {
			r.InputJSON = inputJSON.String
		}
		r.ExecutionStart = executionStarted.String
		r.ExecutionEnd = executionCompleted.String
		r.SubagentStart = subagentStarted.String
		r.SubagentEnd = subagentEnded.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// AssembleTiming stitches scanned per-turn and per-call rows plus
// session metadata into a SessionTiming. Pure logic — shared by the
// SQLite and PostgreSQL backends. `now` is captured by the caller so
// tests can be deterministic if needed.
func AssembleTiming(
	sess *Session, turns []TurnRow, calls []CallRow, now time.Time,
) *SessionTiming {
	running := sess.EndedAt == nil || *sess.EndedAt == ""

	out := &SessionTiming{
		SessionID:  sess.ID,
		ByCategory: []CategoryTotal{},
		Turns:      []TurnTiming{},
		Running:    running,
	}

	switch {
	case sess.StartedAt == nil || *sess.StartedAt == "":
		// No start timestamp: leave total at 0.
	case sess.EndedAt != nil && *sess.EndedAt != "":
		out.TotalDurationMs = millisBetween(
			*sess.StartedAt, *sess.EndedAt,
		)
	default:
		out.TotalDurationMs = millisBetween(
			*sess.StartedAt, now.Format(time.RFC3339),
		)
	}

	intervals := assembleTurnActivity(out, sess, turns, calls, now)
	callsByMsg := map[int64][]CallTiming{}
	for i, r := range calls {
		c := CallTiming{
			ToolUseID:         r.ToolUseID,
			ToolName:          r.ToolName,
			Category:          r.Category,
			SkillName:         r.SkillName,
			SubagentSessionID: r.SubagentSessionID,
			InputPreview:      makeInputPreview(r.Category, r.ToolName, r.InputJSON),
		}
		if interval := intervals[i]; interval != nil {
			v := interval.end - interval.start
			c.DurationMs = &v
		}
		callsByMsg[r.MessageID] = append(callsByMsg[r.MessageID], c)
	}

	for _, t := range turns {
		if !t.HasToolUse {
			continue
		}
		out.TurnCount++
		turnCalls := callsByMsg[t.MessageID]
		if turnCalls == nil {
			turnCalls = []CallTiming{}
		}
		if completedAt, ok := completedCallBoundary(calls, t.MessageID); ok {
			if start, valid := timingTimestamp(t.Timestamp); valid && completedAt >= start {
				v := completedAt - start
				t.DurationMs = &v
			}
		}
		for i := range turnCalls {
			turnCalls[i].IsParallel = len(turnCalls) > 1
			if turnCalls[i].SubagentSessionID != nil {
				out.SubagentCount++
			}
			if turnCalls[i].DurationMs != nil && (out.SlowestCall == nil || *turnCalls[i].DurationMs > *out.SlowestCall.DurationMs) {
				c := turnCalls[i]
				out.SlowestCall = &c
			}
		}
		out.ToolCallCount += len(turnCalls)
		out.Turns = append(out.Turns, TurnTiming{
			MessageID:       t.MessageID,
			Ordinal:         int(t.Ordinal),
			StartedAt:       t.Timestamp,
			DurationMs:      t.DurationMs,
			PrimaryCategory: primaryTimingCategory(turnCalls),
			Calls:           turnCalls,
		})
	}
	return out
}

func completedCallBoundary(calls []CallRow, messageID int64) (int64, bool) {
	var boundary int64
	matched := false
	for _, call := range calls {
		if call.MessageID != messageID {
			continue
		}
		interval, ok := executionInterval(call)
		if !ok {
			return 0, false
		}
		if !matched || interval.end > boundary {
			boundary = interval.end
		}
		matched = true
	}
	return boundary, matched
}

func primaryTimingCategory(calls []CallTiming) string {
	counts := map[string]int{}
	for _, call := range calls {
		counts[call.Category]++
	}
	for category, count := range counts {
		if count*2 > len(calls) {
			return category
		}
	}
	return "Mixed"
}

// millisBetween parses two RFC3339 timestamps and returns
// (b - a) in milliseconds. Returns 0 when either parse fails.
func millisBetween(a, b string) int64 {
	ta, err := time.Parse(time.RFC3339, a)
	if err != nil {
		return 0
	}
	tb, err := time.Parse(time.RFC3339, b)
	if err != nil {
		return 0
	}
	return tb.Sub(ta).Milliseconds()
}

// makeInputPreview returns a short snippet of the tool's input args,
// suitable for display in the timing summary's call list. Mirrors the
// most common cases from frontend/src/lib/utils/tool-params.ts;
// returns "" when no familiar key is found.
//
// Keep this minimal — the frontend rebuilds the full label from raw
// input_json on display. This is purely a hint surfaced via the API
// for clients that don't fetch the full message.
//
// Dispatches on the normalized category first so codex's exec_command
// (category Bash, args under "cmd") shares an arm with Claude's Bash
// (args under "command"). Falls back to the raw tool name when the
// category is too generic ("Tool", "Other") to identify arg shape.
func makeInputPreview(category, toolName, inputJSON string) string {
	if inputJSON == "" {
		return ""
	}
	var params map[string]any
	if err := json.Unmarshal([]byte(inputJSON), &params); err != nil {
		return ""
	}

	pickStr := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := params[k]; ok && v != nil {
				if s, ok := v.(string); ok && s != "" {
					return s
				}
			}
		}
		return ""
	}

	key := category
	if key == "" || key == "Other" || key == "Tool" {
		key = toolName
	}

	var raw string
	switch key {
	case "Read":
		raw = pickStr("file_path", "path")
	case "Edit":
		raw = pickStr("file_path", "path", "filePath", "file")
	case "Write":
		raw = pickStr("file_path", "path", "file")
	case "Grep":
		raw = pickStr("pattern", "query")
	case "Glob":
		raw = pickStr("pattern", "path")
	case "Bash":
		cmd := pickStr("command", "cmd")
		if cmd != "" {
			if i := strings.IndexByte(cmd, '\n'); i >= 0 {
				cmd = cmd[:i]
			}
			raw = cmd
		}
	case "Skill", "skill":
		raw = pickStr("skill", "name")
	default:
		raw = pickStr("file_path", "path", "pattern", "command", "cmd")
	}

	const maxLen = 100
	return stringutil.TruncateRunes(raw, maxLen, "…")
}
