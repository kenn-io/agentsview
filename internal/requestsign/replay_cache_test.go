//go:build cgo

package requestsign

import (
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReplayReusesAdmissionSQL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replay.db")
	require.NoError(t, InitReplay(t.Context(), path))
	store, err := OpenReplay(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	var preparations atomic.Int64
	conn, err := store.db.Conn(t.Context())
	require.NoError(t, err)
	require.NoError(t, conn.Raw(func(raw any) error {
		// SQLite authorizes SELECTs while compiling, not while reading rows.
		raw.(*sqlite3.SQLiteConn).RegisterAuthorizer(func(op int, _, _, _ string) int {
			if op == sqlite3.SQLITE_SELECT {
				preparations.Add(1)
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	}))
	require.NoError(t, conn.Close())
	now := time.Now()
	require.NoError(t, store.Admit(t.Context(), "key-a", fmt.Sprintf("%032x", 0), now.Unix(), now.Unix()+30, now))
	preparations.Store(0)
	var nonce string
	for i := range 10 {
		nonce = fmt.Sprintf("%032x", i+1)
		now = time.Now()
		require.NoError(t, store.Admit(t.Context(), "key-a", nonce, now.Unix(), now.Unix()+30, now))
	}
	assert.Zero(t, preparations.Load(), "later admissions must reuse their compiled SQL")
	now = time.Now()
	require.ErrorIs(t, store.Admit(t.Context(), "key-a", nonce, now.Unix(), now.Unix()+30, now), ErrReplay)
	require.NoError(t, store.Admit(t.Context(), "key-b", nonce, now.Unix(), now.Unix()+30, now))
}
