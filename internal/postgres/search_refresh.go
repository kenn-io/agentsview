package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"
	"unsafe"

	"go.kenn.io/agentsview/internal/db"
)

type pgSearchKey struct {
	id                                string
	priority, pos                     int
	endedAt                           *time.Time
	ordinal                           int
	kind                              string
	sourceKind, processor             *string
	callIndex, eventIndex, imageIndex *int
}
type pgSearchEntry struct {
	revision int64
	token    string
	keys     []pgSearchKey
}

func pgSearchEntryBytes(e pgSearchEntry) int64 {
	size := int64(unsafe.Sizeof(e)) + int64(len(e.token)) + int64(cap(e.keys))*int64(unsafe.Sizeof(pgSearchKey{}))
	for _, k := range e.keys {
		size += int64(len(k.id) + len(k.kind))
		if k.endedAt != nil {
			size += int64(unsafe.Sizeof(*k.endedAt))
		}
		if k.sourceKind != nil {
			size += int64(unsafe.Sizeof(*k.sourceKind)) + int64(len(*k.sourceKind))
		}
		if k.processor != nil {
			size += int64(unsafe.Sizeof(*k.processor)) + int64(len(*k.processor))
		}
		for _, p := range []*int{k.callIndex, k.eventIndex, k.imageIndex} {
			if p != nil {
				size += int64(unsafe.Sizeof(*p))
			}
		}
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
	if ok {
		entry = entries[0]
		after = entry.revision
	}
	through, changed, err := view.searchUsageChanges(ctx, after)
	if err != nil {
		return db.SearchPage{}, err
	}
	if through == 0 {
		return view.search(ctx, f)
	}
	currentToken, previousToken, err := usageHistoryCheckpoints(ctx, view.pg, through, entry.revision)
	if err != nil {
		return db.SearchPage{}, err
	}
	reusable := entry.token != "" && entry.token == previousToken && through >= entry.revision
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
	if currentToken != "" {
		memo.Put(slot, "search", []pgSearchEntry{{through, currentToken, keys}})
	}
	start := max(f.Cursor, 0)
	if start >= len(keys) {
		return db.SearchPage{Results: []db.SearchResult{}}, nil
	}
	end := min(start+f.Limit, len(keys))
	page, err := view.hydrateSearchKeys(ctx, f, keys[start:end])
	if err != nil {
		return db.SearchPage{}, err
	}
	if end < len(keys) {
		page.NextCursor = end
	}
	return page, nil
}

// A journal permission error aborts the snapshot until the savepoint is rolled back.
func (s *Store) searchUsageChanges(ctx context.Context, after int64) (int64, []string, error) {
	if s.readTx == nil {
		return s.LoadUsageChanges(ctx, after)
	}
	if _, err := s.pg.ExecContext(ctx, `SAVEPOINT search_journal`); err != nil {
		return 0, nil, err
	}
	revision, ids, err := s.loadUsageChanges(ctx, after)
	if isInsufficientPrivilege(err) || isUndefinedTable(err) {
		if _, err := s.pg.ExecContext(ctx, `ROLLBACK TO SAVEPOINT search_journal`); err != nil {
			return 0, nil, err
		}
		revision, ids, err = 0, nil, nil
	}
	if err != nil {
		return 0, nil, err
	}
	if _, err := s.pg.ExecContext(ctx, `RELEASE SAVEPOINT search_journal`); err != nil {
		return 0, nil, err
	}
	return revision, ids, nil
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
	return scanSearchKeys(rows)
}

func scanSearchKeys(rows *sql.Rows) ([]pgSearchKey, error) {
	var keys []pgSearchKey
	for rows.Next() {
		var k pgSearchKey
		if err := rows.Scan(&k.id, &k.priority, &k.pos, &k.endedAt, &k.ordinal, &k.kind, &k.sourceKind, &k.callIndex, &k.eventIndex, &k.imageIndex, &k.processor); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

func searchKeyArgs(keys []pgSearchKey) []any {
	ids := make([]string, 0, len(keys))
	priorities := make([]int, 0, len(keys))
	positions := make([]int, 0, len(keys))
	times := make([]*time.Time, 0, len(keys))
	ordinals := make([]int, 0, len(keys))
	kinds := make([]string, 0, len(keys))
	sources := make([]*string, 0, len(keys))
	calls := make([]*int, 0, len(keys))
	events := make([]*int, 0, len(keys))
	images := make([]*int, 0, len(keys))
	processors := make([]*string, 0, len(keys))
	for _, k := range keys {
		ids = append(ids, k.id)
		priorities = append(priorities, k.priority)
		positions = append(positions, k.pos)
		times = append(times, k.endedAt)
		ordinals = append(ordinals, k.ordinal)
		kinds = append(kinds, k.kind)
		sources = append(sources, k.sourceKind)
		calls = append(calls, k.callIndex)
		events = append(events, k.eventIndex)
		images = append(images, k.imageIndex)
		processors = append(processors, k.processor)
	}
	return []any{ids, priorities, positions, times, ordinals, kinds, sources, calls, events, images, processors}
}

const searchKeyColumns = "session_id, match_priority, match_pos, session_ended_at, ordinal, kind, source_kind, call_index, event_index, image_index, processor"

func searchKeyUnnest(first int) string {
	types := []string{"text", "int", "int", "timestamptz", "int", "text", "text", "int", "int", "int", "text"}
	placeholders := make([]string, len(types))
	for i, typ := range types {
		placeholders[i] = fmt.Sprintf("$%d::%s[]", first+i, typ)
	}
	return "unnest(" + strings.Join(placeholders, ", ") + ")"
}

func (s *Store) sortSearchKeys(ctx context.Context, sort string, keys []pgSearchKey) ([]pgSearchKey, error) {
	rows, err := s.pg.QueryContext(ctx, `SELECT `+searchKeyColumns+` FROM `+searchKeyUnnest(1)+` AS k(`+searchKeyColumns+`) ORDER BY `+searchOrderBy(sort), searchKeyArgs(keys)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSearchKeys(rows)
}

func (s *Store) hydrateSearchKeys(ctx context.Context, f db.SearchFilter, keys []pgSearchKey) (db.SearchPage, error) {
	query := `WITH page AS (SELECT * FROM ` + searchKeyUnnest(3) + ` WITH ORDINALITY AS k(` + searchKeyColumns + `, rank_order)) ` + searchRowsTail("p.rank_order")
	prepared := db.PrepareFTSQuery(f.Query)
	terms := db.FTSTerms(prepared)
	if len(terms) == 0 {
		return db.SearchPage{}, nil
	}
	args := append([]any{escapeLike(db.StripFTSQuotes(prepared)), terms[0]}, searchKeyArgs(keys)...)
	rows, err := s.pg.QueryContext(ctx, query, args...)
	if err != nil {
		return db.SearchPage{}, fmt.Errorf("hydrating search: %w", err)
	}
	defer rows.Close()
	return scanSearchRows(rows, f)
}
