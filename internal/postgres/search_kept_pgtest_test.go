//go:build pgtest

package postgres

import (
	"context"
	"encoding/base64"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/docbank/document/ocr"
)

type searchQueryTracer struct {
	count       atomic.Int32
	restricted  atomic.Int32
	ties        atomic.Int32
	keys        atomic.Int32
	hydrate     atomic.Int32
	hydrateIDs  atomic.Int32
	checkpoints atomic.Int32
}

func (tr *searchQueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "SELECT COALESCE((SELECT value FROM sync_metadata WHERE key = $1)") {
		tr.checkpoints.Add(1)
	}
	if strings.Contains(data.SQL, "processor FROM page ORDER BY") {
		tr.keys.Add(1)
	}
	if strings.HasPrefix(data.SQL, "WITH page AS (SELECT * FROM unnest(") {
		tr.hydrate.Add(1)
		// pgx/stdlib prepends result formats before the SQL arguments.
		if ids, ok := data.Args[3].([]string); ok {
			tr.hydrateIDs.Store(int32(len(ids)))
		}
	}
	if strings.Contains(data.SQL, "processor FROM unnest(") {
		tr.ties.Add(1)
	}
	if strings.Contains(data.SQL, "msg_matches AS") {
		tr.count.Add(1)
		if strings.Contains(data.SQL, "s.id = ANY(") {
			tr.restricted.Add(1)
		}
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
		pool.SetMaxOpenConns(1)
		return s, trace
	}
	s, trace := open()
	var retained atomic.Int32
	s.runtimeState().search.Size = func(db.SearchPage) int64 { retained.Add(1); return 1 }
	searchCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	got, err := s.Search(searchCtx, filter)
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Zero(t, trace.count.Load())
	got, err = s.Search(ctx, filter)
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.EqualValues(t, 1, retained.Load())
	other, err := s.Search(ctx, db.SearchFilter{Query: "unmatched-token"})
	require.NoError(t, err)
	require.Empty(t, other.Results)
	require.Positive(t, trace.count.Load())
	write("needle second")
	// Save the updated page for the next store.
	_, err = s.Search(ctx, filter)
	require.NoError(t, err)
	s, trace = open()
	got, err = s.Search(ctx, filter)
	require.NoError(t, err)
	require.Len(t, got.Results, 1)
	require.Contains(t, got.Results[0].Snippet, "second")
	require.Zero(t, trace.count.Load())
}

