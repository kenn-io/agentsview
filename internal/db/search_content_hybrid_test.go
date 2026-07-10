package db

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSearchContentHybridNoSearcherUnavailable pins the same capability gate
// as "semantic": hybrid needs a wired VectorSearcher regardless of FTS
// availability, and reports ErrSemanticUnavailable when none is wired.
func TestSearchContentHybridNoSearcherUnavailable(t *testing.T) {
	d := testDB(t)
	assert.False(t, d.HasSemantic(), "HasSemantic before wiring a searcher")

	_, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "hello", Mode: "hybrid",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrSemanticUnavailable),
		"expected ErrSemanticUnavailable, got %v", err)
}

// TestSearchContentHybridCursorRejected pins the shared semantic/hybrid
// validation: cursor pagination is rejected before the capability check.
func TestSearchContentHybridCursorRejected(t *testing.T) {
	d := testDB(t)
	d.SetVectorSearcher(&fakeVectorSearcher{})

	_, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "hello", Mode: "hybrid", Cursor: 1,
	})
	require.Error(t, err)
	var inputErr *SearchInputError
	assert.True(t, errors.As(err, &inputErr),
		"expected *SearchInputError, got %T: %v", err, err)
}

// TestSearchContentHybridFTSUnavailable pins the FTS-missing capability gate:
// hybrid additionally requires db.HasFTS() and mirrors mode "fts"'s error
// when the messages_fts table is gone.
func TestSearchContentHybridFTSUnavailable(t *testing.T) {
	d := testDB(t)
	if !d.HasFTS() {
		t.Skip("fts5 not available")
	}
	d.SetVectorSearcher(&fakeVectorSearcher{})
	_, err := d.getWriter().Exec("DROP TABLE IF EXISTS messages_fts")
	require.NoError(t, err, "drop messages_fts")

	_, err = d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "hello", Mode: "hybrid",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, errFTSUnavailable),
		"expected errFTSUnavailable, got %v", err)
}

// TestSearchContentHybridBothLegsOutrankSingleLeg pins reciprocal-rank
// fusion's core guarantee: a document ranked top by both the vector and FTS
// legs must fuse to a higher score than a document appearing in only one leg,
// and must sort first.
func TestSearchContentHybridBothLegsOutrankSingleLeg(t *testing.T) {
	d := testDB(t)
	if !d.HasFTS() {
		t.Skip("fts5 not available")
	}
	seedSearchSession(t, d, "both", "proj", [][2]string{
		{"user", "needle in a haystack"},
	})
	seedSearchSession(t, d, "vec-only", "proj", [][2]string{
		{"user", "totally unrelated content"},
	})
	d.SetVectorSearcher(&fakeVectorSearcher{hits: []VectorHit{
		{SessionID: "both", Ordinal: 0, Score: 0.9, Snippet: "needle in a haystack"},
		{SessionID: "vec-only", Ordinal: 0, Score: 0.8, Snippet: "totally unrelated content"},
	}})

	page, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "needle", Mode: "hybrid", Limit: 50,
	})
	require.NoError(t, err, "SearchContent hybrid")
	require.Len(t, page.Matches, 2, "matches")

	require.Equal(t, "both", page.Matches[0].SessionID, "double-leg hit ranks first")
	require.Equal(t, "vec-only", page.Matches[1].SessionID, "single-leg hit ranks second")
	require.NotNil(t, page.Matches[0].Score, "Score")
	require.NotNil(t, page.Matches[1].Score, "Score")
	assert.Greater(t, *page.Matches[0].Score, *page.Matches[1].Score,
		"double-leg fused score must exceed single-leg fused score")
}

