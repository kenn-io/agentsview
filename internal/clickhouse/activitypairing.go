package clickhouse

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"slices"
	"sync"
	"time"
	"unique"
	"unsafe"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"
	"golang.org/x/sync/errgroup"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/stringutil"
)

// clickActivityMessage is one message's pairing input. Role and model
// repeat across messages, so they are interned handles. Timestamps are
// kept as microseconds since the epoch, the precision of the UTC
// DateTime64(6) columns they are read from. counted marks a message the
// Activity message counts include: neither a system entry nor a tool result.
type clickActivityMessage struct {
	ordinal     int
	us          int64
	role, model unique.Handle[string]
	counted     bool
}

// noTimestamp marks a message without a timestamp.
const noTimestamp = math.MinInt64

var (
	assistantRole = unique.Make("assistant")
	userRole      = unique.Make("user")
	noModel       = unique.Make("")
)

func (m clickActivityMessage) stamped() bool { return m.us != noTimestamp }

func (m clickActivityMessage) at() time.Time { return time.UnixMicro(m.us).UTC() }

type clickActivityTerminal struct {
	ordinal, callIndex, eventIndex int
	us                             int64
}

func (e clickActivityTerminal) at() time.Time { return time.UnixMicro(e.us).UTC() }

type clickOrderedCandidate struct {
	activity.IntervalCandidate
	callIndex, eventIndex int
}

// activitySessionInputs are one session's pairing inputs: every message
// in ordinal order and every terminal tool event with a timestamp in time
// order. They depend only on the session's push version, not on the query.
type activitySessionInputs struct {
	messages []clickActivityMessage
	events   []clickActivityTerminal
}

// activitySessionDerived is what a report needs of one session at one push
// version and gap cap: its activity runs and the times of the messages the
// report counts. Neither depends on the report range.
type activitySessionDerived struct {
	pushVersion uint64
	gapCap      float64
	runs        activity.SessionRuns
	// users and assistants are the ascending Unix microsecond timestamps of
	// the counted user and assistant messages.
	users, assistants []int64
}

func (d activitySessionDerived) bytes(id string) int64 {
	// 48 bytes stand in for the map entry and the eviction order slot.
	return int64(unsafe.Sizeof(d)) + d.runs.Bytes() + 8*int64(len(d.users)+len(d.assistants)) +
		int64(len(id)) + 48
}

// activitySessionMemo keeps each session's derived activity per push
// version, so a report reads and pairs only the sessions a push changed.
type activitySessionMemo struct {
	mu      sync.Mutex
	entries map[string]activitySessionDerived
	order   []string
	bytes   int64
}

// activitySessionMemoBytes bounds the kept entries. A report over every
// session must find them all kept: a bound below what a report reads
// evicts each session before the next report reaches it, so every report
// reads them all again.
const activitySessionMemoBytes = 256 << 20

func (m *activitySessionMemo) lookup(id string, pushVersion uint64, gapCap float64) (activitySessionDerived, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[id]
	if !ok || entry.pushVersion != pushVersion || entry.gapCap != gapCap {
		return activitySessionDerived{}, false
	}
	return entry, true
}

func (m *activitySessionMemo) store(id string, entry activitySessionDerived) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entries == nil {
		m.entries = map[string]activitySessionDerived{}
	}
	if old, ok := m.entries[id]; ok {
		m.bytes -= old.bytes(id)
	} else {
		m.order = append(m.order, id)
	}
	m.entries[id] = entry
	m.bytes += entry.bytes(id)
	for m.bytes > activitySessionMemoBytes && len(m.order) > 1 {
		oldest := m.order[0]
		m.bytes -= m.entries[oldest].bytes(oldest)
		delete(m.entries, oldest)
		m.order = m.order[1:]
	}
}

// activityReportDerived returns the derived activity of the candidate
// sessions. versions gives each candidate's push version; sessions kept at
// that version and gap cap are not read again.
func (s *Store) activityReportDerived(
	ctx context.Context, ids []string, versions map[string]uint64, gapCap float64,
) (map[string]activitySessionDerived, error) {
	derived := make(map[string]activitySessionDerived, len(ids))
	var uncached []string
	for _, id := range ids {
		if entry, hit := s.activitySessions.lookup(id, versions[id], gapCap); hit {
			derived[id] = entry
			continue
		}
		uncached = append(uncached, id)
	}
	if len(uncached) == 0 {
		return derived, nil
	}
	inputs, err := s.readActivitySessionInputs(ctx, uncached)
	if err != nil {
		return nil, err
	}
	entries, err := deriveActivitySessions(ctx, uncached, inputs, gapCap)
	if err != nil {
		return nil, err
	}
	for i, id := range uncached {
		entry := entries[i]
		entry.pushVersion = versions[id]
		s.activitySessions.store(id, entry)
		derived[id] = entry
	}
	return derived, nil
}

