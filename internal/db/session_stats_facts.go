package db

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"sync"
	"time"
	"unsafe"

	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/kept"
)

// Facts and their nested maps and slices are immutable after publication.
type sessionStatsFacts struct {
	id                        string
	revision                  int64
	toolCalls, assistantTurns int
	tools                     map[string]int
	models                    map[string]int64
	velocity                  velocityAccumulator
	cache                     *sessionCacheTotals
	planMode                  bool
	subagents                 int
	skills                    map[string]struct{}
}

type sessionStatsFactsByID map[string]*sessionStatsFacts

// Every selected ID is initialized before any contribution is read or filled.
func (facts sessionStatsFactsByID) session(id string) *sessionStatsFacts {
	fact := facts[id]
	if fact == nil {
		panic("missing selected session statistics")
	}
	return fact
}

func (f *sessionStatsFacts) size() int64 {
	// Include the retained slice's pointer and three estimated map headers.
	size := int64(unsafe.Sizeof(*f)+unsafe.Sizeof(f)) + 3*64 + int64(len(f.id))
	size += int64(cap(f.velocity.turnCycles)+cap(f.velocity.firstResponses)) * 8
	size += int64(unsafe.Sizeof(sessionCacheTotals{}))
	for key := range f.tools {
		size += int64(len(key) + 64)
	}
	for key := range f.models {
		size += int64(len(key) + 64)
	}
	for key := range f.skills {
		size += int64(len(key) + 48)
	}
	return size
}

type sessionStatsFactsMemo struct {
	mu         sync.Mutex
	generation uint64
	rows       kept.Memo[*sessionStatsFacts]
}

func newSessionStatsFactsMemo() *sessionStatsFactsMemo {
	return &sessionStatsFactsMemo{rows: kept.Memo[*sessionStatsFacts]{MaxBytes: 128 << 20, Size: func(f *sessionStatsFacts) int64 { return f.size() }}}
}

func (m *sessionStatsFactsMemo) Generation() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.generation
}

func (m *sessionStatsFactsMemo) Lookup(generation uint64, version string) []*sessionStatsFacts {
	m.mu.Lock()
	defer m.mu.Unlock()
	if generation != m.generation {
		return nil
	}
	rows, _ := m.rows.Get("facts", version)
	return rows
}

func (m *sessionStatsFactsMemo) Store(generation uint64, version string, rows, previous []*sessionStatsFacts) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if generation != m.generation {
		return
	}
	selected := make(map[string]bool, len(rows))
	retained := slices.Clone(rows)
	var size int64
	for _, fact := range rows {
		selected[fact.id] = true
		size += fact.size()
	}
	for _, fact := range previous {
		if !selected[fact.id] {
			retained = append(retained, fact)
			size += fact.size()
		}
	}
	if size > m.rows.MaxBytes {
		retained = rows
	}
	m.rows.Put("facts", version, retained)
}

func (m *sessionStatsFactsMemo) Swap(swap func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	swap()
	m.generation++
	m.rows.DeleteSlots(func(string) bool { return true })
}

func (db *DB) invalidateSessionStatsFacts() {
	if db.sessionStatsFacts != nil {
		db.sessionStatsFacts.Swap(func() {})
	}
}

