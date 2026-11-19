package db

import (
	"container/list"
	"context"
	"encoding/json/v2"
	"fmt"
	"slices"
	"sync"
	"time"
	"unsafe"

	"go.kenn.io/agentsview/internal/activity"
)

// activityMemoMaxBytes bounds the kept runs and counted messages. A
// report over every session in a long range must find them all kept: a
// bound below what a report reads evicts each session before the next
// report reaches it. A month of 100,000 sessions takes about a quarter of it.
var activityMemoMaxBytes int64 = 128 << 20

type activityMemoKey struct {
	id    string
	lower int64
}

// activityMemoEntry keeps one session's activity for a pairing window that
// starts at key.lower and ends at upper: its runs and its counted messages.
type activityMemoEntry struct {
	key     activityMemoKey
	version string
	upper   time.Time
	runs    activity.SessionRuns
	counted activityMemoCounted
}

func (e activityMemoEntry) bytes() int64 {
	// 64 bytes stand in for the map entry and the list element.
	return int64(unsafe.Sizeof(e)) + int64(len(e.key.id)+len(e.version)) + e.runs.Bytes() +
		e.counted.bytes() + 64
}

// activityMemoCounted keeps a window's counted messages. A message stored
// at a whole microsecond with an offset from -13:59 to +14:00 keeps only its
// time: for it, the padded stored-string bounds of the message counter pick
// the same messages as the report range does. Every other message keeps its
// stored timestamp.
type activityMemoCounted struct {
	users, assistants []int64
	other             []activityCountedMessage
}

func (c *activityMemoCounted) add(m activityCountedMessage) {
	at, err := parseTimestamp(m.timestamp)
	if err != nil {
		// The message counter cannot count it either.
		return
	}
	_, offset := at.Zone()
	if at.Nanosecond()%1000 != 0 || offset < -(13*3600+59*60) || offset > 14*3600 {
		c.other = append(c.other, m)
		return
	}
	if m.user {
		c.users = append(c.users, at.UnixMicro())
	} else {
		c.assistants = append(c.assistants, at.UnixMicro())
	}
}

func (c *activityMemoCounted) finish() activityMemoCounted {
	slices.Sort(c.users)
	slices.Sort(c.assistants)
	return activityMemoCounted{
		users: slices.Clip(c.users), assistants: slices.Clip(c.assistants), other: slices.Clip(c.other),
	}
}

func (c *activityMemoCounted) bytes() int64 {
	n := 8 * int64(len(c.users)+len(c.assistants))
	for _, m := range c.other {
		n += int64(unsafe.Sizeof(m)) + int64(len(m.timestamp))
	}
	return n
}

type activityCandidateMemo struct {
	mu         sync.Mutex
	generation uint64
	entries    map[activityMemoKey]*list.Element
	order      list.List
	bytes      int64
}

func (m *activityCandidateMemo) swap(swap func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	swap()
	m.generation++
	m.entries = nil
	m.order.Init()
	m.bytes = 0
}

func (m *activityCandidateMemo) store(generation uint64, entries ...activityMemoEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if generation != m.generation {
		return
	}
	if m.entries == nil {
		m.entries = make(map[activityMemoKey]*list.Element)
	}
	for _, entry := range entries {
		if old := m.entries[entry.key]; old != nil {
			m.bytes -= old.Value.(activityMemoEntry).bytes()
			m.order.Remove(old)
			delete(m.entries, entry.key)
		}
	}
	for _, entry := range entries {
		m.entries[entry.key] = m.order.PushBack(entry)
		m.bytes += entry.bytes()
	}
	for m.bytes > activityMemoMaxBytes {
		old := m.order.Front()
		e := old.Value.(activityMemoEntry)
		delete(m.entries, e.key)
		m.bytes -= e.bytes()
		m.order.Remove(old)
	}
}

// activityReportCandidateSource streams the sessions' candidates that start
// in q's pairing window. Reports sweep runs instead; see activityReportRuns.
func (db *DB) activityReportCandidateSource(ids []string, q activity.Query) activity.CandidateSource {
	return func(ctx context.Context, yield func(activity.IntervalCandidate) error) error {
		if len(ids) == 0 {
			return nil
		}
		upper := q.EffectiveEnd.UnixMicro()
		return db.activityReportCandidateSQL(ids, activityPairingWindow(q), nil)(ctx, func(c activity.IntervalCandidate) error {
			if c.Start.UnixMicro() < upper {
				return yield(c)
			}
			return nil
		})
	}
}