// TestSearchContentHybridVectorOnlyAndFTSOnlyBothAppear pins RRF's union
// semantics: a hit found only by the vector leg and a hit found only by the
// FTS leg must both survive the fusion, not just the leg-overlapping ones.
func TestSearchContentHybridVectorOnlyAndFTSOnlyBothAppear(t *testing.T) {
	d := testDB(t)
	if !d.HasFTS() {
		t.Skip("fts5 not available")
	}
	seedSearchSession(t, d, "fts-only", "proj", [][2]string{
		{"user", "needle in a haystack"},
	})
	seedSearchSession(t, d, "vec-only", "proj", [][2]string{
		{"user", "totally unrelated content"},
	})
	d.SetVectorSearcher(&fakeVectorSearcher{hits: []VectorHit{
		{SessionID: "vec-only", Ordinal: 0, Score: 0.9, Snippet: "totally unrelated content"},
	}})

	page, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "needle", Mode: "hybrid", Limit: 50,
	})
	require.NoError(t, err, "SearchContent hybrid")
	require.Len(t, page.Matches, 2, "matches")

	var ids []string
	for _, m := range page.Matches {
		ids = append(ids, m.SessionID)
	}
	assert.ElementsMatch(t, []string{"fts-only", "vec-only"}, ids)
}

// TestSearchContentHybridScoresStrictlyDescending pins that fused RRF scores
// order the page strictly descending, with no inversions or unexpected ties
// across a mix of a double-leg hit and single-leg (vector-only) hits.
func TestSearchContentHybridScoresStrictlyDescending(t *testing.T) {
	d := testDB(t)
	if !d.HasFTS() {
		t.Skip("fts5 not available")
	}
	seedSearchSession(t, d, "both", "proj", [][2]string{
		{"user", "needle in a haystack"},
	})
	seedSearchSession(t, d, "vec2", "proj", [][2]string{
		{"user", "other stuff entirely"},
	})
	seedSearchSession(t, d, "vec3", "proj", [][2]string{
		{"user", "yet more stuff"},
	})
	d.SetVectorSearcher(&fakeVectorSearcher{hits: []VectorHit{
		{SessionID: "both", Ordinal: 0, Score: 0.9, Snippet: "needle in a haystack"},
		{SessionID: "vec2", Ordinal: 0, Score: 0.8, Snippet: "other stuff entirely"},
		{SessionID: "vec3", Ordinal: 0, Score: 0.7, Snippet: "yet more stuff"},
	}})

	page, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "needle", Mode: "hybrid", Limit: 50,
	})
	require.NoError(t, err, "SearchContent hybrid")
	require.Len(t, page.Matches, 3, "matches")

	for i := 1; i < len(page.Matches); i++ {
		require.NotNil(t, page.Matches[i-1].Score, "Score at %d", i-1)
		require.NotNil(t, page.Matches[i].Score, "Score at %d", i)
		assert.Greater(t, *page.Matches[i-1].Score, *page.Matches[i].Score,
			"scores must be strictly descending at index %d", i)
	}
}

// TestSearchContentHybridProjectFilterConstrainsBothLegs pins that the
// session-scope filter narrows both legs: the FTS leg filters in SQL, the
// vector leg is filtered post-hoc before the merge, and a session outside the
// requested project must be dropped from the fused result even though it
// matches both legs' raw searches.
func TestSearchContentHybridProjectFilterConstrainsBothLegs(t *testing.T) {
	d := testDB(t)
	if !d.HasFTS() {
		t.Skip("fts5 not available")
	}
	seedSearchSession(t, d, "in-scope", "alpha", [][2]string{
		{"user", "needle in a haystack"},
	})
	seedSearchSession(t, d, "out-of-scope", "beta", [][2]string{
		{"user", "needle in another haystack"},
	})
	d.SetVectorSearcher(&fakeVectorSearcher{hits: []VectorHit{
		{SessionID: "in-scope", Ordinal: 0, Score: 0.9, Snippet: "needle in a haystack"},
		{SessionID: "out-of-scope", Ordinal: 0, Score: 0.8, Snippet: "needle in another haystack"},
	}})

	page, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "needle", Mode: "hybrid", Limit: 50, Project: "alpha",
	})
	require.NoError(t, err, "SearchContent hybrid")
	require.Len(t, page.Matches, 1, "matches after project filter")
	assert.Equal(t, "in-scope", page.Matches[0].SessionID, "surviving session")
}

