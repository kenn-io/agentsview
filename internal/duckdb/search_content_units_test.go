//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// newUnitsStore syncs the given SQLite session batch into a fresh in-memory
// DuckDB mirror and returns a read Store over it — the standard
// sync-from-SQLite seeding path for conversation-unit derivation tests.
func newUnitsStore(t *testing.T, writes []db.SessionBatchWrite) *Store {
	t.Helper()
	ctx := context.Background()
	local := newLocalDB(t)
	_, err := local.WriteSessionBatchAtomic(writes)
	require.NoError(t, err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(t, createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(t, err)
	return NewStoreFromDB(syncer.DB())
}

// unitSession builds a root session for unit-derivation fixtures.
func unitSession(id string, messageCount int) db.Session {
	return syncSession(id, "proj", id+" first",
		"2026-05-01T10:00:00.000Z", messageCount)
}

// unitChildSession builds a subagent child session so session lineage
// (relationship_type + parent_session_id) survives the sync path.
func unitChildSession(id, parentID string, messageCount int) db.Session {
	sess := unitSession(id, messageCount)
	sess.RelationshipType = "subagent"
	parent := parentID
	sess.ParentSessionID = &parent
	return sess
}

// unitMsg builds a message with explicit is_system and is_sidechain flags on
// top of the shared syncMessage builder, which lacks them.
func unitMsg(
	sessionID string, ordinal int, role, content string,
	isSystem, isSidechain bool, calls ...db.ToolCall,
) db.Message {
	ts := fmt.Sprintf("2026-05-01T10:%02d:%02d.000Z", ordinal/60, ordinal%60)
	m := syncMessage(sessionID, ordinal, role, content, ts, calls...)
	m.IsSystem = isSystem
	m.IsSidechain = isSidechain
	return m
}

// unitMatchesByOrdinal indexes a page's matches by anchor ordinal, requiring
// the ordinals to be unique.
func unitMatchesByOrdinal(
	t *testing.T, page db.ContentSearchPage,
) map[int]db.ContentMatch {
	t.Helper()
	out := make(map[int]db.ContentMatch, len(page.Matches))
	for _, m := range page.Matches {
		_, dup := out[m.Ordinal]
		require.False(t, dup, "duplicate match ordinal %d", m.Ordinal)
		out[m.Ordinal] = m
	}
	return out
}

// TestDuckSearchContentSubstringDerivedRunRange mirrors the SQLite and PG
// tests: every substring match in one assistant run carries the run's full
// range (spanning a non-member system row), an embeddable user row and a
// system row are their own units, and ExcludeSystem changes nothing but
// which rows match.
func TestDuckSearchContentSubstringDerivedRunRange(t *testing.T) {
	store := newUnitsStore(t, []db.SessionBatchWrite{{
		Session: unitSession("duck-unit-run", 6),
		Messages: []db.Message{
			unitMsg("duck-unit-run", 0, "user", "the RUNHIT question", false, false),
			unitMsg("duck-unit-run", 1, "assistant", "RUNHIT step one", false, false),
			unitMsg("duck-unit-run", 2, "user", "sys RUNHIT note", true, false),
			unitMsg("duck-unit-run", 3, "assistant", "RUNHIT step two", false, false),
			unitMsg("duck-unit-run", 4, "assistant", "RUNHIT step three", false, false),
			unitMsg("duck-unit-run", 5, "user", "next question", false, false),
		},
		DataVersion:     1,
		ReplaceMessages: true,
	}})

	ctx := context.Background()
	got, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "RUNHIT", Mode: "substring",
		Sources: []string{"messages"}, IncludeOneShot: true, Limit: 50,
	})
	require.NoError(t, err, "SearchContent")
	require.Len(t, got.Matches, 5, "matches")
	byOrd := unitMatchesByOrdinal(t, got)
	assert.Equal(t, [2]int{0, 0}, byOrd[0].OrdinalRange, "user row is its own unit")
	assert.Equal(t, [2]int{2, 2}, byOrd[2].OrdinalRange, "system row is its own unit")
	for _, o := range []int{1, 3, 4} {
		m := byOrd[o]
		assert.Equal(t, [2]int{1, 4}, m.OrdinalRange, "run member %d", o)
		assert.Equal(t, o, m.Ordinal, "anchor ordinal %d", o)
		assert.False(t, m.Subordinate, "top-level run member %d", o)
		assert.False(t, m.Sidechain, "non-sidechain run member %d", o)
		// The sync fixture stores relationship_type = "root"; the lineage
		// fields are a passthrough of the session row on every backend.
		assert.Equal(t, "root", m.Relationship, "top-level relationship %d", o)
		assert.Empty(t, m.ParentSessionID, "top-level parent %d", o)
	}

	// ExcludeSystem drops the system row but leaves the derived ranges of
	// the surviving rows unchanged.
	ex, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "RUNHIT", Mode: "substring",
		Sources: []string{"messages"}, ExcludeSystem: true,
		IncludeOneShot: true, Limit: 50,
	})
	require.NoError(t, err, "SearchContent ExcludeSystem")
	require.Len(t, ex.Matches, 4, "ExcludeSystem matches")
	exByOrd := unitMatchesByOrdinal(t, ex)
	assert.NotContains(t, exByOrd, 2, "system row excluded")
	assert.Equal(t, [2]int{0, 0}, exByOrd[0].OrdinalRange)
	for _, o := range []int{1, 3, 4} {
		assert.Equal(t, [2]int{1, 4}, exByOrd[o].OrdinalRange,
			"ExcludeSystem run member %d", o)
	}
}