func TestSearchKeptRefreshAfterWrite(t *testing.T) {
	local, syncer, _ := newUsageJournalFixture(t)
	ctx := t.Context()
	url := testPGURL(t)
	config, err := pgx.ParseConfig(url)
	require.NoError(t, err)
	config.RuntimeParams["search_path"] = syncer.schema
	trace := &searchQueryTracer{}
	config.Tracer = trace
	pool := stdlib.OpenDB(*config)
	s := &Store{pg: pool, ownedDB: pool}
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.NoError(t, s.ConfigureUsageCache(ctx, t.TempDir(), url, syncer.schema))
	for _, id := range []string{"one", "two", "three"} {
		require.NoError(t, local.UpsertSession(ctx, db.Session{ID: id, Project: "project", Machine: "device", Agent: "claude", MessageCount: 1}))
		require.NoError(t, local.ReplaceSessionMessages(ctx, id, []db.Message{{SessionID: id, Role: "user", Content: map[string]string{"one": "xneedle", "three": "xxneedle", "two": "xxxneedle"}[id]}}))
	}
	pgKeptPush(t, syncer, false)
	f := db.SearchFilter{Query: "needle", Limit: 2}
	page, err := s.Search(ctx, f)
	require.NoError(t, err)
	require.EqualValues(t, 1, trace.keys.Load())
	require.EqualValues(t, 1, trace.hydrate.Load())
	require.EqualValues(t, 2, trace.hydrateIDs.Load())
	require.EqualValues(t, 1, trace.checkpoints.Load())
	require.Equal(t, []string{"one", "three"}, []string{page.Results[0].SessionID, page.Results[1].SessionID})
	require.NoError(t, local.ReplaceSessionMessages(ctx, "two", []db.Message{{SessionID: "two", Role: "user", Content: "needle changed"}}))
	pgKeptPush(t, syncer, false)
	trace.count.Store(0)
	trace.restricted.Store(0)
	trace.keys.Store(0)
	trace.hydrate.Store(0)
	trace.checkpoints.Store(0)
	trace.ties.Store(0)
	page, err = s.Search(ctx, f)
	require.NoError(t, err)
	require.Equal(t, []string{"two", "one"}, []string{page.Results[0].SessionID, page.Results[1].SessionID})
	require.EqualValues(t, 1, trace.count.Load())
	require.EqualValues(t, 1, trace.restricted.Load())
	require.EqualValues(t, 1, trace.keys.Load())
	require.EqualValues(t, 1, trace.hydrate.Load())
	require.EqualValues(t, 2, trace.hydrateIDs.Load())
	require.EqualValues(t, 1, trace.checkpoints.Load())
	require.EqualValues(t, 1, trace.ties.Load())

	check := func(sort string, wantFirst, wantSecond []string) {
		t.Helper()
		for i, want := range [][]string{wantFirst, wantSecond} {
			filter := db.SearchFilter{Query: "needle", Limit: 2, Cursor: i * 2, Sort: sort}
			got, err := s.Search(ctx, filter)
			require.NoError(t, err)
			ids := make([]string, 0, len(got.Results))
			for _, result := range got.Results {
				ids = append(ids, result.SessionID)
			}
			require.Equal(t, want, ids)
			direct, err := s.search(ctx, filter)
			require.NoError(t, err)
			require.Equal(t, direct, got)
		}
	}
	check("relevance", []string{"two", "one"}, []string{"three"})
	check("recency", []string{"one", "three"}, []string{"two"})
	require.NoError(t, local.SoftDeleteSession(ctx, "one"))
	pgKeptPush(t, syncer, false)
	check("relevance", []string{"two", "three"}, []string{})
	check("recency", []string{"three", "two"}, []string{})
	_, err = syncer.pg.ExecContext(ctx, `DELETE FROM sessions WHERE id = $1`, "three")
	require.NoError(t, err)
	for _, sort := range []string{"relevance", "recency"} {
		check(sort, []string{"two"}, []string{})
	}
	require.NoError(t, local.UpsertSession(ctx, db.Session{ID: "new", Project: "project", Machine: "device", Agent: "claude", MessageCount: 1}))
	require.NoError(t, local.ReplaceSessionMessages(ctx, "new", []db.Message{{SessionID: "new", Role: "user", Content: "needle new"}}))
	pgKeptPush(t, syncer, false)
	for _, sort := range []string{"relevance", "recency"} {
		check(sort, []string{"new", "two"}, []string{})
	}
	image := `[{"type":"input_image","image_url":"data:image/png;base64,` + base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nimage")) + `"}]`
	require.NoError(t, local.UpsertSession(ctx, db.Session{ID: "shot", Project: "project", Machine: "device", Agent: "claude", MessageCount: 1}))
	require.NoError(t, local.InsertMessages(ctx, []db.Message{{SessionID: "shot", Ordinal: 0, Role: "assistant", Content: "look", ToolCalls: []db.ToolCall{{ToolName: "Read", ToolUseID: "image", ResultContent: image}}}}))
	pgKeptPush(t, syncer, false)
	for _, sort := range []string{"relevance", "recency"} {
		check(sort, []string{"new", "two"}, []string{})
	}
	require.NoError(t, local.BackfillImageTextOccurrences(ctx))
	require.NoError(t, local.SaveImageText(ctx, db.RetainedToolResultImages(image)[0].SHA256, ocr.Identity{Fingerprint: "processor", Provider: "test", Model: "model", Revision: "r1"}, "needle image", false))
	pgKeptPush(t, syncer, false)
	check("relevance", []string{"new", "two"}, []string{"shot"})
	check("recency", []string{"new", "shot"}, []string{"two"})
	require.NoError(t, local.UpsertSession(ctx, db.Session{ID: "name", Project: "project", Machine: "device", Agent: "claude", MessageCount: 1, SessionName: new("other")}))
	require.NoError(t, local.ReplaceSessionMessages(ctx, "name", []db.Message{{SessionID: "name", Role: "user", Content: "ordinary"}}))
	pgKeptPush(t, syncer, false)
	check("relevance", []string{"new", "two"}, []string{"shot"})
	require.NoError(t, local.RenameSession(ctx, "name", new("needle name")))
	pgKeptPush(t, syncer, false)
	check("relevance", []string{"new", "two"}, []string{"name", "shot"})
	check("recency", []string{"name", "new"}, []string{"shot", "two"})
	for _, id := range []string{"a-c", "ab", "B1", "a1"} {
		require.NoError(t, local.UpsertSession(ctx, db.Session{ID: id, Project: "ties", Machine: "device", Agent: "claude", MessageCount: 1}))
		require.NoError(t, local.ReplaceSessionMessages(ctx, id, []db.Message{{SessionID: id, Role: "user", Content: "needle"}}))
	}
	pgKeptPush(t, syncer, false)
	for _, sort := range []string{"relevance", "recency"} {
		filter := db.SearchFilter{Query: "needle", Project: "ties", Sort: sort, Limit: 10}
		_, err := s.Search(ctx, filter)
		require.NoError(t, err)
	}
	require.NoError(t, local.ReplaceSessionMessages(ctx, "a-c", []db.Message{{SessionID: "a-c", Role: "user", Content: "needle edited"}}))
	pgKeptPush(t, syncer, false)
	for _, sort := range []string{"relevance", "recency"} {
		filter := db.SearchFilter{Query: "needle", Project: "ties", Sort: sort, Limit: 10}
		got, err := s.Search(ctx, filter)
		require.NoError(t, err)
		direct, err := s.search(ctx, filter)
		require.NoError(t, err)
		require.Equal(t, direct, got)
	}
}
func TestSearchKeptHistoryCheckpointMissing(t *testing.T) {
	for _, mode := range []string{"same revision", "expired", "rewound", "overtaken"} {
		t.Run(mode, func(t *testing.T) {
			local, syncer, s := newUsageJournalFixture(t)
			ctx := t.Context()
			require.NoError(t, s.ConfigureUsageCache(ctx, t.TempDir(), testPGURL(t), syncer.schema))
			require.NoError(t, local.UpsertSession(ctx, db.Session{ID: "one", Project: "project", Machine: "device", Agent: "claude", MessageCount: 1}))
			require.NoError(t, local.ReplaceSessionMessages(ctx, "one", []db.Message{{SessionID: "one", Role: "user", Content: "needle"}}))
			require.NoError(t, local.UpsertSession(ctx, db.Session{ID: "two", Project: "project", Machine: "device", Agent: "claude", MessageCount: 1}))
			require.NoError(t, local.ReplaceSessionMessages(ctx, "two", []db.Message{{SessionID: "two", Role: "user", Content: "ordinary"}}))
			pgKeptPush(t, syncer, false)
			f := db.SearchFilter{Query: "needle"}
			page, err := s.Search(ctx, f)
			require.NoError(t, err)
			require.Len(t, page.Results, 1)
			var revision int64
			require.NoError(t, syncer.pg.QueryRowContext(ctx, `SELECT value::bigint FROM sync_metadata WHERE key='usage_change_revision'`).Scan(&revision))
			tx, err := syncer.pg.BeginTx(ctx, nil)
			require.NoError(t, err)
			_, err = tx.ExecContext(ctx, `DELETE FROM sync_metadata WHERE key='usage_history_checkpoint:' || ($1::bigint)::text`, revision)
			require.NoError(t, err)
			_, err = tx.ExecContext(ctx, `UPDATE messages SET content='gone' WHERE session_id='one'`)
			require.NoError(t, err)
			if mode != "expired" {
				offset := 1
				if mode == "rewound" || mode == "overtaken" {
					offset = 2
				}
				_, err = tx.ExecContext(ctx, `UPDATE sync_metadata SET value=($1::bigint-$2)::text WHERE key='usage_change_revision'`, revision, offset)
				require.NoError(t, err)
			}
			_, err = tx.ExecContext(ctx, `UPDATE sessions SET updated_at=clock_timestamp() WHERE id='two'`)
			require.NoError(t, err)
			require.NoError(t, tx.Commit())
			if mode == "overtaken" {
				for range 3 {
					_, err = syncer.pg.ExecContext(ctx, `UPDATE sessions SET updated_at=clock_timestamp() WHERE id='two'`)
					require.NoError(t, err)
				}
			}
			got, err := s.Search(ctx, f)
			require.NoError(t, err)
			require.Empty(t, got.Results)
			direct, err := s.search(ctx, f)
			require.NoError(t, err)
			require.Equal(t, direct, got)
		})
	}
}

func TestSearchKeptWithoutJournal(t *testing.T) {
	local, syncer, _ := newUsageJournalFixture(t)
	ctx := t.Context()
	require.NoError(t, local.UpsertSession(ctx, db.Session{ID: "one", Project: "project", Machine: "device", Agent: "claude", MessageCount: 1}))
	require.NoError(t, local.ReplaceSessionMessages(ctx, "one", []db.Message{{SessionID: "one", Role: "user", Content: "needle"}}))
	pgKeptPush(t, syncer, false)
	f := db.SearchFilter{Query: "needle", Limit: 10}
	_, err := syncer.pg.ExecContext(ctx, dropUsageChangeJournalSQL)
	require.NoError(t, err)
	config, err := pgx.ParseConfig(testPGURL(t))
	require.NoError(t, err)
	config.RuntimeParams["search_path"] = syncer.schema
	trace := &searchQueryTracer{}
	config.Tracer = trace
	pool := stdlib.OpenDB(*config)
	s := &Store{pg: pool, ownedDB: pool}
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	got, err := s.searchKept(ctx, s, f)
	require.NoError(t, err)
	require.Zero(t, trace.keys.Load())
	require.Zero(t, trace.hydrate.Load())
	direct, err := s.search(ctx, f)
	require.NoError(t, err)
	require.Len(t, got.Results, 1)
	require.Equal(t, direct, got)
}

func TestSearchKeptOversizedThenSmaller(t *testing.T) {
	local, syncer, _ := newUsageJournalFixture(t)
	ctx := t.Context()
	url := testPGURL(t)
	config, err := pgx.ParseConfig(url)
	require.NoError(t, err)
	config.RuntimeParams["search_path"] = syncer.schema
	trace := &searchQueryTracer{}
	config.Tracer = trace
	pool := stdlib.OpenDB(*config)
	s := &Store{pg: pool, ownedDB: pool}
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.NoError(t, s.ConfigureUsageCache(ctx, t.TempDir(), url, syncer.schema))
	s.runtimeState().searchKept.MaxBytes = 300
	for _, id := range []string{"one", "two", "three"} {
		require.NoError(t, local.UpsertSession(ctx, db.Session{ID: id, Project: "project", Machine: "device", Agent: "claude", MessageCount: 1}))
		require.NoError(t, local.ReplaceSessionMessages(ctx, id, []db.Message{{SessionID: id, Role: "user", Content: "needle"}}))
	}
	pgKeptPush(t, syncer, false)
	f := db.SearchFilter{Query: "needle", Limit: 10}
	got, err := s.Search(ctx, f)
	require.NoError(t, err)
	require.Len(t, got.Results, 3)
	// A new write invalidates the saved page, while the complete match set was too large to keep.
	require.NoError(t, local.SoftDeleteSession(ctx, "two"))
	require.NoError(t, local.SoftDeleteSession(ctx, "three"))
	pgKeptPush(t, syncer, false)
	trace.keys.Store(0)
	trace.restricted.Store(0)
	got, err = s.Search(ctx, f)
	require.NoError(t, err)
	require.Len(t, got.Results, 1)
	require.Equal(t, "one", got.Results[0].SessionID)
	require.EqualValues(t, 1, trace.keys.Load())
	require.Zero(t, trace.restricted.Load()) // The oversized set needs a full key search.
	require.NoError(t, local.ReplaceSessionMessages(ctx, "one", []db.Message{{SessionID: "one", Role: "user", Content: "needle updated"}}))
	pgKeptPush(t, syncer, false)
	trace.keys.Store(0)
	trace.restricted.Store(0)
	got, err = s.Search(ctx, f)
	require.NoError(t, err)
	require.Len(t, got.Results, 1)
	require.Contains(t, got.Results[0].Snippet, "updated")
	require.EqualValues(t, 1, trace.keys.Load())
	require.EqualValues(t, 1, trace.restricted.Load()) // Only changed sessions are searched.
	direct, err := s.search(ctx, f)
	require.NoError(t, err)
	require.Equal(t, direct, got)
}