// TestSearchContentHybridRedactsSecretPastChunkTruncation mirrors the
// semantic-mode regression (TestSearchContentSemanticRedactsSecretPastChunkTruncation)
// for hybrid: the vector leg's chunk snippet is truncated mid-PEM-body, before
// the "-----END" marker the PEM rule requires to fire. Redacting that
// fragment in isolation would miss the secret entirely; hybrid must redact
// against the message's full content the same way semantic mode does.
func TestSearchContentHybridRedactsSecretPastChunkTruncation(t *testing.T) {
	d := testDB(t)
	if !d.HasFTS() {
		t.Skip("fts5 not available")
	}
	pem := "-----BEGIN RSA PRIVATE KEY-----\n" +
		strings.Repeat("MIIBSECRETKEYMATERIAL0123456789ABCDEF\n", 5) +
		"-----END RSA PRIVATE KEY-----"
	content := "needle deploy with this attached key " + pem + " ok"
	seedSearchSession(t, d, "s1", "proj", [][2]string{
		{"user", content},
	})

	cut := strings.Index(content, "MIIBSECRETKEYMATERIAL") + len("MIIBSECRETKEYMATERIAL") + 3
	require.Less(t, cut, strings.Index(content, "-----END"),
		"test setup: cut must land before the END marker")
	truncatedSnippet := content[:cut] + "…"

	d.SetVectorSearcher(&fakeVectorSearcher{hits: []VectorHit{
		{SessionID: "s1", Ordinal: 0, Score: 0.9, Snippet: truncatedSnippet},
	}})

	page, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "needle", Mode: "hybrid", Limit: 50,
	})
	require.NoError(t, err, "SearchContent hybrid")
	require.Len(t, page.Matches, 1, "matches")
	assert.NotContains(t, page.Matches[0].Snippet, "SECRETKEYMATERIAL",
		"hybrid snippet leaked key material truncated out of the vector chunk")
	assert.Contains(t, page.Matches[0].Snippet, "needle",
		"snippet lost the matched context")
}

// mergedKeys projects a merged result to its keys in rank order.
func mergedKeys(merged []FusedUnit) []string {
	keys := make([]string, len(merged))
	for i, m := range merged {
		keys[i] = m.Unit.Key
	}
	return keys
}

// TestRRFMergeBothLegsOutrankSingleLeg pins RRF's core guarantee at the
// merge level: a unit ranked by both legs scores the sum of its per-leg
// reciprocal ranks and outranks a unit seen by only one leg.
func TestRRFMergeBothLegsOutrankSingleLeg(t *testing.T) {
	merged := RRFMerge([][]RankedUnit{
		{{Key: "a"}, {Key: "b"}},
		{{Key: "a"}},
	}, 0)
	require.Equal(t, []string{"a", "b"}, mergedKeys(merged))
	assert.InDelta(t, 2.0/61.0, merged[0].Score, 1e-12, "double-leg score")
	assert.InDelta(t, 1.0/62.0, merged[1].Score, 1e-12, "single-leg score")
}

// TestRRFMergeSubordinatePenaltyAcrossLegs pins the rank+5 penalty: a
// subordinate unit at leg rank 1 must fall below a top-level unit at the
// same rank in the other leg.
func TestRRFMergeSubordinatePenaltyAcrossLegs(t *testing.T) {
	merged := RRFMerge([][]RankedUnit{
		{{Key: "sub", Subordinate: true}},
		{{Key: "top"}},
	}, 0)
	require.Equal(t, []string{"top", "sub"}, mergedKeys(merged))
	assert.InDelta(t, 1.0/61.0, merged[0].Score, 1e-12)
	assert.InDelta(t, 1.0/66.0, merged[1].Score, 1e-12, "subordinate uses rank+5")
}

