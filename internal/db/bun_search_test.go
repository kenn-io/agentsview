package db

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
)

type literalFullTextCapability struct {
	available       bool
	hits            []SearchHit
	sessionOrdinals []int
	lastFilter      SearchFilter
	insideGuard     func() bool
}

func (c *literalFullTextCapability) Available() bool { return c.available }

func (c *literalFullTextCapability) Search(
	_ context.Context, _ bun.IDB, filter SearchFilter,
) ([]SearchHit, error) {
	if c.insideGuard != nil && !c.insideGuard() {
		panic("full-text search ran outside the backend guard")
	}
	c.lastFilter = filter
	return append([]SearchHit(nil), c.hits...), nil
}

func (c *literalFullTextCapability) SearchSession(
	context.Context, bun.IDB, string, string,
) ([]int, error) {
	if c.insideGuard != nil && !c.insideGuard() {
		panic("session search ran outside the backend guard")
	}
	return append([]int(nil), c.sessionOrdinals...), nil
}

func (c *literalFullTextCapability) SearchContent(
	context.Context, ContentSearchFilter,
) ([]ContentSearchHit, error) {
	return nil, nil
}

type searchTestBackend struct {
	store       bun.IDB
	fullText    FullTextCapability
	insideGuard bool
	viewCalls   int
}

func (*searchTestBackend) Name() string { return "search-test" }

func (*searchTestBackend) ReadOnly() bool { return true }

func (b *searchTestBackend) Capabilities() BackendCapabilities {
	return BackendCapabilities{FullText: b.fullText}
}

func (*searchTestBackend) SessionQueryDialect() QueryDialect {
	return SQLiteBunSessionQueryDialect()
}

func (*searchTestBackend) SessionVersion(
	context.Context, bun.IDB, string,
) (int, int64, error) {
	return 0, 0, sql.ErrNoRows
}

func (b *searchTestBackend) View(
	_ context.Context, callback func(bun.IDB) error,
) error {
	return b.runGuarded(callback)
}

func (b *searchTestBackend) ConsistentView(
	_ context.Context, callback func(bun.IDB) error,
) error {
	return b.runGuarded(callback)
}

func (b *searchTestBackend) runGuarded(callback func(bun.IDB) error) error {
	b.viewCalls++
	b.insideGuard = true
	defer func() { b.insideGuard = false }()
	return callback(b.store)
}

func (*searchTestBackend) Update(
	context.Context, func(bun.IDB) error,
) error {
	return ErrReadOnly
}

func TestBunStoreSearchHydratesOnlyVisibleCapabilityHits(t *testing.T) {
	database := testDB(t)
	alphaName := "Needle alpha"
	alphaEndedAt := "2026-08-03T10:05:00Z"
	deletedAt := "2026-08-03T13:00:00Z"
	for _, session := range []Session{
		{
			ID: "search-alpha", Project: "alpha", Machine: "host", Agent: "codex",
			DisplayName: &alphaName, CreatedAt: "2026-08-03T10:00:00Z",
			EndedAt: &alphaEndedAt, MessageCount: 2,
		},
		{
			ID: "search-beta", Project: "beta", Machine: "host", Agent: "claude",
			CreatedAt: "2026-08-03T11:00:00Z", MessageCount: 1,
		},
		{
			ID: "search-deleted", Project: "alpha", Machine: "host", Agent: "codex",
			CreatedAt: "2026-08-03T12:00:00Z", DeletedAt: &deletedAt, MessageCount: 1,
		},
	} {
		require.NoError(t, database.UpsertSession(session))
	}
	_, err := database.getWriter().ExecContext(t.Context(),
		"UPDATE sessions SET display_name = ? WHERE id = ?",
		alphaName, "search-alpha",
	)
	require.NoError(t, err)
	_, err = database.getWriter().ExecContext(t.Context(),
		"UPDATE sessions SET deleted_at = ? WHERE id = ?",
		deletedAt, "search-deleted",
	)
	require.NoError(t, err)

	capability := &literalFullTextCapability{
		available: true,
		hits: []SearchHit{
			{SessionID: "search-beta", Ordinal: 0, Snippet: "beta needle", Rank: 0.1},
			{SessionID: "search-deleted", Ordinal: 0, Snippet: "deleted needle", Rank: 0.2},
			{SessionID: "search-alpha", Ordinal: 1, Snippet: "alpha needle", Rank: 0.3},
		},
	}
	backend := &searchTestBackend{store: database.bunReader, fullText: capability}
	capability.insideGuard = func() bool { return backend.insideGuard }
	store := NewBunStore(backend)

	page, err := store.Search(t.Context(), SearchFilter{
		Query: "needle", Project: "alpha", Limit: 10,
	})

	require.NoError(t, err)
	assert.True(t, store.HasFTS())
	assert.Equal(t, 1, backend.viewCalls)
	assert.Equal(t, `"needle"`, capability.lastFilter.Query)
	assert.Equal(t, SearchPage{Results: []SearchResult{
		{
			SessionID: "search-alpha", Project: "alpha", Agent: "codex",
			Name: alphaName, Ordinal: 1, SessionEndedAt: "2026-08-03T10:05:00Z",
			Snippet: "alpha needle", Rank: 0.3,
		},
	}}, page)
}

func TestBunStoreSearchSessionUsesFullTextCapability(t *testing.T) {
	database := testDB(t)
	capability := &literalFullTextCapability{
		available: true, sessionOrdinals: []int{1, 3},
	}
	backend := &searchTestBackend{store: database.bunReader, fullText: capability}
	capability.insideGuard = func() bool { return backend.insideGuard }
	store := NewBunStore(backend)

	ordinals, err := store.SearchSession(t.Context(), "session-id", "needle")

	require.NoError(t, err)
	assert.Equal(t, []int{1, 3}, ordinals)
	assert.Equal(t, 1, backend.viewCalls)
}

func TestSQLiteBunStoreSearchUsesFTSCapability(t *testing.T) {
	database := testDB(t)
	requireFTS(t, database)
	seedSearchSession(t, database, "sqlite-search", "alpha", [][2]string{
		{"user", "find the literal needle here"},
	})

	common := database.BunStore
	page, err := common.Search(t.Context(), SearchFilter{
		Query: "needle", Project: "alpha", Limit: 10,
	})

	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	assert.Equal(t, "sqlite-search", page.Results[0].SessionID)
	assert.Equal(t, 0, page.Results[0].Ordinal)
	assert.Contains(t, page.Results[0].Snippet, "needle")
}
