package service_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

func TestSearchContentHintsBoundedCounts(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	for i := range 53 {
		id := fmt.Sprintf("hidden-%d", i)
		dbtest.SeedSession(t, d, id, "proj", func(s *db.Session) { s.UserMessageCount = 1 })
		require.NoError(t, d.InsertMessages(t.Context(), []db.Message{dbtest.UserMsg(id, 0, "marker")}))
	}
	svc := service.NewDirectBackend(d, nil)
	req := service.ContentSearchRequest{Pattern: "marker"}
	res, err := svc.SearchContent(t.Context(), req)
	require.NoError(t, err)
	hints := service.SearchContentHints(t.Context(), svc, req, res, true, nil)
	require.Len(t, hints, 1)
	assert.Equal(t, "more than 50 matches are in one-shot sessions, hidden by default", hints[0].Message)
	req.IncludeOneShot = true
	res, err = svc.SearchContent(t.Context(), req)
	require.NoError(t, err)
	assert.Empty(t, service.SearchContentHints(t.Context(), svc, req, res, true, nil))
	req.IncludeOneShot = false
	req.Cursor = 1
	assert.Empty(t, service.SearchContentHints(t.Context(), svc, req, &service.ContentSearchResult{}, true, nil))
}

func TestSearchContentHintsChildParentFilters(t *testing.T) {
	for _, tc := range []struct {
		name, project, parentProject, parentDate string
		parentExists, wantHint                   bool
	}{
		{"parent project and date match", "selected", "selected", "2024-06-15T10:00:00Z", true, true},
		{"parent project excluded", "selected", "other", "2024-06-15T10:00:00Z", true, false},
		{"missing parent", "selected", "selected", "2024-06-15T10:00:00Z", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := dbtest.OpenTestDB(t)
			if tc.parentExists {
				dbtest.SeedSession(t, d, "parent", tc.parentProject, func(s *db.Session) {
					s.StartedAt = new(tc.parentDate)
					s.EndedAt = new(tc.parentDate)
					s.UserMessageCount = 2
				})
			}
			dbtest.SeedSession(t, d, "child", "child-project", func(s *db.Session) {
				s.ParentSessionID = new("parent")
				s.RelationshipType = "subagent"
				s.UserMessageCount = 1
				s.StartedAt = new("2020-01-01T10:00:00Z")
				s.EndedAt = new("2020-01-01T10:00:00Z")
			})
			require.NoError(t, d.InsertMessages(t.Context(), []db.Message{dbtest.UserMsg("child", 0, "marker")}))
			svc := service.NewDirectBackend(d, nil)
			req := service.ContentSearchRequest{Pattern: "marker", Project: tc.project, DateFrom: "2024-01-01"}
			res, err := svc.SearchContent(t.Context(), req)
			require.NoError(t, err)
			require.Empty(t, res.Matches)
			hints := service.SearchContentHints(t.Context(), svc, req, res, true, nil)
			if !tc.wantHint {
				assert.Empty(t, hints)
				return
			}
			require.Len(t, hints, 1)
			assert.Equal(t, map[string]any{"include_children": true}, hints[0].RetryWith)
			req.IncludeChildren = true
			retried, err := svc.SearchContent(t.Context(), req)
			require.NoError(t, err)
			require.Len(t, retried.Matches, 1)
			assert.Equal(t, "child", retried.Matches[0].SessionID)
		})
	}
}

func TestSearchContentHintsFTSChildCount(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	for i := range 53 {
		id := fmt.Sprintf("one-shot-%d", i)
		dbtest.SeedSession(t, d, id, "proj", func(s *db.Session) { s.UserMessageCount = 1 })
		require.NoError(t, d.InsertMessages(t.Context(), []db.Message{dbtest.UserMsg(id, 0, "marker")}))
	}
	dbtest.SeedSession(t, d, "parent", "proj", func(s *db.Session) { s.UserMessageCount = 2 })
	dbtest.SeedSession(t, d, "child", "proj", func(s *db.Session) {
		s.ParentSessionID = new("parent")
		s.RelationshipType = "subagent"
		s.UserMessageCount = 1
	})
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{dbtest.UserMsg("child", 0, "marker")}))
	svc := service.NewDirectBackend(d, nil)
	req := service.ContentSearchRequest{Pattern: "marker", Mode: "fts"}
	res, err := svc.SearchContent(t.Context(), req)
	require.NoError(t, err)
	require.Empty(t, res.Matches)
	hints := service.SearchContentHints(t.Context(), svc, req, res, true, nil)
	require.Len(t, hints, 2)
	assert.Equal(t, "hidden_children", hints[1].Code)
	assert.Equal(t, "1 match is in child sessions, hidden by default", hints[1].Message)
}

func TestSearchContentHintsVectorClasses(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	dbtest.SeedSession(t, d, "hidden", "proj", func(s *db.Session) { s.UserMessageCount = 1 })
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{dbtest.UserMsg("hidden", 0, "marker")}))
	d.SetVectorSearcher(&conceptVectorSearcher{hits: map[string][]db.VectorHit{"marker": {{SessionID: "hidden", Ordinal: 0, OrdinalStart: 0, OrdinalEnd: 0, Score: 0.9}}}, ks: map[string][]int{}})
	svc := service.NewDirectBackend(d, nil)
	req := service.ContentSearchRequest{Pattern: "marker", Mode: "semantic"}
	res, err := svc.SearchContent(t.Context(), req)
	require.NoError(t, err)
	require.Empty(t, res.Matches)
	hints := service.SearchContentHints(t.Context(), svc, req, res, false, nil)
	require.Len(t, hints, 1)
	assert.Equal(t, "hidden_one_shot", hints[0].Code)
}

