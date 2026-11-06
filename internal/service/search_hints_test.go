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

func TestSearchContentHintsPreserveExplicitScope(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	dbtest.SeedSession(t, d, "parent", "proj")
	dbtest.SeedSession(t, d, "child", "proj", func(s *db.Session) {
		s.ParentSessionID = new("parent")
		s.RelationshipType = "subagent"
	})
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{dbtest.UserMsg("child", 0, "marker")}))
	svc := service.NewDirectBackend(d, nil)
	for _, scope := range []string{"", "top"} {
		t.Run(scope, func(t *testing.T) {
			req := service.ContentSearchRequest{Pattern: "marker", Mode: "substring", Scope: scope, IncludeOneShot: true, IncludeAutomated: true}
			res, err := svc.SearchContent(t.Context(), req)
			require.NoError(t, err)
			require.Empty(t, res.Matches)
			hints := service.SearchContentHints(t.Context(), svc, req, res, true, nil)
			if scope == "" {
				require.Len(t, hints, 1)
				assert.Equal(t, "hidden_children", hints[0].Code)
			} else {
				assert.Empty(t, hints)
			}
		})
	}
}

func TestSearchContentHintsChildParentFilters(t *testing.T) {
	for _, tc := range []struct {
		name, project, parentProject, parentDate string
		parentExists, wantHint                   bool
	}{
		{"parent project and date match", "selected", "selected", "2024-06-15T10:00:00Z", true, true},
		{"parent project excluded", "selected", "other", "2024-06-15T10:00:00Z", true, false},
		{"missing parent", "selected", "selected", "2024-06-15T10:00:00Z", false, false},
		{"one-shot parent excluded", "selected", "selected", "2024-06-15T10:00:00Z", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := dbtest.OpenTestDB(t)
			if tc.parentExists {
				dbtest.SeedSession(t, d, "parent", tc.parentProject, func(s *db.Session) {
					s.StartedAt = new(tc.parentDate)
					s.EndedAt = new(tc.parentDate)
					s.UserMessageCount = 2
					if tc.name == "one-shot parent excluded" {
						s.UserMessageCount = 1
					}
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