// TestDuckSearchContentSidechainRunSubordinate pins the sidechain rules: a
// sidechain run's members are Subordinate + Sidechain, and the sidechain
// flip bounds both the sidechain run and the following top-level run.
func TestDuckSearchContentSidechainRunSubordinate(t *testing.T) {
	store := newUnitsStore(t, []db.SessionBatchWrite{{
		Session: unitSession("duck-unit-side", 4),
		Messages: []db.Message{
			unitMsg("duck-unit-side", 0, "user", "the question", false, false),
			unitMsg("duck-unit-side", 1, "assistant", "SIDEHIT step a", false, true),
			unitMsg("duck-unit-side", 2, "assistant", "SIDEHIT step b", false, true),
			unitMsg("duck-unit-side", 3, "assistant", "main MAINHIT answer", false, false),
		},
		DataVersion:     1,
		ReplaceMessages: true,
	}})

	ctx := context.Background()
	side, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "SIDEHIT", Mode: "substring",
		Sources: []string{"messages"}, IncludeOneShot: true, Limit: 50,
	})
	require.NoError(t, err, "SearchContent sidechain")
	require.Len(t, side.Matches, 2, "sidechain matches")
	for _, m := range side.Matches {
		assert.Equal(t, [2]int{1, 2}, m.OrdinalRange, "sidechain run range")
		assert.True(t, m.Subordinate, "sidechain run is subordinate")
		assert.True(t, m.Sidechain, "anchor sidechain flag")
		assert.Equal(t, "root", m.Relationship,
			"root session lineage passthrough, no subordinate lineage")
		assert.Empty(t, m.ParentSessionID, "no parent session")
	}

	main, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "MAINHIT", Mode: "substring",
		Sources: []string{"messages"}, IncludeOneShot: true, Limit: 50,
	})
	require.NoError(t, err, "SearchContent main")
	require.Len(t, main.Matches, 1, "main matches")
	m := main.Matches[0]
	assert.Equal(t, [2]int{3, 3}, m.OrdinalRange,
		"sidechain flip bounds the top-level run")
	assert.False(t, m.Subordinate, "top-level run")
	assert.False(t, m.Sidechain, "top-level anchor")
}

