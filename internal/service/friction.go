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

// FrictionService reads stored friction digests, findings and recurrence
// patterns.
type FrictionService interface {
	FrictionDigest(ctx context.Context, date string) (*FrictionDigestView, error)
	FrictionDigests(ctx context.Context, from, to string) ([]FrictionDigestListItem, error)
	FrictionFindings(ctx context.Context, f FrictionFindingFilter) (*FrictionFindingPage, error)
	FrictionPatterns(ctx context.Context, f FrictionPatternFilter) (*FrictionPatternPage, error)
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

// FrictionDigestView is one stored digest. Summary holds the stored summary
// JSON bytes (friction.RenderSummaryJSON form). Date "" in a request means
// latest.
type FrictionDigestView struct {
	Date     string         `json:"date"`
	Timezone string         `json:"timezone"`
	BuiltAt  time.Time      `json:"built_at"`
	Revision int            `json:"revision"`
	Markdown string         `json:"markdown"`
	Summary  jsontext.Value `json:"summary"`
	WebURL   string         `json:"web_url,omitempty"`
}

// FrictionDigestListItem is one digest in a date range, newest first.
type FrictionDigestListItem struct {
	Date            string    `json:"date"`
	Timezone        string    `json:"timezone"`
	RulesVersion    string    `json:"rules_version"`
	BuiltAt         time.Time `json:"built_at"`
	Revision        int       `json:"revision"`
	SessionsScanned int       `json:"sessions_scanned"`
}

// FrictionFindingFilter narrows FrictionFindings. Date selects the sessions
// recorded in that digest.
type FrictionFindingFilter struct {
	Date        string
	Kind        string
	SessionID   string
	Fingerprint string
	Limit       int
	Cursor      string
}

// FrictionFindingView is one stored finding.
type FrictionFindingView struct {
	SessionID      string     `json:"session_id"`
	Kind           string     `json:"kind"`
	Detector       string     `json:"detector"`
	MessageOrdinal *int       `json:"message_ordinal"`
	CallIndex      *int       `json:"call_index"`
	ToolName       string     `json:"tool_name"`
	Label          string     `json:"label"`
	Text           string     `json:"text"`
	Evidence       string     `json:"evidence"`
	Title          string     `json:"title"`
	Fingerprint    string     `json:"fingerprint"`
	OccurredAt     *time.Time `json:"occurred_at"`
	Seq            int        `json:"seq"`
	RulesVersion   string     `json:"rules_version"`
}

// FrictionFindingPage is one page of findings.
type FrictionFindingPage struct {
	Findings   []FrictionFindingView `json:"findings"`
	NextCursor string                `json:"next_cursor"`
}

// FrictionPatternFilter narrows FrictionPatterns.
type FrictionPatternFilter struct {
	Kind   string
	Since  string
	Limit  int
	Cursor string
}

// FrictionPatternView is one recurrence row plus a browser link to its most
// recent occurrence.
type FrictionPatternView struct {
	Fingerprint     string `json:"fingerprint"`
	Kind            string `json:"kind"`
	Title           string `json:"title"`
	FirstSeenDate   string `json:"first_seen_date"`
	LastSeenDate    string `json:"last_seen_date"`
	OccurrenceCount int    `json:"occurrence_count"`
	SessionCount    int    `json:"session_count"`
	LastSubjectID   string `json:"last_subject_id"`
	LastOrdinal     *int   `json:"last_ordinal,omitempty"`
	LastSessionURL  string `json:"last_session_url,omitempty"`
}

// FrictionPatternPage is one page of patterns ranked by occurrences.
type FrictionPatternPage struct {
	Patterns   []FrictionPatternView `json:"patterns"`
	NextCursor string                `json:"next_cursor"`
}
