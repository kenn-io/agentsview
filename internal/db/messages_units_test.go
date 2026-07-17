package db

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scanUnits runs ScanEmbeddableUnits and collects every emitted unit in
// stream order, failing the test on error.
func scanUnits(
	t *testing.T, d *DB, since string, includeAutomated bool,
) ([]EmbeddableUnit, string) {
	t.Helper()
	var got []EmbeddableUnit
	maxEnded, err := d.ScanEmbeddableUnits(
		context.Background(), since, includeAutomated,
		func(u EmbeddableUnit) error {
			got = append(got, u)
			return nil
		})
	require.NoError(t, err)
	return got, maxEnded
}

// TestScanEmbeddableUnitsUserAssistantAlternation asserts that alternating
// user/assistant messages produce one "user" unit per user message and one
// "run" unit per contiguous span of assistant messages, joined with "\n\n"
// and carrying the first/last member's ordinal.
func TestScanEmbeddableUnitsUserAssistantAlternation(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "sess-1", "proj", func(s *Session) {
		s.EndedAt = Ptr(tsHour1)
	})
	insertMessages(t, d,
		Message{
			SessionID: "sess-1", Ordinal: 0, Role: "user",
			Content: "u0", ContentLength: 2, Timestamp: tsZero,
			SourceUUID: "uuid-u0",
		},
		Message{
			SessionID: "sess-1", Ordinal: 1, Role: "assistant",
			Content: "a1", ContentLength: 2, Timestamp: tsZeroS1,
			SourceUUID: "uuid-a1",
		},
		Message{
			SessionID: "sess-1", Ordinal: 2, Role: "assistant",
			Content: "a2", ContentLength: 2, Timestamp: tsZeroS2,
		},
		Message{
			SessionID: "sess-1", Ordinal: 3, Role: "user",
			Content: "u3", ContentLength: 2, Timestamp: tsHour1,
		},
		Message{
			SessionID: "sess-1", Ordinal: 4, Role: "assistant",
			Content: "a4", ContentLength: 2, Timestamp: tsHour1,
		},
	)

	got, _ := scanUnits(t, d, "", true)

	require.Len(t, got, 4)
	assert.Equal(t, EmbeddableUnit{
		SessionID: "sess-1", TranscriptRevision: "1",
		Kind: "user", SourceUUID: "uuid-u0",
		Ordinal: 0, OrdinalEnd: 0, Content: "u0",
	}, got[0])

	assert.Equal(t, "run", got[1].Kind)
	assert.Equal(t, "uuid-a1", got[1].SourceUUID)
	assert.Equal(t, 1, got[1].Ordinal)
	assert.Equal(t, 2, got[1].OrdinalEnd)
	assert.Equal(t, "a1\n\na2", got[1].Content)
	require.Len(t, got[1].Offsets, 2)

	assert.Equal(t, EmbeddableUnit{
		SessionID: "sess-1", TranscriptRevision: "1", Kind: "user",
		Ordinal: 3, OrdinalEnd: 3, Content: "u3",
	}, got[2])

	assert.Equal(t, "run", got[3].Kind)
	assert.Equal(t, 4, got[3].Ordinal)
	assert.Equal(t, 4, got[3].OrdinalEnd)
	require.Len(t, got[3].Offsets, 1)
}

