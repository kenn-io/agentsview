package clickhouse

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"

	"go.kenn.io/agentsview/internal/activity"
)

type clickActivityMessage struct {
	ordinal            int
	ts                 sql.NullTime
	role, model, prior string
}

type clickActivityTerminal struct {
	ordinal, callIndex, eventIndex int
	ts                             time.Time
}

type clickOrderedCandidate struct {
	activity.IntervalCandidate
	callIndex, eventIndex int
}

func (s *Store) activityReportPairs(ctx context.Context, candidates chSessionSet, q activity.Query) ([]activity.IntervalCandidate, []activity.IntervalCandidate, error) {
	lower := q.RangeStart.Add(-time.Duration(q.GapCapSeconds) * time.Second)
	end := q.EffectiveEnd
	var transcript []activity.ActivityEvent
	ctes := "WITH candidate_sessions AS (" + candidates.body + ") "
	const settings = " SETTINGS optimize_move_to_prewhere_if_final=1"
	rows, err := s.queryContext(ctx, ctes+`SELECT session_id,ordinal,timestamp,role,model FROM messages
		WHERE session_id IN (SELECT id FROM candidate_sessions) ORDER BY session_id,ordinal`+settings, candidates.args...)
	if err != nil {
		return nil, nil, fmt.Errorf("querying pairing messages: %w", err)
	}
	defer rows.Close()
	messages := make(map[string][]clickActivityMessage)
	for rows.Next() {
		var id string
		var m clickActivityMessage
		if err := rows.Scan(&id, &m.ordinal, &m.ts, &m.role, &m.model); err != nil {
			return nil, nil, fmt.Errorf("scanning pairing message: %w", err)
		}
		messages[id] = append(messages[id], m)
		if m.ts.Valid {
			transcript = append(transcript, activity.ActivityEvent{SessionID: id, Ordinal: m.ordinal, Timestamp: m.ts.Time.UTC().Format(time.RFC3339Nano), Role: m.role, Model: m.model})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}
	args := append(slices.Clone(candidates.args), lower.UTC().Format(time.RFC3339Nano))
	rows, err = s.queryContext(ctx, ctes+`SELECT session_id,tool_call_message_ordinal,call_index,event_index,timestamp FROM tool_result_events
		WHERE session_id IN (SELECT id FROM candidate_sessions) AND source='tool_execution' AND status IN ('completed','errored')
		AND timestamp IS NOT NULL AND timestamp >= `+chTimestampSQL+` ORDER BY session_id,timestamp,call_index,event_index`+settings, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("querying pairing tool events: %w", err)
	}
	defer rows.Close()
	events := make(map[string][]clickActivityTerminal)
	for rows.Next() {
		var id string
		var e clickActivityTerminal
		if err := rows.Scan(&id, &e.ordinal, &e.callIndex, &e.eventIndex, &e.ts); err != nil {
			return nil, nil, fmt.Errorf("scanning pairing tool event: %w", err)
		}
		events[id] = append(events[id], e)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}
	var out []clickOrderedCandidate
	for id, es := range events {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		ms := messages[id]
		if ms == nil {
			// A session can carry tool events without stamped messages.
			ms = []clickActivityMessage{}
		}
		prior := "unknown"
		lastStamped := -1
		for i := range ms {
			if ms[i].role == "assistant" && ms[i].model != "" {
				prior = ms[i].model
			}
			ms[i].prior = prior
			if ms[i].ts.Valid {
				lastStamped = i
			}
		}
		// Each node records the latest timestamp in its ordinal range. Search
		// left first to find the lowest eligible ordinal, even for clock reversals.
		// This takes linear memory and logarithmic work per tool event instead
		// of comparing every tool event with every message in its session.
		tree := slices.Repeat([]int{-1}, max(1, 4*len(ms)))
		var build func(int, int, int) int
		build = func(node, lo, hi int) int {
			if lo == hi {
				return -1
			}
			if hi-lo == 1 {
				if ms[lo].ts.Valid {
					tree[node] = lo
				}
				return tree[node]
			}
			mid := (lo + hi) / 2
			a, b := build(node*2, lo, mid), build(node*2+1, mid, hi)
			if a == -1 || (b != -1 && ms[b].ts.Time.After(ms[a].ts.Time)) {
				a = b
			}
			tree[node] = a
			return a
		}
		build(1, 0, len(ms))
		var next func(int, int, int, int, time.Time) int
		next = func(node, lo, hi, start int, ts time.Time) int {
			if lo == hi || hi <= start || tree[node] == -1 || !ms[tree[node]].ts.Time.After(ts) {
				return -1
			}
			if hi-lo == 1 {
				return lo
			}
			mid := (lo + hi) / 2
			if i := next(node*2, lo, mid, start, ts); i != -1 {
				return i
			}
			return next(node*2+1, mid, hi, start, ts)
		}
		appendCandidate := func(c activity.IntervalCandidate, e clickActivityTerminal, start int) {
			if !c.Start.Before(end) {
				return
			}
			c.PriorModel = "unknown"
			if start > 0 {
				c.PriorModel = ms[start-1].prior
			}
			out = append(out, clickOrderedCandidate{c, e.callIndex, e.eventIndex})
		}
		for i, e := range es {
			start, _ := slices.BinarySearchFunc(ms, e.ordinal, func(m clickActivityMessage, ordinal int) int { return cmp.Compare(m.ordinal, ordinal) })
			if start < len(ms) && ms[start].ordinal == e.ordinal {
				start++
			}
			j := next(1, 0, len(ms), start, e.ts)
			c := activity.IntervalCandidate{SessionID: id, StartOrdinal: e.ordinal, Start: e.ts}
			if i+1 < len(es) && (j == -1 || es[i+1].ts.Before(ms[j].ts.Time)) {
				c.EndOrdinal, c.End, c.ClosingRole = es[i+1].ordinal, es[i+1].ts, "tool"
			} else if j != -1 {
				c.EndOrdinal, c.End, c.ClosingRole, c.ClosingModel = ms[j].ordinal, ms[j].ts.Time, ms[j].role, ms[j].model
			} else {
				continue
			}
			appendCandidate(c, e, start)
		}
		if lastStamped != -1 {
			m := ms[lastStamped]
			for _, e := range es {
				if e.ts.After(m.ts.Time) {
					appendCandidate(activity.IntervalCandidate{SessionID: id, StartOrdinal: m.ordinal, EndOrdinal: m.ordinal, Start: m.ts.Time, End: e.ts, ClosingRole: "tool"}, e, lastStamped+1)
					break
				}
			}
		}
	}
	slices.SortFunc(out, func(a, b clickOrderedCandidate) int {
		return cmp.Or(a.Start.Compare(b.Start), cmp.Compare(a.SessionID, b.SessionID),
			cmp.Compare(a.StartOrdinal, b.StartOrdinal), cmp.Compare(a.callIndex, b.callIndex),
			cmp.Compare(a.eventIndex, b.eventIndex))
	})
	result := make([]activity.IntervalCandidate, len(out))
	for i, c := range out {
		result[i] = c.IntervalCandidate
	}
	return activity.PairActivityEvents(transcript, q.RangeStart, q.EffectiveEnd, time.Duration(q.GapCapSeconds)*time.Second), result, nil
}
