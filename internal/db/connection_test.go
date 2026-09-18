package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const expectedSQLiteCacheSizeKiB = -8192

func assertSQLiteMemoryPragmas(
	t *testing.T,
	conn *sql.DB,
) {
	t.Helper()

	var got int
	require.NoError(t, conn.QueryRowContext(t.Context(), "PRAGMA cache_size").Scan(&got))
	assert.Equal(t, expectedSQLiteCacheSizeKiB, got)

	var mmapSize int64
	require.NoError(t, conn.QueryRowContext(t.Context(), "PRAGMA mmap_size").Scan(&mmapSize))
}

func TestSQLiteConnectionMemoryPragmas(t *testing.T) {
	tests := []struct {
		name string
		open func(*testing.T, string) *sql.DB
	}{
		{
			name: "Open writer",
			open: func(t *testing.T, path string) *sql.DB {
				database, err := Open(path)
				require.NoError(t, err)
				t.Cleanup(func() {
					require.NoError(t, database.Close())
				})
				return database.rawWriter()
			},
		},
		{
			name: "Open reader",
			open: func(t *testing.T, path string) *sql.DB {
				database, err := Open(path)
				require.NoError(t, err)
				t.Cleanup(func() {
					require.NoError(t, database.Close())
				})
				return database.rawReader()
			},
		},
		{
			name: "OpenReadOnly reader",
			open: func(t *testing.T, path string) *sql.DB {
				writable, err := Open(path)
				require.NoError(t, err)
				require.NoError(t, writable.Close())

				readonly, err := OpenReadOnly(path)
				require.NoError(t, err)
				t.Cleanup(func() {
					require.NoError(t, readonly.Close())
				})
				return readonly.rawReader()
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "sessions.db")
			assertSQLiteMemoryPragmas(t, tt.open(t, path))
		})
	}
}

func TestReaderPoolRetainsConfiguredBurstConnections(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	path := filepath.Join(t.TempDir(), "sessions.db")
	database, err := Open(path)
	require.NoError(err)
	t.Cleanup(func() {
		require.NoError(database.Close())
	})

	reader := database.rawReader()
	require.Equal(readerMaxOpenConns, reader.Stats().MaxOpenConnections)

	connections := make([]*sql.Conn, 0, readerMaxOpenConns)
	for range readerMaxOpenConns {
		conn, connErr := reader.Conn(t.Context())
		require.NoError(connErr)
		connections = append(connections, conn)
	}
	assert.Equal(readerMaxOpenConns, reader.Stats().InUse)

	for _, conn := range connections {
		require.NoError(conn.Close())
	}
	stats := reader.Stats()
	assert.Equal(readerMaxOpenConns, stats.Idle)
	assert.Zero(stats.MaxIdleClosed)
}
