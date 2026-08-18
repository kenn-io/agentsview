package db

import (
	"crypto/rand"
	"database/sql"
	"fmt"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"
)

// OpenPreparedTestDB opens a private test database file that has already been
// initialized with the current schema and data version. It is intentionally
// test-only so production code cannot bypass the normal open/migration path.
func OpenPreparedTestDB(path string) (*DB, error) {
	writer, err := sql.Open("sqlite3", makeDSN(path, false))
	if err != nil {
		return nil, fmt.Errorf("opening prepared test writer: %w", err)
	}
	writer.SetMaxOpenConns(1)
	if err := configureWAL(writer); err != nil {
		writer.Close()
		return nil, fmt.Errorf("configuring prepared test wal: %w", err)
	}

	reader, err := sql.Open(sqliteUsageDriverName, makeDSN(path, true))
	if err != nil {
		writer.Close()
		return nil, fmt.Errorf("opening prepared test reader: %w", err)
	}
	reader.SetMaxOpenConns(4)

	db := &DB{path: path}
	db.writer.Store(writer)
	db.reader.Store(reader)
	db.bunWriter = bun.NewDB(writer, sqlitedialect.New())
	db.bunReader = bun.NewDB(reader, sqlitedialect.New())
	db.BunStore = NewBunStore(&sqliteBunBackend{store: db})

	db.cursorSecret = make([]byte, 32)
	if _, err := rand.Read(db.cursorSecret); err != nil {
		writer.Close()
		reader.Close()
		return nil, fmt.Errorf(
			"generating prepared test cursor secret: %w", err,
		)
	}
	db.BunStore.SetCursorSecret(db.cursorSecret)

	db.startWALCheckpointLoop()
	return db, nil
}
