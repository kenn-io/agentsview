package db

import (
	"crypto/rand"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	_ "github.com/mattn/go-sqlite3"
)

// dataVersion tracks parser changes that require a full
// re-sync. Increment this when parsing logic changes in ways
// that affect stored data (e.g. new fields extracted, content
// formatting changes). Old databases with a lower user_version
// trigger a non-destructive re-sync (mtime reset + skip cache
// clear) so existing session data is preserved.
const dataVersion = 5

//go:embed schema.sql
var schemaSQL string

const schemaFTS = `
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
    content,
    content='messages',
    content_rowid='id',
    tokenize='porter unicode61'
);

CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
    INSERT INTO messages_fts(rowid, content) VALUES (new.id, new.content);
END;

CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, content)
        VALUES('delete', old.id, old.content);
END;

CREATE TRIGGER IF NOT EXISTS messages_au AFTER UPDATE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, content)
        VALUES('delete', old.id, old.content);
    INSERT INTO messages_fts(rowid, content) VALUES (new.id, new.content);
END;
`

// DB manages a write connection and a read-only pool.
// The reader and writer fields use atomic.Pointer so that
// concurrent HTTP handler goroutines can safely read while
// Reopen/CloseConnections swap the underlying *sql.DB.
type DB struct {
	path      string
	writer    atomic.Pointer[sql.DB]
	reader    atomic.Pointer[sql.DB]
	mu        sync.Mutex // serializes writes
	retired   []*sql.DB  // old pools kept open for in-flight reads
	dataStale bool       // set by Open when user_version < dataVersion

	cursorMu     sync.RWMutex
	cursorSecret []byte
}

// getReader returns the current read-only connection pool.
func (db *DB) getReader() *sql.DB { return db.reader.Load() }

// getWriter returns the current write connection.
func (db *DB) getWriter() *sql.DB { return db.writer.Load() }

// Path returns the file path of the database.
func (db *DB) Path() string {
	return db.path
}

// ReadOnly returns false for the local SQLite store.
func (db *DB) ReadOnly() bool { return false }

// SetCursorSecret updates the secret key used for cursor signing.
func (db *DB) SetCursorSecret(secret []byte) {
	db.cursorMu.Lock()
	defer db.cursorMu.Unlock()
	db.cursorSecret = append([]byte(nil), secret...)
}

// makeDSN builds a SQLite connection string with shared pragmas.
func makeDSN(path string, readOnly bool) string {
	params := url.Values{}
	params.Set("_journal_mode", "WAL")
	params.Set("_busy_timeout", "5000")
	params.Set("_foreign_keys", "ON")
	params.Set("_mmap_size", "268435456")
	params.Set("_cache_size", "-64000")
	if readOnly {
		params.Set("mode", "ro")
	} else {
		params.Set("_synchronous", "NORMAL")
	}
	return path + "?" + params.Encode()
}

// Open creates or opens a SQLite database at the given path.
// It configures WAL mode, mmap, and returns a DB with separate
// writer and reader connections.
//
// If an existing database has an outdated schema (missing
// columns), it is deleted and recreated from scratch.
// If the schema is current but the data version is stale,
// the database is preserved and file mtimes are reset to
// trigger a re-sync on the next cycle.
func Open(path string) (*DB, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating db directory: %w", err)
	}

	schemaStale, dataStale, err := probeDatabase(path)
	if err != nil {
		return nil, fmt.Errorf("checking schema: %w", err)
	}
	if schemaStale {
		if err := dropDatabase(path); err != nil {
			return nil, fmt.Errorf(
				"rebuilding database: %w", err,
			)
		}
	}

	d, err := openAndInit(path)
	if err != nil {
		return nil, err
	}

	if err := d.migrateColumns(); err != nil {
		d.Close()
		return nil, fmt.Errorf("migrating columns: %w", err)
	}

	if dataStale && !schemaStale {
		d.dataStale = true
		log.Printf(
			"data version outdated; full resync required",
		)
	} else {
		// Only stamp user_version when data is current.
		// When data is stale, preserve the old version so
		// the "needs resync" state survives process restarts
		// until ResyncAll completes successfully.
		if err := d.setDataVersion(); err != nil {
			d.Close()
			return nil, fmt.Errorf(
				"setting data version: %w", err,
			)
		}
	}

	return d, nil
}

