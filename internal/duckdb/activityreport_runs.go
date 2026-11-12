package duckdb

import (
	"container/list"
	"context"
	"fmt"
	"slices"
	"sync"
	"time"
	"unsafe"

	"go.kenn.io/agentsview/internal/activity"
)

// activityRunsMaxBytes bounds the kept runs and counted messages. A report
// over every session in a long range must find them all kept: a bound below
// what a report reads evicts each session before the next report reaches it.
var activityRunsMaxBytes int64 = 128 << 20

type activityRunsKey struct {
	id     string
	gapCap float64
}

// activityRunsEntry keeps one session's runs and counted message times, as
// microseconds in order, at one push fingerprint.
type activityRunsEntry struct {
	key               activityRunsKey
	version           string
	runs              activity.SessionRuns
	users, assistants []int64
}

func (e activityRunsEntry) bytes() int64 {
	// 64 bytes stand in for the map entry and the list element.
	return int64(unsafe.Sizeof(e)) + int64(len(e.key.id)+len(e.version)) + e.runs.Bytes() +
		8*int64(len(e.users)+len(e.assistants)) + 64
}

// activityRunsMemo keeps sessions' runs across mirror replacements: the push
// fingerprint changes whenever a session's messages or tool events do.
type activityRunsMemo struct {
	mu      sync.Mutex
	entries map[activityRunsKey]*list.Element
	order   list.List
	bytes   int64
}

func (m *activityRunsMemo) lookup(
	ids []string, versions map[string]string, gapCap float64,
) (map[string]activityRunsEntry, []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := make(map[string]activityRunsEntry, len(ids))
	var misses []string
	for _, id := range ids {
		if element := m.entries[activityRunsKey{id, gapCap}]; element != nil {
			if e := element.Value.(activityRunsEntry); e.version == versions[id] {
				kept[id] = e
				m.order.MoveToBack(element)
				continue
			}
		}
		misses = append(misses, id)
	}
	return kept, misses
}

func (m *activityRunsMemo) store(entries []activityRunsEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entries == nil {
		m.entries = make(map[activityRunsKey]*list.Element)
	}
	for _, entry := range entries {
		if old := m.entries[entry.key]; old != nil {
			m.bytes -= old.Value.(activityRunsEntry).bytes()
			m.order.Remove(old)
		}
		m.entries[entry.key] = m.order.PushBack(entry)
		m.bytes += entry.bytes()
	}
	for m.bytes > activityRunsMaxBytes {
		old := m.order.Front()
		e := old.Value.(activityRunsEntry)
		delete(m.entries, e.key)
		m.bytes -= e.bytes()
		m.order.Remove(old)
	}
}

// activityReportRuns returns the sessions' runs clipped to q, in start
// order, and the kept entries that hold their counted messages. Sessions
// the memo holds at their current push fingerprint are not paired again;
// one pairing scan over all time covers the rest.
func (s *Store) activityReportRuns(
	ctx context.Context, ids []string, q activity.Query,
) ([]activity.Run, map[string]activityRunsEntry, error) {
	if len(ids) == 0 {
		return nil, nil, nil
	}
	// A replacement between this read and the pairing scan keeps the new
	// mirror's runs under the old fingerprint, which a later read reaches
	// only if the session returns to the old content.
	versions, err := s.activityRunVersions(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	sessions, misses := s.activityRuns.lookup(ids, versions, q.GapCapSeconds)
	if len(misses) > 0 {
		derived, err := s.activitySessionRuns(ctx, misses, q.GapCapSeconds)
		if err != nil {
			return nil, nil, err
		}
		index := make(map[string]int, len(misses))
		entries := make([]activityRunsEntry, len(misses))
		for i, id := range misses {
			index[id] = i
			entries[i] = activityRunsEntry{
				key: activityRunsKey{id, q.GapCapSeconds}, version: versions[id], runs: derived[id],
			}
		}
		err = s.scanActivityCountedMessages(ctx, misses, func(id, role string, at time.Time) {
			if i, ok := index[id]; ok {
				if role == "user" {
					entries[i].users = append(entries[i].users, at.UnixMicro())
				} else {
					entries[i].assistants = append(entries[i].assistants, at.UnixMicro())
				}
			}
		})
		if err != nil {
			return nil, nil, err
		}
		keep := make([]activityRunsEntry, 0, len(misses))
		var bytes int64
		for _, e := range entries {
			slices.Sort(e.users)
			slices.Sort(e.assistants)
			e.users, e.assistants = slices.Clip(e.users), slices.Clip(e.assistants)
			sessions[e.key.id] = e
			if e.version != "" {
				keep = append(keep, e)
				bytes += e.bytes()
			}
		}
		// A report too large to keep would only evict the rest.
		if bytes <= activityRunsMaxBytes {
			s.activityRuns.store(keep)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	n := 0
	for _, e := range sessions {
		n += e.runs.Runs()
	}
	runs := make([]activity.Run, 0, n)
	for _, id := range ids {
		runs = sessions[id].runs.AppendClipped(runs, id, q.RangeStart, q.EffectiveEnd)
	}
	activity.SortRuns(runs)
	return runs, sessions, nil
}

// activityRunVersions returns the sessions' push fingerprints. An empty
// fingerprint marks a session whose runs are not kept.
func (s *Store) activityRunVersions(ctx context.Context, ids []string) (map[string]string, error) {
	rows, err := s.queryContext(ctx, `SELECT id, COALESCE(agentsview_push_fingerprint, '')
		FROM sessions WHERE id IN (SELECT unnest(?))`, ids)
	if err != nil {
		return nil, fmt.Errorf("querying duckdb activity session fingerprints: %w", err)
	}
	defer rows.Close()
	versions := make(map[string]string, len(ids))
	for rows.Next() {
		var id, version string
		if err := rows.Scan(&id, &version); err != nil {
			return nil, fmt.Errorf("scanning duckdb activity session fingerprint: %w", err)
		}
		versions[id] = version
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating duckdb activity session fingerprints: %w", err)
	}
	return versions, nil
}

// scanActivityCountedMessages passes add the sessions' counted messages: user
// and assistant messages with a timestamp that are neither system entries
// nor tool results.
func (s *Store) scanActivityCountedMessages(
	ctx context.Context, ids []string, add func(sessionID, role string, at time.Time),
) error {
	rows, err := s.queryContext(ctx, `
		SELECT session_id, role, timestamp FROM messages
		WHERE session_id IN (SELECT unnest(?))
			AND role IN ('user', 'assistant') AND is_system = false
			AND source_subtype <> 'tool_result' AND timestamp IS NOT NULL`, ids)
	if err != nil {
		return fmt.Errorf("querying duckdb activity counted messages: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sessionID, role string
		var timestamp time.Time
		if err := rows.Scan(&sessionID, &role, &timestamp); err != nil {
			return fmt.Errorf("scanning duckdb activity counted messages: %w", err)
		}
		add(sessionID, role, timestamp)
	}
	return rows.Err()
}
