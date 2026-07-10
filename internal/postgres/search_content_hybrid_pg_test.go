//go:build pgtest

package postgres

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// hybridFakeSearcher is a canned db.VectorSearcher for PG hybrid tests: it
// returns a fixed, rank-ordered slice of vector hits and resolves message
// refs against an explicit units list first, then against the units implied
// by hits (the real resolver reads the same mirror the hits come from). This
// isolates the fusion + keyword-leg logic (real ILIKE SQL over messages) from
// the pgvector-backed searcher, which Task 8/9 tests already cover. It mirrors
// internal/db's fakeVectorSearcher so parity between the backends is visible.
type hybridFakeSearcher struct {
	hits  []db.VectorHit
	units []db.UnitRef
}

func (f *hybridFakeSearcher) SemanticSearch(
	_ context.Context, _ string, limit int,
) ([]db.VectorHit, error) {
	hits := f.hits
	if limit > 0 && limit < len(hits) {
		hits = hits[:limit]
	}
	return hits, nil
}

func (f *hybridFakeSearcher) ResolveMessageUnits(
	_ context.Context, refs []db.MessageRef,
) ([]db.UnitRef, error) {
	out := make([]db.UnitRef, len(refs))
	for i, ref := range refs {
		out[i] = f.resolveRef(ref)
	}
	return out, nil
}

func (f *hybridFakeSearcher) resolveRef(ref db.MessageRef) db.UnitRef {
	for _, u := range f.units {
		if u.SessionID == ref.SessionID &&
			ref.Ordinal >= u.OrdinalStart && ref.Ordinal <= u.OrdinalEnd {
			return u
		}
	}
	for _, h := range f.hits {
		if h.SessionID == ref.SessionID &&
			ref.Ordinal >= h.OrdinalStart && ref.Ordinal <= h.OrdinalEnd {
			return db.UnitRef{
				DocKey:       fmt.Sprintf("fake:%s:%d", h.SessionID, h.OrdinalStart),
				SessionID:    h.SessionID,
				OrdinalStart: h.OrdinalStart,
				OrdinalEnd:   h.OrdinalEnd,
				Subordinate:  h.Subordinate,
			}
		}
	}
	return db.UnitRef{}
}

// wireHybrid seeds a fresh content-search store, wires the fake searcher, and
// returns the store. It reuses setupContentSearch (base schema, no pgvector),
// since the vector leg and resolver are faked and the keyword leg runs real
// ILIKE SQL over the messages table.
func wireHybrid(t *testing.T, f *hybridFakeSearcher) *Store {
	t.Helper()
	store := setupContentSearch(t)
	store.SetVectorSearcher(f)
	return store
}

const (
	hybridStart = "2026-05-01T10:00:00Z"
	hybridEnd   = "2026-05-01T10:30:00Z"
)

// TestPGHybridBothLegsFuseKeywordAnchor pins the end-to-end unit fusion: a
// vector hit on a run and a keyword hit inside that same run fuse into ONE
// result whose anchor ordinal is the keyword-matched message (overriding the
// vector leg's chunk anchor) and whose snippet centers on the keyword text.
func TestPGHybridBothLegsFuseKeywordAnchor(t *testing.T) {
	f := &hybridFakeSearcher{hits: []db.VectorHit{
		{SessionID: "s1", Ordinal: 1, OrdinalStart: 1, OrdinalEnd: 2,
			Score: 0.9, Snippet: "first step of the answer"},
	}}
	store := wireHybrid(t, f)
	insertCSSession(t, store, "s1", "proj", "claude", hybridStart, hybridEnd)
	insertCSUnitMessage(t, store, "s1", 0, "user", "the question", false, false)
	insertCSUnitMessage(t, store, "s1", 1, "assistant", "first step of the answer", false, false)
	insertCSUnitMessage(t, store, "s1", 2, "assistant", "second step mentions zebra", false, false)

	page, err := store.SearchContent(context.Background(), db.ContentSearchFilter{
		Pattern: "zebra", Mode: "hybrid", Limit: 50,
	})
	require.NoError(t, err, "SearchContent hybrid")
	require.Len(t, page.Matches, 1,
		"the run's vector hit and its keyword-matched member must fuse into one result")

	m := page.Matches[0]
	assert.Equal(t, "s1", m.SessionID)
	assert.Equal(t, 2, m.Ordinal, "anchor overridden to the keyword-matched message")
	assert.Equal(t, "assistant", m.Role, "role of the keyword-matched message")
	assert.Equal(t, [2]int{1, 2}, m.OrdinalRange, "range spans the containing unit")
	assert.Contains(t, m.Snippet, "zebra", "keyword snippet wins for display")
	require.NotNil(t, m.Score)
	assert.InDelta(t, 2.0/61.0, *m.Score, 1e-9, "fused score: rank 1 in both legs")
}