// TestDuckSearchContentSubagentLineage pins session-level lineage on lexical
// rows: a match inside a subagent session is Subordinate with Relationship
// and ParentSessionID populated from the sessions join.
func TestDuckSearchContentSubagentLineage(t *testing.T) {
	store := newUnitsStore(t, []db.SessionBatchWrite{
		{
			Session: unitSession("duck-unit-parent", 1),
			Messages: []db.Message{
				unitMsg("duck-unit-parent", 0, "user", "parent prompt", false, false),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session: unitChildSession("duck-unit-child", "duck-unit-parent", 2),
			Messages: []db.Message{
				unitMsg("duck-unit-child", 0, "user", "subagent prompt", false, false),
				unitMsg("duck-unit-child", 1, "assistant", "SUBHIT answer", false, false),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})

	ctx := context.Background()
	got, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "SUBHIT", Mode: "substring",
		Sources: []string{"messages"}, IncludeChildren: true,
		IncludeOneShot: true, Limit: 50,
	})
	require.NoError(t, err, "SearchContent")
	require.Len(t, got.Matches, 1, "matches")
	m := got.Matches[0]
	assert.Equal(t, [2]int{1, 1}, m.OrdinalRange, "single-member run")
	assert.True(t, m.Subordinate, "subagent session is subordinate")
	assert.Equal(t, "subagent", m.Relationship, "Relationship")
	assert.Equal(t, "duck-unit-parent", m.ParentSessionID, "ParentSessionID")
	assert.False(t, m.Sidechain, "anchor not sidechain")
}

// TestDuckSearchContentToolDerivedRunRange pins derivation for tool_input and
// canonical tool_result rows: the anchor is the tool call's message row, so
// both locations carry the enclosing run's range while the wire Role stays
// the hard-coded "assistant".
func TestDuckSearchContentToolDerivedRunRange(t *testing.T) {
	call := db.ToolCall{
		ToolName: "Bash", Category: "execution", ToolUseID: "tu1",
		InputJSON:     `{"command":"TOOLHIT"}`,
		ResultContent: "output RESHIT data",
	}
	store := newUnitsStore(t, []db.SessionBatchWrite{{
		Session: unitSession("duck-unit-tool", 4),
		Messages: []db.Message{
			unitMsg("duck-unit-tool", 0, "user", "the question", false, false),
			unitMsg("duck-unit-tool", 1, "assistant", "running the tool", false, false, call),
			unitMsg("duck-unit-tool", 2, "assistant", "continuing the answer", false, false),
			unitMsg("duck-unit-tool", 3, "user", "thanks", false, false),
		},
		DataVersion:     1,
		ReplaceMessages: true,
	}})

	ctx := context.Background()
	in, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "TOOLHIT", Mode: "substring",
		Sources: []string{"tool_input"}, IncludeOneShot: true, Limit: 50,
	})
	require.NoError(t, err, "tool_input search")
	require.Len(t, in.Matches, 1, "tool_input matches")
	assert.Equal(t, "assistant", in.Matches[0].Role, "wire role stays assistant")
	assert.Equal(t, 1, in.Matches[0].Ordinal, "anchor ordinal")
	assert.Equal(t, [2]int{1, 2}, in.Matches[0].OrdinalRange,
		"tool_input anchor classified from the real message row")

	res, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "RESHIT", Mode: "substring",
		Sources: []string{"tool_result"}, IncludeOneShot: true, Limit: 50,
	})
	require.NoError(t, err, "tool_result search")
	require.Len(t, res.Matches, 1, "tool_result matches")
	assert.Equal(t, [2]int{1, 2}, res.Matches[0].OrdinalRange,
		"canonical tool_result anchor classified from the real message row")
}

