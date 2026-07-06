//go:build pgtest

package postgres

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// insertCSUnitMessage inserts a message with explicit is_system and
// is_sidechain flags for conversation-unit derivation tests.
func insertCSUnitMessage(
	t *testing.T, store *Store,
	sessionID string, ordinal int, role, content string,
	isSystem, isSidechain bool,
) {
	t.Helper()
	ts := fmt.Sprintf("2026-05-01T10:00:%02dZ", ordinal)
	_, err := store.DB().Exec(`
		INSERT INTO messages
			(session_id, ordinal, role, content, timestamp,
			 content_length, is_system, is_sidechain)
		VALUES ($1, $2, $3, $4, $5::timestamptz, $6, $7, $8)
		ON CONFLICT DO NOTHING`,
		sessionID, ordinal, role, content, ts, len(content),
		isSystem, isSidechain,
	)
	require.NoError(t, err, "insert message ord=%d", ordinal)
}

// csMatchesByOrdinal indexes a page's matches by anchor ordinal, requiring
// the ordinals to be unique.
func csMatchesByOrdinal(
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

// TestPGSearchContentSubstringDerivedRunRange mirrors the SQLite test: every
// substring match in one assistant run carries the run's full range (spanning
// a non-member system row), an embeddable user row and a system row are their
// own units, and ExcludeSystem changes nothing but which rows match.
func TestPGSearchContentSubstringDerivedRunRange(t *testing.T) {
	store := setupContentSearch(t)
	insertCSSession(t, store, "cs-unit-run", "proj", "claude",
		"2026-05-01T10:00:00Z", "2026-05-01T10:30:00Z")
	insertCSUnitMessage(t, store, "cs-unit-run", 0, "user",
		"the RUNHIT question", false, false)
	insertCSUnitMessage(t, store, "cs-unit-run", 1, "assistant",
		"RUNHIT step one", false, false)
	insertCSUnitMessage(t, store, "cs-unit-run", 2, "user",
		"sys RUNHIT note", true, false)
	insertCSUnitMessage(t, store, "cs-unit-run", 3, "assistant",
		"RUNHIT step two", false, false)
	insertCSUnitMessage(t, store, "cs-unit-run", 4, "assistant",
		"RUNHIT step three", false, false)
	insertCSUnitMessage(t, store, "cs-unit-run", 5, "user",
		"next question", false, false)

	ctx := context.Background()
	got, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "RUNHIT", Mode: "substring",
		Sources: []string{"messages"}, Limit: 50,
	})
	require.NoError(t, err, "SearchContent")
	require.Len(t, got.Matches, 5, "matches")
	byOrd := csMatchesByOrdinal(t, got)
	assert.Equal(t, [2]int{0, 0}, byOrd[0].OrdinalRange, "user row is its own unit")
	assert.Equal(t, [2]int{2, 2}, byOrd[2].OrdinalRange, "system row is its own unit")
	for _, o := range []int{1, 3, 4} {
		m := byOrd[o]
		assert.Equal(t, [2]int{1, 4}, m.OrdinalRange, "run member %d", o)
		assert.Equal(t, o, m.Ordinal, "anchor ordinal %d", o)
		assert.False(t, m.Subordinate, "top-level run member %d", o)
		assert.False(t, m.Sidechain, "non-sidechain run member %d", o)
		assert.Empty(t, m.Relationship, "top-level relationship %d", o)
		assert.Empty(t, m.ParentSessionID, "top-level parent %d", o)
	}

	// ExcludeSystem drops the system row but leaves the derived ranges of
	// the surviving rows unchanged.
	ex, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "RUNHIT", Mode: "substring",
		Sources: []string{"messages"}, ExcludeSystem: true, Limit: 50,
	})
	require.NoError(t, err, "SearchContent ExcludeSystem")
	require.Len(t, ex.Matches, 4, "ExcludeSystem matches")
	exByOrd := csMatchesByOrdinal(t, ex)
	assert.NotContains(t, exByOrd, 2, "system row excluded")
	assert.Equal(t, [2]int{0, 0}, exByOrd[0].OrdinalRange)
	for _, o := range []int{1, 3, 4} {
		assert.Equal(t, [2]int{1, 4}, exByOrd[o].OrdinalRange,
			"ExcludeSystem run member %d", o)
	}
}