// TestPGHybridVectorOnlyAndKeywordOnlyBothAppear pins RRF union semantics: a
// hit found only by the vector leg and a hit found only by the keyword leg
// both survive fusion, not just the leg-overlapping ones.
func TestPGHybridVectorOnlyAndKeywordOnlyBothAppear(t *testing.T) {
	f := &hybridFakeSearcher{hits: []db.VectorHit{
		{SessionID: "vec-only", Ordinal: 0, Score: 0.9, Snippet: "totally unrelated content"},
	}}
	store := wireHybrid(t, f)
	insertCSSession(t, store, "kw-only", "proj", "claude", hybridStart, hybridEnd)
	insertCSUnitMessage(t, store, "kw-only", 0, "user", "needle in a haystack", false, false)
	insertCSSession(t, store, "vec-only", "proj", "claude", hybridStart, hybridEnd)
	insertCSUnitMessage(t, store, "vec-only", 0, "user", "totally unrelated content", false, false)

	page, err := store.SearchContent(context.Background(), db.ContentSearchFilter{
		Pattern: "needle", Mode: "hybrid", Limit: 50,
	})
	require.NoError(t, err, "SearchContent hybrid")
	require.Len(t, page.Matches, 2, "the keyword-only and vector-only hits both survive")

	var ids []string
	for _, m := range page.Matches {
		ids = append(ids, m.SessionID)
	}
	assert.ElementsMatch(t, []string{"kw-only", "vec-only"}, ids)
}

// TestPGHybridKeywordHitOutsideUniverseKeepsMessageGranularity pins the
// no-unit escape hatch: a keyword hit on a message with no containing mirror
// unit survives fusion under its own message-granularity key with its own
// ordinal, carrying a structurally derived unit range (the uncovered hit sits
// in a two-message assistant run) rather than a self-range.
func TestPGHybridKeywordHitOutsideUniverseKeepsMessageGranularity(t *testing.T) {
	f := &hybridFakeSearcher{hits: []db.VectorHit{
		{SessionID: "covered", Ordinal: 0, Score: 0.9, Snippet: "unrelated content"},
	}}
	store := wireHybrid(t, f)
	insertCSSession(t, store, "uncovered", "proj", "claude", hybridStart, hybridEnd)
	insertCSUnitMessage(t, store, "uncovered", 0, "user", "irrelevant lead-in", false, false)
	insertCSUnitMessage(t, store, "uncovered", 1, "assistant", "tool output mentions zebra", false, false)
	insertCSUnitMessage(t, store, "uncovered", 2, "assistant", "further elaboration", false, false)
	insertCSSession(t, store, "covered", "proj", "claude", hybridStart, hybridEnd)
	insertCSUnitMessage(t, store, "covered", 0, "user", "unrelated content", false, false)

	page, err := store.SearchContent(context.Background(), db.ContentSearchFilter{
		Pattern: "zebra", Mode: "hybrid", Limit: 50,
	})
	require.NoError(t, err, "SearchContent hybrid")
	require.Len(t, page.Matches, 2, "the unit-less keyword hit must not vanish")

	byID := map[string]db.ContentMatch{}
	for _, m := range page.Matches {
		byID[m.SessionID] = m
	}
	uncovered, ok := byID["uncovered"]
	require.True(t, ok, "no-unit keyword hit survives")
	assert.Equal(t, 1, uncovered.Ordinal, "message-granularity ordinal kept")
	assert.Equal(t, [2]int{1, 2}, uncovered.OrdinalRange,
		"unit-less hit gets the derived run range, not a self-range")
	assert.False(t, uncovered.Subordinate,
		"unit-less top-level non-sidechain hit stays non-subordinate")
	assert.Contains(t, uncovered.Snippet, "zebra")
}

// TestPGHybridScopeFiltersBothLegs pins that Scope filtering sees both legs:
// scope=top drops the vector-leg subordinate hit AND the keyword-leg
// subordinate hit, while scope=subordinate keeps only them. The keyword-leg
// subordinate flag comes from the resolved unit.
func TestPGHybridScopeFiltersBothLegs(t *testing.T) {
	f := &hybridFakeSearcher{
		hits: []db.VectorHit{
			{SessionID: "topvec", Ordinal: 0, Score: 0.9, Snippet: "topvec body"},
			{SessionID: "subvec", Ordinal: 0, Subordinate: true, Score: 0.8, Snippet: "subvec body"},
		},
		units: []db.UnitRef{
			{DocKey: "u:subkw:0", SessionID: "subkw", OrdinalStart: 0, OrdinalEnd: 0, Subordinate: true},
		},
	}
	store := wireHybrid(t, f)
	// Vector-only sessions (no keyword match).
	insertCSSession(t, store, "topvec", "proj", "claude", hybridStart, hybridEnd)
	insertCSUnitMessage(t, store, "topvec", 0, "user", "topvec body", false, false)
	insertCSSession(t, store, "subvec", "proj", "claude", hybridStart, hybridEnd)
	insertCSUnitMessage(t, store, "subvec", 0, "assistant", "subvec body", false, false)
	// Keyword sessions.
	insertCSSession(t, store, "topkw", "proj", "claude", hybridStart, hybridEnd)
	insertCSUnitMessage(t, store, "topkw", 0, "user", "zebra top level", false, false)
	insertCSChildSession(t, store, "subkw", "proj", "claude", "topkw", hybridStart, hybridEnd)
	insertCSUnitMessage(t, store, "subkw", 0, "assistant", "zebra subordinate", false, false)

	ctx := context.Background()
	top, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "zebra", Mode: "hybrid", Scope: "top", Limit: 50,
	})
	require.NoError(t, err, "SearchContent scope=top")
	topIDs := hybridSessionIDs(top)
	assert.ElementsMatch(t, []string{"topvec", "topkw"}, topIDs,
		"scope=top keeps only top-level units from both legs")

	sub, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "zebra", Mode: "hybrid", Scope: "subordinate", Limit: 50,
	})
	require.NoError(t, err, "SearchContent scope=subordinate")
	subIDs := hybridSessionIDs(sub)
	assert.ElementsMatch(t, []string{"subvec", "subkw"}, subIDs,
		"scope=subordinate keeps only subordinate units from both legs")
	for _, m := range sub.Matches {
		assert.True(t, m.Subordinate, "surviving matches are subordinate")
	}
}

