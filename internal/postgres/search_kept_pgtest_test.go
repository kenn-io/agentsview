//go:build pgtest

package postgres

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

type searchQueryTracer struct {
	count atomic.Int32
}

func (tr *searchQueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "msg_matches AS") {
		tr.count.Add(1)
	}
	return ctx
}
func (*searchQueryTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

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
	version := snapshotUsageVersion(ctx, s.pg, 0)
	filter.Limit = db.DefaultSearchLimit
	_, ok := s.trendsDisk.Get("search|"+filter.Slot(), "json|v1|"+version)
	require.True(t, ok)
	s.trendsDisk.Put("other", "v1", []byte("other cache"))
	require.NoError(t, s.SoftDeleteSession(ctx, "session"))
	deleted, err := s.DeleteSessionIfTrashed(ctx, "session")
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)
	_, ok = s.trendsDisk.Get("search|"+filter.Slot(), "json|v1|"+version)
	require.False(t, ok)
	otherData, ok := s.trendsDisk.Get("other", "v1")
	require.True(t, ok)
	require.Equal(t, "other cache", string(otherData))
}
