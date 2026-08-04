package db

import (
	"context"
	"database/sql"

	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

// ScanSearchResults executes a capability query through Bun and scans its
// canonical eight-column projection without backend-specific reflection code.
func ScanSearchResults(
	ctx context.Context, query *bun.RawQuery, capacity int,
) ([]SearchResult, error) {
	return scanSearchResults(ctx, query, capacity, false)
}

// ScanSQLiteSearchResults retains canonical RFC3339 text timestamps already
// returned by SQLite and normalizes only legacy non-canonical forms.
func ScanSQLiteSearchResults(
	ctx context.Context, query *bun.RawQuery, capacity int,
) ([]SearchResult, error) {
	return scanSearchResults(ctx, query, capacity, true)
}

func scanSearchResults(
	ctx context.Context, query *bun.RawQuery, capacity int, timestampText bool,
) ([]SearchResult, error) {
	model := newSearchResultStreamModel(capacity, timestampText)
	if err := query.Scan(ctx, model); err != nil {
		return nil, err
	}
	return model.results, nil
}

type searchResultStreamModel struct {
	row           SearchResult
	endedAt       bunmodel.Timestamp
	endedAtText   string
	timestampText bool
	results       []SearchResult
	dest          [8]any
}

func newSearchResultStreamModel(
	capacity int, timestampText bool,
) *searchResultStreamModel {
	model := &searchResultStreamModel{
		timestampText: timestampText,
		results:       make([]SearchResult, 0, capacity),
	}
	endedAtDest := any(&model.endedAt)
	if timestampText {
		endedAtDest = &model.endedAtText
	}
	model.dest = [8]any{
		&model.row.SessionID,
		&model.row.Project,
		&model.row.Agent,
		&model.row.Name,
		endedAtDest,
		&model.row.Ordinal,
		&model.row.Snippet,
		&model.row.Rank,
	}
	return model
}

func (m *searchResultStreamModel) ScanRows(
	_ context.Context, rows *sql.Rows,
) (int, error) {
	count := 0
	for rows.Next() {
		m.row = SearchResult{}
		m.endedAt = bunmodel.Timestamp{}
		m.endedAtText = ""
		if err := rows.Scan(m.dest[:]...); err != nil {
			return count, err
		}
		if m.timestampText {
			m.row.SessionEndedAt = normalizeSQLiteSearchTimestamp(m.endedAtText)
		} else {
			m.row.SessionEndedAt = formatRequiredUsageTimestamp(m.endedAt)
		}
		m.results = append(m.results, m.row)
		count++
	}
	return count, rows.Err()
}

func (m *searchResultStreamModel) Value() any {
	return &m.results
}

func normalizeSQLiteSearchTimestamp(value string) string {
	if value == "" || len(value) >= len("2006-01-02T15:04:05Z") &&
		value[10] == 'T' && value[len(value)-1] == 'Z' {
		return value
	}
	parsed, err := bunmodel.ParseTimestamp(value)
	if err != nil {
		return value
	}
	return formatRequiredUsageTimestamp(parsed)
}

// ContentSearchHit is the engine-owned portion of a content search result.
// Stable coordinates let BunStore hydrate and order every engine identically.
type ContentSearchHit struct {
	SessionID     string
	Ordinal       int
	OrdinalStart  int
	OrdinalEnd    int
	RangeResolved bool
	Subordinate   bool
	DocKey        string
	Location      string
	MessageID     *int64
	CallIndex     *int
	EventIndex    *int
	SourceUUID    string
	ToolName      string
	Snippet       string
	Timestamp     string
	Score         *float64
}

// FullTextCapability owns engine-specific lexical matching. Search must apply
// the canonical project/deleted-session scope before its LIMIT/OFFSET and
// return at most one hit per visible session; BunStore owns policy and final
// hydration, but this pushdown keeps cursor windows complete and bounded.
type FullTextCapability interface {
	Available() bool
	Search(context.Context, bun.IDB, SearchFilter) ([]SearchResult, error)
}

// SessionSearchCapability owns matching within one known session. It is
// separate from FullTextCapability because adapters can provide this bounded
// operation even when their global full-text index is unavailable.
type SessionSearchCapability interface {
	SearchSession(context.Context, bun.IDB, string, string) ([]int, error)
}

// ContentSearchCapability owns engine-specific lexical candidate matching.
// It must apply the canonical session scope from the supplied filter before
// LIMIT/OFFSET. BunStore owns the scope policy, metadata hydration, and final
// pagination; the capability pushdown prevents rejected rows from consuming
// a bounded candidate window.
type ContentSearchCapability interface {
	Available() bool
	SearchContent(
		context.Context, bun.IDB, ContentSearchFilter,
	) ([]ContentSearchHit, error)
}

// HybridLexicalCapability owns only the engine-specific lexical candidate
// ranking used by hybrid search. BunStore resolves candidates to semantic
// units, applies scope, fuses both legs, and hydrates the final page.
type HybridLexicalCapability interface {
	Available() bool
	SearchHybridContent(
		context.Context, bun.IDB, ContentSearchFilter,
	) ([]ContentSearchHit, error)
}

// SemanticCapability owns vector-index matching and message-to-unit
// resolution. BunStore owns validation, canonical visibility, metadata
// hydration, redaction, scope filtering, fusion, and final ordering.
type SemanticCapability interface {
	Available() bool
	UnavailableError() error
	SearchContent(
		context.Context, bun.IDB, ContentSearchFilter,
	) ([]ContentSearchHit, error)
	ResolveMessageUnits(
		context.Context, bun.IDB, []MessageRef,
	) ([]UnitRef, error)
}

type vectorSemanticCapability struct {
	searcher    func() VectorSearcher
	unavailable func() error
}

// NewVectorSemanticCapability adapts a dynamically wired VectorSearcher to
// the narrow storage capability used by BunStore.
func NewVectorSemanticCapability(
	searcher func() VectorSearcher, unavailable func() error,
) SemanticCapability {
	return vectorSemanticCapability{
		searcher: searcher, unavailable: unavailable,
	}
}

func (c vectorSemanticCapability) Available() bool {
	return c.searcher != nil && c.searcher() != nil
}

func (c vectorSemanticCapability) UnavailableError() error {
	if c.unavailable != nil {
		return c.unavailable()
	}
	return ErrSemanticUnavailable
}

func (c vectorSemanticCapability) SearchContent(
	ctx context.Context, _ bun.IDB, filter ContentSearchFilter,
) ([]ContentSearchHit, error) {
	searcher := c.searcher()
	if searcher == nil {
		return nil, c.UnavailableError()
	}
	hits, err := searcher.SemanticSearch(
		ctx, filter.Pattern, filter.Limit,
	)
	if err != nil {
		return nil, err
	}
	out := make([]ContentSearchHit, len(hits))
	for i, hit := range hits {
		score := float64(hit.Score)
		out[i] = ContentSearchHit{
			SessionID: hit.SessionID, Ordinal: hit.Ordinal,
			OrdinalStart: hit.OrdinalStart, OrdinalEnd: hit.OrdinalEnd,
			RangeResolved: true,
			Subordinate:   hit.Subordinate,
			DocKey:        UnitFusionKey(hit.SessionID, hit.OrdinalStart),
			Location:      "message", Snippet: hit.Snippet, Score: &score,
		}
	}
	return out, nil
}

func (c vectorSemanticCapability) ResolveMessageUnits(
	ctx context.Context, _ bun.IDB, refs []MessageRef,
) ([]UnitRef, error) {
	searcher := c.searcher()
	if searcher == nil {
		return nil, c.UnavailableError()
	}
	return searcher.ResolveMessageUnits(ctx, refs)
}
