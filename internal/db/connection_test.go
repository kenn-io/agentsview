package db

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
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

func TestSubstringSearchesAndAnalyticsCanReadTogether(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, "parallel", "project")
	insertMessages(t, database, userMsg("parallel", 0, "needle"))

	started := make(chan struct{}, bulkReadSlots+bulkSearchSlots)
	release := make(chan struct{})
	var releaseOnce sync.Once
	var workers sync.WaitGroup
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		workers.Wait()
	})
	connections := make([]*sql.Conn, 0, bulkReadSlots+bulkSearchSlots)
	for range bulkReadSlots + bulkSearchSlots {
		conn, err := database.rawReader().Conn(t.Context())
		require.NoError(t, err)
		connections = append(connections, conn)
		var once sync.Once
		require.NoError(t, conn.Raw(func(raw any) error {
			raw.(*diagnosticReaderConn).RegisterAuthorizer(func(op int, _, _, _ string) int {
				if op == sqlite3.SQLITE_READ {
					once.Do(func() {
						started <- struct{}{}
						<-release
					})
				}
				return sqlite3.SQLITE_OK
			})
			return nil
		}))
	}
	for _, conn := range connections {
		require.NoError(t, conn.Close())
	}
	results := make(chan error, bulkReadSlots+bulkSearchSlots)
	for range 4 {
		workers.Go(func() {
			page, err := database.SearchContent(t.Context(), ContentSearchFilter{Pattern: "needle", Mode: "substring", Sources: []string{"messages"}, IncludeOneShot: true})
			if err == nil && (len(page.Matches) != 1 || page.Matches[0].SessionID != "parallel") {
				err = fmt.Errorf("unexpected search matches: %v", page.Matches)
			}
			results <- err
		})
		workers.Go(func() {
			_, err := database.GetAnalyticsSummary(t.Context(), AnalyticsFilter{Timezone: "UTC"})
			results <- err
		})
	}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for range bulkReadSlots + bulkSearchSlots {
		select {
		case <-started:
		case <-deadline.C:
			require.FailNow(t, "searches and analytics did not all reach the reader pool")
		}
	}
	quickCtx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	session, err := database.GetSession(quickCtx, "parallel")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "parallel", session.ID)
	probe, err := database.ActivityReportSourceProbe(quickCtx)
	require.NoError(t, err)
	assert.EqualValues(t, 1, probe.SessionCount)
	releaseOnce.Do(func() { close(release) })
	for range bulkReadSlots + bulkSearchSlots {
		require.NoError(t, <-results)
	}
}

func TestSearchContentDeadlineReleasesSearchLane(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, "session", "project")
	insertMessages(t, database, userMsg("session", 0, "needle"))
	reader := database.rawReader()
	var connections []*sql.Conn
	t.Cleanup(func() {
		for _, conn := range connections {
			_ = conn.Close()
		}
	})
	for range readerMaxOpenConns {
		conn, err := reader.Conn(t.Context())
		require.NoError(t, err)
		connections = append(connections, conn)
	}
	waits := reader.Stats().WaitCount
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	results := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := database.SearchContent(ctx, ContentSearchFilter{Pattern: "needle", Mode: "substring", Sources: []string{"messages"}, IncludeOneShot: true})
		results <- err
	}()
	t.Cleanup(func() { cancel(); <-done })
	require.Eventually(t, func() bool { return reader.Stats().WaitCount > waits }, time.Second, 10*time.Millisecond)
	// The query is waiting for a reader after acquiring its search slot.
	err := <-results
	require.ErrorIs(t, err, context.DeadlineExceeded)
	for _, conn := range connections {
		require.NoError(t, conn.Close())
	}
	connections = nil
	var releases []func()
	t.Cleanup(func() {
		for _, release := range releases {
			release()
		}
	})
	for range bulkSearchSlots - 1 {
		release, err := database.acquireBulkSearch(t.Context())
		require.NoError(t, err)
		releases = append(releases, release)
	}
	probeCtx, probeCancel := context.WithTimeout(t.Context(), time.Second)
	defer probeCancel()
	page, err := database.SearchContent(probeCtx, ContentSearchFilter{Pattern: "needle", Mode: "substring", Sources: []string{"messages"}, IncludeOneShot: true})
	require.NoError(t, err)
	require.Len(t, page.Matches, 1)
	assert.Equal(t, "session", page.Matches[0].SessionID)
}

