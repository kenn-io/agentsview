//go:build !windows && cgo

package vector

import (
	"database/sql"
	"fmt"
	"testing"

	sqlite3 "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

// setConnVarLimit lowers conn's SQLITE_LIMIT_VARIABLE_NUMBER through the
// mattn driver's raw-connection API; varlimit_modernc_test.go provides the
// equivalent for the modernc driver vectors.db uses on Windows or without
// cgo.
func setConnVarLimit(t *testing.T, conn *sql.Conn, limit int) {
	t.Helper()
	require.NoError(t, conn.Raw(func(dc any) error {
		sc, ok := dc.(*sqlite3.SQLiteConn)
		if !ok {
			return fmt.Errorf("index conn is %T, want *sqlite3.SQLiteConn", dc)
		}
		sc.SetLimit(sqlite3.SQLITE_LIMIT_VARIABLE_NUMBER, limit)
		return nil
	}))
}
