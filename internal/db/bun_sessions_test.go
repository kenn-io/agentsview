package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

type sessionContractBackend struct {
	store     bun.IDB
	viewCalls int
}

type replayingReadBackend struct {
	first, second bun.IDB
}

func (*replayingReadBackend) Name() string { return "replaying-read" }

func (*replayingReadBackend) ReadOnly() bool { return true }

func (*replayingReadBackend) Capabilities() BackendCapabilities {
	return BackendCapabilities{}
}

func (*replayingReadBackend) TimestampOrderExpr(column string) string {
	return sqliteTimestampOrderExpr(column)
}

func (*replayingReadBackend) SessionVersion(
	context.Context, bun.IDB, string,
) (int, int64, error) {
	return 0, 0, sql.ErrNoRows
}

func (b *replayingReadBackend) View(
	_ context.Context, fn func(bun.IDB) error,
) error {
	return fn(b.first)
}

func (b *replayingReadBackend) ConsistentView(
	_ context.Context, fn func(bun.IDB) error,
) error {
	if err := fn(b.first); err != nil {
		return err
	}
	return fn(b.second)
}

func (*replayingReadBackend) Update(
	context.Context, func(bun.IDB) error,
) error {
	return ErrReadOnly
}

func (*sessionContractBackend) Name() string { return "session-contract" }

func (*sessionContractBackend) ReadOnly() bool { return true }

func (*sessionContractBackend) Capabilities() BackendCapabilities {
	return BackendCapabilities{}
}

func (*sessionContractBackend) TimestampOrderExpr(column string) string {
	return "julianday(NULLIF(" + column + ", ''))"
}

func (*sessionContractBackend) SessionVersion(
	ctx context.Context, store bun.IDB, id string,
) (int, int64, error) {
	return FileSessionVersion(ctx, store, id)
}

func (b *sessionContractBackend) View(
	_ context.Context, fn func(bun.IDB) error,
) error {
	b.viewCalls++
	return fn(b.store)
}

func (b *sessionContractBackend) ConsistentView(
	_ context.Context, fn func(bun.IDB) error,
) error {
	b.viewCalls++
	return fn(b.store)
}

func (*sessionContractBackend) Update(
	context.Context, func(bun.IDB) error,
) error {
	return ErrReadOnly
}

type countingQueryHook struct {
	selects       int
	queries       []string
	insertQueries []string
}

func (h *countingQueryHook) BeforeQuery(
	ctx context.Context, _ *bun.QueryEvent,
) context.Context {
	return ctx
}

func (h *countingQueryHook) AfterQuery(
	_ context.Context, event *bun.QueryEvent,
) {
	if event.Operation() == "SELECT" {
		h.selects++
		h.queries = append(h.queries, event.Query)
	}
	if event.Operation() == "INSERT" {
		h.insertQueries = append(h.insertQueries, event.Query)
	}
}

func TestBunStoreFindSessionIDsByPartialUsesBoundedKeysetBatches(t *testing.T) {
	raw, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	store := bun.NewDB(raw, sqlitedialect.New())
	require.NoError(t, CreateCommonSchema(t.Context(), store))
	_, err = store.NewInsert().Model(&bunmodel.SourceArchive{
		SourceArchiveID: "archive", SourceArchiveSalt: "salt",
	}).Exec(t.Context())
	require.NoError(t, err)

	newer := bunmodel.NewTimestamp(
		time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC),
	)
	older := bunmodel.NewTimestamp(
		time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC),
	)
	rows := make([]bunmodel.Session, 0, 65)
	for i := range 64 {
		rows = append(rows, bunmodel.Session{
			ID: fmt.Sprintf("MATCH-%03d", i), Project: "alpha",
			Machine: "host", Agent: "codex", CreatedAt: newer,
			SourceArchiveID: "archive", SourceDatabaseGeneration: "generation",
		})
	}
	rows = append(rows, bunmodel.Session{
		ID: "match-valid", Project: "alpha", Machine: "host", Agent: "codex",
		CreatedAt: older, SourceArchiveID: "archive",
		SourceDatabaseGeneration: "generation",
	})
	_, err = store.NewInsert().Model(&rows).Exec(t.Context())
	require.NoError(t, err)

	hook := new(countingQueryHook)
	backend := &sessionContractBackend{store: store.WithQueryHook(hook)}
	common := NewBunStore(backend)
	ids, err := common.FindSessionIDsByPartial(t.Context(), "match", 1)
	require.NoError(t, err)
	assert.Equal(t, []string{"match-valid"}, ids)
	assert.Equal(t, 1, backend.viewCalls)
	assert.Equal(t, 2, hook.selects)
	require.Len(t, hook.queries, 2)
	for _, query := range hook.queries {
		assert.True(t, strings.Contains(query, "LIMIT 64"), query)
	}
}

