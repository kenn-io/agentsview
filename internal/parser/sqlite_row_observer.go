package parser

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

type sqliteObservedTable struct {
	name                string
	cursorExpression    string
	sessionIDExpression string
	identityExpression  string
}

type sqliteRowObserverSpec struct {
	open           func(string, bool) (*sql.DB, error)
	schemaIdentity func(context.Context, *sql.DB) (string, error)
	tables         func(context.Context, *sql.DB) ([]sqliteObservedTable, error)
}

type sqliteRowCursor struct {
	id       int64
	identity string
}

type sqliteObservedTableState struct {
	table  sqliteObservedTable
	cursor sqliteRowCursor
}

type sqliteRowObserverState struct {
	sequence       uint64
	container      SQLiteContainerState
	schemaIdentity string
	tableOrder     []string
	tables         map[string]sqliteObservedTableState
}

type sqliteRowObserverWatermark struct {
	dbPath string
	state  sqliteRowObserverState
}

type sqliteRowObserver struct {
	spec         sqliteRowObserverSpec
	nextSequence atomic.Uint64
	mu           sync.Mutex
	entries      map[string]*sqliteRowObserverEntry
}

type sqliteRowObserverEntry struct {
	mu    sync.Mutex
	known bool
	state sqliteRowObserverState
}

func newSQLiteRowObserver(spec sqliteRowObserverSpec) *sqliteRowObserver {
	return &sqliteRowObserver{
		spec:    spec,
		entries: make(map[string]*sqliteRowObserverEntry),
	}
}

func (o *sqliteRowObserver) entry(dbPath string) *sqliteRowObserverEntry {
	o.mu.Lock()
	defer o.mu.Unlock()
	key := filepath.Clean(dbPath)
	entry := o.entries[key]
	if entry == nil {
		entry = &sqliteRowObserverEntry{}
		o.entries[key] = entry
	}
	return entry
}

func (o *sqliteRowObserver) capture(
	ctx context.Context, dbPath string, stableSnapshot bool,
) (sqliteRowObserverState, error) {
	container, ok := StatSQLiteContainerState(dbPath)
	if !ok {
		return sqliteRowObserverState{}, fmt.Errorf(
			"read SQLite container state for %s", dbPath,
		)
	}
	database, err := o.spec.open(dbPath, stableSnapshot)
	if err != nil {
		return sqliteRowObserverState{}, err
	}
	defer database.Close()
	return o.captureFrom(ctx, database, container)
}

func (o *sqliteRowObserver) captureFrom(
	ctx context.Context, database *sql.DB, container SQLiteContainerState,
) (sqliteRowObserverState, error) {
	schemaIdentity, err := o.spec.schemaIdentity(ctx, database)
	if err != nil {
		return sqliteRowObserverState{}, err
	}
	tables, err := o.spec.tables(ctx, database)
	if err != nil {
		return sqliteRowObserverState{}, err
	}
	state := sqliteRowObserverState{
		sequence:       o.nextSequence.Add(1),
		container:      container,
		schemaIdentity: schemaIdentity,
		tableOrder:     make([]string, 0, len(tables)),
		tables:         make(map[string]sqliteObservedTableState, len(tables)),
	}
	for _, table := range tables {
		if table.name == "" || table.cursorExpression == "" ||
			table.sessionIDExpression == "" || table.identityExpression == "" {
			return sqliteRowObserverState{}, errors.New(
				"SQLite row observer table specification is incomplete",
			)
		}
		if _, exists := state.tables[table.name]; exists {
			return sqliteRowObserverState{}, fmt.Errorf(
				"SQLite row observer table %q is duplicated", table.name,
			)
		}
		cursor, err := latestSQLiteRowCursor(ctx, database, table)
		if err != nil {
			return sqliteRowObserverState{}, err
		}
		state.tableOrder = append(state.tableOrder, table.name)
		state.tables[table.name] = sqliteObservedTableState{
			table: table, cursor: cursor,
		}
	}
	return state, nil
}

func latestSQLiteRowCursor(
	ctx context.Context, database *sql.DB, table sqliteObservedTable,
) (sqliteRowCursor, error) {
	query := "SELECT " + table.cursorExpression + ", " +
		table.identityExpression + " FROM " + table.name + " ORDER BY " +
		table.cursorExpression + " DESC LIMIT 1"
	var cursor sqliteRowCursor
	err := database.QueryRowContext(ctx, query).Scan(&cursor.id, &cursor.identity)
	if errors.Is(err, sql.ErrNoRows) {
		return sqliteRowCursor{}, nil
	}
	if err != nil {
		return sqliteRowCursor{}, fmt.Errorf(
			"read latest SQLite %s row: %w", table.name, err,
		)
	}
	return cursor, nil
}

func (o *sqliteRowObserver) storeDiscoveryWatermarks(
	watermarks []sqliteRowObserverWatermark,
) {
	for _, watermark := range watermarks {
		o.commit(watermark.dbPath, watermark.state)
	}
}

