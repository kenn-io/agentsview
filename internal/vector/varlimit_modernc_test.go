//go:build windows || !cgo

package vector

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"modernc.org/sqlite"
	sqlite3lib "modernc.org/sqlite/lib"
)

// setConnVarLimit lowers conn's SQLITE_LIMIT_VARIABLE_NUMBER through the
// modernc driver's Limit API; varlimit_cgo_test.go provides the equivalent
// for the mattn driver vectors.db uses on Unix with cgo.
func setConnVarLimit(t *testing.T, conn *sql.Conn, limit int) {
	t.Helper()
	_, err := sqlite.Limit(conn, sqlite3lib.SQLITE_LIMIT_VARIABLE_NUMBER, limit)
	require.NoError(t, err)
}