// TestPGSearchContentSidechainRunSubordinate pins the sidechain rules on PG:
// a sidechain run's members are Subordinate + Sidechain, and the sidechain
// flip bounds both the sidechain run and the following top-level run.
func TestPGSearchContentSidechainRunSubordinate(t *testing.T) {
	store := setupContentSearch(t)
	insertCSSession(t, store, "cs-unit-side", "proj", "claude",
		"2026-05-01T10:00:00Z", "2026-05-01T10:30:00Z")
	insertCSUnitMessage(t, store, "cs-unit-side", 0, "user",
		"the question", false, false)
	insertCSUnitMessage(t, store, "cs-unit-side", 1, "assistant",
		"SIDEHIT step a", false, true)
	insertCSUnitMessage(t, store, "cs-unit-side", 2, "assistant",
		"SIDEHIT step b", false, true)
	insertCSUnitMessage(t, store, "cs-unit-side", 3, "assistant",
		"main MAINHIT answer", false, false)

	ctx := context.Background()
	side, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "SIDEHIT", Mode: "substring",
		Sources: []string{"messages"}, Limit: 50,
	})
	require.NoError(t, err, "SearchContent sidechain")
	require.Len(t, side.Matches, 2, "sidechain matches")
	for _, m := range side.Matches {
		assert.Equal(t, [2]int{1, 2}, m.OrdinalRange, "sidechain run range")
		assert.True(t, m.Subordinate, "sidechain run is subordinate")
		assert.True(t, m.Sidechain, "anchor sidechain flag")
		assert.Empty(t, m.Relationship, "no session lineage")
	}

	main, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "MAINHIT", Mode: "substring",
		Sources: []string{"messages"}, Limit: 50,
	})
	require.NoError(t, err, "SearchContent main")
	require.Len(t, main.Matches, 1, "main matches")
	m := main.Matches[0]
	assert.Equal(t, [2]int{3, 3}, m.OrdinalRange,
		"sidechain flip bounds the top-level run")
	assert.False(t, m.Subordinate, "top-level run")
	assert.False(t, m.Sidechain, "top-level anchor")
}

// TestPGSearchContentSubagentLineage pins session-level lineage on lexical
// rows: a match inside a subagent session is Subordinate with Relationship
// and ParentSessionID populated from the sessions join.
func TestPGSearchContentSubagentLineage(t *testing.T) {
	store := setupContentSearch(t)
	insertCSSession(t, store, "cs-unit-parent", "proj", "claude",
		"2026-05-01T10:00:00Z", "2026-05-01T10:30:00Z")
	insertCSChildSession(t, store, "cs-unit-child", "proj", "claude",
		"cs-unit-parent", "2026-05-01T10:05:00Z", "2026-05-01T10:25:00Z")
	insertCSUnitMessage(t, store, "cs-unit-child", 0, "user",
		"subagent prompt", false, false)
	insertCSUnitMessage(t, store, "cs-unit-child", 1, "assistant",
		"SUBHIT answer", false, false)

	ctx := context.Background()
	got, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "SUBHIT", Mode: "substring",
		Sources: []string{"messages"}, IncludeChildren: true, Limit: 50,
	})
	require.NoError(t, err, "SearchContent")
	require.Len(t, got.Matches, 1, "matches")
	m := got.Matches[0]
	assert.Equal(t, [2]int{1, 1}, m.OrdinalRange, "single-member run")
	assert.True(t, m.Subordinate, "subagent session is subordinate")
	assert.Equal(t, "subagent", m.Relationship, "Relationship")
	assert.Equal(t, "cs-unit-parent", m.ParentSessionID, "ParentSessionID")
	assert.False(t, m.Sidechain, "anchor not sidechain")
}