func TestBunStoreListSessionsKeepsQueriesAndResultsBounded(t *testing.T) {
	for _, matchingRows := range []int{2, 50} {
		t.Run(fmt.Sprintf("matching_rows_%d", matchingRows), func(t *testing.T) {
			raw, err := sql.Open("sqlite3", ":memory:")
			require.NoError(t, err)
			raw.SetMaxOpenConns(1)
			t.Cleanup(func() { require.NoError(t, raw.Close()) })
			store := bun.NewDB(raw, sqlitedialect.New())
			require.NoError(t, CreateCommonSchema(t.Context(), store))
			_, err = store.NewInsert().Model(&bunmodel.SourceArchive{
				SourceArchiveID: "archive", SourceArchiveSalt: "salt",
			}).Exec(t.Context())
			require.NoError(t, err)
			createdAt := bunmodel.NewTimestamp(
				time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC),
			)
			rows := make([]bunmodel.Session, 0, matchingRows+1)
			rows = append(rows, bunmodel.Session{
				ID: "wanted", Project: "alpha", Machine: "host", Agent: "codex",
				MessageCount: 2, UserMessageCount: 2, CreatedAt: createdAt,
				SourceArchiveID: "archive", SourceDatabaseGeneration: "generation",
			})
			for i := 1; i < matchingRows; i++ {
				rows = append(rows, bunmodel.Session{
					ID: fmt.Sprintf("extra-%03d", i), Project: "alpha",
					Machine: "host", Agent: "codex", MessageCount: 2,
					UserMessageCount: 2, CreatedAt: createdAt,
					SourceArchiveID: "archive", SourceDatabaseGeneration: "generation",
				})
			}
			rows = append(rows, bunmodel.Session{
				ID: "other-project", Project: "beta", Machine: "host", Agent: "codex",
				MessageCount: 2, UserMessageCount: 2, CreatedAt: createdAt,
				SourceArchiveID: "archive", SourceDatabaseGeneration: "generation",
			})
			_, err = store.NewInsert().Model(&rows).Exec(t.Context())
			require.NoError(t, err)

			hook := new(countingQueryHook)
			backend := &sessionContractBackend{store: store.WithQueryHook(hook)}
			common := NewBunStore(backend)
			page, err := common.ListSessions(t.Context(), SessionFilter{
				Project: "alpha", Limit: 1,
			})
			require.NoError(t, err)
			require.Len(t, page.Sessions, 1)
			assert.Equal(t, "wanted", page.Sessions[0].ID)
			assert.Equal(t, matchingRows, page.Total)
			assert.NotEmpty(t, page.NextCursor)
			assert.Equal(t, 1, backend.viewCalls)
			assert.Equal(t, 2, hook.selects, "count plus bounded page query")
		})
	}
}

func TestBunStoreSessionCompositeReadsPublishOnlyAcceptedReplayAttempt(
	t *testing.T,
) {
	first := testDB(t)
	second := testDB(t)
	seed := func(database *DB, rootIDs []string, childID string) {
		t.Helper()
		for _, id := range rootIDs {
			require.NoError(t, database.UpsertSession(Session{
				ID: id, Project: "replaying-reads", Machine: "host", Agent: "codex",
				MessageCount: 1, UserMessageCount: 1,
			}))
		}
		parentID := rootIDs[0]
		require.NoError(t, database.UpsertSession(Session{
			ID: childID, Project: "replaying-reads", Machine: "host", Agent: "codex",
			MessageCount: 1, UserMessageCount: 1, ParentSessionID: &parentID,
			RelationshipType: "subagent",
		}))
	}
	seed(first, []string{"rejected-root-a", "rejected-root-b"}, "rejected-child")
	seed(second, []string{"accepted-root"}, "accepted-child")

	store := NewBunStore(&replayingReadBackend{
		first: first.bunReader, second: second.bunReader,
	})

	t.Run("list sessions", func(t *testing.T) {
		page, err := store.ListSessions(t.Context(), SessionFilter{
			Project: "replaying-reads", Limit: 10,
		})
		require.NoError(t, err)
		assert.Equal(t, 1, page.Total)
		require.Len(t, page.Sessions, 1)
		assert.Equal(t, "accepted-root", page.Sessions[0].ID)
	})

	t.Run("sidebar index", func(t *testing.T) {
		index, err := store.GetSidebarSessionIndex(t.Context(), SessionFilter{
			Project: "replaying-reads",
		})
		require.NoError(t, err)
		assert.Equal(t, 1, index.Total)
		require.Len(t, index.Sessions, 2)
		assert.ElementsMatch(t, []string{"accepted-root", "accepted-child"}, []string{
			index.Sessions[0].ID, index.Sessions[1].ID,
		})
	})
}