// TestDuckSearchContentToolResultEventsDerived pins the events branch: an
// orphaned event (no message row at its ordinal) still returns its match
// (row cardinality must not change) with the [o, o] fallback and session
// lineage, while an event whose message row sits inside a run gets the run's
// range via the post-scan anchor lookup. The orphan row is seeded with a
// direct insert: the sync path only emits events attached to a synced
// message's tool call, so an event at an ordinal with no message row is not
// representable through the fixture.
func TestDuckSearchContentToolResultEventsDerived(t *testing.T) {
	eventCall := db.ToolCall{
		ToolName: "Bash", Category: "execution", ToolUseID: "tu1",
		InputJSON: `{"command":"x"}`,
		ResultEvents: []db.ToolResultEvent{{
			ToolUseID: "tu1", Source: "agent", Status: "success",
			Content:       "EVHIT streamed output",
			ContentLength: len("EVHIT streamed output"),
			Timestamp:     "2026-05-01T11:00:01.000Z",
			EventIndex:    0,
		}},
	}
	store := newUnitsStore(t, []db.SessionBatchWrite{
		{
			Session: unitSession("duck-ev-boss", 1),
			Messages: []db.Message{
				unitMsg("duck-ev-boss", 0, "user", "boss prompt", false, false),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session: unitChildSession("duck-ev-orph", "duck-ev-boss", 1),
			Messages: []db.Message{
				unitMsg("duck-ev-orph", 0, "user", "orphan opener", false, false),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session: unitSession("duck-ev-run", 3),
			Messages: []db.Message{
				unitMsg("duck-ev-run", 0, "user", "the question", false, false),
				unitMsg("duck-ev-run", 1, "assistant", "running", false, false, eventCall),
				unitMsg("duck-ev-run", 2, "assistant", "wrapping up", false, false),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})
	// Orphan: an event at ordinal 7 with no message row behind it.
	_, err := store.DB().Exec(`
		INSERT INTO tool_result_events (
			session_id, tool_call_message_ordinal, call_index,
			tool_use_id, source, status, content, content_length, event_index
		) VALUES (?, 7, 0, 'tux', 'agent', 'success', ?, ?, 0)`,
		"duck-ev-orph", "ORPHHIT event content", len("ORPHHIT event content"))
	require.NoError(t, err, "insert orphan event")

	ctx := context.Background()
	orph, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "ORPHHIT", Mode: "substring",
		Sources: []string{"tool_result"}, IncludeChildren: true,
		IncludeOneShot: true, Limit: 50,
	})
	require.NoError(t, err, "orphan search")
	require.Len(t, orph.Matches, 1, "orphaned event row must not be dropped")
	m := orph.Matches[0]
	assert.Equal(t, 7, m.Ordinal, "event ordinal")
	assert.Equal(t, [2]int{7, 7}, m.OrdinalRange,
		"missing anchor falls back to [o, o]")
	assert.False(t, m.Sidechain, "missing anchor has no sidechain flag")
	assert.True(t, m.Subordinate, "session lineage still applies")
	assert.Equal(t, "subagent", m.Relationship, "Relationship from sessions join")
	assert.Equal(t, "duck-ev-boss", m.ParentSessionID,
		"ParentSessionID from sessions join")

	ev, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "EVHIT", Mode: "substring",
		Sources: []string{"tool_result"}, IncludeOneShot: true, Limit: 50,
	})
	require.NoError(t, err, "event search")
	require.Len(t, ev.Matches, 1, "event matches")
	assert.Equal(t, 1, ev.Matches[0].Ordinal, "anchor ordinal")
	assert.Equal(t, [2]int{1, 2}, ev.Matches[0].OrdinalRange,
		"event with a message row inside a run gets the run's range")
}

// TestDuckSearchContentRegexDerivedRange spot-checks that regex mode (the
// candidate scan path) routes through the shared derivation pass.
func TestDuckSearchContentRegexDerivedRange(t *testing.T) {
	store := newUnitsStore(t, []db.SessionBatchWrite{{
		Session: unitSession("duck-unit-rx", 3),
		Messages: []db.Message{
			unitMsg("duck-unit-rx", 0, "user", "the question", false, false),
			unitMsg("duck-unit-rx", 1, "assistant", "RXHIT alpha", false, false),
			unitMsg("duck-unit-rx", 2, "assistant", "RXHIT beta", false, false),
		},
		DataVersion:     1,
		ReplaceMessages: true,
	}})

	ctx := context.Background()
	got, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: `RXHIT [a-z]+`, Mode: "regex",
		Sources: []string{"messages"}, IncludeOneShot: true, Limit: 50,
	})
	require.NoError(t, err, "SearchContent regex")
	require.Len(t, got.Matches, 2, "regex matches")
	for _, m := range got.Matches {
		assert.Equal(t, [2]int{1, 2}, m.OrdinalRange, "derived run range")
	}
}