// TestPGSearchContentToolDerivedRunRange pins derivation for tool_input and
// canonical tool_result rows: the anchor is the tool call's message row, so
// both locations carry the enclosing run's range while the wire Role stays
// the hard-coded "assistant".
func TestPGSearchContentToolDerivedRunRange(t *testing.T) {
	store := setupContentSearch(t)
	insertCSSession(t, store, "cs-unit-tool", "proj", "claude",
		"2026-05-01T10:00:00Z", "2026-05-01T10:30:00Z")
	insertCSUnitMessage(t, store, "cs-unit-tool", 0, "user",
		"the question", false, false)
	insertCSUnitMessage(t, store, "cs-unit-tool", 1, "assistant",
		"running the tool", false, false)
	insertCSUnitMessage(t, store, "cs-unit-tool", 2, "assistant",
		"continuing the answer", false, false)
	insertCSUnitMessage(t, store, "cs-unit-tool", 3, "user",
		"thanks", false, false)
	insertCSToolCall(t, store, "cs-unit-tool", 1, 0,
		"Bash", "tu1", `{"command":"TOOLHIT"}`, "output RESHIT data")

	ctx := context.Background()
	in, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "TOOLHIT", Mode: "substring",
		Sources: []string{"tool_input"}, Limit: 50,
	})
	require.NoError(t, err, "tool_input search")
	require.Len(t, in.Matches, 1, "tool_input matches")
	assert.Equal(t, "assistant", in.Matches[0].Role, "wire role stays assistant")
	assert.Equal(t, 1, in.Matches[0].Ordinal, "anchor ordinal")
	assert.Equal(t, [2]int{1, 2}, in.Matches[0].OrdinalRange,
		"tool_input anchor classified from the real message row")

	res, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "RESHIT", Mode: "substring",
		Sources: []string{"tool_result"}, Limit: 50,
	})
	require.NoError(t, err, "tool_result search")
	require.Len(t, res.Matches, 1, "tool_result matches")
	assert.Equal(t, [2]int{1, 2}, res.Matches[0].OrdinalRange,
		"canonical tool_result anchor classified from the real message row")
}

// TestPGSearchContentToolResultEventsDerived pins the events branch: an
// orphaned event (no message row at its ordinal) still returns its match
// (row cardinality must not change) with the [o, o] fallback and session
// lineage, while an event whose message row sits inside a run gets the run's
// range via the post-scan anchor lookup.
func TestPGSearchContentToolResultEventsDerived(t *testing.T) {
	store := setupContentSearch(t)
	insertCSSession(t, store, "cs-ev-boss", "proj", "claude",
		"2026-05-01T10:00:00Z", "2026-05-01T10:30:00Z")
	insertCSChildSession(t, store, "cs-ev-orph", "proj", "claude",
		"cs-ev-boss", "2026-05-01T10:05:00Z", "2026-05-01T10:25:00Z")
	// Orphan: no message row at ordinal 7.
	insertCSToolResultEvent(t, store, "cs-ev-orph", 7, 0, 0,
		"tux", "ORPHHIT event content")

	insertCSSession(t, store, "cs-ev-run", "proj", "claude",
		"2026-05-01T11:00:00Z", "2026-05-01T11:30:00Z")
	insertCSUnitMessage(t, store, "cs-ev-run", 0, "user",
		"the question", false, false)
	insertCSUnitMessage(t, store, "cs-ev-run", 1, "assistant",
		"running", false, false)
	insertCSUnitMessage(t, store, "cs-ev-run", 2, "assistant",
		"wrapping up", false, false)
	insertCSToolCall(t, store, "cs-ev-run", 1, 0,
		"Bash", "tu1", `{"command":"x"}`, "")
	insertCSToolResultEvent(t, store, "cs-ev-run", 1, 0, 0,
		"tu1", "EVHIT streamed output")

	ctx := context.Background()
	orph, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "ORPHHIT", Mode: "substring",
		Sources: []string{"tool_result"}, IncludeChildren: true, Limit: 50,
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
	assert.Equal(t, "cs-ev-boss", m.ParentSessionID,
		"ParentSessionID from sessions join")

	ev, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "EVHIT", Mode: "substring",
		Sources: []string{"tool_result"}, Limit: 50,
	})
	require.NoError(t, err, "event search")
	require.Len(t, ev.Matches, 1, "event matches")
	assert.Equal(t, 1, ev.Matches[0].Ordinal, "anchor ordinal")
	assert.Equal(t, [2]int{1, 2}, ev.Matches[0].OrdinalRange,
		"event with a message row inside a run gets the run's range")
}

