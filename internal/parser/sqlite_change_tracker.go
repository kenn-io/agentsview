package parser

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
)

// sqliteCursorTable describes producer-owned SQL, never user input. Each table
// has an indexed integer cursor and an identity that detects tail-row reuse.
type sqliteCursorTable struct {
	name, rowID, identity, sessionID string
}

type sqliteRowCursor struct {
	id       int64
	identity string
}

type sqliteTrackedDatabase struct {
	schemaVersion int
	inode, device uint64
	tables        []sqliteCursorTable
	cursors       []sqliteRowCursor
}

type sqliteDiscoveryWatermark struct {
	dbPath string
	state  sqliteTrackedDatabase
}

// sqliteChangeTracker retains only table tails per database. Inserts are
// delivered on watcher events; edits and deletions remain reconciliation's job.
// The factory owns it so short-lived provider instances share the same cursors.
type sqliteChangeTracker struct {
	agent  AgentType
	open   func(string, bool) (*sql.DB, error)
	schema func(context.Context, *sql.DB) (int, []sqliteCursorTable, error)

	mu      sync.Mutex
	entries map[string]*sqliteTrackedDatabaseEntry
}

type sqliteTrackedDatabaseEntry struct {
	mu    sync.Mutex
	known bool
	state sqliteTrackedDatabase
}

// Each database has its own lock so a busy container cannot block other roots.
func (t *sqliteChangeTracker) entry(dbPath string) *sqliteTrackedDatabaseEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entries == nil {
		t.entries = make(map[string]*sqliteTrackedDatabaseEntry)
	}
	key := filepath.Clean(dbPath)
	entry := t.entries[key]
	if entry == nil {
		entry = &sqliteTrackedDatabaseEntry{}
		t.entries[key] = entry
	}
	return entry
}

func (t *sqliteChangeTracker) storeDiscoveryWatermarks(watermarks []sqliteDiscoveryWatermark) {
	for _, watermark := range watermarks {
		t.commit(watermark.dbPath, watermark.state)
	}
}

// commit publishes a pre-enumeration snapshot only after enumeration succeeds.
// An older discovery must not retreat cursors advanced by a concurrent watcher.
func (t *sqliteChangeTracker) commit(dbPath string, state sqliteTrackedDatabase) {
	entry := t.entry(dbPath)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.known && !sqliteTrackedDatabaseReplaced(entry.state, state) {
		state.cursors = slices.Clone(state.cursors)
		for i, cursor := range entry.state.cursors {
			if cursor.id > state.cursors[i].id {
				state.cursors[i] = cursor
			}
		}
	}
	entry.state, entry.known = state, true
}

// changedSessionIDs returns cold for a new or replaced database. Its caller
// must enumerate that container and commit the returned snapshot on success.
// A retreated or rewritten table tail skips that table, not the other tables.
func (t *sqliteChangeTracker) changedSessionIDs(
	ctx context.Context, dbPath string, stableSnapshot bool,
) (ids []string, cold bool, snapshot sqliteTrackedDatabase, err error) {
	entry := t.entry(dbPath)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	info, err := os.Stat(dbPath)
	if err != nil {
		return nil, false, sqliteTrackedDatabase{}, fmt.Errorf("stat %s sessions database: %w", t.agent, err)
	}
	db, err := t.open(dbPath, stableSnapshot)
	if err != nil {
		return nil, false, sqliteTrackedDatabase{}, err
	}
	defer db.Close()
	current, err := t.readFrom(ctx, db, info)
	if err != nil {
		return nil, false, sqliteTrackedDatabase{}, err
	}
	if !entry.known || sqliteTrackedDatabaseReplaced(entry.state, current) {
		return nil, true, current, nil
	}
	seen := make(map[string]struct{})
	for i, table := range current.tables {
		previous := entry.state.cursors[i]
		valid, err := t.cursorStillValid(ctx, db, table, previous, current.cursors[i])
		if err != nil {
			return nil, false, sqliteTrackedDatabase{}, err
		}
		if !valid {
			continue
		}
		if err := t.listChangedSessionIDs(ctx, db, table, previous.id, seen); err != nil {
			return nil, false, sqliteTrackedDatabase{}, err
		}
	}
	entry.state, entry.known = current, true
	ids = make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, false, current, nil
}

func sqliteTrackedDatabaseReplaced(previous, current sqliteTrackedDatabase) bool {
	identityChanged := (previous.inode != 0 || previous.device != 0) &&
		(previous.inode != current.inode || previous.device != current.device)
	return identityChanged || previous.schemaVersion != current.schemaVersion ||
		!slices.Equal(previous.tables, current.tables)
}

func (t *sqliteChangeTracker) read(
	ctx context.Context, dbPath string, stableSnapshot bool,
) (sqliteTrackedDatabase, error) {
	info, err := os.Stat(dbPath)
	if err != nil {
		return sqliteTrackedDatabase{}, fmt.Errorf("stat %s sessions database: %w", t.agent, err)
	}
	db, err := t.open(dbPath, stableSnapshot)
	if err != nil {
		return sqliteTrackedDatabase{}, err
	}
	defer db.Close()
	return t.readFrom(ctx, db, info)
}

func (t *sqliteChangeTracker) readFrom(
	ctx context.Context, db *sql.DB, info os.FileInfo,
) (sqliteTrackedDatabase, error) {
	version, tables, err := t.schema(ctx, db)
	if err != nil {
		return sqliteTrackedDatabase{}, err
	}
	inode, device := sourceFileIdentity(info)
	state := sqliteTrackedDatabase{
		schemaVersion: version, inode: inode, device: device,
		tables: tables, cursors: make([]sqliteRowCursor, len(tables)),
	}
	for i, table := range tables {
		query := "SELECT " + table.rowID + ", " + table.identity + " FROM " + table.name +
			" ORDER BY " + table.rowID + " DESC LIMIT 1"
		err := db.QueryRowContext(ctx, query).Scan(&state.cursors[i].id, &state.cursors[i].identity)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return sqliteTrackedDatabase{}, fmt.Errorf("reading latest %s %s row: %w", t.agent, table.name, err)
		}
	}
	return state, nil
}

func (t *sqliteChangeTracker) cursorStillValid(
	ctx context.Context, db *sql.DB, table sqliteCursorTable, previous, current sqliteRowCursor,
) (bool, error) {
	if current.id < previous.id {
		return false, nil
	}
	if previous.id == 0 {
		return true, nil
	}
	query := "SELECT " + table.identity + " FROM " + table.name + " WHERE " + table.rowID + " = ?"
	var identity string
	err := db.QueryRowContext(ctx, query, previous.id).Scan(&identity)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading %s %s cursor identity: %w", t.agent, table.name, err)
	}
	return identity == previous.identity, nil
}

func (t *sqliteChangeTracker) listChangedSessionIDs(
	ctx context.Context, db *sql.DB, table sqliteCursorTable, after int64, seen map[string]struct{},
) error {
	query := "SELECT " + table.sessionID + " FROM " + table.name +
		" WHERE " + table.rowID + " > ? ORDER BY " + table.rowID
	rows, err := db.QueryContext(ctx, query, after)
	if err != nil {
		return fmt.Errorf("listing changed %s sessions: %w", t.agent, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scanning changed %s session ID: %w", t.agent, err)
		}
		if id = strings.TrimSpace(id); id != "" {
			seen[id] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return rows.Close()
}