// probeDatabase checks an existing database for schema and
// data staleness. Returns (schemaStale, dataStale, err).
// schemaStale means required columns are missing and the DB
// must be dropped and recreated. dataStale means the schema
// is fine but user_version < dataVersion, requiring a
// non-destructive re-sync.
func probeDatabase(
	path string,
) (schemaStale, dataStale bool, err error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false, false, nil
		}
		return false, false, fmt.Errorf(
			"checking database file: %w", err,
		)
	}
	conn, err := sql.Open("sqlite3", makeDSN(path, true))
	if err != nil {
		return false, false, fmt.Errorf(
			"probing schema: %w", err,
		)
	}
	defer conn.Close()

	schema, err := needsSchemaRebuild(conn)
	if err != nil {
		return false, false, err
	}
	if schema {
		return true, false, nil
	}

	data, err := needsDataResync(conn)
	if err != nil {
		return false, false, err
	}
	return false, data, nil
}

// needsSchemaRebuild probes for required columns that may be
// missing in databases created by older releases. If any are
// absent, the DB must be dropped and recreated.
func needsSchemaRebuild(conn *sql.DB) (bool, error) {
	probes := []struct {
		table  string
		column string
	}{
		{"sessions", "parent_session_id"},
		{"insights", "date_from"},
		{"tool_calls", "tool_use_id"},
		{"sessions", "user_message_count"},
		{"sessions", "relationship_type"},
		{"tool_calls", "subagent_session_id"},
	}
	for _, p := range probes {
		var count int
		err := conn.QueryRow(fmt.Sprintf(
			"SELECT count(*) FROM pragma_table_info('%s')"+
				" WHERE name = '%s'",
			p.table, p.column,
		)).Scan(&count)
		if err != nil {
			return false, fmt.Errorf(
				"probing schema (%s.%s): %w",
				p.table, p.column, err,
			)
		}
		if count == 0 {
			return true, nil
		}
	}
	return false, nil
}

// needsDataResync checks whether user_version is behind the
// current dataVersion, indicating parser changes that require
// re-processing existing files.
func needsDataResync(conn *sql.DB) (bool, error) {
	var version int
	err := conn.QueryRow(
		"PRAGMA user_version",
	).Scan(&version)
	if err != nil {
		return false, fmt.Errorf(
			"probing data version: %w", err,
		)
	}
	return version < dataVersion, nil
}

// migrateColumns adds columns introduced by this branch to
// databases created by older releases. Each migration is
// idempotent — it only runs when the column is missing.
func (db *DB) migrateColumns() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	w := db.getWriter()

	migrations := []struct {
		table  string
		column string
		ddl    string
	}{
		{
			"sessions", "display_name",
			"ALTER TABLE sessions ADD COLUMN display_name TEXT",
		},
		{
			"sessions", "deleted_at",
			"ALTER TABLE sessions ADD COLUMN deleted_at TEXT",
		},
		{
			"messages", "is_system",
			"ALTER TABLE messages ADD COLUMN is_system INTEGER NOT NULL DEFAULT 0",
		},
		{
			"messages", "model",
			"ALTER TABLE messages ADD COLUMN model TEXT NOT NULL DEFAULT ''",
		},
		{
			"messages", "token_usage",
			"ALTER TABLE messages ADD COLUMN token_usage TEXT NOT NULL DEFAULT ''",
		},
		{
			"messages", "context_tokens",
			"ALTER TABLE messages ADD COLUMN context_tokens INTEGER NOT NULL DEFAULT 0",
		},
		{
			"messages", "output_tokens",
			"ALTER TABLE messages ADD COLUMN output_tokens INTEGER NOT NULL DEFAULT 0",
		},
		{
			"sessions", "total_output_tokens",
			"ALTER TABLE sessions ADD COLUMN total_output_tokens INTEGER NOT NULL DEFAULT 0",
		},
		{
			"sessions", "peak_context_tokens",
			"ALTER TABLE sessions ADD COLUMN peak_context_tokens INTEGER NOT NULL DEFAULT 0",
		},
		{
			"sessions", "local_modified_at",
			"ALTER TABLE sessions ADD COLUMN local_modified_at TEXT",
		},
	}

	for _, m := range migrations {
		var count int
		err := w.QueryRow(fmt.Sprintf(
			"SELECT count(*) FROM pragma_table_info('%s')"+
				" WHERE name = '%s'",
			m.table, m.column,
		)).Scan(&count)
		if err != nil {
			return fmt.Errorf(
				"probing %s.%s: %w",
				m.table, m.column, err,
			)
		}
		if count == 0 {
			if _, err := w.Exec(m.ddl); err != nil {
				return fmt.Errorf(
					"adding %s.%s: %w",
					m.table, m.column, err,
				)
			}
			log.Printf(
				"migration: added column %s.%s",
				m.table, m.column,
			)
		}
	}
	return nil
}

