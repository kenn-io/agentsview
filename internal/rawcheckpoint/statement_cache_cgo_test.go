//go:build !windows && cgo

package rawcheckpoint

import (
	"sync/atomic"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

func TestStoreReusesHeadSQL(t *testing.T) {
	store := openTestStore(t)
	require.NoError(t, store.SetDevice(t.Context(), "device-a"))
	heads := []struct {
		key    string
		commit rawsync.CommitResult
	}{
		{"first", testCommitResult(1, 1)},
		{"second", testCommitResult(2, 1)},
	}
	for _, head := range heads {
		require.NoError(t, store.AdvanceHead(t.Context(), "device-a", parser.AgentClaude,
			"root-a", head.key, "", head.commit))
	}
	store.db.SetMaxOpenConns(1)
	var preparations atomic.Int64
	conn, err := store.db.Conn(t.Context())
	require.NoError(t, err)
	require.NoError(t, conn.Raw(func(raw any) error {
		// The authorizer runs during compilation, not for each row read.
		raw.(*sqlite3.SQLiteConn).RegisterAuthorizer(func(op int, _, _, _ string) int {
			if op == sqlite3.SQLITE_SELECT {
				preparations.Add(1)
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	}))
	require.NoError(t, conn.Close())
	_, _, err = store.SourceHead(t.Context(), parser.AgentClaude, "root-a", "first")
	require.NoError(t, err)
	preparations.Store(0)

	for range 10 {
		for _, want := range heads {
			head, found, err := store.SourceHead(t.Context(), parser.AgentClaude, "root-a", want.key)
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, want.commit.ManifestID, head.ManifestID)
			assert.Equal(t, want.commit.Receipt, head.Receipt)
			assert.Equal(t, int64(1), head.Generation)
		}
		_, found, err := store.SourceHead(t.Context(), parser.AgentClaude, "root-a", "missing")
		require.NoError(t, err)
		assert.False(t, found)
	}
	assert.Zero(t, preparations.Load(), "head lookups must reuse compiled SQL after warmup")
}