func TestBunStoreListSessionsUsesChronologicalSQLiteActivity(t *testing.T) {
	raw, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	store := bun.NewDB(raw, sqlitedialect.New())
	require.NoError(t, CreateCommonSchema(t.Context(), store))
	_, err = store.NewInsert().Model(&bunmodel.SourceArchive{
		SourceArchiveID: "archive", SourceArchiveSalt: "salt",
	}).Exec(t.Context())
	require.NoError(t, err)
	_, err = raw.ExecContext(t.Context(), `
		INSERT INTO sessions (
			id, project, machine, agent, message_count, created_at,
			started_at, ended_at, source_archive_id, source_database_generation
		) VALUES
			('fractional-activity', 'time', 'host', 'codex', 2,
			 '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z', NULL,
			 'archive', 'generation'),
			('offset-before-cutoff', 'time', 'host', 'codex', 1,
			 '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z',
			 '2024-01-01T01:00:00+01:00', 'archive', 'generation');
		INSERT INTO messages (session_id, ordinal, role, content, timestamp, token_usage)
		VALUES
			('fractional-activity', 0, 'assistant', '',
			 '2024-01-01T00:00:01Z', '{}'),
			('fractional-activity', 1, 'assistant', '',
			 '2024-01-01T00:00:01.500Z', '{}')`)
	require.NoError(t, err)

	page, err := NewBunStore(&sessionContractBackend{store: store}).ListSessions(
		t.Context(), SessionFilter{
			Project: "time", ActiveSince: "2024-01-01T00:00:01.250Z", Limit: 10,
		},
	)
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	assert.Equal(t, "fractional-activity", page.Sessions[0].ID)
}

func TestBunStoreListSessionsPaginatesSQLiteTimestampsChronologically(t *testing.T) {
	raw, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	store := bun.NewDB(raw, sqlitedialect.New())
	require.NoError(t, CreateCommonSchema(t.Context(), store))
	_, err = store.NewInsert().Model(&bunmodel.SourceArchive{
		SourceArchiveID: "archive", SourceArchiveSalt: "salt",
	}).Exec(t.Context())
	require.NoError(t, err)
	_, err = raw.ExecContext(t.Context(), `
		INSERT INTO sessions (
			id, project, machine, agent, message_count, created_at,
			ended_at, source_archive_id, source_database_generation
		) VALUES
			('chronological-new', 'time', 'host', 'codex', 1,
			 '2024-01-01T00:30:00Z', '2024-01-01T00:30:00Z',
			 'archive', 'generation'),
			('lexical-new', 'time', 'host', 'codex', 1,
			 '2024-01-01T01:00:00+01:00', '2024-01-01T01:00:00+01:00',
			 'archive', 'generation')`)
	require.NoError(t, err)

	common := NewBunStore(&sessionContractBackend{store: store})
	first, err := common.ListSessions(t.Context(), SessionFilter{
		Project: "time", Limit: 1,
	})
	require.NoError(t, err)
	require.Len(t, first.Sessions, 1)
	assert.Equal(t, "chronological-new", first.Sessions[0].ID)
	assert.NotEmpty(t, first.NextCursor)

	second, err := common.ListSessions(t.Context(), SessionFilter{
		Project: "time", Limit: 1, Cursor: first.NextCursor,
	})
	require.NoError(t, err)
	require.Len(t, second.Sessions, 1)
	assert.Equal(t, "lexical-new", second.Sessions[0].ID)
	assert.Empty(t, second.NextCursor)
}

