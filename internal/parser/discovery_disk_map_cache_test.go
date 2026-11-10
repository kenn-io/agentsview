package parser

import (
	"sync/atomic"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoveryDiskMapReusesLookupSQL(t *testing.T) {
	index, err := newDiscoveryDiskMap(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.close()) })
	require.NoError(t, index.put(t.Context(), "first", "project-a", true))
	require.NoError(t, index.put(t.Context(), "second", "project-b", true))

	var preparations atomic.Int64
	conn, err := index.db.Conn(t.Context())
	require.NoError(t, err)
	require.NoError(t, conn.Raw(func(raw any) error {
		// SQLite calls the authorizer while compiling SQL, not while reading rows.
		raw.(interface {
			RegisterAuthorizer(func(int, string, string, string) int)
		}).RegisterAuthorizer(func(op int, _, _, _ string) int {
			if op == sqlite3.SQLITE_SELECT {
				preparations.Add(1)
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	}))
	require.NoError(t, conn.Close())
	_, _, err = index.get(t.Context(), "first")
	require.NoError(t, err)
	preparations.Store(0)

	for range 10 {
		for _, tc := range []struct {
			key, value string
			found      bool
		}{
			{"first", "project-a", true},
			{"second", "project-b", true},
			{"missing", "", false},
		} {
			value, found, err := index.get(t.Context(), tc.key)
			require.NoError(t, err)
			assert.Equal(t, tc.found, found)
			assert.Equal(t, tc.value, value)
		}
	}
	assert.Zero(t, preparations.Load(), "repeated discovery lookups must reuse compiled SQL")
}
