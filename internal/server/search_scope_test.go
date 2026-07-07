package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

// fakeHitsVectorSearcher returns canned semantic hits. ResolveMessageUnits
// resolves against the units implied by the hits, mirroring the db-layer
// fake, so hybrid requests do not error.
type fakeHitsVectorSearcher struct{ hits []db.VectorHit }

func (f fakeHitsVectorSearcher) SemanticSearch(
	_ context.Context, _ string, _ int,
) ([]db.VectorHit, error) {
	return f.hits, nil
}

func (f fakeHitsVectorSearcher) ResolveMessageUnits(
	_ context.Context, refs []db.MessageRef,
) ([]db.UnitRef, error) {
	return make([]db.UnitRef, len(refs)), nil
}

// TestSearchContentScopeInvalidValueRejected pins the enum gate on the
// scope query param: an out-of-enum value is rejected up front (Huma 422
// remapped to 400), matching how mode is validated.
func TestSearchContentScopeInvalidValueRejected(t *testing.T) {
	te := setup(t)
	te.db.SetVectorSearcher(fakeTransientVectorSearcher{})

	w := te.wrappedRequest(http.MethodGet,
		"/api/v1/search/content?pattern=fox&mode=semantic&scope=bogus",
		withHeader("X-AgentsView-Search-Intent", "semantic"))
	assertStatus(t, w, http.StatusBadRequest)
	assert.Contains(t, w.Body.String(), "scope")
}

// TestSearchContentScopeRequiresSemanticOrHybridMode pins that scope is
// only meaningful for semantic/hybrid: setting it on any other mode is
// rejected rather than silently ignored.
func TestSearchContentScopeRequiresSemanticOrHybridMode(t *testing.T) {
	te := setup(t)

	for _, q := range []string{
		"pattern=fox&scope=top",
		"pattern=fox&mode=substring&scope=top",
		"pattern=fox&mode=regex&scope=all",
		"pattern=fox&mode=fts&scope=subordinate",
	} {
		w := te.get(t, "/api/v1/search/content?"+q)
		assertStatus(t, w, http.StatusBadRequest)
		assert.Contains(t, w.Body.String(), "semantic",
			"error should point at the semantic/hybrid-only restriction")
	}
}

// TestSearchContentScopeFiltersSemanticResults exercises the scope param
// end to end: scope=top drops the subordinate unit, scope=subordinate
// keeps only it, and the default returns both even though include_children
// is not set (precedence over the sidebar-child exclusion).
func TestSearchContentScopeFiltersSemanticResults(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "top-sess", "proj", 2)
	te.seedMessages(t, "top-sess", 1, func(_ int, m *db.Message) {
		m.Content = "zebra at top level"
	})
	te.seedSession(t, "sub-sess", "proj", 2, func(s *db.Session) {
		s.ParentSessionID = new("top-sess")
		s.RelationshipType = "subagent"
	})
	te.seedMessages(t, "sub-sess", 1, func(_ int, m *db.Message) {
		m.Content = "zebra inside the subagent"
	})
	te.db.SetVectorSearcher(fakeHitsVectorSearcher{hits: []db.VectorHit{
		{SessionID: "sub-sess", Ordinal: 0, Subordinate: true, Score: 0.9,
			Snippet: "zebra inside the subagent"},
		{SessionID: "top-sess", Ordinal: 0, Score: 0.5,
			Snippet: "zebra at top level"},
	}})

	search := func(t *testing.T, scope string) []string {
		t.Helper()
		path := "/api/v1/search/content?pattern=zebra&mode=semantic"
		if scope != "" {
			path += "&scope=" + scope
		}
		w := te.wrappedRequest(http.MethodGet, path,
			withHeader("X-AgentsView-Search-Intent", "semantic"))
		assertStatus(t, w, http.StatusOK)
		res := decode[service.ContentSearchResult](t, w)
		ids := make([]string, 0, len(res.Matches))
		for _, m := range res.Matches {
			ids = append(ids, m.SessionID)
		}
		return ids
	}

	def := search(t, "")
	require.Contains(t, def, "sub-sess",
		"default scope must return the subordinate unit despite include_children being unset")
	assert.Contains(t, def, "top-sess")

	assert.Equal(t, []string{"top-sess"}, search(t, "top"))
	assert.Equal(t, []string{"sub-sess"}, search(t, "subordinate"))
	assert.ElementsMatch(t, []string{"top-sess", "sub-sess"}, search(t, "all"))
}

// TestSearchContentSemanticResponseCarriesUnitRangeAndLineage pins the HTTP
// wire shape for run-grouped semantic hits: ordinal stays the anchor while
// ordinal_range, subordinate, and the lineage keys ride along. ordinal_range
// is always present, so the fixture's top-level single-message hit at
// ordinal 0 still serializes "ordinal_range":[0,0] even though its other
// zero-valued unit/lineage fields are omitted via omitempty.
func TestSearchContentSemanticResponseCarriesUnitRangeAndLineage(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "top-sess", "proj", 2)
	te.seedMessages(t, "top-sess", 1, func(_ int, m *db.Message) {
		m.Content = "zebra at top level"
	})
	te.seedSession(t, "sub-sess", "proj", 3, func(s *db.Session) {
		s.ParentSessionID = new("top-sess")
		s.RelationshipType = "subagent"
	})
	te.seedMessages(t, "sub-sess", 3, func(i int, m *db.Message) {
		if i > 0 {
			m.Role = "assistant"
			m.IsSidechain = true
			m.Content = "zebra step inside the subagent"
		}
	})
	te.db.SetVectorSearcher(fakeHitsVectorSearcher{hits: []db.VectorHit{
		{SessionID: "sub-sess", Ordinal: 1, OrdinalStart: 1, OrdinalEnd: 2,
			Subordinate: true, Score: 0.9, Snippet: "zebra step inside the subagent"},
		{SessionID: "top-sess", Ordinal: 0, Score: 0.5,
			Snippet: "zebra at top level"},
	}})

	w := te.wrappedRequest(http.MethodGet,
		"/api/v1/search/content?pattern=zebra&mode=semantic",
		withHeader("X-AgentsView-Search-Intent", "semantic"))
	assertStatus(t, w, http.StatusOK)

	var res struct {
		Matches []map[string]any `json:"matches"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &res))
	require.Len(t, res.Matches, 2)
	byID := map[string]map[string]any{}
	for _, m := range res.Matches {
		byID[m["session_id"].(string)] = m
	}

	sub, ok := byID["sub-sess"]
	require.True(t, ok, "subordinate run hit present")
	assert.EqualValues(t, 1, sub["ordinal"], "ordinal stays the anchor")
	assert.Equal(t, []any{float64(1), float64(2)}, sub["ordinal_range"])
	assert.Equal(t, true, sub["subordinate"])
	assert.Equal(t, "subagent", sub["relationship"])
	assert.Equal(t, "top-sess", sub["parent_session_id"])
	assert.Equal(t, true, sub["is_sidechain"])

	top, ok := byID["top-sess"]
	require.True(t, ok, "top-level hit present")
	assert.Equal(t, []any{float64(0), float64(0)}, top["ordinal_range"],
		"ordinal_range is always present, even for a zero-valued single-message hit")
	for _, key := range []string{
		"subordinate", "relationship", "parent_session_id", "is_sidechain",
	} {
		assert.NotContains(t, top, key,
			"zero-valued lineage keys must be omitted from the wire")
	}
}