// TestRRFMergeOneLegSubordinatePenaltyReorders pins the one-leg (semantic-
// only) fusion contract: a subordinate unit ranked immediately above a
// top-level unit drops below it after the merge.
func TestRRFMergeOneLegSubordinatePenaltyReorders(t *testing.T) {
	merged := RRFMerge([][]RankedUnit{{
		{Key: "sub", Subordinate: true},
		{Key: "top"},
	}}, 0)
	assert.Equal(t, []string{"top", "sub"}, mergedKeys(merged))
}

// TestRRFMergeDeterministicTieBreak pins tie handling: a subordinate unit at
// rank 1 (effective rank 6) scores exactly like a top-level unit at rank 6,
// and the tie breaks by ascending key, not map iteration order.
func TestRRFMergeDeterministicTieBreak(t *testing.T) {
	leg := []RankedUnit{
		{Key: "zzz", Subordinate: true},
		{Key: "m2"}, {Key: "m3"}, {Key: "m4"}, {Key: "m5"},
		{Key: "aaa"},
	}
	for range 20 {
		merged := RRFMerge([][]RankedUnit{leg}, 0)
		require.Equal(t,
			[]string{"m2", "m3", "m4", "m5", "aaa", "zzz"}, mergedKeys(merged))
	}
}

// TestRRFMergeLimitHonored pins truncation: limit > 0 caps the merged list
// at the top-scored units.
func TestRRFMergeLimitHonored(t *testing.T) {
	merged := RRFMerge([][]RankedUnit{
		{{Key: "a"}, {Key: "b"}, {Key: "c"}},
	}, 2)
	assert.Equal(t, []string{"a", "b"}, mergedKeys(merged))
}

// TestSearchContentHybridFTSHitInsideRunFusesWithFTSAnchor is the
// end-to-end unit-fusion test: an FTS-matched message inside a run must fuse
// with the run's semantic hit into ONE result whose anchor ordinal is the
// FTS-matched message (overriding the vector leg's chunk anchor) and whose
// snippet centers on the FTS-matched text.
func TestSearchContentHybridFTSHitInsideRunFusesWithFTSAnchor(t *testing.T) {
	d := testDB(t)
	if !d.HasFTS() {
		t.Skip("fts5 not available")
	}
	seedSearchSession(t, d, "s1", "proj", [][2]string{
		{"user", "the question"},
		{"assistant", "first step of the answer"},
		{"assistant", "second step mentions zebra"},
	})
	// The run [1,2] anchors its semantic hit at ordinal 1; FTS matches
	// ordinal 2.
	d.SetVectorSearcher(&fakeVectorSearcher{hits: []VectorHit{
		{SessionID: "s1", Ordinal: 1, OrdinalStart: 1, OrdinalEnd: 2,
			Score: 0.9, Snippet: "first step of the answer"},
	}})

	page, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "zebra", Mode: "hybrid", Limit: 50,
	})
	require.NoError(t, err, "SearchContent hybrid")
	require.Len(t, page.Matches, 1,
		"the run's semantic hit and its FTS-matched member must fuse into one result")
	m := page.Matches[0]
	assert.Equal(t, "s1", m.SessionID)
	assert.Equal(t, 2, m.Ordinal, "anchor overridden to the FTS-matched message")
	assert.Equal(t, "assistant", m.Role, "role of the FTS-matched message")
	assert.Contains(t, m.Snippet, "zebra", "FTS snippet wins for display")
	require.NotNil(t, m.Score)
	assert.InDelta(t, 2.0/61.0, *m.Score, 1e-9,
		"fused score: rank 1 in both legs")
}

