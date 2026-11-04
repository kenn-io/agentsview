package server_test

import (
	"context"
	"encoding/json/v2"
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

func (f fakeHitsVectorSearcher) PrepareSemanticSearch(
	context.Context, string,
) (db.VectorSearchPage, int, error) {
	return func(context.Context, int) ([]db.VectorHit, bool, error) {
		return f.hits, true, nil
	}, 0, nil
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

func TestSearchContentTermsAndExactFiltersThroughHTTP(t *testing.T) {
	te := setup(t)
	for _, id := range []string{"target-session", "other-session"} {
		te.seedSession(t, id, "proj", 2, func(s *db.Session) {
			s.GitBranch = "feature/memory"
		})
		te.seedMessages(t, id, 1, func(_ int, m *db.Message) {
			m.Role = "user"
			m.Content = "alpha beta"
		})
	}

	w := te.get(t, "/api/v1/search/content?pattern=alpha+beta&mode=terms&scope=all"+
		"&session_id=target-session&git_branch_exact=feature%2Fmemory&include_one_shot=true")
	assertStatus(t, w, http.StatusOK)
	res := decode[service.ContentSearchResult](t, w)
	require.Len(t, res.Matches, 1)
	assert.Equal(t, "target-session", res.Matches[0].SessionID)
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
		{
			SessionID: "sub-sess", Ordinal: 0, Subordinate: true, Score: 0.9,
			Snippet: "zebra inside the subagent",
		},
		{
			SessionID: "top-sess", Ordinal: 0, Score: 0.5,
			Snippet: "zebra at top level",
		},
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
		{
			SessionID: "sub-sess", Ordinal: 1, OrdinalStart: 1, OrdinalEnd: 2,
			Subordinate: true, Score: 0.9, Snippet: "zebra step inside the subagent",
		},
		{
			SessionID: "top-sess", Ordinal: 0, Score: 0.5,
			Snippet: "zebra at top level",
		},
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

// queryHitsVectorSearcher returns canned semantic hits per query text, so a
// multi-concept search sees different evidence for each concept.
type queryHitsVectorSearcher map[string][]db.VectorHit

func (f queryHitsVectorSearcher) PrepareSemanticSearch(
	_ context.Context, query string,
) (db.VectorSearchPage, int, error) {
	return func(context.Context, int) ([]db.VectorHit, bool, error) {
		return f[query], true, nil
	}, 0, nil
}

func (f queryHitsVectorSearcher) ResolveMessageUnits(
	_ context.Context, refs []db.MessageRef,
) ([]db.UnitRef, error) {
	return make([]db.UnitRef, len(refs)), nil
}

func TestSearchContentConceptsIntersectDifferentExchangesThroughHTTP(t *testing.T) {
	te := setup(t)
	for _, id := range []string{"both", "auth-only"} {
		te.seedSession(t, id, "proj", 4)
		te.seedMessages(t, id, 4, func(i int, m *db.Message) {
			m.Content = "message"
			if i%2 == 1 {
				m.Role = "assistant"
			}
		})
	}
	te.db.SetVectorSearcher(queryHitsVectorSearcher{
		"auth model": {
			{SessionID: "auth-only", Ordinal: 0, OrdinalStart: 0, OrdinalEnd: 1, Score: 0.95},
			{SessionID: "both", Ordinal: 0, OrdinalStart: 0, OrdinalEnd: 1, Score: 0.7},
		},
		"retry policy": {
			{SessionID: "both", Ordinal: 2, OrdinalStart: 2, OrdinalEnd: 3, Score: 0.9},
		},
	})

	w := te.wrappedRequest(http.MethodGet,
		"/api/v1/search/content?concepts=auth+model&concepts=retry+policy&scope=top&limit=10",
		withHeader("X-AgentsView-Search-Intent", "semantic"))
	assertStatus(t, w, http.StatusOK)
	res := decode[service.ContentSearchResult](t, w)
	require.Len(t, res.Matches, 1)
	assert.Equal(t, "both", res.Matches[0].SessionID)
	require.NotNil(t, res.Matches[0].Score)
	assert.InDelta(t, 0.8, *res.Matches[0].Score, 0.0001)
	require.Len(t, res.Matches[0].ConceptEvidence, 2)
	assert.Equal(t, "auth model", res.Matches[0].ConceptEvidence[0].Concept)
	assert.Equal(t, [2]int{0, 1}, res.Matches[0].ConceptEvidence[0].OrdinalRange)
	assert.Equal(t, "retry policy", res.Matches[0].ConceptEvidence[1].Concept)
	assert.Equal(t, [2]int{2, 3}, res.Matches[0].ConceptEvidence[1].OrdinalRange)
}

func TestSearchContentConceptsRejectedRequestsThroughHTTP(t *testing.T) {
	te := setup(t)
	intent := withHeader("X-AgentsView-Search-Intent", "semantic")
	for _, tc := range []struct {
		name   string
		query  string
		opts   []requestOpt
		status int
		want   string
	}{
		{"neither", "", []requestOpt{intent}, http.StatusBadRequest, "pattern or concepts required"},
		{"both", "pattern=x&concepts=a&concepts=b", []requestOpt{intent}, http.StatusBadRequest, "mutually exclusive"},
		{"one concept", "concepts=a", []requestOpt{intent}, http.StatusBadRequest, "between 2 and 5"},
		{"lexical mode", "concepts=a&concepts=b&mode=substring", []requestOpt{intent}, http.StatusBadRequest, "mode must be semantic"},
		{"no intent header", "concepts=a&concepts=b", nil, http.StatusForbidden, "X-AgentsView-Search-Intent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := te.wrappedRequest(http.MethodGet, "/api/v1/search/content?"+tc.query, tc.opts...)
			assertStatus(t, w, tc.status)
			assert.Contains(t, w.Body.String(), tc.want)
		})
	}
}
