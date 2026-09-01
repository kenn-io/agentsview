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
	contentHits     []ContentSearchHit
	lastContent     ContentSearchFilter
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
	_ context.Context, _ bun.IDB, filter ContentSearchFilter,
) ([]ContentSearchHit, error) {
	if c.insideGuard != nil && !c.insideGuard() {
		panic("content search ran outside the backend guard")
	}
	c.lastContent = filter
	return append([]ContentSearchHit(nil), c.contentHits...), nil
}

type literalSemanticCapability struct {
	available         bool
	hits              []ContentSearchHit
	units             []UnitRef
	lastFilter        ContentSearchFilter
	insideGuard       func() bool
	outsideConsistent func() bool
}

type literalHybridLexicalCapability struct {
	available   bool
	hits        []ContentSearchHit
	lastFilter  ContentSearchFilter
	insideGuard func() bool
}

func (c *literalHybridLexicalCapability) Available() bool { return c.available }

func (c *literalHybridLexicalCapability) SearchHybridContent(
	_ context.Context, _ bun.IDB, filter ContentSearchFilter,
) ([]ContentSearchHit, error) {
	if c.insideGuard != nil && !c.insideGuard() {
		panic("hybrid lexical search ran outside the backend guard")
	}
	c.lastFilter = filter
	return append([]ContentSearchHit(nil), c.hits...), nil
}

func (c *literalSemanticCapability) Available() bool { return c.available }

func (c *literalSemanticCapability) UnavailableError() error {
	return ErrSemanticUnavailable
}

func (c *literalSemanticCapability) SearchContent(
	_ context.Context, _ bun.IDB, filter ContentSearchFilter,
) ([]ContentSearchHit, error) {
	if c.insideGuard != nil && !c.insideGuard() {
		panic("semantic search ran outside the backend guard")
	}
	if c.outsideConsistent != nil && !c.outsideConsistent() {
		panic("semantic search ran inside a consistent-view transaction")
	}
	c.lastFilter = filter
	return append([]ContentSearchHit(nil), c.hits...), nil
}

func (c *literalSemanticCapability) ResolveMessageUnits(
	context.Context, bun.IDB, []MessageRef,
) ([]UnitRef, error) {
	if c.outsideConsistent != nil && !c.outsideConsistent() {
		panic("semantic unit resolution ran inside a consistent-view transaction")
	}
	return append([]UnitRef(nil), c.units...), nil
}

type searchTestBackend struct {
	store            bun.IDB
	fullText         FullTextCapability
	sessionSearch    SessionSearchCapability
	contentSearch    ContentSearchCapability
	semantic         SemanticCapability
	hybridLexical    HybridLexicalCapability
	insideGuard      bool
	insideConsistent bool
	viewCalls        int
}

func (*searchTestBackend) Name() string { return "search-test" }

func (*searchTestBackend) ReadOnly() bool { return true }

func (b *searchTestBackend) Capabilities() BackendCapabilities {
	return BackendCapabilities{
		FullText: b.fullText, SessionSearch: b.sessionSearch,
		ContentSearch: b.contentSearch, Semantic: b.semantic,
		HybridLexical: b.hybridLexical,
	}
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
	return b.runGuarded(false, callback)
}

func (b *searchTestBackend) ConsistentView(
	_ context.Context, callback func(bun.IDB) error,
) error {
	return b.runGuarded(true, callback)
}

func (b *searchTestBackend) runGuarded(
	consistent bool, callback func(bun.IDB) error,
) error {
	b.viewCalls++
	b.insideGuard = true
	b.insideConsistent = consistent
	defer func() {
		b.insideGuard = false
		b.insideConsistent = false
	}()
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
	backend := &searchTestBackend{
		store: database.bunReader, fullText: capability, contentSearch: capability,
	}
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
	backend := &searchTestBackend{store: database.bunReader, sessionSearch: capability}
	capability.insideGuard = func() bool { return backend.insideGuard }
	store := NewBunStore(backend)

	ordinals, err := store.SearchSession(t.Context(), "session-id", "needle")

	require.NoError(t, err)
	assert.Equal(t, []int{1, 3}, ordinals)
	assert.Equal(t, 1, backend.viewCalls)
}

