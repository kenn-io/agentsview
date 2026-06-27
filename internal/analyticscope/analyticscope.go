// Package analyticscope holds the backend-agnostic core of model-scoped
// analytics: the matched-message reducer and its projections. It is pure (no
// database, SQL, timezone, or db.AnalyticsFilter dependency) so the user-turn
// pairing semantics live in one place and are unit-tested without a database.
package analyticscope

import "time"

// Filter is the message-grain predicate, adapted by each backend from
// db.AnalyticsFilter. Models holds the already-split model CSV. DayOfWeek and
// Hour are the day/hour match targets (nil = unconstrained); DayOfWeek uses ISO
// numbering Monday=0..Sunday=6.
type Filter struct {
	Models    map[string]struct{}
	DayOfWeek *int
	Hour      *int
}

// MessageInput is one candidate row, already filtered by the backend's
// panel-specific predicate and already time-localized. Rows MUST be pushed
// grouped by SessionID (each session contiguous) with non-decreasing Ordinal
// within each session; cross-session order is unconstrained. See Reducer.Push.
type MessageInput struct {
	SessionID       string
	Ordinal         int
	Role            string
	Model           string
	IsSystem        bool
	Timestamp       string
	LocalTime       time.Time
	HasLocalTime    bool
	HasThinking     bool
	HasToolUse      bool
	OutputTokens    int
	HasOutputTokens bool
	ContentLength   int
	Content         string
}

// ScopedMessage is a matched, emitted row. It preserves the raw Timestamp
// string for downstream timing (e.g. Signals evidence) alongside the localized
// time used for the day/hour match.
type ScopedMessage struct {
	SessionID       string
	Ordinal         int
	Role            string
	Content         string
	IsSystem        bool
	HasThinking     bool
	HasToolUse      bool
	Timestamp       string
	LocalTime       time.Time
	HasLocalTime    bool
	OutputTokens    int
	HasOutputTokens bool
	ContentLength   int
}

// MessageStats is the per-session aggregate of matched rows.
type MessageStats struct {
	Messages          int
	UserMessages      int
	AssistantMessages int
	ToolUseMessages   int
	ThinkingMessages  int
	OutputTokens      int
	HasOutputTokens   bool
}

// TimingMessage is the velocity projection of a matched row.
type TimingMessage struct {
	Role          string
	Time          time.Time
	Valid         bool
	ContentLength int
}

// MatchesDayHour reports whether a localized time satisfies the day/hour
// filter. With no constraint it always matches (even when the time is
// unparsed); with a constraint, an unparsed time never matches.
func (f Filter) MatchesDayHour(t time.Time, has bool) bool {
	if f.DayOfWeek == nil && f.Hour == nil {
		return true
	}
	if !has {
		return false
	}
	if f.DayOfWeek != nil && (int(t.Weekday())+6)%7 != *f.DayOfWeek {
		return false
	}
	if f.Hour != nil && t.Hour() != *f.Hour {
		return false
	}
	return true
}
