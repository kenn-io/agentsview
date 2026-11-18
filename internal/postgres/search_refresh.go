package postgres

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"math"
	"strings"
	"time"
	"unsafe"

	"go.kenn.io/agentsview/internal/db"
)

type pgSearchKey struct {
	ID                                string
	Priority, Pos                     int
	EndedAt                           *time.Time
	Ordinal                           int
	Kind                              string
	SourceKind, Processor             *string
	CallIndex, EventIndex, ImageIndex *int
}
type pgSearchEntry struct {
	Revision int64
	Token    string
	Keys     []pgSearchKey
}

func pgSearchEntryBytes(e pgSearchEntry) int64 {
	size := int64(unsafe.Sizeof(e)) + int64(len(e.Token)) + int64(cap(e.Keys))*int64(unsafe.Sizeof(pgSearchKey{}))
	for _, k := range e.Keys {
		size += int64(len(k.ID) + len(k.Kind))
		if k.EndedAt != nil {
			size += int64(unsafe.Sizeof(*k.EndedAt))
		}
		if k.SourceKind != nil {
			size += int64(unsafe.Sizeof(*k.SourceKind)) + int64(len(*k.SourceKind))
		}
		if k.Processor != nil {
			size += int64(unsafe.Sizeof(*k.Processor)) + int64(len(*k.Processor))
		}
		for _, p := range []*int{k.CallIndex, k.EventIndex, k.ImageIndex} {
			if p != nil {
				size += int64(unsafe.Sizeof(*p))
			}
		}
	}
	return size
}

func searchKeySlot(f db.SearchFilter) string {
	f.Limit, f.Cursor = 0, 0
	return f.Slot()
}

func (s *Store) keepSearchKeys(f db.SearchFilter) {
	if !s.trendsDisk.Enabled() {
		return
	}
	slot := searchKeySlot(f)
	if entries, ok := s.runtimeState().searchKept.Get(slot, "search"); ok {
		s.trendsDisk.Keep("search-keys|"+slot, "1", func() ([]byte, error) { return json.Marshal(entries[0]) })
	}
}

func (s *Store) searchKept(ctx context.Context, view *Store, f db.SearchFilter) (db.SearchPage, error) {
	if db.StripFTSQuotes(db.PrepareFTSQuery(f.Query)) == "" {
		return db.SearchPage{}, nil
	}
	slot := searchKeySlot(f)
	memo := &s.runtimeState().searchKept
	entries, ok := memo.Get(slot, "search")
	var entry pgSearchEntry
	after := int64(math.MaxInt64)
	if ok {
		entry = entries[0]
	} else if data, found := s.trendsDisk.Get("search-keys|"+slot, "1"); found {
		var saved pgSearchEntry
		if json.Unmarshal(data, &saved) == nil {
			entry, ok = saved, true
		}
	}
	if ok {
		after = entry.Revision
	}
	through, changed, err := view.searchUsageChanges(ctx, after)
	if err != nil {
		return db.SearchPage{}, err
	}
	if through == 0 {
		return view.search(ctx, f)
	}
	currentToken, previousToken, err := usageHistoryCheckpoints(ctx, view.pg, through, entry.Revision)
	if err != nil {
		return db.SearchPage{}, err
	}
	reusable := entry.Token != "" && entry.Token == previousToken && through >= entry.Revision
	keys := entry.Keys
	if !reusable {
		keys, err = view.searchKeys(ctx, f, nil)
	} else if through > entry.Revision && len(changed) > 0 {
		removed := make(map[string]bool, len(changed))
		for _, id := range changed {
			removed[id] = true
		}
		keys = make([]pgSearchKey, 0, len(entry.Keys))
		for _, key := range entry.Keys {
			if !removed[key.ID] {
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
		updated := pgSearchEntry{through, currentToken, keys}
		memo.Put(slot, "search", []pgSearchEntry{updated})
		if (!reusable || through != entry.Revision) && s.trendsDisk.Enabled() {
			if data, err := json.Marshal(updated); err == nil {
				s.trendsDisk.Put("search-keys|"+slot, "1", data)
			}
		}
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
		if err := rows.Scan(&k.ID, &k.Priority, &k.Pos, &k.EndedAt, &k.Ordinal, &k.Kind, &k.SourceKind, &k.CallIndex, &k.EventIndex, &k.ImageIndex, &k.Processor); err != nil {
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
		ids = append(ids, k.ID)
		priorities = append(priorities, k.Priority)
		positions = append(positions, k.Pos)
		times = append(times, k.EndedAt)
		ordinals = append(ordinals, k.Ordinal)
		kinds = append(kinds, k.Kind)
		sources = append(sources, k.SourceKind)
		calls = append(calls, k.CallIndex)
		events = append(events, k.EventIndex)
		images = append(images, k.ImageIndex)
		processors = append(processors, k.Processor)
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
