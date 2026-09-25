package clickhouse

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"
	"unique"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/ext"

	"go.kenn.io/agentsview/internal/activity"
)

// clickActivityMessage is one message's pairing input. Role and model
// repeat across messages, so they are interned handles. A zero ts marks a
// message without a timestamp: ClickHouse cannot store year 1.
type clickActivityMessage struct {
	ordinal     int
	ts          time.Time
	role, model unique.Handle[string]
}

var (
	assistantRole = unique.Make("assistant")
	noModel       = unique.Make("")
)

func (m clickActivityMessage) stamped() bool { return !m.ts.IsZero() }

type clickActivityTerminal struct {
	ordinal, callIndex, eventIndex int
	ts                             time.Time
}

type clickOrderedCandidate struct {
	activity.IntervalCandidate
	callIndex, eventIndex int
}

// activitySessionInputs are one session's pairing inputs: every message
// in ordinal order and every terminal tool event with a timestamp in time
// order. They depend only on the session's push version, not on the query.
type activitySessionInputs struct {
	pushVersion uint64
	messages    []clickActivityMessage
	events      []clickActivityTerminal
}

// activitySessionMemo keeps pairing inputs per session and push version, so
// a report re-reads only the sessions a push changed.
type activitySessionMemo struct {
	mu      sync.Mutex
	entries map[string]activitySessionInputs
	order   []string
}

const activitySessionMemoLimit = 8000

func (m *activitySessionMemo) lookup(id string, pushVersion uint64) (activitySessionInputs, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[id]
	if !ok || entry.pushVersion != pushVersion {
		return activitySessionInputs{}, false
	}
	return entry, true
}

func (m *activitySessionMemo) store(id string, entry activitySessionInputs) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entries == nil {
		m.entries = map[string]activitySessionInputs{}
	}
	if _, ok := m.entries[id]; !ok {
		m.order = append(m.order, id)
		if len(m.order) > activitySessionMemoLimit {
			delete(m.entries, m.order[0])
			m.order = m.order[1:]
		}
	}
	m.entries[id] = entry
}

// activityReportPairs pairs tool events with the messages that follow them
// for the candidate sessions. versions gives each candidate's push version;
// sessions memoized at that version are not read again. Without versions
// every candidate is read through the candidates set.
func (s *Store) activityReportPairs(
	ctx context.Context, candidates chSessionSet, ids []string, versions map[string]uint64, q activity.Query,
) ([]activity.IntervalCandidate, []activity.IntervalCandidate, error) {
	lower := q.RangeStart.Add(-time.Duration(q.GapCapSeconds) * time.Second)
	end := q.EffectiveEnd
	inputs := make(map[string]activitySessionInputs, len(ids))
	var uncached []string
	for _, id := range ids {
		if version, ok := versions[id]; ok {
			if entry, hit := s.activitySessions.lookup(id, version); hit {
				inputs[id] = entry
				continue
			}
		}
		uncached = append(uncached, id)
	}
	if versions != nil {
		if len(uncached) == 0 {
			return s.pairActivitySessions(inputs, ids, lower, end, q)
		}
		table, err := ext.NewTable("activity_uncached_ids", ext.Column("id", "String"))
		if err != nil {
			return nil, nil, fmt.Errorf("creating activity pairing table: %w", err)
		}
		for _, id := range uncached {
			if err := table.Append(id); err != nil {
				return nil, nil, fmt.Errorf("adding activity pairing session: %w", err)
			}
		}
		ctx = chdriver.Context(ctx, chdriver.WithExternalTable(table))
		candidates = chSessionSet{body: "SELECT id FROM activity_uncached_ids"}
	}
	read, err := s.readActivitySessionInputs(ctx, candidates)
	if err != nil {
		return nil, nil, err
	}
	for _, id := range uncached {
		entry := read[id]
		if version, ok := versions[id]; ok {
			// Keep exact-length copies: the read's slices grew by doubling.
			entry.pushVersion = version
			entry.messages = slices.Clone(entry.messages)
			entry.events = slices.Clone(entry.events)
			s.activitySessions.store(id, entry)
		}
		inputs[id] = entry
	}
	if versions == nil {
		maps.Copy(inputs, read)
	}
	return s.pairActivitySessions(inputs, ids, lower, end, q)
}