// TestScanEmbeddableUnitsSystemPrefixedUserRowDoesNotSplitRun asserts that a
// system-prefixed user row (excluded from the embeddable universe by
// SystemPrefixSQL) is invisible to the reducer and therefore does not split
// the assistant run around it.
func TestScanEmbeddableUnitsSystemPrefixedUserRowDoesNotSplitRun(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "sess-1", "proj", func(s *Session) {
		s.EndedAt = Ptr(tsHour1)
	})
	insertMessages(t, d,
		Message{
			SessionID: "sess-1", Ordinal: 0, Role: "assistant",
			Content: "a0", ContentLength: 2, Timestamp: tsZero,
		},
		Message{
			SessionID: "sess-1", Ordinal: 1, Role: "user",
			Content: "<task-notification> x", ContentLength: 22,
			Timestamp: tsZeroS1,
		},
		Message{
			SessionID: "sess-1", Ordinal: 2, Role: "assistant",
			Content: "a2", ContentLength: 2, Timestamp: tsZeroS2,
		},
	)

	got, _ := scanUnits(t, d, "", true)

	require.Len(t, got, 1)
	assert.Equal(t, "run", got[0].Kind)
	assert.Equal(t, 0, got[0].Ordinal)
	assert.Equal(t, 2, got[0].OrdinalEnd)
	assert.Equal(t, "a0\n\na2", got[0].Content)
}

// TestScanEmbeddableUnitsIsSystemUserRowDoesNotSplitButPlainUserRowDoes
// asserts that an is_system=1 user row (also excluded from the embeddable
// universe) does not split a run, while a plain embeddable user row does.
func TestScanEmbeddableUnitsIsSystemUserRowDoesNotSplitButPlainUserRowDoes(
	t *testing.T,
) {
	d := testDB(t)
	insertSession(t, d, "sess-1", "proj", func(s *Session) {
		s.EndedAt = Ptr(tsHour1)
	})
	insertMessages(t, d,
		Message{
			SessionID: "sess-1", Ordinal: 0, Role: "assistant",
			Content: "a0", ContentLength: 2, Timestamp: tsZero,
		},
		Message{
			SessionID: "sess-1", Ordinal: 1, Role: "user",
			Content: "system flag set", ContentLength: 15,
			Timestamp: tsZeroS1, IsSystem: true,
		},
		Message{
			SessionID: "sess-1", Ordinal: 2, Role: "assistant",
			Content: "a2", ContentLength: 2, Timestamp: tsZeroS2,
		},
		Message{
			SessionID: "sess-1", Ordinal: 3, Role: "user",
			Content: "u3", ContentLength: 2, Timestamp: tsHour1,
		},
	)

	got, _ := scanUnits(t, d, "", true)

	require.Len(t, got, 2)
	assert.Equal(t, "run", got[0].Kind)
	assert.Equal(t, 0, got[0].Ordinal)
	assert.Equal(t, 2, got[0].OrdinalEnd)
	assert.Equal(t, "user", got[1].Kind)
	assert.Equal(t, 3, got[1].Ordinal)
}

// TestScanEmbeddableUnitsSidechainTransitionSplitsRun asserts that a
// transition in is_sidechain between consecutive assistant messages closes
// the open run and starts a new one, and that a run whose members are
// is_sidechain is marked Subordinate even in an otherwise top-level session.
func TestScanEmbeddableUnitsSidechainTransitionSplitsRun(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "sess-1", "proj", func(s *Session) {
		s.EndedAt = Ptr(tsHour1)
	})
	insertMessages(t, d,
		Message{
			SessionID: "sess-1", Ordinal: 0, Role: "assistant",
			Content: "a0", ContentLength: 2, Timestamp: tsZero,
		},
		Message{
			SessionID: "sess-1", Ordinal: 1, Role: "assistant",
			Content: "a1", ContentLength: 2, Timestamp: tsZeroS1,
			IsSidechain: true,
		},
		Message{
			SessionID: "sess-1", Ordinal: 2, Role: "assistant",
			Content: "a2", ContentLength: 2, Timestamp: tsZeroS2,
			IsSidechain: true,
		},
		Message{
			SessionID: "sess-1", Ordinal: 3, Role: "assistant",
			Content: "a3", ContentLength: 2, Timestamp: tsHour1,
		},
	)

	got, _ := scanUnits(t, d, "", true)

	require.Len(t, got, 3)

	assert.Equal(t, 0, got[0].Ordinal)
	assert.Equal(t, 0, got[0].OrdinalEnd)
	assert.False(t, got[0].Subordinate)

	assert.Equal(t, 1, got[1].Ordinal)
	assert.Equal(t, 2, got[1].OrdinalEnd)
	assert.True(t, got[1].Subordinate,
		"a run whose members are is_sidechain must be marked subordinate")

	assert.Equal(t, 3, got[2].Ordinal)
	assert.Equal(t, 3, got[2].OrdinalEnd)
	assert.False(t, got[2].Subordinate)
}

