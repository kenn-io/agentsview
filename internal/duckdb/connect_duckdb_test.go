//go:build !(windows && arm64)

package duckdb

import (
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

func TestServingReadOnlyConnections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pool.duckdb")
	writable, err := Open(t.Context(), path)
	require.NoError(t, err)
	assert.Equal(t, 1, writable.Stats().MaxOpenConnections)
	require.NoError(t, writable.Close())

	readOnly, err := OpenReadOnly(t.Context(), path)
	require.NoError(t, err)
	assert.Equal(t, 1, readOnly.Stats().MaxOpenConnections)
	require.NoError(t, readOnly.Close())

	serving, err := openServingReadOnly(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, serving.Close()) })
	assert.Equal(t, 4, serving.Stats().MaxOpenConnections)
	first, err := serving.Conn(t.Context())
	require.NoError(t, err)
	defer first.Close()
	second, err := serving.Conn(t.Context())
	require.NoError(t, err)
	defer second.Close()
	for _, conn := range []*sql.Conn{first, second} {
		var threads string
		require.NoError(t, conn.QueryRowContext(t.Context(), `SELECT value FROM duckdb_settings() WHERE name = 'threads'`).Scan(&threads))
		assert.Equal(t, strconv.Itoa(duckDBThreadCount()), threads)
	}
}