// TestSearchContentHybridNoUnitFTSHitKeepsMessageGranularity pins the
// no-unit escape hatch: an FTS hit on a message with no containing unit
// (outside the mirror) survives fusion under its own message-granularity key
// with its own ordinal, and carries the structurally derived unit range
// rather than a self-range: the "uncovered" hit sits in a two-message
// assistant run, so its range must span the run even though the mirror knows
// nothing about the session.
func TestSearchContentHybridNoUnitFTSHitKeepsMessageGranularity(t *testing.T) {
	d := testDB(t)
	if !d.HasFTS() {
		t.Skip("fts5 not available")
	}
	seedSearchSession(t, d, "uncovered", "proj", [][2]string{
		{"user", "irrelevant lead-in"},
		{"assistant", "tool output mentions zebra"},
		{"assistant", "further elaboration on the output"},
	})
	seedSearchSession(t, d, "covered", "proj", [][2]string{
		{"user", "unrelated content"},
	})
	// The searcher's mirror knows only the "covered" session, so the
	// resolver returns a zero UnitRef for the "uncovered" FTS hit.
	d.SetVectorSearcher(&fakeVectorSearcher{hits: []VectorHit{
		{SessionID: "covered", Ordinal: 0, Score: 0.9, Snippet: "unrelated content"},
	}})

	page, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "zebra", Mode: "hybrid", Limit: 50,
	})
	require.NoError(t, err, "SearchContent hybrid")
	require.Len(t, page.Matches, 2, "the unit-less FTS hit must not vanish")

	byID := map[string]ContentMatch{}
	for _, m := range page.Matches {
		byID[m.SessionID] = m
	}
	uncovered, ok := byID["uncovered"]
	require.True(t, ok, "no-unit FTS hit survives")
	assert.Equal(t, 1, uncovered.Ordinal, "message-granularity ordinal kept")
	assert.Equal(t, [2]int{1, 2}, uncovered.OrdinalRange,
		"unit-less hit gets the derived run range, not a self-range")
	assert.False(t, uncovered.Subordinate,
		"unit-less top-level non-sidechain hit stays non-subordinate")
	assert.Contains(t, uncovered.Snippet, "zebra")
}

// seedUnitlessSidechainFixture seeds one top-level session ("side") whose
// matching assistant message is a sidechain row (ordinals 1-2 form one
// sidechain run) and one plain top-level session ("plain") matching once.
// "side" repeats the term so bm25 ranks it above "plain" in the FTS leg. The
// returned searcher's mirror knows neither session, so both FTS hits are
// unit-less and their classification must be structurally derived.
func seedUnitlessSidechainFixture(t *testing.T, d *DB) *fakeVectorSearcher {
	t.Helper()
	insertSession(t, d, "side", "proj", func(s *Session) {
		s.Agent = "claude"
		s.UserMessageCount = 2
	})
	require.NoError(t, d.ReplaceSessionMessages("side", []Message{
		{SessionID: "side", Ordinal: 0, Role: "user",
			Content: "the question", Timestamp: "2026-05-20T12:00:00Z"},
		{SessionID: "side", Ordinal: 1, Role: "assistant", IsSidechain: true,
			Content: "zebra zebra zebra zebra", Timestamp: "2026-05-20T12:00:01Z"},
		{SessionID: "side", Ordinal: 2, Role: "assistant", IsSidechain: true,
			Content: "sidechain elaboration", Timestamp: "2026-05-20T12:00:02Z"},
	}))
	seedSearchSession(t, d, "plain", "proj", [][2]string{
		{"user", "zebra appears once here"},
	})
	return &fakeVectorSearcher{}
}