// TestScanEmbeddableUnitsSubordinateClassification covers the session-level
// Subordinate rule: subagent/fork sessions are always subordinate, a
// continuation with a parent is top-level, a parent-linked session with an
// empty relationship_type is subordinate, and a session with neither a
// parent nor a relationship is top-level.
func TestScanEmbeddableUnitsSubordinateClassification(t *testing.T) {
	tests := []struct {
		name             string
		relationshipType string
		parentSessionID  *string
		wantSubordinate  bool
	}{
		{"SubagentIsSubordinate", "subagent", nil, true},
		{"ForkIsSubordinate", "fork", nil, true},
		{
			"ContinuationWithParentIsTopLevel", "continuation",
			Ptr("parent-1"), false,
		},
		{
			"ParentLinkedWithEmptyRelationshipIsSubordinate", "",
			Ptr("parent-1"), true,
		},
		{"NoParentNoRelationshipIsTopLevel", "", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "sess-1", "proj", func(s *Session) {
				s.EndedAt = Ptr(tsHour1)
				s.RelationshipType = tt.relationshipType
				s.ParentSessionID = tt.parentSessionID
			})
			insertMessages(t, d, Message{
				SessionID: "sess-1", Ordinal: 0, Role: "user",
				Content: "u0", ContentLength: 2, Timestamp: tsZero,
			})

			got, _ := scanUnits(t, d, "", true)

			require.Len(t, got, 1)
			assert.Equal(t, tt.wantSubordinate, got[0].Subordinate)
		})
	}
}

// TestScanEmbeddableUnitsOffsetsMultiByteContent asserts that member offsets
// into a run's joined content are computed in rune and byte units that
// correctly account for multi-byte UTF-8 characters, and that each offset
// locates the start of its member's own text within Content.
func TestScanEmbeddableUnitsOffsetsMultiByteContent(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "sess-1", "proj", func(s *Session) {
		s.EndedAt = Ptr(tsHour1)
	})
	first := "héllo…"
	second := "world"
	insertMessages(t, d,
		Message{
			SessionID: "sess-1", Ordinal: 0, Role: "assistant",
			Content: first, ContentLength: len(first), Timestamp: tsZero,
		},
		Message{
			SessionID: "sess-1", Ordinal: 1, Role: "assistant",
			Content: second, ContentLength: len(second), Timestamp: tsZeroS1,
		},
	)

	got, _ := scanUnits(t, d, "", true)

	require.Len(t, got, 1)
	unit := got[0]
	require.Len(t, unit.Offsets, 2)

	assert.Equal(t, 0, unit.Offsets[0].RuneStart)
	assert.Equal(t, 0, unit.Offsets[0].ByteStart)
	assert.True(t, strings.HasPrefix(
		unit.Content[unit.Offsets[0].ByteStart:], first,
	))

	wantSecondRuneStart := utf8.RuneCountInString(first) + utf8.RuneCountInString("\n\n")
	wantSecondByteStart := len(first) + len("\n\n")
	assert.Equal(t, wantSecondRuneStart, unit.Offsets[1].RuneStart)
	assert.Equal(t, wantSecondByteStart, unit.Offsets[1].ByteStart)
	assert.True(t, strings.HasPrefix(
		unit.Content[unit.Offsets[1].ByteStart:], second,
	))

	assert.Equal(t,
		utf8.RuneCountInString(unit.Content[:unit.Offsets[1].ByteStart]),
		unit.Offsets[1].RuneStart,
		"RuneStart must equal the rune count of everything preceding it in Content")
}

