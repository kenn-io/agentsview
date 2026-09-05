package db

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedEmbeddableScopeCorpus inserts one session per scope case an
// incremental refresh has to decide about, each carrying two messages so a
// caller can also assert per-session ordering. Every session ends after the
// watermark the tests pass, ends before it, or has no usable ended_at at
// all.
func seedEmbeddableScopeCorpus(t *testing.T, d *DB) {
	t.Helper()
	sessions := []struct {
		id       string
		endedAt  *string
		mutate   func(*Session)
		softKill bool
	}{
		{id: "a-new", endedAt: Ptr(tsMidYear)},
		{id: "b-old", endedAt: Ptr(tsZero)},
		{id: "c-open", endedAt: nil},
		{id: "d-legacy", endedAt: Ptr("")},
		{id: "e-auto", endedAt: Ptr(tsMidYear), mutate: func(s *Session) {
			s.IsAutomated = true
		}},
		{id: "f-trashed", endedAt: Ptr(tsMidYear), softKill: true},
	}
	for _, sess := range sessions {
		insertSession(t, d, sess.id, "proj", func(s *Session) {
			s.EndedAt = sess.endedAt
			if sess.mutate != nil {
				sess.mutate(s)
			}
		})
		insertMessages(t, d,
			Message{
				SessionID: sess.id, Ordinal: 0, Role: "user",
				Content: sess.id + "-u0", ContentLength: len(sess.id) + 3,
				Timestamp: tsZero,
			},
			Message{
				SessionID: sess.id, Ordinal: 1, Role: "user",
				Content: sess.id + "-u1", ContentLength: len(sess.id) + 3,
				Timestamp: tsZeroS1,
			},
		)
		if sess.softKill {
			require.NoError(t, d.SoftDeleteSession(sess.id))
		}
	}
}

// TestScanEmbeddableUnitsSinceScope pins the exact session set an
// incremental scan emits now that the since watermark is applied through a
// session-id subquery rather than a predicate on the joined sessions row.
// The two forms must agree on every scope case: newer than the watermark,
// older than it, no ended_at at all (NULL or the legacy empty-string
// sentinel), automated, and trashed.
func TestScanEmbeddableUnitsSinceScope(t *testing.T) {
	d := testDB(t)
	seedEmbeddableScopeCorpus(t, d)

	tests := []struct {
		name             string
		since            string
		includeAutomated bool
		want             []string
	}{
		{
			name:             "SinceKeepsNewerOpenAndLegacySessions",
			since:            tsHour1,
			includeAutomated: true,
			want:             []string{"a-new", "c-open", "d-legacy", "e-auto"},
		},
		{
			name:             "SinceStillExcludesAutomatedByDefault",
			since:            tsHour1,
			includeAutomated: false,
			want:             []string{"a-new", "c-open", "d-legacy"},
		},
		{
			name:             "EmptySinceScansEverySession",
			since:            "",
			includeAutomated: true,
			want:             []string{"a-new", "b-old", "c-open", "d-legacy", "e-auto"},
		},
		{
			name:             "SinceAfterEveryEndedAtKeepsOnlyUnendedSessions",
			since:            "2030-01-01T00:00:00Z",
			includeAutomated: true,
			want:             []string{"c-open", "d-legacy"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := scanUnits(t, d, tt.since, tt.includeAutomated)

			var ids []string
			for _, u := range got {
				ids = append(ids, u.SessionID)
			}
			// Two user messages per session, so every kept session
			// contributes exactly two units in session order.
			var want []string
			for _, id := range tt.want {
				want = append(want, id, id)
			}
			assert.Equal(t, want, ids)
			assert.NotContains(t, ids, "f-trashed",
				"a trashed session must never reach the embedding scan")
		})
	}
}