func TestBunStoreSearchSessionDoesNotRequireGlobalFTS(t *testing.T) {
	database := testDB(t)
	capability := &literalFullTextCapability{
		available: false, sessionOrdinals: []int{2, 4},
	}
	backend := &searchTestBackend{store: database.bunReader, sessionSearch: capability}
	capability.insideGuard = func() bool { return backend.insideGuard }

	ordinals, err := NewBunStore(backend).SearchSession(
		t.Context(), "session-id", "needle",
	)

	require.NoError(t, err)
	assert.Equal(t, []int{2, 4}, ordinals)
	assert.Equal(t, 1, backend.viewCalls)
}

func TestSQLiteBunStoreSearchRecencyUsesCreatedAtFallback(t *testing.T) {
	database := testDB(t)
	requireFTS(t, database)
	for _, fixture := range []struct {
		id        string
		createdAt string
	}{
		{id: "created-old", createdAt: "2026-01-01T00:00:00Z"},
		{id: "created-new", createdAt: "2026-02-01T00:00:00Z"},
	} {
		insertSession(t, database, fixture.id, "alpha", func(session *Session) {
			session.CreatedAt = fixture.createdAt
		})
		_, err := database.getWriter().ExecContext(t.Context(),
			"UPDATE sessions SET created_at = ?, started_at = '', ended_at = '' WHERE id = ?",
			fixture.createdAt, fixture.id,
		)
		require.NoError(t, err)
		insertMessages(t, database, userMsg(fixture.id, 0, "created fallback needle"))
	}

	common := database.BunStore
	page, err := common.Search(t.Context(), SearchFilter{
		Query: "needle", Sort: "recency", Limit: 10,
	})

	require.NoError(t, err)
	require.Len(t, page.Results, 2)
	assert.Equal(t, "created-new", page.Results[0].SessionID)
	assert.Equal(t, "2026-02-01T00:00:00Z", page.Results[0].SessionEndedAt)
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

func TestBunStoreSearchContentHydratesVisibleCapabilityHits(t *testing.T) {
	database := testDB(t)
	deletedAt := "2026-08-03T13:00:00Z"
	for _, session := range []Session{
		{ID: "content-visible", Project: "alpha", Machine: "host", Agent: "codex",
			CreatedAt: "2026-08-03T10:00:00Z", MessageCount: 1},
		{ID: "content-other", Project: "beta", Machine: "host", Agent: "claude",
			CreatedAt: "2026-08-03T11:00:00Z", MessageCount: 1},
		{ID: "content-deleted", Project: "alpha", Machine: "host", Agent: "codex",
			CreatedAt: "2026-08-03T12:00:00Z", DeletedAt: &deletedAt, MessageCount: 1},
	} {
		require.NoError(t, database.UpsertSession(session))
	}
	_, err := database.getWriter().ExecContext(t.Context(),
		"UPDATE sessions SET deleted_at = ? WHERE id = ?",
		deletedAt, "content-deleted",
	)
	require.NoError(t, err)
	insertMessages(t, database,
		userMsg("content-visible", 2, "literal needle"),
		userMsg("content-other", 3, "other needle"),
		userMsg("content-deleted", 4, "deleted needle"),
	)

	capability := &literalFullTextCapability{available: true, contentHits: []ContentSearchHit{
		{SessionID: "content-other", Ordinal: 3, Location: "message", Snippet: "other"},
		{SessionID: "content-deleted", Ordinal: 4, Location: "message", Snippet: "deleted"},
		{SessionID: "content-visible", Ordinal: 2, OrdinalStart: 1, OrdinalEnd: 3,
			RangeResolved: true, Location: "message",
			Snippet: "literal <mark>needle</mark>"},
	}}
	backend := &searchTestBackend{
		store: database.bunReader, fullText: capability, contentSearch: capability,
	}
	capability.insideGuard = func() bool { return backend.insideGuard }

	page, err := NewBunStore(backend).SearchContent(t.Context(), ContentSearchFilter{
		Pattern: "needle", Mode: "fts", Project: "alpha",
		IncludeOneShot: true, Limit: 10,
	})

	require.NoError(t, err)
	require.Len(t, page.Matches, 1)
	assert.Equal(t, ContentMatch{
		SessionID: "content-visible", Project: "alpha", Agent: "codex",
		Location: "message", Role: "user", Ordinal: 2,
		Timestamp: "2024-01-01T00:00:00Z",
		Snippet:   "literal <mark>needle</mark>", OrdinalRange: [2]int{1, 3},
	}, page.Matches[0])
	assert.Equal(t, 1, backend.viewCalls)
	assert.Equal(t, 11, capability.lastContent.Limit)
}

func TestBunStoreSearchContentSubstringUsesCanonicalRows(t *testing.T) {
	database := testDB(t)
	seedSearchSession(t, database, "substring-session", "alpha", [][2]string{
		{"user", "prefix literal needle suffix"},
	})
	store := NewBunStore(&searchTestBackend{store: database.bunReader})

	page, err := store.SearchContent(t.Context(), ContentSearchFilter{
		Pattern: "needle", Project: "alpha", Sources: []string{"messages"},
		IncludeOneShot: true, Limit: 10,
	})

	require.NoError(t, err)
	require.Len(t, page.Matches, 1)
	assert.Equal(t, "substring-session", page.Matches[0].SessionID)
	assert.Equal(t, "alpha", page.Matches[0].Project)
	assert.Equal(t, "message", page.Matches[0].Location)
	assert.Contains(t, page.Matches[0].Snippet, "needle")
}

func TestSQLiteBunStoreSearchContentSubstringPreservesNonASCIICase(t *testing.T) {
	database := testDB(t)
	seedSearchSession(t, database, "unicode-content", "alpha", [][2]string{
		{"user", "identical uppercase CAFÉ content"},
	})

	page, err := database.BunStore.SearchContent(t.Context(), ContentSearchFilter{
		Pattern: "CAFÉ", Sources: []string{"messages"},
		IncludeOneShot: true, Limit: 10,
	})

	require.NoError(t, err)
	require.Len(t, page.Matches, 1)
	assert.Equal(t, "unicode-content", page.Matches[0].SessionID)
}

func TestSQLiteBunStoreSearchContentUsesCanonicalRows(t *testing.T) {
	database := testDB(t)
	seedSearchSession(t, database, "sqlite-content", "alpha", [][2]string{
		{"user", "sqlite common substring needle"},
	})
	common := database.BunStore

	page, err := common.SearchContent(t.Context(), ContentSearchFilter{
		Pattern: "needle", Project: "alpha", Sources: []string{"messages"},
		IncludeOneShot: true, Limit: 10,
	})

	require.NoError(t, err)
	require.Len(t, page.Matches, 1)
	assert.Equal(t, "sqlite-content", page.Matches[0].SessionID)
}

func TestBunStoreSearchContentRegexUsesBoundedCanonicalCandidates(t *testing.T) {
	database := testDB(t)
	seedSearchSession(t, database, "regex-session", "alpha", [][2]string{
		{"user", "prefix needle-42 suffix"},
	})
	store := NewBunStore(&searchTestBackend{store: database.bunReader})

	page, err := store.SearchContent(t.Context(), ContentSearchFilter{
		Pattern: `needle-[0-9]+`, Mode: "regex", Project: "alpha",
		Sources: []string{"messages"}, IncludeOneShot: true, Limit: 10,
	})

	require.NoError(t, err)
	require.Len(t, page.Matches, 1)
	assert.Equal(t, "regex-session", page.Matches[0].SessionID)
	assert.Contains(t, page.Matches[0].Snippet, "needle-42")
}

func TestSQLiteBunStoreSearchContentUsesFTSCapability(t *testing.T) {
	database := testDB(t)
	requireFTS(t, database)
	seedSearchSession(t, database, "sqlite-content-fts", "alpha", [][2]string{
		{"user", "quick text with distant needle token"},
	})
	common := database.BunStore

	page, err := common.SearchContent(t.Context(), ContentSearchFilter{
		Pattern: "quick needle", Mode: "fts", Project: "alpha",
		Sources: []string{"messages"}, IncludeOneShot: true, Limit: 10,
	})

	require.NoError(t, err)
	require.Len(t, page.Matches, 1)
	assert.Equal(t, "sqlite-content-fts", page.Matches[0].SessionID)
}

func TestBunStoreSearchContentFTSRejectsNonMessageSources(t *testing.T) {
	database := testDB(t)
	capability := &literalFullTextCapability{available: true}
	backend := &searchTestBackend{
		store: database.bunReader, fullText: capability, contentSearch: capability,
	}

	_, err := NewBunStore(backend).SearchContent(t.Context(), ContentSearchFilter{
		Pattern: "needle", Mode: "fts", Sources: []string{"tool_result"},
		Limit: 10,
	})

	var inputErr *SearchInputError
	require.ErrorAs(t, err, &inputErr)
	assert.Contains(t, inputErr.Error(), "only supports the messages source")
}

func TestSQLiteBunStoreSearchContentFTSOrdersByCanonicalRecency(t *testing.T) {
	database := testDB(t)
	requireFTS(t, database)
	for _, fixture := range []struct {
		id      string
		endedAt string
	}{
		{id: "fts-older", endedAt: "2026-01-01T00:00:00Z"},
		{id: "fts-newer", endedAt: "2026-02-01T00:00:00Z"},
	} {
		seedSearchSession(t, database, fixture.id, "alpha", [][2]string{
			{"user", "canonical recency needle"},
		})
		_, err := database.getWriter().ExecContext(t.Context(),
			"UPDATE sessions SET ended_at = ? WHERE id = ?",
			fixture.endedAt, fixture.id,
		)
		require.NoError(t, err)
	}

	page, err := database.BunStore.SearchContent(t.Context(), ContentSearchFilter{
		Pattern: "needle", Mode: "fts", Sources: []string{"messages"},
		IncludeOneShot: true, Limit: 1,
	})

	require.NoError(t, err)
	require.Len(t, page.Matches, 1)
	assert.Equal(t, "fts-newer", page.Matches[0].SessionID)
	assert.Equal(t, 1, page.NextCursor)
}

func TestBunStoreSearchContentSemanticHydratesVisibleCapabilityHits(t *testing.T) {
	database := testDB(t)
	seedSearchSession(t, database, "semantic-visible", "alpha", [][2]string{
		{"user", "full visible semantic content"},
	})
	seedSearchSession(t, database, "semantic-filtered", "beta", [][2]string{
		{"user", "full filtered semantic content"},
	})
	visibleScore := 0.75
	filteredScore := 0.95
	capability := &literalSemanticCapability{
		available: true,
		hits: []ContentSearchHit{
			{SessionID: "semantic-filtered", Ordinal: 0, OrdinalStart: 0,
				OrdinalEnd: 0, RangeResolved: true,
				Location: "message", Snippet: "filtered semantic",
				Score: &filteredScore},
			{SessionID: "semantic-visible", Ordinal: 0, OrdinalStart: 0,
				OrdinalEnd: 0, RangeResolved: true,
				Location: "message", Snippet: "visible semantic",
				Score: &visibleScore},
		},
	}
	backend := &searchTestBackend{store: database.bunReader, semantic: capability}
	capability.insideGuard = func() bool { return backend.insideGuard }
	capability.outsideConsistent = func() bool { return !backend.insideConsistent }

	page, err := NewBunStore(backend).SearchContent(t.Context(), ContentSearchFilter{
		Pattern: "semantic", Mode: "semantic", Project: "alpha",
		IncludeOneShot: true, Limit: 10,
	})

	require.NoError(t, err)
	require.Len(t, page.Matches, 1)
	assert.Equal(t, ContentMatch{
		SessionID: "semantic-visible", Project: "alpha", Agent: "claude",
		Location: "message", Role: "user", Ordinal: 0,
		Timestamp: "2026-05-20T12:00:00Z",
		Snippet:   "full visible semantic content", Score: &visibleScore,
		OrdinalRange: [2]int{0, 0},
	}, page.Matches[0])
	assert.Equal(t, 2, backend.viewCalls)
	assert.Equal(t, SemanticOverfetchMin, capability.lastFilter.Limit)
}

func TestBunStoreSearchContentHybridFusesCapabilitiesWithLexicalAnchor(t *testing.T) {
	database := testDB(t)
	seedSearchSession(t, database, "hybrid", "alpha", [][2]string{
		{"user", "question"},
		{"assistant", "semantic anchor"},
		{"assistant", "lexical anchor contains zebra"},
	})
	semantic := &literalSemanticCapability{
		available: true,
		hits: []ContentSearchHit{{
			SessionID: "hybrid", Ordinal: 1, OrdinalStart: 1, OrdinalEnd: 2,
			RangeResolved: true, Location: "message", Snippet: "semantic anchor",
		}},
		units: []UnitRef{{
			DocKey: "run:hybrid:1", SessionID: "hybrid",
			OrdinalStart: 1, OrdinalEnd: 2,
		}},
	}
	lexical := &literalHybridLexicalCapability{
		available: true,
		hits: []ContentSearchHit{{
			SessionID: "hybrid", Ordinal: 2, Location: "message",
			Snippet: "lexical anchor contains zebra",
		}},
	}
	backend := &searchTestBackend{
		store: database.bunReader, semantic: semantic, hybridLexical: lexical,
	}
	semantic.insideGuard = func() bool { return backend.insideGuard }
	semantic.outsideConsistent = func() bool { return !backend.insideConsistent }
	lexical.insideGuard = func() bool { return backend.insideGuard }

	page, err := NewBunStore(backend).SearchContent(t.Context(), ContentSearchFilter{
		Pattern: "zebra", Mode: "hybrid", Project: "alpha",
		IncludeOneShot: true, Limit: 10,
	})

	require.NoError(t, err)
	require.Len(t, page.Matches, 1)
	match := page.Matches[0]
	assert.Equal(t, "hybrid", match.SessionID)
	assert.Equal(t, 2, match.Ordinal)
	assert.Equal(t, [2]int{1, 2}, match.OrdinalRange)
	assert.Equal(t, "lexical anchor contains zebra", match.Snippet)
	require.NotNil(t, match.Score)
	assert.InDelta(t, 2.0/61.0, *match.Score, 1e-9)
	assert.Equal(t, 2, backend.viewCalls)
	assert.Equal(t, SemanticOverfetchMin, semantic.lastFilter.Limit)
	assert.Equal(t, SemanticOverfetchMin, lexical.lastFilter.Limit)
}

func TestBunStoreSearchContentSemanticDropsMissingAnchor(t *testing.T) {
	database := testDB(t)
	seedSearchSession(t, database, "stale-vector", "alpha", [][2]string{
		{"user", "live message"},
	})
	capability := &literalSemanticCapability{
		available: true,
		hits: []ContentSearchHit{{
			SessionID: "stale-vector", Ordinal: 99,
			OrdinalStart: 99, OrdinalEnd: 99, RangeResolved: true,
			Location: "message", Snippet: "stale message",
		}},
	}
	backend := &searchTestBackend{store: database.bunReader, semantic: capability}

	page, err := NewBunStore(backend).SearchContent(t.Context(), ContentSearchFilter{
		Pattern: "stale", Mode: "semantic", Project: "alpha",
		IncludeOneShot: true, Limit: 10,
	})

	require.NoError(t, err)
	assert.Empty(t, page.Matches)
}

func TestBunStoreSearchContentSemanticPreservesResolvedSingletonRange(t *testing.T) {
	database := testDB(t)
	seedSearchSession(t, database, "singleton-vector", "alpha", [][2]string{
		{"user", "question"},
		{"assistant", "singleton vector anchor"},
		{"assistant", "adjacent assistant message"},
	})
	capability := &literalSemanticCapability{
		available: true,
		hits: []ContentSearchHit{{
			SessionID: "singleton-vector", Ordinal: 1,
			OrdinalStart: 1, OrdinalEnd: 1, RangeResolved: true,
			Location: "message", Snippet: "singleton vector anchor",
		}},
	}
	backend := &searchTestBackend{store: database.bunReader, semantic: capability}

	page, err := NewBunStore(backend).SearchContent(t.Context(), ContentSearchFilter{
		Pattern: "singleton", Mode: "semantic", Project: "alpha",
		IncludeOneShot: true, Limit: 10,
	})

	require.NoError(t, err)
	require.Len(t, page.Matches, 1)
	assert.Equal(t, [2]int{1, 1}, page.Matches[0].OrdinalRange)
}
