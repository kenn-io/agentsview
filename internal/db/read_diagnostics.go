package db

import (
	"context"
	"database/sql/driver"
	"fmt"
	"log"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/mattn/go-sqlite3"
)

// readRegistry lives as long as DB, including reads in retired pools. Only
// executing queries, unclosed rows, and open transactions have records.
type readRegistry struct {
	mu     sync.Mutex
	nextID uint64
	active map[uint64]openRead
}

type openRead struct {
	id      uint64
	started time.Time
	stack   [32]uintptr
	depth   int
}

func (r *readRegistry) start() uint64 {
	read := openRead{started: time.Now()}
	read.depth = runtime.Callers(2, read.stack[:])
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	read.id = r.nextID
	if r.active == nil {
		r.active = make(map[uint64]openRead)
	}
	r.active[read.id] = read
	return read.id
}

func (r *readRegistry) finish(id uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.active, id)
}

func (r *readRegistry) logOpenReads() {
	r.mu.Lock()
	reads := make([]openRead, 0, len(r.active))
	for _, read := range r.active {
		reads = append(reads, read)
	}
	r.mu.Unlock()

	log.Printf("sqlite open reads: %d", len(reads))
	for _, read := range reads {
		owner := "unknown"
		frames := runtime.CallersFrames(read.stack[:read.depth])
		for frame, more := frames.Next(); ; frame, more = frames.Next() {
			if !strings.HasSuffix(frame.File, "/read_diagnostics.go") &&
				!strings.HasPrefix(frame.Function, "database/sql.") &&
				!strings.Contains(frame.Function, ".(*readerHandle).") {
				owner = fmt.Sprintf("%s (%s:%d)", frame.Function, frame.File, frame.Line)
				break
			}
			if !more {
				break
			}
		}
		log.Printf("sqlite open read: id=%d age=%s owner=%s", read.id, time.Since(read.started).Round(time.Millisecond), owner)
	}
}

func (r *readRegistry) rows(id uint64, rows driver.Rows, err error) (driver.Rows, error) {
	if err != nil {
		r.finish(id)
		return nil, err
	}
	return &diagnosticReaderRows{SQLiteRows: rows.(*sqlite3.SQLiteRows), reads: r, id: id}, nil
}

func (r *readRegistry) transaction(id uint64, tx driver.Tx, err error) (driver.Tx, error) {
	if err != nil {
		r.finish(id)
		return nil, err
	}
	return &diagnosticReaderTx{Tx: tx, reads: r, id: id}, nil
}

// Embedding preserves the SQLite driver's optional capabilities, including
// context-aware execution, row type metadata, and Raw registration hooks.
type diagnosticReaderConn struct {
	*sqlite3.SQLiteConn
	reads *readRegistry
}

func (c *diagnosticReaderConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	id := c.reads.start()
	rows, err := c.SQLiteConn.QueryContext(ctx, query, args)
	return c.reads.rows(id, rows, err)
}

func (c *diagnosticReaderConn) Query(query string, args []driver.Value) (driver.Rows, error) {
	id := c.reads.start()
	rows, err := c.SQLiteConn.Query(query, args)
	return c.reads.rows(id, rows, err)
}

func (c *diagnosticReaderConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	id := c.reads.start()
	tx, err := c.SQLiteConn.BeginTx(ctx, opts)
	return c.reads.transaction(id, tx, err)
}

func (c *diagnosticReaderConn) Begin() (driver.Tx, error) {
	id := c.reads.start()
	tx, err := c.SQLiteConn.Begin()
	return c.reads.transaction(id, tx, err)
}

func (c *diagnosticReaderConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	stmt, err := c.SQLiteConn.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	return &diagnosticReaderStmt{SQLiteStmt: stmt.(*sqlite3.SQLiteStmt), reads: c.reads}, nil
}

func (c *diagnosticReaderConn) Prepare(query string) (driver.Stmt, error) {
	stmt, err := c.SQLiteConn.Prepare(query)
	if err != nil {
		return nil, err
	}
	return &diagnosticReaderStmt{SQLiteStmt: stmt.(*sqlite3.SQLiteStmt), reads: c.reads}, nil
}

type diagnosticReaderStmt struct {
	*sqlite3.SQLiteStmt
	reads *readRegistry
}

func (s *diagnosticReaderStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	id := s.reads.start()
	rows, err := s.SQLiteStmt.QueryContext(ctx, args)
	return s.reads.rows(id, rows, err)
}

func (s *diagnosticReaderStmt) Query(args []driver.Value) (driver.Rows, error) {
	id := s.reads.start()
	rows, err := s.SQLiteStmt.Query(args)
	return s.reads.rows(id, rows, err)
}

type diagnosticReaderRows struct {
	*sqlite3.SQLiteRows
	reads *readRegistry
	id    uint64
}

func (r *diagnosticReaderRows) Close() error {
	err := r.SQLiteRows.Close()
	r.reads.finish(r.id)
	return err
}

type diagnosticReaderTx struct {
	driver.Tx
	reads *readRegistry
	id    uint64
}

func (t *diagnosticReaderTx) Commit() error {
	err := t.Tx.Commit()
	t.reads.finish(t.id)
	return err
}

func (t *diagnosticReaderTx) Rollback() error {
	err := t.Tx.Rollback()
	t.reads.finish(t.id)
	return err
}