// TestScanEmbeddableUnitsSingleMessageRunDegenerates asserts that a run
// consisting of a single assistant message has Ordinal == OrdinalEnd and a
// single zero-based offset.
func TestScanEmbeddableUnitsSingleMessageRunDegenerates(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "sess-1", "proj", func(s *Session) {
		s.EndedAt = Ptr(tsHour1)
	})
	insertMessages(t, d, Message{
		SessionID: "sess-1", Ordinal: 5, Role: "assistant",
		Content: "solo", ContentLength: 4, Timestamp: tsZero,
		SourceUUID: "uuid-solo",
	})

	got, _ := scanUnits(t, d, "", true)

	require.Len(t, got, 1)
	assert.Equal(t, "run", got[0].Kind)
	assert.Equal(t, "uuid-solo", got[0].SourceUUID)
	assert.Equal(t, 5, got[0].Ordinal)
	assert.Equal(t, got[0].Ordinal, got[0].OrdinalEnd)
	assert.Equal(t, "solo", got[0].Content)
	require.Len(t, got[0].Offsets, 1)
	assert.Equal(t, UnitOffset{Ordinal: 5, RuneStart: 0, ByteStart: 0},
		got[0].Offsets[0])
}

// TestScanEmbeddableUnitsMixedFractionalPrecisionSinceAndMaxEnded asserts
// the since filter and the returned maxEnded watermark compare mixed
// fractional-second ended_at precision chronologically, not
// lexicographically: a raw string comparison ranks "...01Z" above
// "...01.500Z" because '.' sorts below 'Z', so a buggy implementation would
// both wrongly exclude a since-eligible fractional row and wrongly report an
// earlier whole-second row as the max.
func TestScanEmbeddableUnitsMixedFractionalPrecisionSinceAndMaxEnded(t *testing.T) {
	d := testDB(t)

	seed := func(id, endedAt string) {
		insertSession(t, d, id, "proj", func(s *Session) {
			s.EndedAt = Ptr(endedAt)
		})
		insertMessages(t, d, Message{
			SessionID: id, Ordinal: 0, Role: "user",
			Content: id + " content", ContentLength: len(id) + len(" content"),
			Timestamp: tsZero,
		})
	}

	seed("too-old", "2024-01-01T00:00:00Z")
	seed("frac-after-since", "2024-01-01T00:00:01.500Z")
	seed("whole-second-max-trap", "2024-01-01T00:00:05Z")
	seed("true-max-fractional", "2024-01-01T00:00:05.900Z")

	got, maxEnded := scanUnits(t, d, "2024-01-01T00:00:01Z", true)

	var ids []string
	for _, u := range got {
		ids = append(ids, u.SessionID)
	}
	assert.NotContains(t, ids, "too-old",
		"a session ended before since must be excluded")
	assert.Contains(t, ids, "frac-after-since")
	assert.Contains(t, ids, "whole-second-max-trap")
	assert.Contains(t, ids, "true-max-fractional")
	assert.Equal(t, "2024-01-01T00:00:05.900Z", maxEnded,
		"maxEnded must be the chronologically latest ended_at")
}

// TestScanEmbeddableUnitsExcludesAutomatedByDefault asserts an automated
// session's units are excluded when includeAutomated is false (the embedding
// index's default scope, mirroring session search's default exclusion of
// automated sessions) and included when true.
func TestScanEmbeddableUnitsExcludesAutomatedByDefault(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "human-sess", "proj", func(s *Session) {
		s.EndedAt = Ptr(tsHour1)
	})
	insertMessages(t, d, Message{
		SessionID: "human-sess", Ordinal: 0, Role: "user",
		Content: "human content", ContentLength: len("human content"),
		Timestamp: tsZero,
	})

	insertSession(t, d, "auto-sess", "proj", func(s *Session) {
		s.EndedAt = Ptr(tsHour1)
		s.IsAutomated = true
	})
	insertMessages(t, d, Message{
		SessionID: "auto-sess", Ordinal: 0, Role: "user",
		Content: "automated content", ContentLength: len("automated content"),
		Timestamp: tsZero,
	})

	tests := []struct {
		name             string
		includeAutomated bool
		want             []string
	}{
		{"ExcludesAutomatedByDefault", false, []string{"human-sess"}},
		{"IncludesAutomatedWhenOptedIn", true, []string{"auto-sess", "human-sess"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := scanUnits(t, d, "", tt.includeAutomated)
			var ids []string
			for _, u := range got {
				ids = append(ids, u.SessionID)
			}
			assert.ElementsMatch(t, tt.want, ids)
		})
	}
}

