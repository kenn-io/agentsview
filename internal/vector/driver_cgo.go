//go:build !windows && cgo

package vector

import (
	"net/url"

	_ "github.com/mattn/go-sqlite3"
)

// vectorDriverName selects the database/sql driver vectors.db opens with.
// On Unix with cgo it is mattn/go-sqlite3 — the same driver as the main
// archive — with the cgo sqlite-vec extension kit's sqlitevec.Register
// loads. On Windows the cgo sqlite-vec bindings do not build, so
// driver_modernc.go substitutes the pure-Go modernc driver instead.
const vectorDriverName = "sqlite3"

// vectorDSN builds the sqlite3 DSN for vectors.db. The rw path mirrors
// internal/db.Open's pragmas (WAL, busy timeout, NORMAL synchronous); the
// ro path opens with mode=ro and no immutable hint, since vectors.db can be
// concurrently rewritten by another agentsview process, and carries its own
// busy timeout so a reader waits out a concurrent writer's lock instead of
// failing immediately with SQLITE_BUSY.
//
// Both branches emit a file: URI. mattn/go-sqlite3 forwards the `_`-prefixed
// pragma params either way, but it only honors mode=ro when the DSN carries
// the file: scheme — a bare path silently opens read-write, so the ro
// contract depends on the prefix.
//
// The path component is percent-encoded (slashes kept intact): SQLite
// percent-decodes URI paths and splits params at `?`, so a raw path
// containing `%`, `?`, or `#` would be misparsed — e.g. a literal "%41" in a
// directory name would silently open a different file.
func vectorDSN(path string, readOnly bool) string {
	params := url.Values{}
	if readOnly {
		params.Set("mode", "ro")
		params.Set("_busy_timeout", "5000")
	} else {
		params.Set("_journal_mode", "WAL")
		params.Set("_busy_timeout", "5000")
		params.Set("_synchronous", "NORMAL")
	}
	escaped := (&url.URL{Path: path}).EscapedPath()
	return "file:" + escaped + "?" + params.Encode()
}