// readActivitySessionInputs reads every message and every timestamped
// terminal tool event of the candidate sessions.
func (s *Store) readActivitySessionInputs(ctx context.Context, candidates chSessionSet) (map[string]activitySessionInputs, error) {
	s.activityInputQueries.Add(1)
	ctes := "WITH candidate_sessions AS (" + candidates.body + ") "
	const settings = " SETTINGS optimize_move_to_prewhere_if_final=1"
	rows, err := s.queryContext(ctx, ctes+`SELECT session_id,ordinal,timestamp,role,model FROM messages
		WHERE session_id IN (SELECT id FROM candidate_sessions) ORDER BY session_id,ordinal`+settings, candidates.args...)
	if err != nil {
		return nil, fmt.Errorf("querying pairing messages: %w", err)
	}
	defer rows.Close()
	read := map[string]activitySessionInputs{}
	// Roles and models take few values; look each up in the global table once.
	handles := map[string]unique.Handle[string]{}
	handle := func(v string) unique.Handle[string] {
		h, ok := handles[v]
		if !ok {
			h = unique.Make(v)
			handles[v] = h
		}
		return h
	}
	for rows.Next() {
		var id, role, model string
		var ordinal int
		var ts sql.NullTime
		if err := rows.Scan(&id, &ordinal, &ts, &role, &model); err != nil {
			return nil, fmt.Errorf("scanning pairing message: %w", err)
		}
		entry := read[id]
		m := clickActivityMessage{ordinal: ordinal, role: handle(role), model: handle(model)}
		if ts.Valid {
			m.ts = ts.Time
		}
		entry.messages = append(entry.messages, m)
		read[id] = entry
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	rows, err = s.queryContext(ctx, ctes+`SELECT session_id,tool_call_message_ordinal,call_index,event_index,timestamp FROM tool_result_events
		WHERE session_id IN (SELECT id FROM candidate_sessions) AND source='tool_execution' AND status IN ('completed','errored')
		AND timestamp IS NOT NULL ORDER BY session_id,timestamp,call_index,event_index`+settings, slices.Clone(candidates.args)...)
	if err != nil {
		return nil, fmt.Errorf("querying pairing tool events: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var e clickActivityTerminal
		if err := rows.Scan(&id, &e.ordinal, &e.callIndex, &e.eventIndex, &e.ts); err != nil {
			return nil, fmt.Errorf("scanning pairing tool event: %w", err)
		}
		entry := read[id]
		entry.events = append(entry.events, e)
		read[id] = entry
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return read, rows.Close()
}

// pairActivitySessions runs the pairing over the sessions' inputs. Events
// before lower are left out here rather than by the read, so the inputs
// stay query-independent. The memoized inputs are shared and only read.
func (s *Store) pairActivitySessions(
	inputs map[string]activitySessionInputs, ids []string, lower, end time.Time, q activity.Query,
) ([]activity.IntervalCandidate, []activity.IntervalCandidate, error) {
	// A DateTime64(6) comparison in the read saw the bound at microseconds.
	lower = lower.Truncate(time.Microsecond)
	gapCap := time.Duration(q.GapCapSeconds) * time.Second
	pairLower := q.RangeStart.Add(-gapCap)
	// A message pair starts at a stamped message inside the pairing window,
	// so their count bounds the pairs; reserving it avoids regrowing.
	pairs := 0
	for _, id := range ids {
		for _, m := range inputs[id].messages {
			if ts := m.ts.UTC(); m.stamped() && !ts.Before(pairLower) && ts.Before(q.EffectiveEnd) {
				pairs++
			}
		}
	}
	transcript := make([]activity.IntervalCandidate, 0, pairs)
	var out []clickOrderedCandidate
	var tree []int
	var priorAt []int
	for _, id := range ids {
		entry, ok := inputs[id]
		if !ok {
			continue
		}
		ms := entry.messages
		transcript = appendMessagePairs(transcript, id, ms, pairLower, q.EffectiveEnd)
		first := len(entry.events)
		for i, e := range entry.events {
			if !e.ts.Before(lower) {
				first = i
				break
			}
		}
		es := entry.events[first:]
		if len(es) == 0 {
			continue
		}
		// priorAt[i] is the latest assistant message with a model at or
		// before message i, or -1.
		priorAt = slices.Grow(priorAt[:0], len(ms))[:len(ms)]
		prior := -1
		lastStamped := -1
		for i := range ms {
			if ms[i].role == assistantRole && ms[i].model != noModel {
				prior = i
			}
			priorAt[i] = prior
			if ms[i].stamped() {
				lastStamped = i
			}
		}
		// Each node records the latest timestamp in its ordinal range. Search
		// left first to find the lowest eligible ordinal, even for clock reversals.
		// This takes linear memory and logarithmic work per tool event instead
		// of comparing every tool event with every message in its session.
		size := max(1, 4*len(ms))
		tree = slices.Grow(tree[:0], size)[:size]
		for i := range tree {
			tree[i] = -1
		}
		var build func(int, int, int) int
		build = func(node, lo, hi int) int {
			if lo == hi {
				return -1
			}
			if hi-lo == 1 {
				if ms[lo].stamped() {
					tree[node] = lo
				}
				return tree[node]
			}
			mid := (lo + hi) / 2
			a, b := build(node*2, lo, mid), build(node*2+1, mid, hi)
			if a == -1 || (b != -1 && ms[b].ts.After(ms[a].ts)) {
				a = b
			}
			tree[node] = a
			return a
		}
		build(1, 0, len(ms))
		var next func(int, int, int, int, time.Time) int
		next = func(node, lo, hi, start int, ts time.Time) int {
			if lo == hi || hi <= start || tree[node] == -1 || !ms[tree[node]].ts.After(ts) {
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
			if start > 0 && priorAt[start-1] != -1 {
				c.PriorModel = ms[priorAt[start-1]].model.Value()
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
			if i+1 < len(es) && (j == -1 || es[i+1].ts.Before(ms[j].ts)) {
				c.EndOrdinal, c.End, c.ClosingRole = es[i+1].ordinal, es[i+1].ts, "tool"
			} else if j != -1 {
				c.EndOrdinal, c.End, c.ClosingRole, c.ClosingModel = ms[j].ordinal, ms[j].ts, ms[j].role.Value(), ms[j].model.Value()
			} else {
				continue
			}
			appendCandidate(c, e, start)
		}
		if lastStamped != -1 {
			m := ms[lastStamped]
			for _, e := range es {
				if e.ts.After(m.ts) {
					appendCandidate(activity.IntervalCandidate{SessionID: id, StartOrdinal: m.ordinal, EndOrdinal: m.ordinal, Start: m.ts, End: e.ts, ClosingRole: "tool"}, e, lastStamped+1)
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
	if len(transcript) == 0 {
		// activity.PairActivityEvents returns nil for no pairs.
		transcript = nil
	}
	slices.SortFunc(transcript, func(a, b activity.IntervalCandidate) int {
		return cmp.Or(a.Start.Compare(b.Start), cmp.Compare(a.SessionID, b.SessionID),
			cmp.Compare(a.StartOrdinal, b.StartOrdinal))
	})
	return transcript, result, nil
}

// appendMessagePairs pairs each stamped message with the next stamped one
// in ordinal order, the way activity.PairActivityEvents pairs a session's
// transcript, without rendering and reparsing every timestamp.
func appendMessagePairs(
	out []activity.IntervalCandidate, id string, ms []clickActivityMessage, lower, calculationEnd time.Time,
) []activity.IntervalCandidate {
	lastModel := "unknown"
	previous := -1
	for i := range ms {
		// PairActivityEvents keeps only timestamps that render as RFC 3339.
		if !ms[i].stamped() || ms[i].ts.Year() < 0 || ms[i].ts.Year() > 9999 {
			continue
		}
		if previous == -1 {
			previous = i
			continue
		}
		prev, cur := ms[previous].ts.UTC(), ms[i].ts.UTC()
		if !prev.Before(lower) && prev.Before(calculationEnd) {
			out = append(out, activity.IntervalCandidate{
				SessionID:    id,
				StartOrdinal: ms[previous].ordinal,
				EndOrdinal:   ms[i].ordinal,
				Start:        prev,
				End:          cur,
				ClosingRole:  ms[i].role.Value(),
				ClosingModel: ms[i].model.Value(),
				PriorModel:   lastModel,
			})
		}
		if cur.After(prev) && ms[i].role == assistantRole && ms[i].model != noModel {
			lastModel = ms[i].model.Value()
		}
		previous = i
	}
	return out
}
