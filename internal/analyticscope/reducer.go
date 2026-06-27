package analyticscope

import "fmt"

// Reducer applies model membership, user-turn pairing, and the day/hour match
// to a stream of candidate rows, calling emit for each matched ScopedMessage.
// Emit order mirrors the reference getAnalyticsModelScopedMessages
// (internal/db/analytics.go): buffered user turns are flushed only when their
// selected assistant arrives, so a selected non-assistant row that lands
// between a pending user and that assistant is emitted ahead of the user.
// Changing this ordering is a cross-backend behavior change, not a local fix.
//
// Candidate rows MUST be grouped by session (every row of a session
// contiguous) with non-decreasing Ordinal within each session -- exactly what
// SQL "ORDER BY session_id, ordinal" yields under any collation. Cross-session
// order is unconstrained, so the reducer never assumes Go byte order matches
// the database collation. A session reappearing after its group ended, or an
// ordinal moving backwards within a session, is a query bug and is returned as
// an error (never a panic), since this runs in request handling.
type Reducer struct {
	filter  Filter
	emit    func(ScopedMessage)
	session string
	lastOrd int
	started bool
	pending []ScopedMessage
	seen    map[string]struct{}
}

// NewReducer returns a Reducer that calls emit for each matched row.
func NewReducer(f Filter, emit func(ScopedMessage)) *Reducer {
	return &Reducer{filter: f, emit: emit, seen: make(map[string]struct{})}
}

// Push feeds one candidate row. O(1) grouping check, no allocation beyond the
// pending buffer (and one map entry per completed session).
func (r *Reducer) Push(row MessageInput) error {
	switch {
	case !r.started:
		r.started = true
		r.session = row.SessionID
	case row.SessionID == r.session:
		if row.Ordinal < r.lastOrd {
			return fmt.Errorf(
				"analyticscope: ordinal out of order in session %q: %d after %d",
				row.SessionID, row.Ordinal, r.lastOrd,
			)
		}
	default:
		if _, done := r.seen[row.SessionID]; done {
			return fmt.Errorf(
				"analyticscope: session %q reappeared; candidate rows must be grouped by session",
				row.SessionID,
			)
		}
		r.seen[r.session] = struct{}{}
		r.session = row.SessionID
		r.pending = r.pending[:0]
	}
	r.lastOrd = row.Ordinal

	scoped := scopedFrom(row)
	if row.Role == "user" && !row.IsSystem && row.Model == "" {
		r.pending = append(r.pending, scoped)
		return nil
	}
	_, selected := r.filter.Models[row.Model]
	switch row.Role {
	case "assistant":
		if selected {
			r.flush()
			r.appendMatched(scoped)
			return nil
		}
		r.pending = r.pending[:0]
	default:
		if selected {
			r.appendMatched(scoped)
		}
	}
	return nil
}

func (r *Reducer) flush() {
	for _, row := range r.pending {
		r.appendMatched(row)
	}
	r.pending = r.pending[:0]
}

func (r *Reducer) appendMatched(row ScopedMessage) {
	if !r.filter.MatchesDayHour(row.LocalTime, row.HasLocalTime) {
		return
	}
	r.emit(row)
}

func scopedFrom(row MessageInput) ScopedMessage {
	return ScopedMessage{
		SessionID:       row.SessionID,
		Ordinal:         row.Ordinal,
		Role:            row.Role,
		Content:         row.Content,
		IsSystem:        row.IsSystem,
		HasThinking:     row.HasThinking,
		HasToolUse:      row.HasToolUse,
		Timestamp:       row.Timestamp,
		LocalTime:       row.LocalTime,
		HasLocalTime:    row.HasLocalTime,
		OutputTokens:    row.OutputTokens,
		HasOutputTokens: row.HasOutputTokens,
		ContentLength:   row.ContentLength,
	}
}
