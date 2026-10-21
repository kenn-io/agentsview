// Package docbank resolves exact retained content without uploading local metadata.
package docbank

import (
	"context"
	"time"
)

type Lookup struct {
	SHA256          string
	Size            int64
	ReferenceOffset int
	Refresh         bool
}
type Match struct {
	NodeID    int64  `json:"node_id"`
	VersionID string `json:"version_id"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
	State     string `json:"state"`
	Name      string `json:"name"`
	Path      string `json:"path,omitempty"`
	OpenURL   string `json:"open_url,omitempty"`
}

type Page struct {
	State     string
	CheckedAt *time.Time
	Matches   []Match
	// MatchOffsets are raw remote positions, aligned with Matches after dedupe.
	// Consumers trimming Matches resume at the first omitted raw position.
	MatchOffsets        []int
	ReferenceTotal      *int
	NextReferenceOffset *int
}
type Resolver interface {
	Lookup(context.Context, Lookup) (Page, error)
	Close() error
}