// TestScanEmbeddableUnitsFiltersRolesAndPrefixes seeds one session with a
// mix of user/assistant/tool/system-role messages plus a system-prefixed and
// an is_system user message, and asserts only the clean user/assistant rows
// contribute units, with maxEnded reporting the session's ended_at.
func TestScanEmbeddableUnitsFiltersRolesAndPrefixes(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "sess-1", "proj", func(s *Session) {
		s.EndedAt = Ptr(tsHour1)
	})
	insertMessages(t, d,
		Message{
			SessionID: "sess-1", Ordinal: 0, Role: "user",
			Content: "hello there", ContentLength: len("hello there"),
			Timestamp: tsZero,
		},
		Message{
			SessionID: "sess-1", Ordinal: 1, Role: "assistant",
			Content: "hi back", ContentLength: len("hi back"),
			Timestamp: tsZeroS1,
		},
		Message{
			SessionID: "sess-1", Ordinal: 2, Role: "tool",
			Content: "tool output", ContentLength: len("tool output"),
			Timestamp: tsZeroS2,
		},
		Message{
			SessionID: "sess-1", Ordinal: 3, Role: "system",
			Content: "system note", ContentLength: len("system note"),
			Timestamp: tsHour1,
		},
		Message{
			SessionID: "sess-1", Ordinal: 4, Role: "user",
			Content:       "This session is being continued from a previous one",
			ContentLength: 10, Timestamp: tsHour1,
		},
		Message{
			SessionID: "sess-1", Ordinal: 5, Role: "user",
			Content: "is_system flag set", ContentLength: 19,
			Timestamp: tsHour1, IsSystem: true,
		},
	)

	got, maxEnded := scanUnits(t, d, "", true)

	require.Len(t, got, 2)
	assert.Equal(t, EmbeddableUnit{
		SessionID: "sess-1", TranscriptRevision: "1",
		Kind: "user", Ordinal: 0, OrdinalEnd: 0,
		Content: "hello there",
	}, got[0])
	assert.Equal(t, "run", got[1].Kind)
	assert.Equal(t, 1, got[1].Ordinal)
	assert.Equal(t, 1, got[1].OrdinalEnd)
	assert.Equal(t, "hi back", got[1].Content)
	assert.Equal(t, tsHour1, maxEnded)
}

// TestScanEmbeddableUnitsSinceFiltersOlderSessions asserts that since
// restricts the scan to sessions whose ended_at is >= since, excluding an
// older session entirely.
func TestScanEmbeddableUnitsSinceFiltersOlderSessions(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "old-sess", "proj", func(s *Session) {
		s.EndedAt = Ptr(tsZero)
	})
	insertMessages(t, d, Message{
		SessionID: "old-sess", Ordinal: 0, Role: "user",
		Content: "old content", ContentLength: len("old content"),
		Timestamp: tsZero,
	})

	insertSession(t, d, "new-sess", "proj", func(s *Session) {
		s.EndedAt = Ptr(tsMidYear)
	})
	insertMessages(t, d, Message{
		SessionID: "new-sess", Ordinal: 0, Role: "user",
		Content: "new content", ContentLength: len("new content"),
		Timestamp: tsMidYear,
	})

	got, maxEnded := scanUnits(t, d, tsHour1, true)

	require.Len(t, got, 1)
	assert.Equal(t, "new-sess", got[0].SessionID)
	assert.Equal(t, tsMidYear, maxEnded)
}

