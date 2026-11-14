//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenConfiguresThreadCount(t *testing.T) {
	duck, err := Open(t.Context(), filepath.Join(t.TempDir(), "threads.duckdb"))
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, duck.Close())
	})

	var got string
	require.NoError(t, duck.QueryRowContext(t.Context(), `
		SELECT value FROM duckdb_settings() WHERE name = 'threads'`,
	).Scan(&got))
	assert.Equal(t, strconv.Itoa(duckDBThreadCount()), got)
}

func TestLocalConnectionLimits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	writer, err := Open(t.Context(), path)
	require.NoError(t, err)
	assert.Equal(t, 1, writer.Stats().MaxOpenConnections)
	require.NoError(t, writer.Close())

	for _, tc := range []struct {
		name string
		open func(context.Context, string) (*sql.DB, error)
		max  int
	}{
		{"probe", OpenReadOnly, 1},
		{"serving", openServingReadOnly, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			duck, err := tc.open(t.Context(), path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, duck.Close()) })
			assert.Equal(t, tc.max, duck.Stats().MaxOpenConnections)
			for range tc.max {
				conn, err := duck.Conn(t.Context())
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, conn.Close()) })
				var threads string
				require.NoError(t, conn.QueryRowContext(t.Context(),
					"SELECT value FROM duckdb_settings() WHERE name = 'threads'",
				).Scan(&threads))
				assert.Equal(t, strconv.Itoa(duckDBThreadCount()), threads)
			}
			assert.Equal(t, tc.max, duck.Stats().InUse)
		})
	}
}
