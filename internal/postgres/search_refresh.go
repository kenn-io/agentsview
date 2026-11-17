package postgres

import (
	"context"
	"fmt"
	"math"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

type pgSearchKey struct {
	id            string
	priority, pos int
	endedAt       *time.Time
}
type pgSearchEntry struct {
	revision int64
	token    string
	keys     []pgSearchKey
}

func pgSearchEntryBytes(e pgSearchEntry) int64 {
	size := int64(len(e.token) + 64)
	for _, k := range e.keys {
		size += int64(len(k.id) + 80)
	}
	return size
}

func (s *Store) searchKept(ctx context.Context, view *Store, f db.SearchFilter) (db.SearchPage, error) {
	if db.StripFTSQuotes(db.PrepareFTSQuery(f.Query)) == "" {
		return db.SearchPage{}, nil
	}
	slotFilter := f
	slotFilter.Limit, slotFilter.Cursor = 0, 0
	slot := slotFilter.Slot()
	memo := &s.runtimeState().searchKept
	entries, ok := memo.Get(slot, "search")
	var entry pgSearchEntry
	after := int64(math.MaxInt64)
	if ok && len(entries) != 0 {
		entry = entries[0]
		after = entry.revision
	}
	through, changed, err := view.LoadUsageChanges(ctx, after)
	if err != nil {
		return db.SearchPage{}, err
	}
	if through == 0 {
		return view.search(ctx, f)
	}
	checkpoints, err := usageHistoryCheckpoints(ctx, view.pg, through, entry.revision)
	if err != nil {
		return db.SearchPage{}, err
	}
	reusable := entry.token != "" && entry.token == checkpoints[entry.revision] && through >= entry.revision
	keys := entry.keys
	if !reusable {
		keys, err = view.searchKeys(ctx, f, nil)
	} else if through > entry.revision && len(changed) > 0 {
		removed := make(map[string]bool, len(changed))
		for _, id := range changed {
			removed[id] = true
		}
		keys = make([]pgSearchKey, 0, len(entry.keys))
		for _, key := range entry.keys {
			if !removed[key.id] {
				keys = append(keys, key)
			}
		}
		var updated []pgSearchKey
		updated, err = view.searchKeys(ctx, f, changed)
		keys = append(keys, updated...)
		if err == nil {
			keys, err = view.sortSearchKeys(ctx, f.Sort, keys)
		}
	}
	if err != nil {
		return db.SearchPage{}, err
	}
	if token := checkpoints[through]; token != "" {
		memo.Put(slot, "search", []pgSearchEntry{{through, token, keys}})
	}
	start := max(f.Cursor, 0)
	if start >= len(keys) {
		return db.SearchPage{Results: []db.SearchResult{}}, nil
	}
	end := min(start+f.Limit, len(keys))
	ids := make([]string, 0, end-start)
	for _, k := range keys[start:end] {
		ids = append(ids, k.id)
	}
	page, err := view.searchRows(ctx, f, ids)
	if err != nil {
		return db.SearchPage{}, err
	}
	if end < len(keys) {
		page.NextCursor = end
	}
	return page, nil
}

func (s *Store) searchKeys(ctx context.Context, f db.SearchFilter, ids []string) ([]pgSearchKey, error) {
	query, args := s.searchQuery(f, ids, true)
	if query == "" {
		return nil, nil
	}
	rows, err := s.pg.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("searching keys: %w", err)
	}
	defer rows.Close()
	var keys []pgSearchKey
	for rows.Next() {
		var k pgSearchKey
		if err := rows.Scan(&k.id, &k.priority, &k.pos, &k.endedAt); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

func (s *Store) sortSearchKeys(ctx context.Context, sort string, keys []pgSearchKey) ([]pgSearchKey, error) {
	ids := make([]string, 0, len(keys))
	priorities := make([]int, 0, len(keys))
	positions := make([]int, 0, len(keys))
	times := make([]*time.Time, 0, len(keys))
	byID := make(map[string]pgSearchKey, len(keys))
	for _, k := range keys {
		ids = append(ids, k.id)
		priorities = append(priorities, k.priority)
		positions = append(positions, k.pos)
		times = append(times, k.endedAt)
		byID[k.id] = k
	}
	rows, err := s.pg.QueryContext(ctx, `SELECT session_id FROM unnest($1::text[], $2::int[], $3::int[], $4::timestamptz[]) AS k(session_id, match_priority, match_pos, session_ended_at) ORDER BY `+searchOrderBy(sort), ids, priorities, positions, times)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sorted := make([]pgSearchKey, 0, len(keys))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		sorted = append(sorted, byID[id])
	}
	return sorted, rows.Err()
}