// TestDuckSearchContentFTSDerivedRange spot-checks that DuckDB's fts mode
// (ILIKE terms over messages) routes through the shared derivation pass.
func TestDuckSearchContentFTSDerivedRange(t *testing.T) {
	store := newUnitsStore(t, []db.SessionBatchWrite{{
		Session: unitSession("duck-unit-fts", 3),
		Messages: []db.Message{
			unitMsg("duck-unit-fts", 0, "user", "the question", false, false),
			unitMsg("duck-unit-fts", 1, "assistant", "ftshit alpha step", false, false),
			unitMsg("duck-unit-fts", 2, "assistant", "ftshit beta step", false, false),
		},
		DataVersion:     1,
		ReplaceMessages: true,
	}})

	ctx := context.Background()
	got, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "ftshit", Mode: "fts",
		Sources: []string{"messages"}, IncludeOneShot: true, Limit: 50,
	})
	require.NoError(t, err, "SearchContent fts")
	require.Len(t, got.Matches, 2, "fts matches")
	for _, m := range got.Matches {
		assert.Equal(t, [2]int{1, 2}, m.OrdinalRange, "derived run range")
		assert.False(t, m.Subordinate, "top-level run")
	}
}

// TestDuckSearchContentDenseFlowDerivedRanges exercises the DENSE derivation
// flow against DuckDB's dense-fetch SQL (scanDuckUserBoundaryOrdinals): one
// session whose runs supply at least db.UnitBoundsFlowFactor distinct run
// anchors on a single page, so the shared flow selection fetches real user
// bounds with the batched boundary statement before resolving extents. Run
// lengths are derived from the exported gate so the page stays dense if the
// factor changes. The structure packs a main run, a sidechain run, and a
// flip-bounded main run, and every match must carry its run's exact range.
func TestDuckSearchContentDenseFlowDerivedRanges(t *testing.T) {
	// Three runs of runLen anchors each: 3*runLen > UnitBoundsFlowFactor.
	runLen := db.UnitBoundsFlowFactor/2 + 1
	runA := [2]int{1, runLen}                  // main run after user 0
	side := [2]int{runLen + 2, 2*runLen + 1}   // sidechain run after user runLen+1
	runC := [2]int{2*runLen + 2, 3*runLen + 1} // main run bounded left by the flip
	lastUser := 3*runLen + 2

	msgs := []db.Message{
		unitMsg("duck-unit-dense", 0, "user", "first question", false, false),
	}
	for o := runA[0]; o <= runA[1]; o++ {
		msgs = append(msgs, unitMsg("duck-unit-dense", o, "assistant",
			fmt.Sprintf("DFHIT main-a %d", o), false, false))
	}
	msgs = append(msgs, unitMsg("duck-unit-dense", runLen+1, "user",
		"second question", false, false))
	for o := side[0]; o <= side[1]; o++ {
		msgs = append(msgs, unitMsg("duck-unit-dense", o, "assistant",
			fmt.Sprintf("DFHIT side %d", o), false, true))
	}
	for o := runC[0]; o <= runC[1]; o++ {
		msgs = append(msgs, unitMsg("duck-unit-dense", o, "assistant",
			fmt.Sprintf("DFHIT main-c %d", o), false, false))
	}
	msgs = append(msgs, unitMsg("duck-unit-dense", lastUser, "user",
		"done", false, false))
	store := newUnitsStore(t, []db.SessionBatchWrite{{
		Session:         unitSession("duck-unit-dense", len(msgs)),
		Messages:        msgs,
		DataVersion:     1,
		ReplaceMessages: true,
	}})

	anchorCount := 3 * runLen
	require.GreaterOrEqual(t, anchorCount, db.UnitBoundsFlowFactor,
		"single-session page must clear the dense-flow gate")

	ctx := context.Background()
	got, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "DFHIT", Mode: "substring",
		Sources: []string{"messages"}, IncludeOneShot: true,
		Limit: anchorCount + 10,
	})
	require.NoError(t, err, "SearchContent")
	require.Len(t, got.Matches, anchorCount, "matches")
	byOrd := unitMatchesByOrdinal(t, got)
	for o := runA[0]; o <= runA[1]; o++ {
		assert.Equal(t, runA, byOrd[o].OrdinalRange, "run A member %d", o)
		assert.False(t, byOrd[o].Sidechain, "run A member %d flag", o)
	}
	for o := side[0]; o <= side[1]; o++ {
		assert.Equal(t, side, byOrd[o].OrdinalRange, "sidechain member %d", o)
		assert.True(t, byOrd[o].Sidechain, "sidechain member %d flag", o)
		assert.True(t, byOrd[o].Subordinate, "sidechain member %d subordinate", o)
	}
	for o := runC[0]; o <= runC[1]; o++ {
		assert.Equal(t, runC, byOrd[o].OrdinalRange,
			"flip-bounded run C member %d", o)
		assert.False(t, byOrd[o].Sidechain, "run C member %d flag", o)
	}
}