// NeedsResync reports whether the database was opened with a
// stale data version, indicating the caller should trigger a
// full resync (build fresh DB, copy orphaned data, swap)
// rather than an incremental sync.
func (db *DB) NeedsResync() bool {
	return db.dataStale
}

func dropDatabase(path string) error {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil &&
			!os.IsNotExist(err) {
			return fmt.Errorf(
				"removing %s: %w", path+suffix, err,
			)
		}
	}
	return nil
}

func openAndInit(path string) (*DB, error) {
	writer, err := sql.Open("sqlite3", makeDSN(path, false))
	if err != nil {
		return nil, fmt.Errorf("opening writer: %w", err)
	}
	writer.SetMaxOpenConns(1)

	reader, err := sql.Open("sqlite3", makeDSN(path, true))
	if err != nil {
		writer.Close()
		return nil, fmt.Errorf("opening reader: %w", err)
	}
	reader.SetMaxOpenConns(4)

	db := &DB{path: path}
	db.writer.Store(writer)
	db.reader.Store(reader)

	db.cursorSecret = make([]byte, 32)
	if _, err := rand.Read(db.cursorSecret); err != nil {
		writer.Close()
		reader.Close()
		return nil, fmt.Errorf(
			"generating cursor secret: %w", err,
		)
	}

	if err := db.init(); err != nil {
		db.Close()
		return nil, fmt.Errorf("initializing schema: %w", err)
	}
	return db, nil
}

// DropFTS drops the FTS table and its triggers. This makes
// bulk message delete+reinsert fast by avoiding per-row FTS
// index updates. Call RebuildFTS after to restore search.
func (db *DB) DropFTS() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	stmts := []string{
		"DROP TRIGGER IF EXISTS messages_ai",
		"DROP TRIGGER IF EXISTS messages_ad",
		"DROP TRIGGER IF EXISTS messages_au",
		"DROP TABLE IF EXISTS messages_fts",
	}
	w := db.getWriter()
	for _, s := range stmts {
		if _, err := w.Exec(s); err != nil {
			return fmt.Errorf("drop fts (%s): %w", s, err)
		}
	}
	return nil
}

// RebuildFTS recreates the FTS table, triggers, and
// repopulates the index from the messages table.
func (db *DB) RebuildFTS() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	w := db.getWriter()
	if _, err := w.Exec(schemaFTS); err != nil {
		return fmt.Errorf("recreate fts: %w", err)
	}
	_, err := w.Exec(
		"INSERT INTO messages_fts(messages_fts)" +
			" VALUES('rebuild')",
	)
	if err != nil {
		return fmt.Errorf("rebuild fts index: %w", err)
	}
	return nil
}

// HasFTS checks if Full Text Search is available.
func (db *DB) HasFTS() bool {
	// We need to actually try to access the table, because it might exist
	// in sqlite_master but fail to load if the fts5 module is missing
	// in the current runtime.
	_, err := db.getReader().Exec(
		"SELECT 1 FROM messages_fts LIMIT 1",
	)
	return err == nil
}

// setDataVersion stamps the current dataVersion into
// user_version, but never downgrades a higher version left
// by a newer build. Called by Open() only when data is
// current (not stale), so the marker survives until
// ResyncAll completes.
func (db *DB) setDataVersion() error {
	db.mu.Lock()
	defer db.mu.Unlock()

	var current int
	if err := db.getWriter().QueryRow(
		"PRAGMA user_version",
	).Scan(&current); err != nil {
		return fmt.Errorf("reading data version: %w", err)
	}
	if current >= dataVersion {
		return nil
	}

	_, err := db.getWriter().Exec(
		fmt.Sprintf("PRAGMA user_version = %d", dataVersion),
	)
	if err != nil {
		return fmt.Errorf("setting data version: %w", err)
	}
	return nil
}