func TestBunStoreTerminationFilterUsesSQLiteInstants(t *testing.T) {
	raw, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	store := bun.NewDB(raw, sqlitedialect.New())
	require.NoError(t, CreateCommonSchema(t.Context(), store))
	_, err = store.NewInsert().Model(&bunmodel.SourceArchive{
		SourceArchiveID: "archive", SourceArchiveSalt: "salt",
	}).Exec(t.Context())
	require.NoError(t, err)

	now := time.Now().UTC()
	offset := time.FixedZone("plus-three", 3*60*60)
	activeText := now.Add(-5 * time.Minute).In(offset).Format(time.RFC3339Nano)
	staleText := now.Add(-30 * time.Minute).In(offset).Format(time.RFC3339Nano)
	uncleanText := now.Add(-2 * time.Hour).In(offset).Format(time.RFC3339Nano)
	_, err = raw.ExecContext(t.Context(), `
		INSERT INTO sessions (
			id, project, machine, agent, message_count, created_at, ended_at,
			termination_status, source_archive_id, source_database_generation
		) VALUES
			('offset-active', 'time', 'host', 'codex', 1, ?, ?, 'tool_call_pending',
				'archive', 'generation'),
			('offset-stale', 'time', 'host', 'codex', 1, ?, ?, 'tool_call_pending',
				'archive', 'generation'),
			('offset-unclean', 'time', 'host', 'codex', 1, ?, ?, 'tool_call_pending',
				'archive', 'generation')`,
		activeText, activeText, staleText, staleText, uncleanText, uncleanText,
	)
	require.NoError(t, err)

	common := NewBunStore(&sessionContractBackend{store: store})
	for _, test := range []struct {
		termination string
		wantID      string
	}{
		{termination: "active", wantID: "offset-active"},
		{termination: "stale", wantID: "offset-stale"},
		{termination: "unclean", wantID: "offset-unclean"},
	} {
		t.Run(test.termination, func(t *testing.T) {
			page, err := common.ListSessions(t.Context(), SessionFilter{
				Project: "time", Termination: test.termination, Limit: 10,
			})
			require.NoError(t, err)
			require.Len(t, page.Sessions, 1)
			assert.Equal(t, test.wantID, page.Sessions[0].ID)
		})
	}
}

func TestBunStoreSidebarPaginatesSQLiteActivityChronologically(t *testing.T) {
	raw, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	store := bun.NewDB(raw, sqlitedialect.New())
	require.NoError(t, CreateCommonSchema(t.Context(), store))
	_, err = store.NewInsert().Model(&bunmodel.SourceArchive{
		SourceArchiveID: "archive", SourceArchiveSalt: "salt",
	}).Exec(t.Context())
	require.NoError(t, err)
	_, err = raw.ExecContext(t.Context(), `
		INSERT INTO sessions (
			id, project, machine, agent, message_count, created_at, ended_at,
			source_archive_id, source_database_generation
		) VALUES
			('chronological-new', 'time', 'host', 'codex', 1,
			 '2024-01-01T00:30:00Z', '2024-01-01T00:30:00Z',
			 'archive', 'generation'),
			('lexical-new', 'time', 'host', 'codex', 1,
			 '2024-01-01T01:00:00+01:00', '2024-01-01T01:00:00+01:00',
			 'archive', 'generation')`)
	require.NoError(t, err)

	common := NewBunStore(&sessionContractBackend{store: store})
	first, err := common.GetSidebarSessionIndex(t.Context(), SessionFilter{
		Project: "time", Limit: 1,
	})
	require.NoError(t, err)
	require.Len(t, first.Sessions, 1)
	assert.Equal(t, "chronological-new", first.Sessions[0].ID)
	assert.NotEmpty(t, first.NextCursor)

	second, err := common.GetSidebarSessionIndex(t.Context(), SessionFilter{
		Project: "time", Limit: 1, Cursor: first.NextCursor,
	})
	require.NoError(t, err)
	require.Len(t, second.Sessions, 1)
	assert.Equal(t, "lexical-new", second.Sessions[0].ID)
	assert.Empty(t, second.NextCursor)
}
