package service

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"time"
)

// ErrFrictionDigestNotFound is returned when the requested (or latest)
// digest does not exist. HTTP transports map a 404 back to it.
var ErrFrictionDigestNotFound = errors.New("friction digest not found")

// FrictionCapability is implemented by services that can read friction
// digests and patterns. MCP registers the friction tools only when true.
type FrictionCapability interface {
	SupportsFriction() bool
}

// FrictionService reads stored friction digests and recurrence patterns.
// Ledger reads live in service.LedgerQueryCapability (PR 15), not here.
type FrictionService interface {
	FrictionDigest(ctx context.Context, date string) (*FrictionDigestView, error)
	FrictionPatterns(ctx context.Context, f FrictionPatternFilter) ([]FrictionPatternView, error)
}

// SupportsFriction reports whether svc can serve friction reads.
func SupportsFriction(svc SessionService) bool {
	capability, ok := svc.(FrictionCapability)
	if !ok || !capability.SupportsFriction() {
		return false
	}
	_, ok = svc.(FrictionService)
	return ok
}

// FrictionDigestView is one stored digest. Summary holds the canonical
// schema-v3 bytes (RenderSummaryJSON form). Date "" in a request means latest.
type FrictionDigestView struct {
	Date     string         `json:"date"`
	Timezone string         `json:"timezone"`
	BuiltAt  time.Time      `json:"built_at"`
	Revision int            `json:"revision"`
	Markdown string         `json:"markdown"`
	Summary  jsontext.Value `json:"summary"`
	WebURL   string         `json:"web_url,omitempty"`
}

// FrictionPatternFilter narrows FrictionPatterns. Linked nil = all.
type FrictionPatternFilter struct {
	Kind   string
	Since  string
	Limit  int
	Linked *bool
}

// FrictionPatternView is one recurrence row plus browser links. The Kata
// fields stay empty until issue filing exists (PR 10).
type FrictionPatternView struct {
	Fingerprint      string `json:"fingerprint"`
	Kind             string `json:"kind"`
	Title            string `json:"title"`
	FirstSeenDate    string `json:"first_seen_date"`
	LastSeenDate     string `json:"last_seen_date"`
	OccurrenceCount  int    `json:"occurrence_count"`
	SessionCount     int    `json:"session_count"`
	LastSubjectID    string `json:"last_subject_id"`
	LastOrdinal      *int   `json:"last_ordinal,omitempty"`
	LastSessionURL   string `json:"last_session_url,omitempty"`
	IssueQualifiedID string `json:"issue_qualified_id,omitempty"`
	IssueWebURL      string `json:"issue_web_url,omitempty"`
}

func frictionLinkState(linked *bool) string {
	if linked == nil {
		return ""
	}
	if *linked {
		return "linked"
	}
	return "unlinked"
}
