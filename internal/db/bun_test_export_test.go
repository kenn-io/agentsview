package db

import "github.com/uptrace/bun"

// BunWriterForTest exposes the guarded SQLite test store to external
// cross-backend contracts without adding a production-only accessor.
func (db *DB) BunWriterForTest() bun.IDB {
	return db.bunWriter
}
