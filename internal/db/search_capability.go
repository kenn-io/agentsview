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

// FullTextCapability owns only engine-specific lexical matching.
type FullTextCapability interface {
	Available() bool
	Search(context.Context, bun.IDB, SearchFilter) ([]SearchHit, error)
	SearchSession(context.Context, bun.IDB, string, string) ([]int, error)
}

// ContentSearchCapability owns only engine-specific lexical candidate
// matching. BunStore owns canonical metadata hydration and pagination.
type ContentSearchCapability interface {
	Available() bool
	SearchContent(
		context.Context, bun.IDB, ContentSearchFilter,
	) ([]ContentSearchHit, error)
}
