package db

import (
	"container/list"
	"context"
	"encoding/json/v2"
	"sort"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/activity"
)

var activityMemoMaxCandidates = 200_000

type activityMemoKey struct {
	id    string
	lower int64
}

type activityMemoEntry struct {
	key        activityMemoKey
	version    string
	upper      time.Time
	candidates []activity.IntervalCandidate
}

type activityCandidateMemo struct {
	mu         sync.Mutex
	generation uint64
	entries    map[activityMemoKey]*list.Element
	order      list.List
	rows       int
}

func (m *activityCandidateMemo) swap(swap func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	swap()
	m.generation++
	m.entries = nil
	m.order.Init()
	m.rows = 0
}

func (m *activityCandidateMemo) store(generation uint64, entry activityMemoEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if generation != m.generation || len(entry.candidates)+1 > activityMemoMaxCandidates {
		return
	}
	if m.entries == nil {
		m.entries = make(map[activityMemoKey]*list.Element)
	}
	if old := m.entries[entry.key]; old != nil {
		m.rows -= len(old.Value.(activityMemoEntry).candidates) + 1
		m.order.Remove(old)
	}
	m.entries[entry.key] = m.order.PushBack(entry)
	m.rows += len(entry.candidates) + 1
	for m.rows > activityMemoMaxCandidates {
		old := m.order.Front()
		e := old.Value.(activityMemoEntry)
		delete(m.entries, e.key)
		m.rows -= len(e.candidates) + 1
		m.order.Remove(old)
	}
}

func (db *DB) activityReportCandidateSource(ids []string, q activity.Query) activity.CandidateSource {
	return func(ctx context.Context, yield func(activity.IntervalCandidate) error) error {
		if len(ids) == 0 {
			return ctx.Err()
		}
		m := &db.activityMemo
		m.mu.Lock()
		generation := m.generation
		m.mu.Unlock()
		encoded, err := json.Marshal(ids)
		if err != nil {
			return err
		}
		rows, err := db.getReader().QueryContext(ctx, `SELECT id, transcript_revision || '|' || created_at FROM sessions WHERE id IN (SELECT value FROM json_each(?))`, string(encoded))
		if err != nil {
			return err
		}
		versions := make(map[string]string, len(ids))
		for rows.Next() {
			var id, version string
			if err := rows.Scan(&id, &version); err != nil {
				rows.Close()
				return err
			}
			versions[id] = version
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		lower := q.RangeStart.Add(-time.Duration(q.GapCapSeconds) * time.Second).UnixMicro()
		var hits []activity.IntervalCandidate
		var misses []string
		m.mu.Lock()
		for _, id := range ids {
			element := m.entries[activityMemoKey{id, lower}]
			if element != nil && generation == m.generation {
				e := element.Value.(activityMemoEntry)
				if version, exists := versions[id]; exists && e.version == version && !q.EffectiveEnd.After(e.upper) {
					n := sort.Search(len(e.candidates), func(i int) bool { return !e.candidates[i].Start.Before(q.EffectiveEnd) })
					hits = append(hits, e.candidates[:n]...)
					m.order.MoveToBack(element)
					continue
				}
			}
			misses = append(misses, id)
		}
		m.mu.Unlock()
		sort.SliceStable(hits, func(i, j int) bool { return activity.CandidateLess(hits[i], hits[j]) })
		wide := q
		if wide.RangeEnd.After(wide.EffectiveEnd) {
			wide.EffectiveEnd = wide.RangeEnd
		}
		buffers := make(map[string][]activity.IntervalCandidate, min(len(misses), activityMemoMaxCandidates))
		for _, id := range misses {
			if len(buffers) < activityMemoMaxCandidates {
				buffers[id] = nil
			}
		}
		buffered := len(buffers)
		source := func(ctx context.Context, emit func(activity.IntervalCandidate) error) error {
			return db.activityReportCandidateSQL(misses, wide)(ctx, func(c activity.IntervalCandidate) error {
				if buffer, ok := buffers[c.SessionID]; ok {
					buffers[c.SessionID] = append(buffer, c)
					buffered++
				}
				for buffered > activityMemoMaxCandidates {
					largest := ""
					for id, buffer := range buffers {
						if largest == "" || len(buffer) > len(buffers[largest]) {
							largest = id
						}
					}
					buffered -= len(buffers[largest]) + 1
					delete(buffers, largest)
				}
				if c.Start.Before(q.EffectiveEnd) {
					return emit(c)
				}
				return nil
			})
		}
		if err := activity.MergeCandidateSlice(hits, source)(ctx, yield); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		for id, buffer := range buffers {
			if version, exists := versions[id]; exists {
				m.store(generation, activityMemoEntry{activityMemoKey{id, lower}, version, wide.EffectiveEnd, buffer})
			}
		}
		return nil
	}
}