// TestPGHybridRRFOrderParity pins that the page order equals what db.RRFMerge
// produces for the two legs' known ranks: the doubly-ranked unit leads, then a
// tie between the two single-leg units breaks by ascending fusion key (the
// message key sorts before the unit key), independent of session recency.
func TestPGHybridRRFOrderParity(t *testing.T) {
	f := &hybridFakeSearcher{hits: []db.VectorHit{
		{SessionID: "both", Ordinal: 0, Score: 0.9, Snippet: "needle here"},
		{SessionID: "vec", Ordinal: 0, Score: 0.8, Snippet: "no match here"},
	}}
	store := wireHybrid(t, f)
	insertCSSession(t, store, "both", "proj", "claude", hybridStart, hybridEnd)
	insertCSUnitMessage(t, store, "both", 0, "user", "needle here", false, false)
	insertCSSession(t, store, "vec", "proj", "claude", hybridStart, hybridEnd)
	insertCSUnitMessage(t, store, "vec", 0, "user", "no match here", false, false)
	insertCSSession(t, store, "kw", "proj", "claude", hybridStart, hybridEnd)
	insertCSUnitMessage(t, store, "kw", 0, "user", "needle appears", false, false)

	page, err := store.SearchContent(context.Background(), db.ContentSearchFilter{
		Pattern: "needle", Mode: "hybrid", Limit: 50,
	})
	require.NoError(t, err, "SearchContent hybrid")

	// The vector leg ranks [both, vec]; the keyword leg ranks [both, kw]
	// (both/kw match "needle"; vec does not). "both" resolves to the vector
	// unit in both legs; "kw" has no unit, so it keeps a message key.
	vecRanked := []db.RankedUnit{
		{Key: db.UnitFusionKey("both", 0)}, {Key: db.UnitFusionKey("vec", 0)},
	}
	kwRanked := []db.RankedUnit{
		{Key: db.UnitFusionKey("both", 0)}, {Key: db.MessageFusionKey("kw", 0)},
	}
	merged := db.RRFMerge([][]db.RankedUnit{vecRanked, kwRanked}, 50)
	keyToSession := map[string]string{
		db.UnitFusionKey("both", 0):  "both",
		db.UnitFusionKey("vec", 0):   "vec",
		db.MessageFusionKey("kw", 0): "kw",
	}
	want := make([]string, len(merged))
	for i, u := range merged {
		want[i] = keyToSession[u.Unit.Key]
	}
	require.Equal(t, want, hybridSessionIDs(page),
		"page order must match db.RRFMerge over the two legs")
}

// TestPGHybridKeywordSnippetCentersOnTerm pins the term-aware keyword snippet:
// a two-term query whose terms are non-adjacent in the message (so the raw
// pattern is not a literal substring) centers the snippet on a matched term, not
// the message start. The first matched term sits far past the start window, so
// under the old raw-pattern centering the snippet would not contain it.
func TestPGHybridKeywordSnippetCentersOnTerm(t *testing.T) {
	f := &hybridFakeSearcher{}
	store := wireHybrid(t, f)

	// "alpha" lands at byte 160, past the ~60-char start snippet window; "omega"
	// follows later. The raw pattern "alpha omega" is not a substring.
	lead := strings.Repeat("x ", 80)
	content := lead + "alpha then a stretch of filler words before omega ends it"
	insertCSSession(t, store, "s1", "proj", "claude", hybridStart, hybridEnd)
	insertCSUnitMessage(t, store, "s1", 0, "user", content, false, false)

	page, err := store.SearchContent(context.Background(), db.ContentSearchFilter{
		Pattern: "alpha omega", Mode: "hybrid", Limit: 50,
	})
	require.NoError(t, err, "SearchContent hybrid")
	require.Len(t, page.Matches, 1, "the two-term keyword hit survives")
	assert.Contains(t, page.Matches[0].Snippet, "alpha",
		"snippet centers on a matched term, not the message start")
}

// hybridSessionIDs projects a page's matches to session IDs in order.
func hybridSessionIDs(page db.ContentSearchPage) []string {
	out := make([]string, len(page.Matches))
	for i, m := range page.Matches {
		out[i] = m.SessionID
	}
	return out
}
