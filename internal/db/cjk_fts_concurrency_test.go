package db

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
)

func TestCJKSearchUsesGuardedTransactionDuringReopen(t *testing.T) {
	if !simpleFTSRuntimeConfig.available() {
		t.Skip("simple FTS5 runtime is not installed for this test process")
	}
	for _, route := range []string{"sessions", "content", "hybrid"} {
		t.Run(route, func(t *testing.T) {
			d := testDB(t)
			seedSearchSession(t, d, "match", "proj", [][2]string{
				{"user", "中文和搜索之间插入了额外内容。"},
			})
			// One transaction occupies the entire pool. Query preparation must
			// use its connection as well as its archive-lifecycle guard.
			d.reader.Load().SetMaxOpenConns(1)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			reopenDone := make(chan error, 1)
			searchDone := make(chan struct{})
			var count int
			var searchErr error
			err := d.consistentView(ctx, func(store bun.IDB) error {
				go func() { reopenDone <- d.Reopen() }()
				queued := assert.Eventually(t, func() bool {
					if d.connMu.TryRLock() {
						d.connMu.RUnlock()
						return false
					}
					return true
				}, time.Second, time.Millisecond, "reopen must wait behind the search transaction")
				if !queued {
					close(searchDone)
					return nil
				}
				go func() {
					defer close(searchDone)
					capability := sqliteFullTextCapability{store: d}
					filter := ContentSearchFilter{Pattern: "中文 搜索", Limit: 10}
					switch route {
					case "sessions":
						hits, err := capability.Search(ctx, store, SearchFilter{Query: "中文 搜索", Limit: 10})
						count, searchErr = len(hits), err
					case "content":
						hits, err := capability.SearchContent(ctx, store, filter)
						count, searchErr = len(hits), err
					case "hybrid":
						hits, err := capability.SearchHybridContent(ctx, store, filter)
						count, searchErr = len(hits), err
					}
				}()
				select {
				case <-searchDone:
					return searchErr
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			// Releasing the outer view also lets a broken nested reader finish,
			// so a regression fails without leaving goroutines or pools behind.
			require.NoError(t, <-reopenDone)
			<-searchDone
			require.NoError(t, err)
			require.NoError(t, searchErr)
			assert.Equal(t, 1, count, "segmented search still finds the message")
		})
	}
}
