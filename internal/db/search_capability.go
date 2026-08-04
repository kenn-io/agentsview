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