// TestPGSearchContentRegexDerivedRange spot-checks that regex mode routes
// through the shared derivation pass.
func TestPGSearchContentRegexDerivedRange(t *testing.T) {
	store := setupContentSearch(t)
	insertCSSession(t, store, "cs-unit-rx", "proj", "claude",
		"2026-05-01T10:00:00Z", "2026-05-01T10:30:00Z")
	insertCSUnitMessage(t, store, "cs-unit-rx", 0, "user",
		"the question", false, false)
	insertCSUnitMessage(t, store, "cs-unit-rx", 1, "assistant",
		"RXHIT alpha", false, false)
	insertCSUnitMessage(t, store, "cs-unit-rx", 2, "assistant",
		"RXHIT beta", false, false)

	ctx := context.Background()
	got, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: `RXHIT [a-z]+`, Mode: "regex",
		Sources: []string{"messages"}, Limit: 50,
	})
	require.NoError(t, err, "SearchContent regex")
	require.Len(t, got.Matches, 2, "regex matches")
	for _, m := range got.Matches {
		assert.Equal(t, [2]int{1, 2}, m.OrdinalRange, "derived run range")
	}
}

// TestPGSearchContentFTSDerivedRange spot-checks that PG's fts fallback
// (ILIKE terms over messages) routes through the shared derivation pass.
func TestPGSearchContentFTSDerivedRange(t *testing.T) {
	store := setupContentSearch(t)
	insertCSSession(t, store, "cs-unit-fts", "proj", "claude",
		"2026-05-01T10:00:00Z", "2026-05-01T10:30:00Z")
	insertCSUnitMessage(t, store, "cs-unit-fts", 0, "user",
		"the question", false, false)
	insertCSUnitMessage(t, store, "cs-unit-fts", 1, "assistant",
		"ftshit alpha step", false, false)
	insertCSUnitMessage(t, store, "cs-unit-fts", 2, "assistant",
		"ftshit beta step", false, false)

	ctx := context.Background()
	got, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "ftshit", Mode: "fts",
		Sources: []string{"messages"}, Limit: 50,
	})
	require.NoError(t, err, "SearchContent fts")
	require.Len(t, got.Matches, 2, "fts matches")
	for _, m := range got.Matches {
		assert.Equal(t, [2]int{1, 2}, m.OrdinalRange, "derived run range")
		assert.False(t, m.Subordinate, "top-level run")
	}
}

