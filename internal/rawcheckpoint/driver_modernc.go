//go:build windows || !cgo

package rawcheckpoint

import (
	"net/url"

	_ "modernc.org/sqlite"
)

// checkpointDriverName selects the database/sql driver the checkpoint
// database opens with. The cgo bindings do not build on Windows, so the
// checkpoint database opens with modernc's pure-Go "sqlite" driver there.
// The main archive keeps its own driver — only the checkpoint database
// switches.
const checkpointDriverName = "sqlite"

// checkpointDSN builds the modernc "sqlite" DSN for the checkpoint database,
// matching the pragmas driver_cgo.go sets through mattn's `_`-prefixed
// params: WAL, busy timeout, and NORMAL synchronous on the rw path; mode=ro
// plus a busy timeout on the ro path so a reader waits out a concurrent
// writer's lock instead of failing immediately with SQLITE_BUSY. modernc
// expresses connection pragmas as repeated `_pragma=name(value)` params on a
// file: URI.
//
// The path component is percent-encoded (slashes kept intact) for the same
// reason as driver_cgo.go: SQLite percent-decodes URI paths and splits
// params at `?`, so a raw `%`, `?`, or `#` in the path would be misparsed.
func checkpointDSN(path string, readOnly bool) string {
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
