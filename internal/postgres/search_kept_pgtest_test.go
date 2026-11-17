//go:build pgtest

package postgres

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

type searchQueryTracer struct {
	count    atomic.Int32
	versions atomic.Int32
}

func (tr *searchQueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "msg_matches AS") {
		tr.count.Add(1)
	}
	if strings.Contains(data.SQL, "to_regclass('usage_session_changes')") {
		tr.versions.Add(1)
	}
	return ctx
}
func (*searchQueryTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestSearchKeptSingleConnection(t *testing.T) {
	local, syncer, store := newUsageJournalFixture(t)
	ctx := t.Context()
	require.NoError(t, local.UpsertSession(ctx, db.Session{ID: "session", Project: "project", Machine: "device", Agent: "claude", MessageCount: 1}))
	require.NoError(t, local.ReplaceSessionMessages(ctx, "session", []db.Message{{SessionID: "session", Role: "user", Content: "needle0 needle1 needle2 needle3 needle4"}}))
	pgKeptPush(t, syncer, false)
	store.DB().SetMaxOpenConns(1)
	searchCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	page, err := store.Search(searchCtx, db.SearchFilter{Query: "needle"})
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	require.Equal(t, "session", page.Results[0].SessionID)
	store.DB().SetMaxOpenConns(5)
	concurrentCtx, cancelConcurrent := context.WithTimeout(ctx, 5*time.Second)
	defer cancelConcurrent()
	type result struct {
		page db.SearchPage
		err  error
	}
	results := make(chan result, 5)
	for i := range 5 {
		go func() {
			page, err := store.Search(concurrentCtx, db.SearchFilter{Query: fmt.Sprintf("needle%d", i)})
			results <- result{page, err}
		}()
	}
	for range 5 {
		select {
		case got := <-results:
			require.NoError(t, got.err)
			require.Len(t, got.page.Results, 1)
			require.Equal(t, "session", got.page.Results[0].SessionID)
		case <-concurrentCtx.Done():
			t.Fatal("concurrent searches did not finish")
		}
	}
}

func TestSearchKeptRestart(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, first := newUsageJournalFixture(t)
	ctx := t.Context()
	url := testPGURL(t)
	cache := t.TempDir()
	require.NoError(t, first.ConfigureUsageCache(ctx, cache, url, syncer.schema))
	require.NoError(t, local.UpsertSession(ctx, db.Session{ID: "session", Project: "project", Machine: "device", Agent: "claude", MessageCount: 1}))
	write := func(content string) {
		t.Helper()
		require.NoError(t, local.ReplaceSessionMessages(ctx, "session", []db.Message{{SessionID: "session", Role: "user", Content: content}}))
		pgKeptPush(t, syncer, false)
	}
	write("needle first")
	filter := db.SearchFilter{Query: "needle"}
	want, err := first.Search(ctx, filter)
	require.NoError(t, err)
	require.Len(t, want.Results, 1)
	open := func() (*Store, *searchQueryTracer) {
		t.Helper()
		config, err := pgx.ParseConfig(url)
		require.NoError(t, err)
		config.RuntimeParams["search_path"] = syncer.schema
		config.RuntimeParams["TimeZone"] = "UTC"
		trace := &searchQueryTracer{}
		config.Tracer = trace
		pool := stdlib.OpenDB(*config)
		s := &Store{pg: pool, ownedDB: pool}
		t.Cleanup(func() { require.NoError(t, s.Close()) })
		require.NoError(t, s.ConfigureUsageCache(ctx, cache, url, syncer.schema))
		return s, trace
	}
	s, trace := open()
	got, err := s.Search(ctx, filter)
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Zero(t, trace.count.Load())
	require.EqualValues(t, 2, trace.versions.Load())
	other, err := s.Search(ctx, db.SearchFilter{Query: "unmatched-token"})
	require.NoError(t, err)
	require.Empty(t, other.Results)
	require.Positive(t, trace.count.Load())
	before := trace.count.Load()
	got, err = s.Search(ctx, filter)
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, before, trace.count.Load())
	write("needle second")
	got, err = s.Search(ctx, filter)
	require.NoError(t, err)
	require.Len(t, got.Results, 1)
	require.Contains(t, got.Results[0].Snippet, "second")
	require.Greater(t, trace.count.Load(), before)
	s, trace = open()
	got, err = s.Search(ctx, filter)
	require.NoError(t, err)
	require.Len(t, got.Results, 1)
	require.Contains(t, got.Results[0].Snippet, "second")
	require.Zero(t, trace.count.Load())
}