// TestPGSearchContentDenseFlowDerivedRanges exercises the DENSE derivation
// flow against PG's dense-fetch SQL (scanPGUserBoundaryOrdinals): one session
// whose runs supply at least db.UnitBoundsFlowFactor distinct run anchors on
// a single page, so the shared flow selection fetches real user bounds with
// PG's batched boundary statement before resolving extents. Run lengths are
// derived from the exported gate so the page stays dense if the factor
// changes. The structure packs a main run, a sidechain run, and a
// flip-bounded main run, and every match must carry its run's exact range.
func TestPGSearchContentDenseFlowDerivedRanges(t *testing.T) {
	store := setupContentSearch(t)
	insertCSSession(t, store, "cs-unit-dense", "proj", "claude",
		"2026-05-01T10:00:00Z", "2026-05-01T10:30:00Z")

	// Three runs of runLen anchors each: 3*runLen > UnitBoundsFlowFactor.
	runLen := db.UnitBoundsFlowFactor/2 + 1
	runA := [2]int{1, runLen}                  // main run after user 0
	side := [2]int{runLen + 2, 2*runLen + 1}   // sidechain run after user runLen+1
	runC := [2]int{2*runLen + 2, 3*runLen + 1} // main run bounded left by the flip
	lastUser := 3*runLen + 2

	insertCSUnitMessage(t, store, "cs-unit-dense", 0, "user",
		"first question", false, false)
	for o := runA[0]; o <= runA[1]; o++ {
		insertCSUnitMessage(t, store, "cs-unit-dense", o, "assistant",
			fmt.Sprintf("DFHIT main-a %d", o), false, false)
	}
	insertCSUnitMessage(t, store, "cs-unit-dense", runLen+1, "user",
		"second question", false, false)
	for o := side[0]; o <= side[1]; o++ {
		insertCSUnitMessage(t, store, "cs-unit-dense", o, "assistant",
			fmt.Sprintf("DFHIT side %d", o), false, true)
	}
	for o := runC[0]; o <= runC[1]; o++ {
		insertCSUnitMessage(t, store, "cs-unit-dense", o, "assistant",
			fmt.Sprintf("DFHIT main-c %d", o), false, false)
	}
	insertCSUnitMessage(t, store, "cs-unit-dense", lastUser, "user",
		"done", false, false)

	anchorCount := 3 * runLen
	require.GreaterOrEqual(t, anchorCount, db.UnitBoundsFlowFactor,
		"single-session page must clear the dense-flow gate")

	ctx := context.Background()
	got, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "DFHIT", Mode: "substring",
		Sources: []string{"messages"}, Limit: anchorCount + 10,
	})
	require.NoError(t, err, "SearchContent")
	require.Len(t, got.Matches, anchorCount, "matches")
	byOrd := csMatchesByOrdinal(t, got)
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

// TestPGSearchContentMultiRunReducerParity is the hand-computed parity check
// against the SQLite-side embedding reducer: one session with two top-level
// runs, a sidechain run between them, and an interior system row that must
// not close the run it sits inside.
func TestPGSearchContentMultiRunReducerParity(t *testing.T) {
	store := setupContentSearch(t)
	insertCSSession(t, store, "cs-unit-multi", "proj", "claude",
		"2026-05-01T10:00:00Z", "2026-05-01T10:30:00Z")
	insertCSUnitMessage(t, store, "cs-unit-multi", 0, "user",
		"PARHIT q1", false, false)
	insertCSUnitMessage(t, store, "cs-unit-multi", 1, "assistant",
		"PARHIT a", false, false)
	insertCSUnitMessage(t, store, "cs-unit-multi", 2, "assistant",
		"PARHIT b", false, false)
	insertCSUnitMessage(t, store, "cs-unit-multi", 3, "assistant",
		"PARHIT sc1", false, true)
	insertCSUnitMessage(t, store, "cs-unit-multi", 4, "assistant",
		"PARHIT sc2", false, true)
	insertCSUnitMessage(t, store, "cs-unit-multi", 5, "assistant",
		"PARHIT c", false, false)
	insertCSUnitMessage(t, store, "cs-unit-multi", 6, "user",
		"PARHIT sys note", true, false)
	insertCSUnitMessage(t, store, "cs-unit-multi", 7, "assistant",
		"PARHIT d", false, false)
	insertCSUnitMessage(t, store, "cs-unit-multi", 8, "user",
		"PARHIT q2", false, false)

	ctx := context.Background()
	got, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "PARHIT", Mode: "substring",
		Sources: []string{"messages"}, Limit: 50,
	})
	require.NoError(t, err, "SearchContent")
	require.Len(t, got.Matches, 9, "matches")
	byOrd := csMatchesByOrdinal(t, got)

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
