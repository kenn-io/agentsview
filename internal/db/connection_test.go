package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
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
	configureReaderPool(reader)
	blocked := make(chan struct{})
	hookReached := make(chan struct{}, readerMaxOpenConns)
	var unblockOnce sync.Once
	unblock := func() { unblockOnce.Do(func() { close(blocked) }) }
	var summaries sync.WaitGroup
	t.Cleanup(func() {
		unblock()
		summaries.Wait()
	})
	connections := make([]*sql.Conn, 0, readerMaxOpenConns)
	for range readerMaxOpenConns {
		conn, err := reader.Conn(t.Context())
		require.NoError(t, err)
		connections = append(connections, conn)
		require.NoError(t, conn.Raw(func(driverConn any) error {
			return driverConn.(*sqlite3.SQLiteConn).RegisterFunc("strftime", func(...any) string {
				select {
				case hookReached <- struct{}{}:
				default:
				}
				<-blocked
				return "2024-06-01"
			}, false)
		}))
	}
	for _, conn := range connections {
		require.NoError(t, conn.Close())
	}
	results := make(chan error, readerMaxOpenConns)
	for range readerMaxOpenConns {
		summaries.Add(1)
		go func() {
			defer summaries.Done()
			_, err := database.GetAnalyticsSummary(t.Context(), baseFilter())
			results <- err
		}()
	}
	for range bulkReadSlots {
		select {
		case <-hookReached:
		case <-time.After(10 * time.Second):
			t.Fatal("analytics did not reach the blocking strftime hook")
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	session, err := database.GetSession(ctx, "summary")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "summary", session.ID)
	unblock()
	for range readerMaxOpenConns {
		select {
		case err := <-results:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("analytics did not finish after unblocking reads")
		}
	}
}

func TestAcquireBulkReadHonorsContext(t *testing.T) {
	database := &DB{}
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
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	release, err := database.acquireBulkRead(ctx)
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, release)
	releases[0]()
	releases = releases[1:]
	release, err = database.acquireBulkRead(t.Context())
	require.NoError(t, err)
	release()
}
