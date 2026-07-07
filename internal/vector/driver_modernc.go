//go:build windows || !cgo

package vector

import (
	"net/url"

	_ "modernc.org/sqlite"
)

// vectorDriverName selects the database/sql driver vectors.db opens with.
// The cgo sqlite-vec bindings do not build on Windows, so kit's sqlitevec
// registers the pure-Go modernc.org/sqlite/vec extension there (via
// sqlite3_auto_extension at package init) and expects databases opened with
// modernc's "sqlite" driver; sqlitevec.Register is a no-op in this build.
// The main archive keeps its own driver — only vectors.db switches.
const vectorDriverName = "sqlite"

// vectorDSN builds the modernc "sqlite" DSN for vectors.db, matching the
// pragmas driver_cgo.go sets through mattn's `_`-prefixed params: WAL, busy
// timeout, and NORMAL synchronous on the rw path; mode=ro plus a busy
// timeout on the ro path so a reader waits out a concurrent writer's lock
// instead of failing immediately with SQLITE_BUSY. modernc expresses
// connection pragmas as repeated `_pragma=name(value)` params on a file:
// URI.
//
// The path component is percent-encoded (slashes kept intact) for the same
// reason as driver_cgo.go: SQLite percent-decodes URI paths and splits
// params at `?`, so a raw `%`, `?`, or `#` in the path would be misparsed.
func vectorDSN(path string, readOnly bool) string {
	params := url.Values{}
	params.Add("_pragma", "busy_timeout(5000)")
	if readOnly {
		params.Set("mode", "ro")
	} else {
		params.Add("_pragma", "journal_mode(WAL)")
		params.Add("_pragma", "synchronous(NORMAL)")
	}
	escaped := (&url.URL{Path: path}).EscapedPath()
	return "file:" + escaped + "?" + params.Encode()
}