// TestDuckSearchContentMultiRunReducerParity is the hand-computed parity
// check against the SQLite-side embedding reducer: one session with two
// top-level runs, a sidechain run between them, and an interior system row
// that must not close the run it sits inside.
func TestDuckSearchContentMultiRunReducerParity(t *testing.T) {
	store := newUnitsStore(t, []db.SessionBatchWrite{{
		Session: unitSession("duck-unit-multi", 9),
		Messages: []db.Message{
			unitMsg("duck-unit-multi", 0, "user", "PARHIT q1", false, false),
			unitMsg("duck-unit-multi", 1, "assistant", "PARHIT a", false, false),
			unitMsg("duck-unit-multi", 2, "assistant", "PARHIT b", false, false),
			unitMsg("duck-unit-multi", 3, "assistant", "PARHIT sc1", false, true),
			unitMsg("duck-unit-multi", 4, "assistant", "PARHIT sc2", false, true),
			unitMsg("duck-unit-multi", 5, "assistant", "PARHIT c", false, false),
			unitMsg("duck-unit-multi", 6, "user", "PARHIT sys note", true, false),
			unitMsg("duck-unit-multi", 7, "assistant", "PARHIT d", false, false),
			unitMsg("duck-unit-multi", 8, "user", "PARHIT q2", false, false),
		},
		DataVersion:     1,
		ReplaceMessages: true,
	}})

	ctx := context.Background()
	got, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "PARHIT", Mode: "substring",
		Sources: []string{"messages"}, IncludeOneShot: true, Limit: 50,
	})
	require.NoError(t, err, "SearchContent")
	require.Len(t, got.Matches, 9, "matches")
	byOrd := unitMatchesByOrdinal(t, got)

	want := map[int][2]int{
		0: {0, 0}, // embeddable user row: its own unit
		1: {1, 2}, // first top-level run, closed by the sidechain flip
		2: {1, 2},
		3: {3, 4}, // sidechain run, bounded by flips on both sides
		4: {3, 4},
		5: {5, 7}, // second top-level run, spanning the interior system row
		6: {6, 6}, // interior system row: its own unit, doesn't close the run
		7: {5, 7},
		8: {8, 8}, // closing embeddable user row
	}
	for o, r := range want {
		assert.Equal(t, r, byOrd[o].OrdinalRange, "ordinal %d range", o)
	}
	for _, o := range []int{3, 4} {
		assert.True(t, byOrd[o].Subordinate, "sidechain member %d subordinate", o)
		assert.True(t, byOrd[o].Sidechain, "sidechain member %d flag", o)
	}
	for _, o := range []int{0, 1, 2, 5, 6, 7, 8} {
		assert.False(t, byOrd[o].Subordinate, "top-level row %d", o)
		assert.False(t, byOrd[o].Sidechain, "top-level row %d flag", o)
	}
}
