package db

import (
	"sort"
	"time"
)

// Model-scoped analytics core. The matched-message reducer
// (messagescope_reducer.go) and these projections turn a backend's candidate
// message rows into per-session stats and timing. The user-turn pairing
// semantics live here so analytics and trends across the SQLite, PostgreSQL,
// and DuckDB backends share one implementation instead of duplicating it per
// panel. This code is pure (no SQL, timezone, or AnalyticsFilter dependency)
// and is unit-tested without a database in messagescope_test.go.

// ScopeFilter is the message-grain predicate, adapted by each backend from
// AnalyticsFilter. Models holds the already-split model CSV. DayOfWeek and Hour
// are the day/hour match targets (nil = unconstrained); DayOfWeek uses ISO
// numbering Monday=0..Sunday=6.
type ScopeFilter struct {
	Models    map[string]struct{}
	DayOfWeek *int
	Hour      *int
}

// MessageInput is one candidate row, already filtered by the backend's
// panel-specific predicate and already time-localized. Rows MUST be pushed
// grouped by SessionID (each session contiguous) with non-decreasing Ordinal
// within each session; cross-session order is unconstrained. See
// ScopeReducer.Push.
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
func (f ScopeFilter) MatchesDayHour(t time.Time, has bool) bool {
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

// ScopeStats aggregates matched rows into MessageStats. The caller groups rows
// by session before calling.
func ScopeStats(rows []ScopedMessage) MessageStats {
	var s MessageStats
	for _, row := range rows {
		s.Messages++
		switch row.Role {
		case "user":
			if !row.IsSystem {
				s.UserMessages++
			}
		case "assistant":
			s.AssistantMessages++
			if row.HasToolUse {
				s.ToolUseMessages++
			}
		}
		if row.HasThinking {
			s.ThinkingMessages++
		}
		if row.HasOutputTokens {
			s.OutputTokens += row.OutputTokens
			s.HasOutputTokens = true
		}
	}
	return s
}

// ScopeTiming projects matched rows into the velocity timing view in ordinal
// (conversation) order. The reducer emits a selected non-assistant row ahead of
// an earlier buffered user turn (see ScopeReducer), so emitted rows can be out
// of ordinal order. Velocity pairs each prompt with the following response by
// position, so the timing view is re-sorted by ordinal here without disturbing
// the emit order the message and stats projections rely on.
func ScopeTiming(rows []ScopedMessage) []TimingMessage {
	ordered := make([]ScopedMessage, len(rows))
	copy(ordered, rows)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Ordinal < ordered[j].Ordinal
	})
	out := make([]TimingMessage, 0, len(ordered))
	for _, row := range ordered {
		out = append(out, TimingMessage{
			Role:          row.Role,
			Time:          row.LocalTime,
			Valid:         row.HasLocalTime,
			ContentLength: row.ContentLength,
		})
	}
	return out
}
