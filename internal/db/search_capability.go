package db

import "context"

import "github.com/uptrace/bun"

// SearchHit is the engine-owned portion of a session search result. The
// common store hydrates session metadata, enforces visibility, and paginates.
type SearchHit struct {
	SessionID string
	Ordinal   int
	Snippet   string
	Rank      float64
}

// ContentSearchHit is the engine-owned portion of a content search result.
// Stable coordinates let BunStore hydrate and order every engine identically.
type ContentSearchHit struct {
	SessionID    string
	Ordinal      int
	OrdinalStart int
	OrdinalEnd   int
	Subordinate  bool
	DocKey       string
	Location     string
	MessageID    *int64
	CallIndex    *int
	EventIndex   *int
	SourceUUID   string
	ToolName     string
	Snippet      string
	Timestamp    string
	Score        *float64
}

// FullTextCapability owns engine-specific lexical matching. Search must apply
// the canonical project/deleted-session scope before its LIMIT/OFFSET and
// return at most one hit per visible session; BunStore owns policy and final
// hydration, but this pushdown keeps cursor windows complete and bounded.
// SearchSession is a separate substring operation and remains callable when
// Available reports that the global full-text index is absent.
type FullTextCapability interface {
	Available() bool
	Search(context.Context, bun.IDB, SearchFilter) ([]SearchHit, error)
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
			Subordinate: hit.Subordinate,
			DocKey:      UnitFusionKey(hit.SessionID, hit.OrdinalStart),
			Location:    "message", Snippet: hit.Snippet, Score: &score,
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