// TestSearchContentHybridUnitlessSidechainClassifiedSubordinate pins the
// pre-merge classification of unit-less FTS hits: a hit anchored in a
// sidechain run (no mirror unit) must carry subordinate=true and the derived
// sidechain-run range on the wire — matching what lexical mode emits for the
// same anchor — and must be rank-penalized below an equal top-level hit at
// the default (all) scope even though the FTS leg ranks it first.
func TestSearchContentHybridUnitlessSidechainClassifiedSubordinate(t *testing.T) {
	d := testDB(t)
	if !d.HasFTS() {
		t.Skip("fts5 not available")
	}
	d.SetVectorSearcher(seedUnitlessSidechainFixture(t, d))

	page, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "zebra", Mode: "hybrid", Limit: 50,
	})
	require.NoError(t, err, "SearchContent hybrid")
	require.Len(t, page.Matches, 2)
	assert.Equal(t, "plain", page.Matches[0].SessionID,
		"subordinate penalty must drop the higher-FTS-ranked sidechain hit below top-level")
	side := page.Matches[1]
	require.Equal(t, "side", side.SessionID)
	assert.True(t, side.Subordinate, "unit-less sidechain hit classified subordinate")
	assert.True(t, side.Sidechain, "anchor message is_sidechain")
	assert.Equal(t, 1, side.Ordinal, "message-granularity anchor kept")
	assert.Equal(t, [2]int{1, 2}, side.OrdinalRange,
		"derived sidechain-run range, not a self-range")

	// Lexical parity: mode "fts" must classify the same anchor identically.
	lexical, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "zebra", Mode: "fts", Sources: []string{"messages"}, Limit: 50,
	})
	require.NoError(t, err, "SearchContent fts")
	var found bool
	for _, m := range lexical.Matches {
		if m.SessionID == "side" && m.Ordinal == 1 {
			found = true
			assert.Equal(t, side.Subordinate, m.Subordinate, "Subordinate parity")
			assert.Equal(t, side.Sidechain, m.Sidechain, "Sidechain parity")
			assert.Equal(t, side.OrdinalRange, m.OrdinalRange, "OrdinalRange parity")
		}
	}
	require.True(t, found, "lexical fts must also match the sidechain anchor")
}

// TestSearchContentHybridUnitlessSidechainScopeFiltering pins that scope
// filtering sees the derived classification of unit-less hits: scope=top
// excludes the sidechain hit, scope=subordinate keeps only it.
func TestSearchContentHybridUnitlessSidechainScopeFiltering(t *testing.T) {
	d := testDB(t)
	if !d.HasFTS() {
		t.Skip("fts5 not available")
	}
	d.SetVectorSearcher(seedUnitlessSidechainFixture(t, d))

	top, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "zebra", Mode: "hybrid", Scope: "top", Limit: 50,
	})
	require.NoError(t, err, "SearchContent hybrid scope=top")
	require.Len(t, top.Matches, 1, "scope=top excludes the unit-less sidechain hit")
	assert.Equal(t, "plain", top.Matches[0].SessionID)

	sub, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "zebra", Mode: "hybrid", Scope: "subordinate", Limit: 50,
	})
	require.NoError(t, err, "SearchContent hybrid scope=subordinate")
	require.Len(t, sub.Matches, 1, "scope=subordinate keeps only the sidechain hit")
	assert.Equal(t, "side", sub.Matches[0].SessionID)
	assert.True(t, sub.Matches[0].Subordinate)
}

// TestSearchContentHybridFTSLegSubordinateUnitPenalized pins the FTS-side
// subordinate flag: an FTS hit resolving to a subordinate unit is penalized
// in the merge, while a unit-less top-level FTS hit is not — so the
// lower-FTS-ranked top-level message overtakes the subordinate unit.
func TestSearchContentHybridFTSLegSubordinateUnitPenalized(t *testing.T) {
	d := testDB(t)
	if !d.HasFTS() {
		t.Skip("fts5 not available")
	}
	// "subd" repeats the term so bm25 ranks it above "plain" in the FTS leg.
	seedSearchSession(t, d, "subd", "proj", [][2]string{
		{"assistant", "zebra zebra zebra zebra"},
	})
	seedSearchSession(t, d, "plain", "proj", [][2]string{
		{"user", "zebra appears once here"},
	})
	// The vector leg is empty; the subordinate unit is known only to the
	// resolver.
	d.SetVectorSearcher(&fakeVectorSearcher{units: []UnitRef{
		{DocKey: "r:subd:0", SessionID: "subd",
			OrdinalStart: 0, OrdinalEnd: 0, Subordinate: true},
	}})

	page, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "zebra", Mode: "hybrid", Limit: 50,
	})
	require.NoError(t, err, "SearchContent hybrid")
	require.Len(t, page.Matches, 2)
	assert.Equal(t, "plain", page.Matches[0].SessionID,
		"unpenalized message-granularity hit must overtake the subordinate unit")
	assert.Equal(t, "subd", page.Matches[1].SessionID)
}