func (db *DB) readSessionStatsFacts(ctx context.Context, filter StatsFilter, from, to time.Time) ([]sessionStatsRow, []sessionStatsRow, sessionStatsFactsByID, error) {
	var generation uint64
	memo := db.sessionStatsFacts
	if memo != nil {
		generation = memo.Generation()
	}
	conn, err := db.getReader().Conn(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, nil, nil, err
	}
	defer func() { _ = tx.Rollback() }()
	roots, err := loadSessionsInWindow(ctx, tx, filter, from, to, false)
	if err != nil {
		return nil, nil, nil, err
	}
	all, err := loadSessionsInWindow(ctx, tx, filter, from, to, true)
	if err != nil {
		return nil, nil, nil, err
	}
	var version string
	var previous []*sessionStatsFacts
	if memo != nil {
		identity, pricingRevision, e := databaseIDAndUsagePricingRevisionFrom(ctx, tx)
		if e != nil {
			return nil, nil, nil, e
		}
		if identity != "" {
			version = fmt.Sprintf("%s|%d", identity, pricingRevision)
			previous = memo.Lookup(generation, version)
		}
	}
	old := make(sessionStatsFactsByID, len(previous))
	for _, f := range previous {
		old[f.id] = f
	}
	rootIDs := make(map[string]bool, len(roots))
	for _, r := range roots {
		rootIDs[r.id] = true
	}
	agents := make(map[string]string, len(all))
	for _, row := range all {
		agents[row.id] = row.agent
	}
	ids := make([]string, 0, len(agents))
	for id := range agents {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	facts := make(sessionStatsFactsByID, len(ids))
	var missing, rootMiss, claudeMiss []string
	if memo != nil {
		err = queryChunked(ids, func(chunk []string) error {
			ph, args := InPlaceholders(chunk)
			rows, e := tx.QueryContext(ctx, `SELECT session_id,revision FROM usage_session_changes WHERE session_id IN `+ph, args...)
			if e != nil {
				return e
			}
			defer rows.Close()
			for rows.Next() {
				f := &sessionStatsFacts{}
				if e = rows.Scan(&f.id, &f.revision); e != nil {
					return e
				}
				facts[f.id] = f
			}
			return rows.Err()
		})
		if err != nil {
			return nil, nil, nil, err
		}
	}
	var publish []*sessionStatsFacts
	for _, id := range ids {
		f := facts[id]
		if f == nil {
			f = &sessionStatsFacts{id: id}
			facts[id] = f
		}
		hit := old[id]
		root, claude := rootIDs[id], rootIDs[id] && agents[id] == "claude"
		if hit != nil && hit.revision == f.revision {
			facts[id] = hit
			publish = append(publish, hit)
			continue
		}
		f.tools = map[string]int{}
		f.models = map[string]int64{}
		f.skills = map[string]struct{}{}
		missing = append(missing, id)
		if root {
			rootMiss = append(rootMiss, id)
		}
		if claude {
			claudeMiss = append(claudeMiss, id)
		}
		publish = append(publish, f)
	}
	if err = loadSessionStatsCountsAndMix(ctx, tx, missing, facts); err != nil {
		return nil, nil, nil, err
	}
	msgs := map[string][]TimingMessage{}
	err = queryChunked(rootMiss, func(chunk []string) error { return db.queryVelocityMsgs(ctx, tx, chunk, time.UTC, msgs) })
	if err != nil {
		return nil, nil, nil, err
	}
	for _, id := range rootMiss {
		if len(msgs[id]) >= 2 {
			addSessionVelocity([]*velocityAccumulator{&facts.session(id).velocity}, ComputeSessionVelocity(msgs[id]), 0)
		}
	}
	if len(claudeMiss) > 0 {
		pricing, e := db.loadPricingMapFrom(ctx, tx)
		if e != nil {
			return nil, nil, nil, e
		}
		resolver := export.NewPricingResolver(pricing)
		cache := map[string]*sessionCacheTotals{}
		err = queryChunked(claudeMiss, func(chunk []string) error {
			if e := accumulateCacheTotals(ctx, tx, chunk, resolver, cache); e != nil {
				return e
			}
			return accumulateAdoption(ctx, tx, chunk, facts)
		})
		if err != nil {
			return nil, nil, nil, err
		}
		for _, id := range claudeMiss {
			facts.session(id).cache = cache[id]
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, nil, nil, err
	}
	if memo != nil && version != "" {
		memo.Store(generation, version, publish, previous)
	}
	return roots, all, facts, nil
}

func loadSessionStatsCountsAndMix(ctx context.Context, tx *sql.Tx, ids []string, facts sessionStatsFactsByID) error {
	return queryChunked(ids, func(chunk []string) error {
		if err := accumulateToolMix(ctx, tx, facts, chunk); err != nil {
			return err
		}
		if err := accumulateModelMix(ctx, tx, facts, chunk); err != nil {
			return err
		}
		ph, args := InPlaceholders(chunk)
		// System-only assistant rows represent injected metadata, not turns.
		rows, err := tx.QueryContext(ctx, `SELECT session_id,COUNT(*) FROM messages WHERE session_id IN `+ph+` AND role='assistant' AND is_system=0 GROUP BY session_id`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var count int
			if err = rows.Scan(&id, &count); err != nil {
				return err
			}
			facts.session(id).assistantTurns = count
		}
		return rows.Err()
	})
}