// TestScanEmbeddableUnitsSinceOrdering asserts the (session_id, ordinal)
// stream contract unitReducer depends on still holds for a since-filtered
// scan, where the query now drives from sessions instead of messages.
func TestScanEmbeddableUnitsSinceOrdering(t *testing.T) {
	d := testDB(t)
	for _, id := range []string{"sess-c", "sess-a", "sess-b"} {
		insertSession(t, d, id, "proj", func(s *Session) {
			s.EndedAt = Ptr(tsMidYear)
		})
		insertMessages(t, d,
			Message{
				SessionID: id, Ordinal: 2, Role: "user",
				Content: id + "-2", ContentLength: 8, Timestamp: tsZeroS2,
			},
			Message{
				SessionID: id, Ordinal: 0, Role: "user",
				Content: id + "-0", ContentLength: 8, Timestamp: tsZero,
			},
			Message{
				SessionID: id, Ordinal: 1, Role: "user",
				Content: id + "-1", ContentLength: 8, Timestamp: tsZeroS1,
			},
		)
	}

	got, maxEnded := scanUnits(t, d, tsHour1, true)

	require.Len(t, got, 9)
	var seen []string
	for _, u := range got {
		seen = append(seen, u.Content)
	}
	assert.Equal(t, []string{
		"sess-a-0", "sess-a-1", "sess-a-2",
		"sess-b-0", "sess-b-1", "sess-b-2",
		"sess-c-0", "sess-c-1", "sess-c-2",
	}, seen)
	assert.Equal(t, tsMidYear, maxEnded)
}

// explainQueryPlan returns the flattened EXPLAIN QUERY PLAN detail lines for
// query.
func explainQueryPlan(t *testing.T, d *DB, query string, args ...any) []string {
	t.Helper()
	rows, err := d.getReader().QueryContext(
		context.Background(), "EXPLAIN QUERY PLAN "+query, args...)
	require.NoError(t, err)
	defer rows.Close()

	var details []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notUsed, &detail))
		details = append(details, detail)
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, details)
	return details
}

// TestEmbeddableUnitsQueryPlanDrivesFromSessionsWhenSinceIsSet is the
// regression guard for the after-sync refresh reading the entire message
// corpus (including content) on every run. With a watermark the planner must
// look messages up per candidate session through
// idx_messages_session_ordinal; a plan that scans messages means the
// watermark stopped narrowing the scan. Without a watermark the full scan is
// the intended shape and must stay.
func TestEmbeddableUnitsQueryPlanDrivesFromSessionsWhenSinceIsSet(t *testing.T) {
	d := testDB(t)
	seedEmbeddableScopeCorpus(t, d)

	tests := []struct {
		name             string
		since            string
		includeAutomated bool
		wantScanMessages bool
	}{
		{name: "SinceSetHumanScope", since: tsHour1, wantScanMessages: false},
		{
			name: "SinceSetIncludingAutomated", since: tsHour1,
			includeAutomated: true, wantScanMessages: false,
		},
		{name: "EmptySinceKeepsFullScan", since: "", wantScanMessages: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var args []any
			if tt.since != "" {
				args = append(args, tt.since)
			}
			plan := explainQueryPlan(t, d,
				embeddableUnitsQuery(tt.since, tt.includeAutomated), args...)
			joined := strings.Join(plan, "\n")

			scansMessages := false
			searchesMessages := false
			for _, line := range plan {
				if strings.HasPrefix(line, "SCAN m") {
					scansMessages = true
				}
				if strings.HasPrefix(line,
					"SEARCH m USING INDEX idx_messages_session_ordinal (session_id=") {
					searchesMessages = true
				}
			}
			assert.Equal(t, tt.wantScanMessages, scansMessages,
				"unexpected messages scan in plan:\n%s", joined)
			assert.Equal(t, !tt.wantScanMessages, searchesMessages,
				"expected a per-session index search in plan:\n%s", joined)
			if tt.since != "" {
				assert.NotContains(t, joined, "USE TEMP B-TREE FOR ORDER BY",
					"the (session_id, ordinal) order must come from the index, not a sort:\n%s",
					joined)
			}
		})
	}
}
