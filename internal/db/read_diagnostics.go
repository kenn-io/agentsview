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
	active map[*openRead]struct{}
}

type openRead struct {
	started time.Time
	stack   [32]uintptr
	depth   int
}

func (r *readRegistry) start() *openRead {
	read := &openRead{started: time.Now()}
	read.depth = runtime.Callers(2, read.stack[:])
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active == nil {
		r.active = make(map[*openRead]struct{})
	}
	r.active[read] = struct{}{}
	return read
}

func (r *readRegistry) finish(read *openRead) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.active, read)
}

func (r *readRegistry) logOpenReads() {
	r.mu.Lock()
	reads := make([]*openRead, 0, len(r.active))
	for read := range r.active {
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
		log.Printf("sqlite open read: age=%s owner=%s", time.Since(read.started).Round(time.Millisecond), owner)
	}
}

// Embedding preserves the SQLite driver's optional capabilities, including
// context-aware execution, row type metadata, and Raw registration hooks.
type diagnosticReaderConn struct {
	*sqlite3.SQLiteConn
	reads *readRegistry
}

func (c *diagnosticReaderConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	read := c.reads.start()
	rows, err := c.SQLiteConn.QueryContext(ctx, query, args)
	if err != nil {
		c.reads.finish(read)
		return nil, err
	}
	return &diagnosticReaderRows{SQLiteRows: rows.(*sqlite3.SQLiteRows), reads: c.reads, read: read}, nil
}

func (c *diagnosticReaderConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	read := c.reads.start()
	defer c.reads.finish(read)
	return c.SQLiteConn.ExecContext(ctx, query, args)
}

func (c *diagnosticReaderConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	read := c.reads.start()
	tx, err := c.SQLiteConn.BeginTx(ctx, opts)
	if err != nil {
		c.reads.finish(read)
		return nil, err
	}
	return &diagnosticReaderTx{Tx: tx, reads: c.reads, read: read}, nil
}

type diagnosticReaderRows struct {
	*sqlite3.SQLiteRows
	reads *readRegistry
	read  *openRead
}

func (r *diagnosticReaderRows) Close() error {
	err := r.SQLiteRows.Close()
	r.reads.finish(r.read)
	return err
}

type diagnosticReaderTx struct {
	driver.Tx
	reads *readRegistry
	read  *openRead
}

func (t *diagnosticReaderTx) Commit() error {
	err := t.Tx.Commit()
	t.reads.finish(t.read)
	return err
}

func (t *diagnosticReaderTx) Rollback() error {
	err := t.Tx.Rollback()
	t.reads.finish(t.read)
	return err
}
