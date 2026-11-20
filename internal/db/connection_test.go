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

func TestSearchWaitsForBulkReadAndLeavesReaderForQuickRead(t *testing.T) {
	for _, tc := range []struct {
		name, pattern, mode string
	}{
		{"sessions", "searchable", ""},
		{"content words", "searchable text", "fts"},
		{"content word", "searchable", "fts"},
		{"hybrid", "searchable", "hybrid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testSearchWaitsForBulkRead(t, tc.pattern, tc.mode)
		})
	}
}

func testSearchWaitsForBulkRead(t *testing.T, pattern, mode string) {
	t.Helper()
	database := testDB(t)
	seedSearchSession(t, database, "summary", "project", [][2]string{{"user", "searchable text"}})

	releases := make([]func(), 0, bulkReadSlots)
	connections := make([]*sql.Conn, 0, bulkReadSlots)
	t.Cleanup(func() {
		for _, conn := range connections {
			assert.NoError(t, conn.Close())
		}
		for _, release := range releases {
			release()
		}
	})
	for range bulkReadSlots {
		release, err := database.acquireBulkRead(t.Context())
		require.NoError(t, err)
		releases = append(releases, release)
		conn, err := database.rawReader().Conn(t.Context())
		require.NoError(t, err)
		connections = append(connections, conn)
	}

	type searchResult struct {
		ids []string
		err error
	}
	result := make(chan searchResult, 1)
	go func() {
		if mode != "" {
			page, err := database.SearchContent(t.Context(), ContentSearchFilter{Pattern: pattern, Mode: mode, KeywordOnly: mode == "hybrid"})
			var ids []string
			for _, match := range page.Matches {
				ids = append(ids, match.SessionID)
			}
			result <- searchResult{ids, err}
			return
		}
		page, err := database.Search(t.Context(), SearchFilter{Query: pattern})
		var ids []string
		for _, match := range page.Results {
			ids = append(ids, match.SessionID)
		}
		result <- searchResult{ids, err}
	}()
	select {
	case got := <-result:
		require.FailNow(t, "search returned while bulk slots were occupied", "%v", got.err)
	case <-time.After(100 * time.Millisecond): //nolint:kennlint // deadlinetest: absence check; occupied bulk slots keep the search waiting
	}
	quickCtx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	session, err := database.GetSession(quickCtx, "summary")
	require.NoError(t, err)
	require.NotNil(t, session)
	quickMode := mode
	if quickMode == "" {
		quickMode = "fts"
	}
	page, err := database.SearchContent(quickCtx, ContentSearchFilter{SessionID: "summary", Pattern: pattern, Mode: quickMode, KeywordOnly: quickMode == "hybrid"})
	require.NoError(t, err)
	require.Len(t, page.Matches, 1)

	require.NoError(t, connections[0].Close())
	connections = connections[1:]
	releases[0]()
	releases = releases[1:]
	select {
	case got := <-result:
		require.NoError(t, got.err)
		assert.Equal(t, []string{"summary"}, got.ids)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "search did not resume after a bulk slot was released")
	}
}

type blockingEmbeddingSearcher struct {
	fakeVectorSearcher
	started chan struct{}
	release chan struct{}
}

func (s *blockingEmbeddingSearcher) PrepareSemanticSearch(ctx context.Context, _ string) (VectorSearchPage, int, error) {
	close(s.started)
	select {
	case <-s.release:
		return func(context.Context, int) ([]VectorHit, bool, error) { return nil, true, nil }, 0, nil
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}
}

func TestHybridEmbeddingLeavesBulkReaderAvailable(t *testing.T) {
	database := testDB(t)
	seedSearchSession(t, database, "summary", "project", [][2]string{{"user", "searchable text"}})
	for range bulkReadSlots - 1 {
		release, err := database.acquireBulkRead(t.Context())
		require.NoError(t, err)
		defer release()
	}
	searcher := &blockingEmbeddingSearcher{started: make(chan struct{}), release: make(chan struct{})}
	database.SetVectorSearcher(searcher)
	done := make(chan error, 1)
	go func() {
		_, err := database.SearchContent(t.Context(), ContentSearchFilter{Pattern: "searchable", Mode: "hybrid"})
		done <- err
	}()
	defer func() {
		close(searcher.release)
		assert.NoError(t, <-done)
	}()
	select {
	case <-searcher.started:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "embedding did not start")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	page, err := database.SearchContent(ctx, ContentSearchFilter{Pattern: "searchable", Mode: "fts"})
	require.NoError(t, err)
	require.Len(t, page.Matches, 1)
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