func TestSessionSearchSharesSearchLane(t *testing.T) {
	database := testDB(t)
	require.True(t, database.HasFTS(t.Context()), "run with -tags fts5")
	insertSession(t, database, "session", "project")
	insertMessages(t, database, userMsg("session", 0, "needle0 needle1 needle2 needle3 needle4 needle5 needle6 needle7 needle8"))
	ready := make(chan struct{}, readerMaxOpenConns)
	start := make(chan struct{})
	started := make(chan struct{}, readerMaxOpenConns)
	release := make(chan struct{})
	var startOnce, releaseOnce sync.Once
	var workers sync.WaitGroup
	t.Cleanup(func() {
		startOnce.Do(func() { close(start) })
		releaseOnce.Do(func() { close(release) })
		workers.Wait()
	})
	database.afterSearchRead = func() {
		ready <- struct{}{}
		<-start
	}
	var connections []*sql.Conn
	for range readerMaxOpenConns {
		conn, err := database.rawReader().Conn(t.Context())
		require.NoError(t, err)
		connections = append(connections, conn)
		var once sync.Once
		require.NoError(t, conn.Raw(func(raw any) error {
			raw.(*diagnosticReaderConn).RegisterAuthorizer(func(op int, table, _, _ string) int {
				if op == sqlite3.SQLITE_READ && strings.HasPrefix(table, "messages_fts") {
					once.Do(func() {
						started <- struct{}{}
						<-release
					})
				}
				return sqlite3.SQLITE_OK
			})
			return nil
		}))
	}
	for _, conn := range connections {
		require.NoError(t, conn.Close())
	}
	searchResults := make(chan error, readerMaxOpenConns)
	for i := range readerMaxOpenConns {
		workers.Go(func() {
			page, err := database.Search(t.Context(), SearchFilter{Query: fmt.Sprintf("needle%d", i)})
			if err == nil && (len(page.Results) != 1 || page.Results[0].SessionID != "session") {
				err = fmt.Errorf("unexpected search results: %v", page.Results)
			}
			searchResults <- err
		})
	}
	guard := time.NewTimer(10 * time.Second)
	defer guard.Stop()
	for range readerMaxOpenConns {
		select {
		case <-ready:
		case <-guard.C:
			require.FailNow(t, "session searches did not reach the search path")
		}
	}
	startOnce.Do(func() { close(start) })
	for range bulkSearchSlots {
		select {
		case <-started:
		case <-guard.C:
			require.FailNow(t, "session searches did not reach SQLite")
		}
	}
	select {
	case <-started:
		require.FailNow(t, "more than four session searches reached SQLite")
	case <-time.After(time.Second):
	}
	analytics := make(chan error, bulkReadSlots)
	for range bulkReadSlots {
		workers.Go(func() {
			_, err := database.GetAnalyticsSummary(t.Context(), AnalyticsFilter{Timezone: "UTC"})
			analytics <- err
		})
	}
	for range bulkReadSlots {
		select {
		case err := <-analytics:
			require.NoError(t, err)
		case <-guard.C:
			require.FailNow(t, "analytics waited for session searches")
		}
	}
	session, err := database.GetSession(t.Context(), "session")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "session", session.ID)
	releaseOnce.Do(func() { close(release) })
	for range readerMaxOpenConns {
		select {
		case err := <-searchResults:
			require.NoError(t, err)
		case <-guard.C:
			require.FailNow(t, "session searches did not finish")
		}
	}
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
