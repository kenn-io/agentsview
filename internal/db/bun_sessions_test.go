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

func (*sessionContractBackend) Name() string { return "session-contract" }

func (*sessionContractBackend) ReadOnly() bool { return true }

func (*sessionContractBackend) Capabilities() BackendCapabilities {
	return BackendCapabilities{}
}

func (*sessionContractBackend) SessionQueryDialect() QueryDialect {
	return SQLiteBunSessionQueryDialect()
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

func (*sessionContractBackend) Update(
	context.Context, func(bun.IDB) error,
) error {
	return ErrReadOnly
}

type countingQueryHook struct {
	selects int
	queries []string
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