// TestSearchContentHybridMatchCarriesUnitRangeAndLineage pins the hybrid
// surface for run-grouped units: a fused FTS-in-run hit exposes the
// containing unit's ordinal range and subordinate flag (from the resolved
// UnitRef) plus the anchor's lineage, while Ordinal stays the FTS-overridden
// anchor ordinal.
func TestSearchContentHybridMatchCarriesUnitRangeAndLineage(t *testing.T) {
	d := testDB(t)
	if !d.HasFTS() {
		t.Skip("fts5 not available")
	}
	insertSession(t, d, "top", "proj", func(s *Session) {
		s.UserMessageCount = 2
	})
	insertSession(t, d, "child", "proj", func(s *Session) {
		s.UserMessageCount = 2
		s.ParentSessionID = Ptr("top")
		s.RelationshipType = "subagent"
	})
	require.NoError(t, d.ReplaceSessionMessages("child", []Message{
		{SessionID: "child", Ordinal: 0, Role: "user",
			Content: "the question", Timestamp: "2026-05-20T12:00:00Z"},
		{SessionID: "child", Ordinal: 1, Role: "assistant", IsSidechain: true,
			Content: "first step of the answer", Timestamp: "2026-05-20T12:00:01Z"},
		{SessionID: "child", Ordinal: 2, Role: "assistant", IsSidechain: true,
			Content: "second step mentions zebra", Timestamp: "2026-05-20T12:00:02Z"},
	}))
	// The run [1,2] anchors its semantic hit at ordinal 1; FTS matches
	// ordinal 2 and overrides the anchor.
	d.SetVectorSearcher(&fakeVectorSearcher{hits: []VectorHit{
		{SessionID: "child", Ordinal: 1, OrdinalStart: 1, OrdinalEnd: 2,
			Subordinate: true, Score: 0.9, Snippet: "first step of the answer"},
	}})

	page, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "zebra", Mode: "hybrid", Limit: 50,
	})
	require.NoError(t, err, "SearchContent hybrid")
	require.Len(t, page.Matches, 1, "the two legs must fuse into one result")
	m := page.Matches[0]
	assert.Equal(t, 2, m.Ordinal, "Ordinal stays the FTS-overridden anchor")
	assert.Equal(t, [2]int{1, 2}, m.OrdinalRange, "OrdinalRange spans the containing unit")
	assert.True(t, m.Subordinate, "Subordinate carries the unit flag")
	assert.Equal(t, "subagent", m.Relationship)
	assert.Equal(t, "top", m.ParentSessionID)
	assert.True(t, m.Sidechain, "anchor message is_sidechain")
}

// TestSearchContentHybridFTSLegCollapseRefillsFromDeeperRanks pins the
// batched FTS leg against unit collapse: when more than k (the
// SemanticOverfetchMin=200 fusion depth) rank-ordered FTS rows all fall
// inside ONE run-unit, they collapse to a single leg entry, and a match in a
// different unit ranked below all of them must still be fetched and returned
// rather than being cut off by the first batch's window.
func TestSearchContentHybridFTSLegCollapseRefillsFromDeeperRanks(t *testing.T) {
	d := testDB(t)
	if !d.HasFTS() {
		t.Skip("fts5 not available")
	}
	// One run-unit spanning 205 assistant messages, each repeating the term
	// so bm25 ranks every one of them above the single-occurrence session.
	const runLen = 205
	runMsgs := make([][2]string, runLen)
	for i := range runMsgs {
		runMsgs[i] = [2]string{"assistant", "zebra zebra zebra zebra"}
	}
	seedSearchSession(t, d, "bigrun", "proj", runMsgs)
	seedSearchSession(t, d, "other", "proj", [][2]string{
		{"user", "zebra appears once in a much longer unrelated sentence"},
	})
	// The vector leg is empty; the resolver knows the whole run as one unit.
	d.SetVectorSearcher(&fakeVectorSearcher{units: []UnitRef{
		{DocKey: "r:bigrun:0", SessionID: "bigrun",
			OrdinalStart: 0, OrdinalEnd: runLen - 1},
	}})

	page, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "zebra", Mode: "hybrid", Limit: 10,
	})
	require.NoError(t, err, "SearchContent hybrid")
	require.Len(t, page.Matches, 2,
		"the lower-ranked unit past the collapsed run must be fetched")
	ids := []string{page.Matches[0].SessionID, page.Matches[1].SessionID}
	assert.ElementsMatch(t, []string{"bigrun", "other"}, ids)
}

