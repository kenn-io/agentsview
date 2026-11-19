//go:build !windows && cgo

package vector

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVectorMirrorReusesSQL(t *testing.T) {
	ix := openTestIndex(t)
	var preparations atomic.Int64
	conn, err := ix.db.Conn(t.Context())
	require.NoError(t, err)
	require.NoError(t, conn.Raw(func(raw any) error {
		// Count compilation of the mirror's slot and content-hash lookups.
		raw.(*sqlite3.SQLiteConn).RegisterAuthorizer(func(op int, _, _, _ string) int {
			if op == sqlite3.SQLITE_SELECT {
				preparations.Add(1)
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	}))
	require.NoError(t, conn.Close())

	sentinel := 0
	for i := range 22 {
		unit := userDoc("session", fmt.Sprintf("source-%d", i), i, fmt.Sprintf("document %d", i))
		key := DocKey(unit.Kind, unit.SessionID, unit.SourceUUID, i, 1)
		unchanged, evicted, err := ix.upsertMirrorRow(t.Context(), key, unit, &sentinel)
		require.NoError(t, err)
		require.False(t, unchanged)
		require.Empty(t, evicted)
		if i == 1 {
			preparations.Store(0)
		}
	}
	assert.Zero(t, preparations.Load(), "later documents must reuse compiled mirror lookups")
	var count int
	require.NoError(t, ix.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM vector_messages`).Scan(&count))
	assert.Equal(t, 22, count)
	var content string
	require.NoError(t, ix.db.QueryRowContext(t.Context(),
		`SELECT content FROM vector_messages WHERE session_id = 'session' AND ordinal = 21`).Scan(&content))
	assert.Equal(t, "document 21", content)
}