// TestScanEmbeddableUnitsSinceIncludesNullEndedAtSessions asserts that a
// session whose ended_at is NULL (still in progress, or never set by its
// parser) is not silently excluded from an incremental (since-watermark)
// scan — only a full rescan (since="") previously caught it, leaving its
// messages invisible to the embedding index until then. A session that
// genuinely ended before since must still be excluded.
func TestScanEmbeddableUnitsSinceIncludesNullEndedAtSessions(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "open-sess", "proj") // EndedAt left NULL
	insertMessages(t, d, Message{
		SessionID: "open-sess", Ordinal: 0, Role: "user",
		Content: "still running", ContentLength: len("still running"),
		Timestamp: tsZero,
	})

	insertSession(t, d, "old-sess", "proj", func(s *Session) {
		s.EndedAt = Ptr(tsZero)
	})
	insertMessages(t, d, Message{
		SessionID: "old-sess", Ordinal: 0, Role: "user",
		Content: "old content", ContentLength: len("old content"),
		Timestamp: tsZero,
	})

	got, _ := scanUnits(t, d, tsHour1, true)

	var ids []string
	for _, u := range got {
		ids = append(ids, u.SessionID)
	}
	assert.Contains(t, ids, "open-sess",
		"a NULL ended_at session must still be visible to an incremental scan")
	assert.NotContains(t, ids, "old-sess",
		"a session that genuinely ended before since must still be excluded")
}

// TestScanEmbeddableUnitsSinceIncludesEmptyStringEndedAtSessions asserts
// that a session whose ended_at is the legacy empty-string sentinel (not
// NULL, but never populated by an older parser run) behaves the same as a
// NULL ended_at in an incremental scan: it must not be excluded once any
// refresh watermark exists, and it must never become the reported maxEnded
// watermark, since "" is not a valid timestamp to persist.
func TestScanEmbeddableUnitsSinceIncludesEmptyStringEndedAtSessions(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "legacy-sess", "proj", func(s *Session) {
		s.EndedAt = Ptr("")
	})
	insertMessages(t, d, Message{
		SessionID: "legacy-sess", Ordinal: 0, Role: "user",
		Content: "legacy content", ContentLength: len("legacy content"),
		Timestamp: tsZero,
	})

	insertSession(t, d, "old-sess", "proj", func(s *Session) {
		s.EndedAt = Ptr(tsZero)
	})
	insertMessages(t, d, Message{
		SessionID: "old-sess", Ordinal: 0, Role: "user",
		Content: "old content", ContentLength: len("old content"),
		Timestamp: tsZero,
	})

	got, maxEnded := scanUnits(t, d, tsHour1, true)

	var ids []string
	for _, u := range got {
		ids = append(ids, u.SessionID)
	}
	assert.Contains(t, ids, "legacy-sess",
		"a legacy empty-string ended_at session must still be visible to "+
			"an incremental scan")
	assert.NotContains(t, ids, "old-sess",
		"a session that genuinely ended before since must still be excluded")
	assert.Empty(t, maxEnded,
		"an empty-string ended_at must never become the refresh watermark")
}

// TestScanEmbeddableUnitsEmptyReturnsEmptyWatermark asserts that scanning an
// archive with no embeddable messages returns an empty maxEnded and never
// emits a unit.
func TestScanEmbeddableUnitsEmptyReturnsEmptyWatermark(t *testing.T) {
	d := testDB(t)

	got, maxEnded := scanUnits(t, d, "", true)
	assert.Empty(t, got)
	assert.Empty(t, maxEnded)
}

