package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// SessionTiming is the payload of GET /api/v1/sessions/{id}/timing.
// All durations are in milliseconds. *int64 fields are null when the
// underlying value is unknown (running, missing timestamp, parallel
// non-sub-agent call).
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
	MessageID  int64
	Ordinal    int64
	Timestamp  string
	HasToolUse bool
	DurationMs *int64
}

// CallRow is the per-tool_call row returned by the per-call SQL query.
// Both backends populate this and pass slices to AssembleTiming.
type CallRow struct {
	MessageID         int64
	ToolUseID         string
	ToolName          string
	Category          string
	SkillName         *string
	SubagentSessionID *string
	InputJSON         string
	DurationMs        *int64
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
		  END AS turn_duration_ms
		FROM (
		  SELECT
		    m.*,
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
	now := time.Now().UTC().Format(time.RFC3339)
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT
		  tc.message_id,
		  tc.tool_use_id,
		  tc.tool_name,
		  tc.category,
		  tc.skill_name,
		  tc.subagent_session_id,
		  tc.input_json,
		  CASE
		    WHEN tc.subagent_session_id IS NOT NULL
		         AND s_sub.started_at IS NOT NULL THEN
		      CAST(
		        ROUND(
		          (julianday(COALESCE(s_sub.ended_at, ?))
		           - julianday(s_sub.started_at)) * 86400000
		        ) AS INTEGER
		      )
		    ELSE NULL
		  END AS subagent_duration_ms
		FROM tool_calls tc
		LEFT JOIN sessions s_sub
		  ON s_sub.id = tc.subagent_session_id
		WHERE tc.session_id = ?
		ORDER BY tc.message_id, tc.id
	`, now, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CallRow
	for rows.Next() {
		var r CallRow
		var toolUseID, inputJSON sql.NullString
		var skill, sub sql.NullString
		var subDur sql.NullInt64
		if err := rows.Scan(
			&r.MessageID, &toolUseID, &r.ToolName, &r.Category,
			&skill, &sub, &inputJSON, &subDur,
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
		if subDur.Valid {
			v := subDur.Int64
			r.DurationMs = &v
		}
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
		SessionID: sess.ID,
		Running:   running,
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

	// Group calls by message id.
	callsByMsg := map[int64][]CallTiming{}
	for _, r := range calls {
		c := CallTiming{
			ToolUseID:         r.ToolUseID,
			ToolName:          r.ToolName,
			Category:          r.Category,
			SkillName:         r.SkillName,
			SubagentSessionID: r.SubagentSessionID,
			DurationMs:        r.DurationMs,
			InputPreview:      makeInputPreview(r.Category, r.ToolName, r.InputJSON),
		}
		callsByMsg[r.MessageID] = append(callsByMsg[r.MessageID], c)
	}

	categoryTotals := map[string]*CategoryTotal{}
	var slowest *CallTiming

	for _, t := range turns {
		if !t.HasToolUse {
			continue
		}
		out.TurnCount++
		turnCalls := callsByMsg[t.MessageID]

		for i := range turnCalls {
			turnCalls[i].IsParallel = len(turnCalls) > 1
			// Solo non-sub-agent: propagate the turn's duration to the
			// call. Per spec: DurationMs is null only for parallel
			// non-sub-agent siblings; solo calls and sub-agents always
			// have a duration.
			if !turnCalls[i].IsParallel &&
				turnCalls[i].SubagentSessionID == nil &&
				turnCalls[i].DurationMs == nil {
				turnCalls[i].DurationMs = t.DurationMs
			}
			if turnCalls[i].SubagentSessionID != nil {
				out.SubagentCount++
			}
			if turnCalls[i].DurationMs != nil {
				if slowest == nil ||
					*turnCalls[i].DurationMs > *slowest.DurationMs {
					c := turnCalls[i]
					slowest = &c
				}
			}
		}
		out.ToolCallCount += len(turnCalls)

		attribution := attributeTurnGo(t.DurationMs, turnCalls)
		bucket(
			categoryTotals,
			attribution.PrimaryCategory,
			attribution.RemainderMs,
			len(turnCalls),
		)
		for _, sa := range attribution.SubagentDurations {
			bucket(categoryTotals, "Task", sa, 1)
		}

		out.Turns = append(out.Turns, TurnTiming{
			MessageID:       t.MessageID,
			Ordinal:         int(t.Ordinal),
			StartedAt:       t.Timestamp,
			DurationMs:      t.DurationMs,
			PrimaryCategory: attribution.PrimaryCategory,
			Calls:           turnCalls,
		})
		if t.DurationMs != nil {
			out.ToolDurationMs += *t.DurationMs
		}
	}
	out.SlowestCall = slowest

	for _, total := range categoryTotals {
		out.ByCategory = append(out.ByCategory, *total)
	}
	sort.Slice(out.ByCategory, func(i, j int) bool {
		return out.ByCategory[i].DurationMs >
			out.ByCategory[j].DurationMs
	})

	return out
}

// turnAttribution is the result of attributeTurnGo. RemainderMs is the
// portion of the turn's duration attributed to PrimaryCategory after
// subtracting the union of any sub-agent durations. SubagentDurations
// holds each sub-agent's exact duration so the caller can attribute
// them individually to "Task".
type turnAttribution struct {
	PrimaryCategory   string
	RemainderMs       int64
	SubagentDurations []int64
}

// attributeTurnGo is a Go port of attributeTurn from
// frontend/src/lib/utils/categoryAttribution.ts. It computes the
// turn's primary non-sub-agent category and the remainder duration
// after subtracting the union of sub-agent ranges.
//
// v1 approximation: the union of sub-agent ranges is computed as
// max(durations) instead of an exact interval merge. This is exact
// for the common case (one sub-agent per turn) and under-estimates
// for rare parallel sub-agents that don't fully overlap. To get exact
// union, extend the call query to return started_at/ended_at and
// merge intervals; see the spec for context.
func attributeTurnGo(
	turnDur *int64, calls []CallTiming,
) turnAttribution {
	if turnDur == nil {
		return turnAttribution{PrimaryCategory: "Mixed"}
	}
	var subTotals []int64
	var nonSub []CallTiming
	for _, c := range calls {
		if c.SubagentSessionID != nil && c.DurationMs != nil {
			subTotals = append(subTotals, *c.DurationMs)
		} else {
			nonSub = append(nonSub, c)
		}
	}

	subUnion := int64(0)
	for _, d := range subTotals {
		if d > subUnion {
			subUnion = d
		}
	}
	remainder := max(*turnDur-subUnion, 0)
	// When a sub-agent's wall time meets or exceeds whatever non-
	// sub-agent work happened in the same turn, attribute the turn
	// to "Task". The user's mental model is "the sub-agent did the
	// work" — surfacing that on the turns lane is more honest than
	// letting a couple of fast parallel siblings win the count vote.
	if subUnion > 0 && subUnion >= remainder {
		return turnAttribution{
			PrimaryCategory:   "Task",
			RemainderMs:       remainder,
			SubagentDurations: subTotals,
		}
	}
	if len(nonSub) == 0 {
		return turnAttribution{
			PrimaryCategory:   "Mixed",
			RemainderMs:       remainder,
			SubagentDurations: subTotals,
		}
	}
	counts := map[string]int{}
	for _, c := range nonSub {
		counts[c.Category]++
	}
	primary := "Mixed"
	for cat, n := range counts {
		if n*2 > len(nonSub) {
			primary = cat
			break
		}
	}
	return turnAttribution{
		PrimaryCategory:   primary,
		RemainderMs:       remainder,
		SubagentDurations: subTotals,
	}
}

// bucket adds duration and call count to a category total in m,
// creating the entry on first use. Zero-or-negative durations are
// dropped so empty turns don't produce a row.
func bucket(
	m map[string]*CategoryTotal,
	cat string, dur int64, callCount int,
) {
	if dur <= 0 {
		return
	}
	t, ok := m[cat]
	if !ok {
		t = &CategoryTotal{Category: cat}
		m[cat] = t
	}
	t.DurationMs += dur
	t.CallCount += callCount
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
	if r := []rune(raw); len(r) > maxLen {
		raw = string(r[:maxLen]) + "…"
	}
	return raw
}