func (o *sqliteRowObserver) commit(
	dbPath string, state sqliteRowObserverState,
) {
	entry := o.entry(dbPath)
	entry.mu.Lock()
	entry.mergeLocked(state)
	entry.mu.Unlock()
}

func (e *sqliteRowObserverEntry) mergeLocked(state sqliteRowObserverState) {
	if !e.known {
		e.state = cloneSQLiteRowObserverState(state)
		e.known = true
		return
	}
	if sqliteRowObserverDatabaseReplaced(e.state, state) {
		if state.sequence > e.state.sequence {
			e.state = cloneSQLiteRowObserverState(state)
		}
		return
	}
	merged := cloneSQLiteRowObserverState(state)
	for name, previous := range e.state.tables {
		current, ok := merged.tables[name]
		if ok && previous.cursor.id > current.cursor.id {
			current.cursor = previous.cursor
			merged.tables[name] = current
		}
	}
	if e.state.sequence > state.sequence {
		merged.sequence = e.state.sequence
		merged.container = e.state.container
	}
	e.state = merged
}

func cloneSQLiteRowObserverState(state sqliteRowObserverState) sqliteRowObserverState {
	cloned := state
	cloned.tableOrder = append([]string(nil), state.tableOrder...)
	cloned.tables = make(map[string]sqliteObservedTableState, len(state.tables))
	maps.Copy(cloned.tables, state.tables)
	return cloned
}

func sqliteRowObserverDatabaseReplaced(
	previous, current sqliteRowObserverState,
) bool {
	identityChanged := (previous.container.DBInode != 0 ||
		previous.container.DBDevice != 0) &&
		(previous.container.DBInode != current.container.DBInode ||
			previous.container.DBDevice != current.container.DBDevice)
	if identityChanged || previous.schemaIdentity != current.schemaIdentity ||
		len(previous.tables) != len(current.tables) {
		return true
	}
	for name, previousTable := range previous.tables {
		currentTable, ok := current.tables[name]
		if !ok || previousTable.table != currentTable.table {
			return true
		}
	}
	return false
}

func (o *sqliteRowObserver) changedSessionIDs(
	ctx context.Context, dbPath string, stableSnapshot bool,
) (ids []string, cold bool, snapshot sqliteRowObserverState, err error) {
	entry := o.entry(dbPath)
	entry.mu.Lock()
	previous := cloneSQLiteRowObserverState(entry.state)
	known := entry.known
	entry.mu.Unlock()

	container, ok := StatSQLiteContainerState(dbPath)
	if !ok {
		return nil, false, sqliteRowObserverState{}, fmt.Errorf(
			"read SQLite container state for %s", dbPath,
		)
	}
	if known && previous.container == container {
		return nil, false, previous, nil
	}
	database, err := o.spec.open(dbPath, stableSnapshot)
	if err != nil {
		return nil, false, sqliteRowObserverState{}, err
	}
	defer database.Close()
	current, err := o.captureFrom(ctx, database, container)
	if err != nil {
		return nil, false, sqliteRowObserverState{}, err
	}
	if !known || sqliteRowObserverDatabaseReplaced(previous, current) {
		return nil, true, current, nil
	}

	seen := make(map[string]struct{})
	for _, name := range current.tableOrder {
		currentTable := current.tables[name]
		previousTable := previous.tables[name]
		valid, err := sqliteRowCursorStillValid(
			ctx, database, previousTable, currentTable,
		)
		if err != nil {
			return nil, false, sqliteRowObserverState{}, err
		}
		if !valid {
			continue
		}
		if err := listChangedSQLiteSessionIDs(
			ctx, database, currentTable.table, previousTable.cursor.id, seen,
		); err != nil {
			return nil, false, sqliteRowObserverState{}, err
		}
	}
	ids = make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, false, current, nil
}

func sqliteRowCursorStillValid(
	ctx context.Context, database *sql.DB,
	previous, current sqliteObservedTableState,
) (bool, error) {
	if current.cursor.id < previous.cursor.id {
		return false, nil
	}
	if previous.cursor.id == 0 {
		return true, nil
	}
	query := "SELECT " + current.table.identityExpression + " FROM " +
		current.table.name + " WHERE " + current.table.cursorExpression + " = ?"
	var identity string
	err := database.QueryRowContext(ctx, query, previous.cursor.id).Scan(&identity)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf(
			"read SQLite %s cursor identity: %w", current.table.name, err,
		)
	}
	return identity == previous.cursor.identity, nil
}

func listChangedSQLiteSessionIDs(
	ctx context.Context, database *sql.DB, table sqliteObservedTable,
	after int64, seen map[string]struct{},
) error {
	query := "SELECT " + table.sessionIDExpression + " FROM " + table.name +
		" WHERE " + table.cursorExpression + " > ? ORDER BY " +
		table.cursorExpression
	rows, err := database.QueryContext(ctx, query, after)
	if err != nil {
		return fmt.Errorf("list changed SQLite %s sessions: %w", table.name, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan changed SQLite session ID: %w", err)
		}
		id = strings.TrimSpace(id)
		if id != "" {
			seen[id] = struct{}{}
		}
	}
	return rows.Err()
}
