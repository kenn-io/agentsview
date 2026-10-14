package db

import (
	"container/heap"
	"container/list"
	"context"
	"encoding/json/v2"
	"fmt"
	"sort"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/activity"
)

var activityMemoMaxCandidates = 200_000

// A full 200K-candidate, 4,096-entry cache measured 64.3 MiB of HeapInuse.
var activityMemoMaxEntries = 4_096

type activityMemoHits [][]activity.IntervalCandidate

func (h *activityMemoHits) Len() int           { return len(*h) }
func (h *activityMemoHits) Less(i, j int) bool { return activity.CandidateLess((*h)[i][0], (*h)[j][0]) }
func (h *activityMemoHits) Swap(i, j int)      { (*h)[i], (*h)[j] = (*h)[j], (*h)[i] }
func (h *activityMemoHits) Push(v any)         { *h = append(*h, v.([]activity.IntervalCandidate)) }
func (h *activityMemoHits) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}

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
			m.rows -= len(old.Value.(activityMemoEntry).candidates)
			m.order.Remove(old)
			delete(m.entries, entry.key)
		}
	}
	for _, entry := range entries {
		m.entries[entry.key] = m.order.PushBack(entry)
		m.rows += len(entry.candidates)
	}
	for m.rows > activityMemoMaxCandidates || len(m.entries) > activityMemoMaxEntries {
		old := m.order.Front()
		e := old.Value.(activityMemoEntry)
		delete(m.entries, e.key)
		m.rows -= len(e.candidates)
		m.order.Remove(old)
	}
}

// activityReportCandidateSource reuses pairing for unchanged session windows.
func (db *DB) activityReportCandidateSource(ids []string, q activity.Query) activity.CandidateSource {
	return func(ctx context.Context, yield func(activity.IntervalCandidate) error) error {
		if len(ids) == 0 {
			return nil
		}
		m := &db.activityMemo
		m.mu.Lock()
		generation := m.generation
		m.mu.Unlock()
		encoded, err := json.Marshal(ids)
		if err != nil {
			return fmt.Errorf("encoding activity report session IDs: %w", err)
		}
		rows, err := db.getReader().QueryContext(ctx, `SELECT id, transcript_revision || '|' || created_at FROM sessions WHERE id IN (SELECT value FROM json_each(?))`, string(encoded))
		if err != nil {
			return fmt.Errorf("querying activity report session versions: %w", err)
		}
		defer rows.Close()
		versions := make(map[string]string, len(ids))
		for rows.Next() {
			var id, version string
			if err := rows.Scan(&id, &version); err != nil {
				return fmt.Errorf("scanning activity report session version: %w", err)
			}
			versions[id] = version
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return fmt.Errorf("iterating activity report session versions: %w", err)
		}
		lower := q.RangeStart.Add(-time.Duration(q.GapCapSeconds) * time.Second).UnixMicro()
		upper := q.EffectiveEnd.UnixMicro()
		var hits activityMemoHits
		var misses []string
		buffered := 0
		m.mu.Lock()
		for _, id := range ids {
			element := m.entries[activityMemoKey{id, lower}]
			if element != nil && generation == m.generation {
				e := element.Value.(activityMemoEntry)
				if version, exists := versions[id]; exists && e.version == version && !q.EffectiveEnd.After(e.upper) {
					n := sort.Search(len(e.candidates), func(i int) bool { return e.candidates[i].Start.UnixMicro() >= upper })
					if n > 0 {
						hits = append(hits, e.candidates[:n])
					}
					buffered += len(e.candidates)
					m.order.MoveToBack(element)
					continue
				}
			}
			misses = append(misses, id)
		}
		m.mu.Unlock()
		heap.Init(&hits)
		wide := q
		if wide.RangeEnd.After(wide.EffectiveEnd) {
			wide.EffectiveEnd = wide.RangeEnd
		}
		retained := misses[:min(len(misses), activityMemoMaxEntries)]
		buffers := make(map[string][]activity.IntervalCandidate, len(retained))
		for _, id := range retained {
			buffers[id] = nil
		}
		source := func(ctx context.Context, emit func(activity.IntervalCandidate) error) error {
			if len(misses) == 0 {
				return nil
			}
			return db.activityReportCandidateSQL(misses, wide)(ctx, func(c activity.IntervalCandidate) error {
				if buffer, ok := buffers[c.SessionID]; ok {
					if buffered == activityMemoMaxCandidates {
						// Skip storing reports that cannot fit to avoid cache churn.
						buffers = nil
					} else {
						buffers[c.SessionID] = append(buffer, c)
						buffered++
					}
				}
				if c.Start.UnixMicro() < upper {
					return emit(c)
				}
				return nil
			})
		}
		emitHits := func(before *activity.IntervalCandidate) error {
			for len(hits) > 0 && (before == nil || activity.CandidateLess(hits[0][0], *before)) {
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := yield(hits[0][0]); err != nil {
					return err
				}
				hits[0] = hits[0][1:]
				if len(hits[0]) == 0 {
					heap.Pop(&hits)
				} else {
					heap.Fix(&hits, 0)
				}
			}
			return nil
		}
		if err := source(ctx, func(c activity.IntervalCandidate) error {
			if err := emitHits(&c); err != nil {
				return err
			}
			return yield(c)
		}); err != nil {
			return err
		}
		if err := emitHits(nil); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if buffers != nil {
			entries := make([]activityMemoEntry, 0, len(retained))
			for _, id := range retained {
				if version, exists := versions[id]; exists {
					entries = append(entries, activityMemoEntry{activityMemoKey{id, lower}, version, wide.EffectiveEnd, buffers[id]})
				}
			}
			m.store(generation, entries...)
		}
		return nil
	}
}