func (db *DB) init() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	w := db.getWriter()
	if _, err := w.Exec(schemaSQL); err != nil {
		return err
	}

	// Add result_content column to tool_calls if not present
	// (non-destructive migration for existing databases).
	var rcCount int
	if err := w.QueryRow(
		`SELECT count(*) FROM pragma_table_info('tool_calls')` +
			` WHERE name = 'result_content'`,
	).Scan(&rcCount); err != nil {
		return fmt.Errorf("probing result_content column: %w", err)
	}
	if rcCount == 0 {
		if _, err := w.Exec(
			`ALTER TABLE tool_calls ADD COLUMN result_content TEXT`,
		); err != nil {
			return fmt.Errorf("adding result_content column: %w", err)
		}
	}

	// Check if FTS table exists before trying to create it
	var ftsCount int
	if err := w.QueryRow(
		"SELECT count(*) FROM sqlite_master" +
			" WHERE type='table' AND name='messages_fts'",
	).Scan(&ftsCount); err != nil {
		return fmt.Errorf("checking fts table: %w", err)
	}
	hadFTS := ftsCount > 0

	// Attempt to initialize FTS. Failure is non-fatal
	// (might be missing module).
	if _, err := w.Exec(schemaFTS); err != nil {
		if !strings.Contains(
			err.Error(), "no such module",
		) {
			return fmt.Errorf("initializing FTS: %w", err)
		}
	} else if !hadFTS {
		// Schema init succeeded and we didn't have FTS
		// before. Populate the index for existing messages.
		if _, err := w.Exec(
			"INSERT INTO messages_fts(messages_fts)" +
				" VALUES('rebuild')",
		); err != nil {
			return fmt.Errorf("backfilling FTS: %w", err)
		}
	}

	return nil
}

// Close closes both writer and reader connections, plus any
// retired pools left over from previous Reopen calls.
func (db *DB) Close() error {
	db.mu.Lock()
	w := db.getWriter()
	r := db.getReader()
	retired := db.retired
	db.retired = nil
	db.mu.Unlock()

	errs := []error{w.Close(), r.Close()}
	for _, p := range retired {
		errs = append(errs, p.Close())
	}
	return errors.Join(errs...)
}

// CloseConnections closes both connections without reopening,
// releasing file locks so the database file can be renamed.
// Also drains any retired pools from previous Reopen calls.
// Callers must call Reopen afterwards to restore service.
func (db *DB) CloseConnections() error {
	db.mu.Lock()
	defer db.mu.Unlock()

	errs := []error{
		db.getWriter().Close(),
		db.getReader().Close(),
	}
	for _, p := range db.retired {
		errs = append(errs, p.Close())
	}
	db.retired = nil
	return errors.Join(errs...)
}

// Reopen closes and reopens both connections to the same
// path. Used after an atomic file swap to pick up the new
// database contents. Preserves cursorSecret.
func (db *DB) Reopen() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.reopenLocked()
}

// reopenLocked performs the reopen while db.mu is already
// held. New connections are opened before closing old ones
// so the struct never points at closed handles on failure.
func (db *DB) reopenLocked() error {
	writer, err := sql.Open(
		"sqlite3", makeDSN(db.path, false),
	)
	if err != nil {
		return fmt.Errorf("reopening writer: %w", err)
	}
	writer.SetMaxOpenConns(1)

	reader, err := sql.Open(
		"sqlite3", makeDSN(db.path, true),
	)
	if err != nil {
		writer.Close()
		return fmt.Errorf("reopening reader: %w", err)
	}
	reader.SetMaxOpenConns(4)

	// Close pools from any previous reopen. They have been
	// retired for at least one full Reopen cycle, so all
	// in-flight queries on them have long since completed.
	for _, p := range db.retired {
		if err := p.Close(); err != nil {
			log.Printf(
				"warning: closing retired db pool: %v", err,
			)
		}
	}
	db.retired = db.retired[:0]

	oldWriter := db.writer.Swap(writer)
	oldReader := db.reader.Swap(reader)

	// Retire the just-swapped pools. Concurrent readers that
	// loaded the old pointer before the swap may still have
	// in-flight queries; these pools will be closed on the
	// next Reopen, CloseConnections, or Close call.
	db.retired = append(db.retired, oldWriter, oldReader)
	return nil
}

// Update executes fn within a write lock and transaction.
// The transaction is committed if fn returns nil, rolled back
// otherwise.
func (db *DB) Update(fn func(tx *sql.Tx) error) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.getWriter().Begin()
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Reader returns the read-only connection pool.
func (db *DB) Reader() *sql.DB {
	return db.getReader()
}

// GetSyncState reads a value from the pg_sync_state table.
func (db *DB) GetSyncState(key string) (string, error) {
	var value string
	err := db.getReader().QueryRow(
		"SELECT value FROM pg_sync_state WHERE key = ?", key,
	).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

// SetSyncState writes a value to the pg_sync_state table.
func (db *DB) SetSyncState(key, value string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.getWriter().Exec(
		`INSERT INTO pg_sync_state (key, value)
		 VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	)
	return err
}