// TestSearchContentHybridFTSLegScopeExcludedRowsRefill pins the batched FTS
// leg against scope discard: with scope=subordinate, when the first k FTS
// rows are all top-level (and so all dropped), a subordinate match ranked
// below them must still be fetched and returned.
func TestSearchContentHybridFTSLegScopeExcludedRowsRefill(t *testing.T) {
	d := testDB(t)
	if !d.HasFTS() {
		t.Skip("fts5 not available")
	}
	// 205 top-level sessions would be slow; one top-level session with 205
	// matching messages fills the first batch the same way, since each
	// message is its own unit-less row classified top-level (non-sidechain,
	// no subordinate lineage).
	const topLen = 205
	topMsgs := make([][2]string, topLen)
	for i := range topMsgs {
		topMsgs[i] = [2]string{"user", "zebra zebra zebra zebra"}
	}
	seedSearchSession(t, d, "toplots", "proj", topMsgs)
	seedSubagentSession(t, d, "sub", "toplots", "proj", [][2]string{
		{"user", "zebra appears once in a much longer subagent sentence"},
	})
	d.SetVectorSearcher(&fakeVectorSearcher{units: []UnitRef{
		{DocKey: "u:sub:0", SessionID: "sub",
			OrdinalStart: 0, OrdinalEnd: 0, Subordinate: true},
	}})

	page, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "zebra", Mode: "hybrid", Scope: "subordinate", Limit: 10,
	})
	require.NoError(t, err, "SearchContent hybrid")
	require.Len(t, page.Matches, 1,
		"the subordinate match past the excluded top-level rows must be fetched")
	assert.Equal(t, "sub", page.Matches[0].SessionID)
}

// TestSearchContentHybridVectorOnlyMatchCarriesUnitRange pins the
// vector-leg display path: a unit only the semantic leg found keeps its
// chunk anchor and still exposes the unit range and subordinate flag.
func TestSearchContentHybridVectorOnlyMatchCarriesUnitRange(t *testing.T) {
	d := testDB(t)
	if !d.HasFTS() {
		t.Skip("fts5 not available")
	}
	seedSearchSession(t, d, "s1", "proj", [][2]string{
		{"user", "the question"},
		{"assistant", "first step of the answer"},
		{"assistant", "second step of the answer"},
	})
	d.SetVectorSearcher(&fakeVectorSearcher{hits: []VectorHit{
		{SessionID: "s1", Ordinal: 2, OrdinalStart: 1, OrdinalEnd: 2,
			Score: 0.9, Snippet: "second step of the answer"},
	}})

	page, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "nomatchinfts", Mode: "hybrid", Limit: 50,
	})
	require.NoError(t, err, "SearchContent hybrid")
	require.Len(t, page.Matches, 1, "vector-only hit survives fusion")
	m := page.Matches[0]
	assert.Equal(t, 2, m.Ordinal, "vector leg keeps its chunk anchor")
	assert.Equal(t, [2]int{1, 2}, m.OrdinalRange)
	assert.False(t, m.Subordinate)
}