// deriveActivitySessions derives each session in ids from its inputs in
// parallel. Each session's derivation reads only its own inputs, so the
// order the sessions are derived in does not change the result. The result
// is in the order of ids.
func deriveActivitySessions(
	ctx context.Context, ids []string, inputs map[string]activitySessionInputs, gapCap float64,
) ([]activitySessionDerived, error) {
	out := make([]activitySessionDerived, len(ids))
	if err := activity.EachInParallel(ctx, len(ids), func() *activityDeriver {
		return newActivityDeriver(gapCap)
	}, func(deriver *activityDeriver, i int) {
		out[i] = deriver.derive(ids[i], inputs[ids[i]])
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// activityPairingSliceSessions is the fewest sessions worth a slice of
// their own in a pairing read; see readActivitySessionInputs.
const activityPairingSliceSessions = 4096

// activityPairingMaxSlices caps the slices a pairing read runs at once. A
// report runs its pairing read beside the usage read's
// activityUsageMaxSlices slices, which usually finish last. On a year of
// real sessions, two slices made the pairing read ready about as soon as
// four did, and four made each usage query on the server about 40% slower.
const activityPairingMaxSlices = 2

// activitySessionMessages is one session's pairing messages in ordinal order.
type activitySessionMessages struct {
	id       string
	messages []clickActivityMessage
}

// readActivitySessionInputs reads every message and every timestamped
// terminal tool event of the sessions in ids. One goroutine scanning
// millions of message rows takes seconds while ClickHouse waits to send
// them, so the sessions are dealt into slices whose messages are read at
// once, beside the read of the tool events.
func (s *Store) readActivitySessionInputs(ctx context.Context, ids []string) (map[string]activitySessionInputs, error) {
	s.activityInputQueries.Add(1)
	read := make(map[string]activitySessionInputs, len(ids))
	if len(ids) == 0 {
		return read, nil
	}
	n := min(activityPairingMaxSlices, (len(ids)+activityPairingSliceSessions-1)/activityPairingSliceSessions)
	parts := make([][]string, n)
	for i, id := range ids {
		parts[i%n] = append(parts[i%n], id)
	}
	messages := make([][]activitySessionMessages, n)
	var events map[string][]clickActivityTerminal
	g, readCtx := errgroup.WithContext(ctx)
	for i, part := range parts {
		g.Go(func() error {
			var err error
			messages[i], err = s.readActivityMessages(readCtx, part)
			return err
		})
	}
	g.Go(func() error {
		var err error
		events, err = s.readActivityTerminalEvents(readCtx, ids)
		return err
	})
	if err := g.Wait(); err != nil {
		return nil, err
	}
	for _, part := range messages {
		for _, session := range part {
			read[session.id] = activitySessionInputs{messages: session.messages}
		}
	}
	for id, es := range events {
		entry := read[id]
		entry.events = es
		read[id] = entry
	}
	return read, nil
}

// activityPairingSettings lets ClickHouse filter before deduplicating
// under the connection's final=1.
const activityPairingSettings = " SETTINGS optimize_move_to_prewhere_if_final=1"

// readActivityMessages reads the pairing messages of the sessions in ids
// on the native connection, each session's in ordinal order.
func (s *Store) readActivityMessages(ctx context.Context, ids []string) ([]activitySessionMessages, error) {
	table, err := usageSessionListTable("activity_pairing_ids", ids)
	if err != nil {
		return nil, err
	}
	ctx = chdriver.Context(ctx, chdriver.WithExternalTable(table))
	rows, err := s.bulk.Query(ctx, `SELECT session_id,ordinal,timestamp,role,model,
		toBool(is_system = false AND source_subtype <> 'tool_result') FROM messages
		WHERE session_id IN (SELECT id FROM activity_pairing_ids) ORDER BY session_id,ordinal`+activityPairingSettings)
	if err != nil {
		return nil, fmt.Errorf("querying pairing messages: %w", err)
	}
	defer rows.Close()
	out := make([]activitySessionMessages, 0, len(ids))
	// Roles and models take few values; look each up in the global table once.
	handles := stringutil.Interner{}
	// The native API requires destinations that match the column types.
	var id, role, model string
	var ordinal int64
	var ts *time.Time
	var counted bool
	for rows.Next() {
		if err := rows.Scan(&id, &ordinal, &ts, &role, &model, &counted); err != nil {
			return nil, fmt.Errorf("scanning pairing message: %w", err)
		}
		// Rows arrive grouped by session.
		if len(out) == 0 || out[len(out)-1].id != id {
			out = append(out, activitySessionMessages{id: id})
		}
		m := clickActivityMessage{ordinal: int(ordinal), us: noTimestamp, role: handles.Intern(role), model: handles.Intern(model), counted: counted}
		if ts != nil {
			m.us = ts.UnixMicro()
		}
		session := &out[len(out)-1]
		session.messages = append(session.messages, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating pairing messages: %w", err)
	}
	return out, nil
}

// readActivityTerminalEvents reads the timestamped terminal tool events of
// the sessions in ids, each session's in time order.
func (s *Store) readActivityTerminalEvents(ctx context.Context, ids []string) (map[string][]clickActivityTerminal, error) {
	table, err := usageSessionListTable("activity_pairing_ids", ids)
	if err != nil {
		return nil, err
	}
	ctx = chdriver.Context(ctx, chdriver.WithExternalTable(table))
	rows, err := s.queryContext(ctx, `SELECT session_id,tool_call_message_ordinal,call_index,event_index,timestamp FROM tool_result_events
		WHERE session_id IN (SELECT id FROM activity_pairing_ids) AND source='tool_execution' AND status IN ('completed','errored')
		AND timestamp IS NOT NULL ORDER BY session_id,timestamp,call_index,event_index`+activityPairingSettings)
	if err != nil {
		return nil, fmt.Errorf("querying pairing tool events: %w", err)
	}
	defer rows.Close()
	read := map[string][]clickActivityTerminal{}
	var id string
	var session []clickActivityTerminal
	for rows.Next() {
		var next string
		var e clickActivityTerminal
		var ts time.Time
		if err := rows.Scan(&next, &e.ordinal, &e.callIndex, &e.eventIndex, &ts); err != nil {
			return nil, fmt.Errorf("scanning pairing tool event: %w", err)
		}
		// Rows arrive grouped by session.
		if next != id {
			if session != nil {
				read[id] = session
			}
			id, session = next, nil
		}
		e.us = ts.UnixMicro()
		session = append(session, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if session != nil {
		read[id] = session
	}
	return read, rows.Close()
}

// pairActivitySessions runs the pairing over the sessions' inputs and
// orders the pairs as the candidate stream emits them. Events before lower
// are left out here rather than by the read, so the inputs stay
// query-independent. The inputs are only read.
func (s *Store) pairActivitySessions(
	ctx context.Context, inputs map[string]activitySessionInputs, ids []string, lower, end time.Time, q activity.Query,
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
			if m.stamped() && !m.at().Before(pairLower) && m.at().Before(q.EffectiveEnd) {
				pairs++
			}
		}
	}
	transcript := make([]activity.IntervalCandidate, 0, pairs)
	var out []clickOrderedCandidate
	var pairer activityPairer
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		entry, ok := inputs[id]
		if !ok {
			continue
		}
		transcript, out = pairer.pair(id, entry, lower, pairLower, end, transcript, out)
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	slices.SortFunc(out, compareClickOrderedCandidates)
	result := make([]activity.IntervalCandidate, len(out))
	for i, c := range out {
		result[i] = c.IntervalCandidate
	}
	if len(transcript) == 0 {
		// activity.PairActivityEvents returns nil for no pairs.
		transcript = nil
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	slices.SortFunc(transcript, compareTranscriptCandidates)
	return transcript, result, nil
}

func compareTranscriptCandidates(a, b activity.IntervalCandidate) int {
	return cmp.Or(a.Start.Compare(b.Start), cmp.Compare(a.SessionID, b.SessionID),
		cmp.Compare(a.StartOrdinal, b.StartOrdinal))
}

func compareClickOrderedCandidates(a, b clickOrderedCandidate) int {
	return cmp.Or(compareTranscriptCandidates(a.IntervalCandidate, b.IntervalCandidate),
		cmp.Compare(a.callIndex, b.callIndex), cmp.Compare(a.eventIndex, b.eventIndex))
}

// activityPairer pairs one session at a time, reusing its scratch space.
type activityPairer struct {
	tree, priorAt []int
}

// pair appends one session's message pairs that start in [pairLower, end)
// to transcript, and its tool intervals that start before end to out. Tool
// events before eventLower are left out.
func (p *activityPairer) pair(
	id string, entry activitySessionInputs, eventLower, pairLower, end time.Time,
	transcript []activity.IntervalCandidate, out []clickOrderedCandidate,
) ([]activity.IntervalCandidate, []clickOrderedCandidate) {
	ms := entry.messages
	transcript = appendMessagePairs(transcript, id, ms, pairLower, end)
	first := len(entry.events)
	for i, e := range entry.events {
		if !e.at().Before(eventLower) {
			first = i
			break
		}
	}
	es := entry.events[first:]
	if len(es) == 0 {
		return transcript, out
	}
	// priorAt[i] is the latest assistant message with a model at or
	// before message i, or -1.
	p.priorAt = slices.Grow(p.priorAt[:0], len(ms))[:len(ms)]
	priorAt := p.priorAt
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
	p.tree = slices.Grow(p.tree[:0], size)[:size]
	tree := p.tree
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
		if a == -1 || (b != -1 && ms[b].us > ms[a].us) {
			a = b
		}
		tree[node] = a
		return a
	}
	build(1, 0, len(ms))
	var next func(int, int, int, int, int64) int
	next = func(node, lo, hi, start int, us int64) int {
		if lo == hi || hi <= start || tree[node] == -1 || ms[tree[node]].us <= us {
			return -1
		}
		if hi-lo == 1 {
			return lo
		}
		mid := (lo + hi) / 2
		if i := next(node*2, lo, mid, start, us); i != -1 {
			return i
		}
		return next(node*2+1, mid, hi, start, us)
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
		j := next(1, 0, len(ms), start, e.us)
		c := activity.IntervalCandidate{SessionID: id, StartOrdinal: e.ordinal, Start: e.at()}
		if i+1 < len(es) && (j == -1 || es[i+1].us < ms[j].us) {
			c.EndOrdinal, c.End, c.ClosingRole = es[i+1].ordinal, es[i+1].at(), "tool"
		} else if j != -1 {
			c.EndOrdinal, c.End, c.ClosingRole, c.ClosingModel = ms[j].ordinal, ms[j].at(), ms[j].role.Value(), ms[j].model.Value()
		} else {
			continue
		}
		appendCandidate(c, e, start)
	}
	if lastStamped != -1 {
		m := ms[lastStamped]
		for _, e := range es {
			if e.us > m.us {
				appendCandidate(activity.IntervalCandidate{SessionID: id, StartOrdinal: m.ordinal, EndOrdinal: m.ordinal, Start: m.at(), End: e.at(), ClosingRole: "tool"}, e, lastStamped+1)
				break
			}
		}
	}
	return transcript, out
}

// activityAllTime bounds a pairing that leaves no candidate out. The
// candidates a report range leaves out of its pairing are ones the
// aggregator rejects for that range anyway.
var activityAllTime = [2]time.Time{time.Unix(-1<<50, 0), time.Unix(1<<50, 0)}

// activityDeriver derives sessions' activity, reusing its scratch space.
type activityDeriver struct {
	gapCap     float64
	pairer     activityPairer
	runs       *activity.SessionRunsBuilder
	transcript []activity.IntervalCandidate
	terminal   []clickOrderedCandidate
}

func newActivityDeriver(gapCap float64) *activityDeriver {
	return &activityDeriver{gapCap: gapCap, runs: activity.NewSessionRunsBuilder(gapCap)}
}

// derive pairs one session over all time and reduces its candidates to
// runs, taking them in the order the report's candidate stream emits them.
func (d *activityDeriver) derive(id string, entry activitySessionInputs) activitySessionDerived {
	d.transcript, d.terminal = d.pairer.pair(id, entry, activityAllTime[0], activityAllTime[0], activityAllTime[1],
		d.transcript[:0], d.terminal[:0])
	slices.SortFunc(d.transcript, compareTranscriptCandidates)
	slices.SortFunc(d.terminal, compareClickOrderedCandidates)
	// activity.MergeCandidateSlice emits a message pair before a tool
	// interval with the same start and ordinal.
	terminal := d.terminal
	for _, c := range d.transcript {
		for len(terminal) > 0 && activity.CandidateLess(terminal[0].IntervalCandidate, c) {
			d.runs.Add(terminal[0].IntervalCandidate)
			terminal = terminal[1:]
		}
		d.runs.Add(c)
	}
	for _, c := range terminal {
		d.runs.Add(c.IntervalCandidate)
	}
	derived := activitySessionDerived{gapCap: d.gapCap, runs: d.runs.Finish()}
	for _, m := range entry.messages {
		if !m.counted || !m.stamped() {
			continue
		}
		switch m.role {
		case userRole:
			derived.users = append(derived.users, m.us)
		case assistantRole:
			derived.assistants = append(derived.assistants, m.us)
		}
	}
	slices.Sort(derived.users)
	slices.Sort(derived.assistants)
	derived.users = slices.Clip(derived.users)
	derived.assistants = slices.Clip(derived.assistants)
	return derived
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
		if !ms[i].stamped() || ms[i].at().Year() < 0 || ms[i].at().Year() > 9999 {
			continue
		}
		if previous == -1 {
			previous = i
			continue
		}
		prev, cur := ms[previous].at(), ms[i].at()
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