// activityPairingWindow widens q's pairing window to its range end, so the
// runs a window keeps answer every effective end inside the range.
func activityPairingWindow(q activity.Query) activity.Query {
	if q.RangeEnd.After(q.EffectiveEnd) {
		q.EffectiveEnd = q.RangeEnd
	}
	return q
}

// activityReportRuns returns the sessions' runs clipped to q, in start
// order, and their counted messages. Sessions the memo holds for this
// window and version are not paired again; one pairing scan covers the
// rest.
func (db *DB) activityReportRuns(
	ctx context.Context, ids []string, q activity.Query,
) ([]activity.Run, map[string]activityMemoCounted, error) {
	if len(ids) == 0 {
		return nil, nil, nil
	}
	m := &db.activityMemo
	generation, versions, err := db.activityMemoVersions(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	lower := q.RangeStart.Add(-time.Duration(q.GapCapSeconds) * time.Second).UnixMicro()
	window := activityPairingWindow(q)
	sessions := make(map[string]activityMemoEntry, len(ids))
	var misses []string
	m.mu.Lock()
	for _, id := range ids {
		if element := m.entries[activityMemoKey{id, lower}]; element != nil && generation == m.generation {
			e := element.Value.(activityMemoEntry)
			if version, exists := versions[id]; exists && e.version == version && !q.EffectiveEnd.After(e.upper) {
				sessions[id] = e
				m.order.MoveToBack(element)
				continue
			}
		}
		misses = append(misses, id)
	}
	m.mu.Unlock()
	if len(misses) > 0 {
		index := make(map[string]int, len(misses))
		counted := make([]activityMemoCounted, len(misses))
		for i, id := range misses {
			index[id] = i
		}
		terminal, pairs, err := db.readActivityCandidates(ctx, misses, window, func(id string, message activityCountedMessage) {
			if i, ok := index[id]; ok {
				counted[i].add(message)
			}
		})
		if err != nil {
			return nil, nil, err
		}
		derived, err := activity.DeriveSessionRunsInParallel(ctx, misses, q.GapCapSeconds,
			activity.GroupSessionCandidates(misses, pairs, terminal))
		if err != nil {
			return nil, nil, err
		}
		entries := make([]activityMemoEntry, 0, len(misses))
		var bytes int64
		for i, id := range misses {
			entry := activityMemoEntry{
				key: activityMemoKey{id, lower}, version: versions[id], upper: window.EffectiveEnd,
				runs: derived[id], counted: counted[i].finish(),
			}
			sessions[id] = entry
			if _, exists := versions[id]; exists {
				entries = append(entries, entry)
				bytes += entry.bytes()
			}
		}
		// A report too large to keep would only evict the rest.
		if bytes <= activityMemoMaxBytes {
			m.store(generation, entries...)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	// The candidate stream leaves out candidates that start at or after the
	// effective end floored to microseconds.
	before := time.UnixMicro(q.EffectiveEnd.UnixMicro()).UTC()
	n := 0
	for _, e := range sessions {
		n += e.runs.Runs()
	}
	runs := make([]activity.Run, 0, n)
	counts := make(map[string]activityMemoCounted, len(ids))
	for _, id := range ids {
		e := sessions[id]
		runs = e.runs.AppendClippedBefore(runs, id, q.RangeStart, q.EffectiveEnd, before)
		counts[id] = e.counted
	}
	activity.SortRuns(runs)
	return runs, counts, nil
}

// activityMemoVersions returns the memo generation and the sessions' current
// versions. Read the generation first so a reader swap during the query
// invalidates what the caller stores or reads.
func (db *DB) activityMemoVersions(
	ctx context.Context, ids []string,
) (uint64, map[string]string, error) {
	m := &db.activityMemo
	m.mu.Lock()
	generation := m.generation
	m.mu.Unlock()
	encoded, err := json.Marshal(ids)
	if err != nil {
		return 0, nil, fmt.Errorf("encoding activity report session IDs: %w", err)
	}
	rows, err := db.getReader().QueryContext(ctx, `SELECT id, transcript_revision || '|' || created_at FROM sessions WHERE id IN (SELECT value FROM json_each(?))`, string(encoded))
	if err != nil {
		return 0, nil, fmt.Errorf("querying activity report session versions: %w", err)
	}
	defer rows.Close()
	versions := make(map[string]string, len(ids))
	for rows.Next() {
		var id, version string
		if err := rows.Scan(&id, &version); err != nil {
			return 0, nil, fmt.Errorf("scanning activity report session version: %w", err)
		}
		versions[id] = version
	}
	if err := rows.Err(); err != nil {
		return 0, nil, fmt.Errorf("iterating activity report session versions: %w", err)
	}
	return generation, versions, nil
}