// TestScanEmbeddableUnitsOrdersBySessionThenOrdinal asserts units stream in
// (session_id, ordinal) order of their first member across multiple
// sessions, regardless of insertion order.
func TestScanEmbeddableUnitsOrdersBySessionThenOrdinal(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "sess-b", "proj", func(s *Session) {
		s.EndedAt = Ptr(tsHour1)
	})
	insertSession(t, d, "sess-a", "proj", func(s *Session) {
		s.EndedAt = Ptr(tsHour1)
	})
	insertMessages(t, d,
		Message{
			SessionID: "sess-b", Ordinal: 0, Role: "user",
			Content: "b0", ContentLength: 2, Timestamp: tsZero,
		},
		Message{
			SessionID: "sess-a", Ordinal: 1, Role: "assistant",
			Content: "a1", ContentLength: 2, Timestamp: tsZeroS1,
			SourceUUID: "uuid-a1",
		},
		Message{
			SessionID: "sess-a", Ordinal: 0, Role: "user",
			Content: "a0", ContentLength: 2, Timestamp: tsZero,
			SourceUUID: "uuid-a0",
		},
	)

	got, _ := scanUnits(t, d, "", true)

	require.Len(t, got, 3)
	assert.Equal(t, "sess-a", got[0].SessionID)
	assert.Equal(t, 0, got[0].Ordinal)
	assert.Equal(t, "uuid-a0", got[0].SourceUUID)
	assert.Equal(t, "sess-a", got[1].SessionID)
	assert.Equal(t, 1, got[1].Ordinal)
	assert.Equal(t, "uuid-a1", got[1].SourceUUID)
	assert.Equal(t, "sess-b", got[2].SessionID)
	assert.Equal(t, 0, got[2].Ordinal)
}

// TestScanEmbeddableUnitsExcludesTrashedSessions asserts that units
// belonging to a soft-deleted (trashed) session never stream, since a
// trashed session's content should not be indexed for semantic search.
func TestScanEmbeddableUnitsExcludesTrashedSessions(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "trashed-sess", "proj", func(s *Session) {
		s.EndedAt = Ptr(tsHour1)
	})
	insertMessages(t, d, Message{
		SessionID: "trashed-sess", Ordinal: 0, Role: "user",
		Content: "trashed content", ContentLength: len("trashed content"),
		Timestamp: tsZero,
	})
	require.NoError(t, d.SoftDeleteSession("trashed-sess"))

	insertSession(t, d, "live-sess", "proj", func(s *Session) {
		s.EndedAt = Ptr(tsHour1)
	})
	insertMessages(t, d, Message{
		SessionID: "live-sess", Ordinal: 0, Role: "user",
		Content: "live content", ContentLength: len("live content"),
		Timestamp: tsZero,
	})

	got, _ := scanUnits(t, d, "", true)

	require.Len(t, got, 1)
	assert.Equal(t, "live-sess", got[0].SessionID)
}

// TestScanEmbeddableUnitsSessionChangeClosesOpenRun asserts that a run left
// open at the end of one session is closed and emitted before any unit from
// the next session, even though both sessions end in an open assistant run.
func TestScanEmbeddableUnitsSessionChangeClosesOpenRun(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "sess-a", "proj", func(s *Session) {
		s.EndedAt = Ptr(tsHour1)
	})
	insertSession(t, d, "sess-b", "proj", func(s *Session) {
		s.EndedAt = Ptr(tsHour1)
	})
	insertMessages(t, d,
		Message{
			SessionID: "sess-a", Ordinal: 0, Role: "assistant",
			Content: "a0", ContentLength: 2, Timestamp: tsZero,
		},
		Message{
			SessionID: "sess-a", Ordinal: 1, Role: "assistant",
			Content: "a1", ContentLength: 2, Timestamp: tsZeroS1,
		},
		Message{
			SessionID: "sess-b", Ordinal: 0, Role: "assistant",
			Content: "b0", ContentLength: 2, Timestamp: tsZero,
		},
	)

	got, _ := scanUnits(t, d, "", true)

	require.Len(t, got, 2)
	assert.Equal(t, "sess-a", got[0].SessionID)
	assert.Equal(t, 0, got[0].Ordinal)
	assert.Equal(t, 1, got[0].OrdinalEnd)
	assert.Equal(t, "sess-b", got[1].SessionID)
	assert.Equal(t, 0, got[1].Ordinal)
}