func TestSearchContentHintsCombinedExclusions(t *testing.T) {
	for _, tc := range []struct {
		name                            string
		automated, oneShotParent, mixed bool
	}{
		{"automated child", true, false, false},
		{"child of one-shot parent", false, true, false},
		{"one-shot and automated child", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := dbtest.OpenTestDB(t)
			dbtest.SeedSession(t, d, "parent", "proj", func(s *db.Session) {
				s.UserMessageCount = 2
				if tc.oneShotParent {
					s.UserMessageCount = 1
				}
			})
			dbtest.SeedSession(t, d, "child", "proj", func(s *db.Session) {
				s.ParentSessionID = new("parent")
				s.RelationshipType = "subagent"
				s.UserMessageCount = 1
				s.IsAutomated = tc.automated
			})
			require.NoError(t, d.InsertMessages(t.Context(), []db.Message{dbtest.UserMsg("child", 0, "marker")}))
			if tc.mixed {
				dbtest.SeedSession(t, d, "one", "proj", func(s *db.Session) { s.UserMessageCount = 1 })
				require.NoError(t, d.InsertMessages(t.Context(), []db.Message{dbtest.UserMsg("one", 0, "marker")}))
			}
			svc := service.NewDirectBackend(d, nil)
			req := service.ContentSearchRequest{Pattern: "marker"}
			res, err := svc.SearchContent(t.Context(), req)
			require.NoError(t, err)
			require.Empty(t, res.Matches)
			hints := service.SearchContentHints(t.Context(), svc, req, res, true, nil)
			if tc.mixed {
				require.Len(t, hints, 2)
				assert.Equal(t, "hidden_one_shot", hints[0].Code)
				assert.Equal(t, "1 match is in one-shot sessions, hidden by default", hints[0].Message)
			} else {
				require.Len(t, hints, 1)
			}
			hint := hints[len(hints)-1]
			assert.Equal(t, "hidden_combined", hint.Code)
			assert.Equal(t, "1 match is in child sessions hidden by more than one default", hint.Message)
			assert.Equal(t, map[string]any{"include_one_shot": true, "include_automated": true, "include_children": true}, hint.RetryWith)
			req.IncludeChildren = hint.RetryWith["include_children"].(bool)
			req.IncludeOneShot = hint.RetryWith["include_one_shot"].(bool)
			req.IncludeAutomated = hint.RetryWith["include_automated"].(bool)
			retried, err := svc.SearchContent(t.Context(), req)
			require.NoError(t, err)
			ids := make([]string, 0, len(retried.Matches))
			for _, match := range retried.Matches {
				ids = append(ids, match.SessionID)
			}
			assert.Contains(t, ids, "child")
		})
	}
}

func TestSearchContentHintsPreserveSemanticCandidateWindow(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	seedServiceSearchSession(t, d, "noise", "other", "marker")
	dbtest.SeedSession(t, d, "hidden", "selected", func(s *db.Session) { s.UserMessageCount = 1 })
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{dbtest.UserMsg("hidden", 0, "marker")}))
	hits := make([]db.VectorHit, 200, 201)
	for i := range hits {
		hits[i] = db.VectorHit{SessionID: "noise", Ordinal: i, OrdinalStart: i, OrdinalEnd: i, Score: 0.9}
	}
	hits = append(hits, db.VectorHit{SessionID: "hidden", Ordinal: 0, OrdinalStart: 0, OrdinalEnd: 0, Score: 0.5})
	d.SetVectorSearcher(&conceptVectorSearcher{hits: map[string][]db.VectorHit{"marker": hits}, ks: map[string][]int{}})
	svc := service.NewDirectBackend(d, nil)
	req := service.ContentSearchRequest{Pattern: "marker", Mode: "semantic", Project: "selected", Limit: 100}
	res, err := svc.SearchContent(t.Context(), req)
	require.NoError(t, err)
	require.Empty(t, res.Matches)
	hints := service.SearchContentHints(t.Context(), svc, req, res, false, nil)
	require.Len(t, hints, 1)
	assert.Equal(t, "hidden_one_shot", hints[0].Code)
	req.IncludeOneShot = true
	retried, err := svc.SearchContent(t.Context(), req)
	require.NoError(t, err)
	require.Len(t, retried.Matches, 1)
	assert.Equal(t, "hidden", retried.Matches[0].SessionID)
}

func TestSearchContentHintsFullSemanticPageIsLowerBound(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	hits := make([]db.VectorHit, 0, 53)
	for i := range 53 {
		id := fmt.Sprintf("hidden-%d", i)
		dbtest.SeedSession(t, d, id, "proj", func(s *db.Session) { s.UserMessageCount = 1 })
		require.NoError(t, d.InsertMessages(t.Context(), []db.Message{dbtest.UserMsg(id, 0, "marker")}))
		hits = append(hits, db.VectorHit{SessionID: id, Ordinal: 0, OrdinalStart: 0, OrdinalEnd: 0, Score: 0.9})
	}
	d.SetVectorSearcher(&conceptVectorSearcher{hits: map[string][]db.VectorHit{"marker": hits}, ks: map[string][]int{}})
	svc := service.NewDirectBackend(d, nil)
	req := service.ContentSearchRequest{Pattern: "marker", Mode: "semantic"}
	res, err := svc.SearchContent(t.Context(), req)
	require.NoError(t, err)
	require.Empty(t, res.Matches)
	hints := service.SearchContentHints(t.Context(), svc, req, res, false, nil)
	require.Len(t, hints, 1)
	assert.Equal(t, "at least 50 matches are in one-shot sessions, hidden by default", hints[0].Message)
}
