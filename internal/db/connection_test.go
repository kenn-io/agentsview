package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

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
				t.Helper()
				database, err := Open(t.Context(), path)
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
				t.Helper()
				database, err := Open(t.Context(), path)
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
				t.Helper()

				writable, err := Open(t.Context(), path)
				require.NoError(t, err)
				require.NoError(t, writable.Close())

				readonly, err := OpenReadOnly(t.Context(), path)
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
	path := filepath.Join(t.TempDir(), "sessions.db")
	database, err := Open(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, database.Close())
	})

	reader := database.rawReader()
	require.Equal(t, readerMaxOpenConns, reader.Stats().MaxOpenConnections)

	connections := make([]*sql.Conn, 0, readerMaxOpenConns)
	for range readerMaxOpenConns {
		conn, connErr := reader.Conn(t.Context())
		require.NoError(t, connErr)
		connections = append(connections, conn)
	}
	assert.Equal(t, readerMaxOpenConns, reader.Stats().InUse)

	for _, conn := range connections {
		require.NoError(t, conn.Close())
	}
	stats := reader.Stats()
	assert.Equal(t, readerMaxOpenConns, stats.Idle)
	assert.Zero(t, stats.MaxIdleClosed)
}

func TestQuickReadKeepsAReaderWhileAnalyticsFillThePool(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, "summary", "project", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
	})
	reader := database.rawReader()
	for range bulkReadSlots {
		release, err := database.acquireBulkRead(t.Context())
		require.NoError(t, err)
		defer release()
		conn, err := reader.Conn(t.Context())
		require.NoError(t, err)
		defer conn.Close()
	}
	quickCtx, quickCancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer quickCancel()
	session, err := database.GetSession(quickCtx, "summary")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "summary", session.ID)
	probe, err := database.ActivityReportSourceProbe(quickCtx)
	require.NoError(t, err)
	assert.EqualValues(t, 1, probe.SessionCount)
}

func TestAcquireBulkReadHonorsContext(t *testing.T) {
	database := &DB{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for range 100 {
		release, err := database.acquireBulkRead(ctx)
		if release != nil {
			release()
		}
		require.ErrorIs(t, err, context.Canceled)
		assert.Nil(t, release)
	}
	releases := make([]func(), 0, bulkReadSlots)
	for range bulkReadSlots {
		release, err := database.acquireBulkRead(t.Context())
		require.NoError(t, err)
		releases = append(releases, release)
	}
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	release, err := database.acquireBulkRead(ctx)
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, release)
	releases[0]()
	releases = releases[1:]
	release, err = database.acquireBulkRead(t.Context())
	require.NoError(t, err)
	release()
}
