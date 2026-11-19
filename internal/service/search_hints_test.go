package service_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

func TestSearchContentHintsBoundedCounts(t *testing.T) {
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
	req := service.ContentSearchRequest{Pattern: "marker"}
	res, err := svc.SearchContent(t.Context(), req)
	require.NoError(t, err)
	hints := service.SearchContentHints(t.Context(), svc, req, res, true, nil)
	require.Len(t, hints, 1)
	// The page shared with automated sessions is wider than the caller's 50.
	assert.Equal(t, "53 matches are in one-shot sessions, hidden by default", hints[0].Message)
	req.IncludeAutomated = true
	res, err = svc.SearchContent(t.Context(), req)
	require.NoError(t, err)
	hints = service.SearchContentHints(t.Context(), svc, req, res, false, nil)
	require.Len(t, hints, 1)
	assert.Equal(t, "more than 50 matches are in one-shot sessions, hidden by default", hints[0].Message)
	req.IncludeAutomated = false
	req.Mode = "semantic"
	res, err = svc.SearchContent(t.Context(), req)
	require.NoError(t, err)
	require.Empty(t, res.Matches)
	hints = service.SearchContentHints(t.Context(), svc, req, res, true, nil)
	require.Len(t, hints, 1)
	assert.Equal(t, "at least 50 matches are in one-shot sessions, hidden by default", hints[0].Message)
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
	assert.Equal(t, "excluded_children", hints[1].Code)
	assert.Equal(t, "1 match is in child sessions, hidden by default", hints[1].Message)
}

func TestSearchContentHintsCombinedExclusions(t *testing.T) {
	for _, tc := range []struct {
		name                                                   string
		automated, oneShotParent, mixed, includeChildren, root bool
	}{
		{"child of one-shot parent", false, true, false, false, false},
		{"one-shot and automated child", true, false, true, false, false},
		{"included child with one-shot parent and automation", true, true, false, true, false},
		{"automated root", true, false, false, false, true},
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
				if !tc.root {
					s.ParentSessionID = new("parent")
					s.RelationshipType = "subagent"
				}
				s.UserMessageCount = 1
				s.IsAutomated = tc.automated
			})
			require.NoError(t, d.InsertMessages(t.Context(), []db.Message{dbtest.UserMsg("child", 0, "marker")}))
			if tc.mixed {
				dbtest.SeedSession(t, d, "one", "proj", func(s *db.Session) { s.UserMessageCount = 1 })
				require.NoError(t, d.InsertMessages(t.Context(), []db.Message{dbtest.UserMsg("one", 0, "marker")}))
			}
			svc := service.NewDirectBackend(d, nil)
			req := service.ContentSearchRequest{Pattern: "marker", IncludeChildren: tc.includeChildren}
			res, err := svc.SearchContent(t.Context(), req)
			require.NoError(t, err)
			require.Empty(t, res.Matches)
			hints := service.SearchContentHints(t.Context(), svc, req, res, true, nil)
			if tc.mixed {
				require.Len(t, hints, 2)
				assert.Equal(t, "excluded_one_shot", hints[0].Code)
				assert.Equal(t, "1 match is in one-shot sessions, hidden by default", hints[0].Message)
			} else {
				require.Len(t, hints, 1)
			}
			hint := hints[len(hints)-1]
			wantRetry := map[string]any{"include_one_shot": true, "include_automated": true}
			if !tc.includeChildren {
				wantRetry["include_children"] = true
			}
			if tc.root {
				assert.Equal(t, "excluded_automated", hint.Code)
				assert.Equal(t, "1 match is in automated sessions, hidden by default", hint.Message)
				wantRetry = map[string]any{"include_automated": true}
			} else {
				assert.Equal(t, "excluded_combined", hint.Code)
				assert.Equal(t, "1 match is hidden by default", hint.Message)
			}
			assert.Equal(t, wantRetry, hint.RetryWith)
			children, _ := hint.RetryWith["include_children"].(bool)
			req.IncludeChildren = tc.includeChildren || children
			req.IncludeOneShot, _ = hint.RetryWith["include_one_shot"].(bool)
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
	dbtest.SeedSession(t, d, "noise", "other", func(s *db.Session) { s.MessageCount, s.UserMessageCount = 200, 200 })
	dbtest.SeedSession(t, d, "hidden", "selected", func(s *db.Session) { s.UserMessageCount = 1 })
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{dbtest.UserMsg("hidden", 0, "marker")}))
	messages := make([]db.Message, 0, 200)
	hits := make([]db.VectorHit, 200, 201)
	for i := range hits {
		messages = append(messages, dbtest.UserMsg("noise", i, "marker"))
		hits[i] = db.VectorHit{SessionID: "noise", Ordinal: i, OrdinalStart: i, OrdinalEnd: i, Score: 0.9}
	}
	require.NoError(t, d.InsertMessages(t.Context(), messages))
	hits = append(hits, db.VectorHit{SessionID: "hidden", Ordinal: 0, OrdinalStart: 0, OrdinalEnd: 0, Score: 0.5})
	d.SetVectorSearcher(&conceptVectorSearcher{hits: map[string][]db.VectorHit{"marker": hits}, ks: map[string][]int{}})
	svc := service.NewDirectBackend(d, nil)
	req := service.ContentSearchRequest{Pattern: "marker", Mode: "semantic", Project: "selected", Limit: 100}
	res, err := svc.SearchContent(t.Context(), req)
	require.NoError(t, err)
	require.Empty(t, res.Matches)
	hints := service.SearchContentHints(t.Context(), svc, req, res, true, nil)
	require.Len(t, hints, 1)
	assert.Equal(t, "excluded_one_shot", hints[0].Code)
	req.IncludeOneShot = true
	retried, err := svc.SearchContent(t.Context(), req)
	require.NoError(t, err)
	require.Len(t, retried.Matches, 1)
	assert.Equal(t, "hidden", retried.Matches[0].SessionID)
}

// The service boundary records the diagnostic work required for a miss.
type hintProbeService struct {
	service.SessionService
	search func(service.ContentSearchRequest) (*service.ContentSearchResult, error)
}

func (s hintProbeService) SearchContent(_ context.Context, req service.ContentSearchRequest) (*service.ContentSearchResult, error) {
	return s.search(req)
}

func TestSearchContentHintsExpandedGate(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		page                 service.ContentSearchResult
		err                  error
		single               bool
		wantCalls, wantHints int
	}{
		{name: "complete miss", wantCalls: 1},
		{name: "candidate truncation", page: service.ContentSearchResult{CandidateTruncated: true}, wantCalls: 3, wantHints: 2},
		{name: "failed gate", err: errors.New("unavailable"), wantCalls: 3, wantHints: 2},
		{name: "one disabled flag", single: true, wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			svc := hintProbeService{search: func(req service.ContentSearchRequest) (*service.ContentSearchResult, error) {
				calls++
				assert.Equal(t, "selected", req.Project)
				if calls == 1 {
					// A shared lexical page is widened; a vector page and the
					// one-class searches keep the caller's limit.
					wantLimit := 10
					if !tc.single && !tc.page.CandidateTruncated {
						wantLimit = 500
					}
					assert.Equal(t, wantLimit, req.Limit)
					assert.True(t, req.IncludeOneShot && req.IncludeAutomated && req.IncludeChildren)
					return &tc.page, tc.err
				}
				assert.Equal(t, 10, req.Limit)
				return &service.ContentSearchResult{Matches: []db.ContentMatch{{SessionID: "hidden"}}}, nil
			}}
			req := service.ContentSearchRequest{Pattern: "marker", Project: "selected", Limit: 10, IncludeChildren: true, IncludeAutomated: tc.single}
			if tc.page.CandidateTruncated {
				req.Pattern, req.Concepts = "", []string{"error handling", "session history"}
			}
			hints := service.SearchContentHints(t.Context(), svc, req, &service.ContentSearchResult{}, true, nil)
			assert.Equal(t, tc.wantCalls, calls)
			require.Len(t, hints, tc.wantHints)
			if tc.wantHints > 0 {
				assert.Equal(t, "excluded_one_shot", hints[0].Code)
			}
		})
	}
}

func TestSearchContentHintsContinuationOutsideParentProject(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	dbtest.SeedSession(t, d, "parent", "other", func(s *db.Session) { s.UserMessageCount = 2 })
	dbtest.SeedSession(t, d, "continuation", "selected", func(s *db.Session) {
		s.ParentSessionID = new("parent")
		s.RelationshipType = "continuation"
		s.UserMessageCount = 1
	})
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{dbtest.UserMsg("continuation", 0, "marker")}))
	svc := service.NewDirectBackend(d, nil)
	req := service.ContentSearchRequest{Pattern: "marker", Project: "selected", Limit: 10}
	res, err := svc.SearchContent(t.Context(), req)
	require.NoError(t, err)
	require.Empty(t, res.Matches)
	hints := service.SearchContentHints(t.Context(), svc, req, res, true, nil)
	require.Len(t, hints, 1)
	assert.Equal(t, "excluded_one_shot", hints[0].Code)
	req.IncludeOneShot = hints[0].RetryWith["include_one_shot"].(bool)
	retried, err := svc.SearchContent(t.Context(), req)
	require.NoError(t, err)
	require.Len(t, retried.Matches, 1)
	assert.Equal(t, "continuation", retried.Matches[0].SessionID)
}

func TestSearchContentHintsFilteredResultGuards(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cursor int
		page   service.ContentSearchResult
	}{
		{name: "request cursor", cursor: 1},
		{name: "continuation", page: service.ContentSearchResult{NextCursor: 1}},
		{name: "truncated", page: service.ContentSearchResult{CandidateTruncated: true}},
		{name: "backend filtered", page: service.ContentSearchResult{ActiveFilterApplied: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.page.Matches = []db.ContentMatch{{SessionID: "active"}}
			calls := 0
			svc := hintProbeService{search: func(service.ContentSearchRequest) (*service.ContentSearchResult, error) {
				calls++
				return &service.ContentSearchResult{}, nil
			}}
			req := service.ContentSearchRequest{Pattern: "marker", Limit: 1, Cursor: tc.cursor}
			if tc.page.CandidateTruncated {
				req.Pattern, req.Concepts = "", []string{"error handling", "session history"}
			}
			assert.Empty(t, service.SearchContentHints(t.Context(), svc, req, &tc.page, false, func(db.ContentMatch) bool { return false }))
			assert.Zero(t, calls)
		})
	}
}

// countingService records how many searches and session lookups the hints make.
type countingService struct {
	service.SessionService
	searches, lists int
}

func (s *countingService) SearchContent(ctx context.Context, req service.ContentSearchRequest) (*service.ContentSearchResult, error) {
	s.searches++
	return s.SessionService.SearchContent(ctx, req)
}

func (s *countingService) List(ctx context.Context, f service.ListFilter) (*service.SessionList, error) {
	s.lists++
	return s.SessionService.List(ctx, f)
}

func TestSearchContentHintsSortOneExpandedPage(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	dbtest.SeedSession(t, d, "one", "proj", func(s *db.Session) { s.UserMessageCount = 1 })
	dbtest.SeedSession(t, d, "auto", "proj", func(s *db.Session) { s.UserMessageCount, s.IsAutomated = 2, true })
	dbtest.SeedSession(t, d, "parent", "proj", func(s *db.Session) { s.UserMessageCount = 2 })
	dbtest.SeedSession(t, d, "kid", "proj", func(s *db.Session) {
		s.ParentSessionID, s.RelationshipType, s.UserMessageCount = new("parent"), "subagent", 1
	})
	dbtest.SeedSession(t, d, "auto-kid", "proj", func(s *db.Session) {
		s.ParentSessionID, s.RelationshipType, s.UserMessageCount, s.IsAutomated = new("parent"), "subagent", 1, true
	})
	for _, id := range []string{"one", "auto", "kid", "auto-kid"} {
		require.NoError(t, d.InsertMessages(t.Context(), []db.Message{dbtest.UserMsg(id, 0, "marker")}))
	}
	svc := &countingService{SessionService: service.NewDirectBackend(d, nil)}
	req := service.ContentSearchRequest{Pattern: "marker"}
	res, err := svc.SearchContent(t.Context(), req)
	require.NoError(t, err)
	require.Empty(t, res.Matches)
	svc.searches = 0
	hints := service.SearchContentHints(t.Context(), svc, req, res, true, nil)
	assert.Equal(t, 2, svc.searches, "one flat and one tree search")
	assert.Equal(t, []service.ContentSearchHint{
		{Code: "excluded_one_shot", Message: "1 match is in one-shot sessions, hidden by default", RetryWith: map[string]any{"include_one_shot": true}},
		{Code: "excluded_automated", Message: "1 match is in automated sessions, hidden by default", RetryWith: map[string]any{"include_automated": true}},
		{Code: "excluded_children", Message: "1 match is in child sessions, hidden by default", RetryWith: map[string]any{"include_children": true}},
		{Code: "excluded_combined", Message: "1 match is hidden by default", RetryWith: map[string]any{"include_one_shot": true, "include_automated": true, "include_children": true}},
	}, hints)

	empty := &countingService{SessionService: service.NewDirectBackend(dbtest.OpenTestDB(t), nil)}
	assert.Empty(t, service.SearchContentHints(t.Context(), empty, req, &service.ContentSearchResult{}, true, nil))
	assert.Equal(t, 2, empty.searches, "a miss everywhere costs two searches")
	assert.Zero(t, empty.lists)
}

func TestSearchContentHintsDeepChildChain(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	dbtest.SeedSession(t, d, "root", "proj", func(s *db.Session) { s.UserMessageCount = 1 })
	parent := "root"
	for i := range 10 {
		id := fmt.Sprintf("level-%d", i)
		dbtest.SeedSession(t, d, id, "proj", func(s *db.Session) {
			s.ParentSessionID, s.RelationshipType, s.UserMessageCount = new(parent), "subagent", 1
			s.IsAutomated = i == 9
		})
		parent = id
	}
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{dbtest.UserMsg("level-9", 0, "marker")}))
	svc := service.NewDirectBackend(d, nil)
	req := service.ContentSearchRequest{Pattern: "marker"}
	res, err := svc.SearchContent(t.Context(), req)
	require.NoError(t, err)
	require.Empty(t, res.Matches)
	// Only the combined retry shows an automated child eleven sessions below
	// a one-shot root.
	hints := service.SearchContentHints(t.Context(), svc, req, res, true, nil)
	assert.Equal(t, []service.ContentSearchHint{
		{Code: "excluded_combined", Message: "1 match is hidden by default", RetryWith: map[string]any{"include_one_shot": true, "include_automated": true, "include_children": true}},
	}, hints)
}

func TestSearchContentHintsFullSemanticPage(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	hits := make([]db.VectorHit, 0, 51)
	for i := range 50 {
		id := fmt.Sprintf("one-shot-%d", i)
		dbtest.SeedSession(t, d, id, "proj", func(s *db.Session) { s.UserMessageCount = 1 })
		require.NoError(t, d.InsertMessages(t.Context(), []db.Message{dbtest.UserMsg(id, 0, "marker")}))
		hits = append(hits, db.VectorHit{SessionID: id, Ordinal: 0, OrdinalStart: 0, OrdinalEnd: 0, Score: 0.9})
	}
	dbtest.SeedSession(t, d, "auto", "proj", func(s *db.Session) { s.UserMessageCount, s.IsAutomated = 2, true })
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{dbtest.UserMsg("auto", 0, "marker")}))
	hits = append(hits, db.VectorHit{SessionID: "auto", Ordinal: 0, OrdinalStart: 0, OrdinalEnd: 0, Score: 0.5})
	d.SetVectorSearcher(&conceptVectorSearcher{hits: map[string][]db.VectorHit{"marker": hits}, ks: map[string][]int{}})
	svc := service.NewDirectBackend(d, nil)
	req := service.ContentSearchRequest{Pattern: "marker", Mode: "semantic"}
	res, err := svc.SearchContent(t.Context(), req)
	require.NoError(t, err)
	require.Empty(t, res.Matches)
	// One-shot matches fill the shared semantic page, so the automated
	// session, which scores lower, needs its own search.
	hints := service.SearchContentHints(t.Context(), svc, req, res, true, nil)
	assert.Equal(t, []service.ContentSearchHint{
		{Code: "excluded_one_shot", Message: "at least 50 matches are in one-shot sessions, hidden by default", RetryWith: map[string]any{"include_one_shot": true}},
		{Code: "excluded_automated", Message: "at least 1 match is in automated sessions, hidden by default", RetryWith: map[string]any{"include_automated": true}},
	}, hints)
}

// rowService answers searches from a fixed function and session lookups
// from fixed rows, the way a hosted read lists several parents.
type rowService struct {
	service.SessionService
	search          func(service.ContentSearchRequest) service.ContentSearchResult
	rows            map[string]db.Session
	searches, lists int
}

func (s *rowService) SearchContent(_ context.Context, req service.ContentSearchRequest) (*service.ContentSearchResult, error) {
	s.searches++
	page := s.search(req)
	return &page, nil
}

func (s *rowService) List(_ context.Context, f service.ListFilter) (*service.SessionList, error) {
	s.lists++
	var list service.SessionList
	for _, id := range f.IDs {
		if row, ok := s.rows[id]; ok {
			list.Sessions = append(list.Sessions, row)
		}
	}
	return &list, nil
}

// expandedOnly returns page for the shared search that shows one-shot and
// automated sessions, and nothing for any other retry.
func expandedOnly(page service.ContentSearchResult) func(service.ContentSearchRequest) service.ContentSearchResult {
	return func(req service.ContentSearchRequest) service.ContentSearchResult {
		if req.IncludeOneShot && req.IncludeAutomated {
			return page
		}
		return service.ContentSearchResult{}
	}
}

func TestSearchContentHintsEveryParent(t *testing.T) {
	kid := func(parents ...string) db.Session {
		return db.Session{ID: "kid", RelationshipType: "subagent", UserMessageCount: 2, ParentSessionIDs: parents}
	}
	roots := map[string]db.Session{
		"lonely": {ID: "lonely", UserMessageCount: 1},
		"quiet":  {ID: "quiet", UserMessageCount: 1},
		"busy":   {ID: "busy", UserMessageCount: 3},
	}
	page := service.ContentSearchResult{Matches: []db.ContentMatch{{SessionID: "kid"}}}
	req := service.ContentSearchRequest{Pattern: "marker", IncludeChildren: true}
	oneShot := service.ContentSearchHint{Code: "excluded_one_shot", Message: "1 match is in one-shot sessions, hidden by default", RetryWith: map[string]any{"include_one_shot": true}}

	// Both parents are one-shot roots, so only the one-shot retry reaches
	// the child, though the child itself has two user messages.
	rows := map[string]db.Session{"kid": kid("lonely", "quiet"), "lonely": roots["lonely"], "quiet": roots["quiet"]}
	svc := &rowService{search: expandedOnly(page), rows: rows}
	hints := service.SearchContentHints(t.Context(), svc, req, &service.ContentSearchResult{}, true, nil)
	assert.Equal(t, []service.ContentSearchHint{oneShot}, hints)
	assert.Equal(t, 1, svc.searches)

	// The parents disagree about the automated retry. The busy root may
	// fail a filter such as the project, so that retry runs on its own and
	// finds nothing here.
	rows = map[string]db.Session{"kid": kid("lonely", "busy"), "lonely": roots["lonely"], "busy": roots["busy"]}
	svc = &rowService{search: expandedOnly(page), rows: rows}
	hints = service.SearchContentHints(t.Context(), svc, req, &service.ContentSearchResult{}, true, nil)
	assert.Equal(t, []service.ContentSearchHint{oneShot}, hints)
	assert.Equal(t, 2, svc.searches)
}

func TestSearchContentHintsFlatPageSkipsAncestors(t *testing.T) {
	rows := map[string]db.Session{
		"cont": {ID: "cont", RelationshipType: "continuation", UserMessageCount: 1, ParentSessionIDs: []string{"p1"}},
		"p1":   {ID: "p1", RelationshipType: "continuation", UserMessageCount: 2, ParentSessionIDs: []string{"p2"}},
		"p2":   {ID: "p2", UserMessageCount: 2},
	}
	flat := service.ContentSearchResult{Matches: []db.ContentMatch{{SessionID: "cont", ParentSessionID: "p1"}}}
	svc := &rowService{rows: rows, search: func(req service.ContentSearchRequest) service.ContentSearchResult {
		if req.IncludeOneShot && req.IncludeAutomated && !req.IncludeChildren {
			return flat
		}
		return service.ContentSearchResult{}
	}}
	// The tree search finds nothing, as when the root belongs to another
	// project. The flat hint reads only the continuation's own row.
	hints := service.SearchContentHints(t.Context(), svc, service.ContentSearchRequest{Pattern: "marker"}, &service.ContentSearchResult{}, true, nil)
	assert.Equal(t, []service.ContentSearchHint{
		{Code: "excluded_one_shot", Message: "1 match is in one-shot sessions, hidden by default", RetryWith: map[string]any{"include_one_shot": true}},
	}, hints)
	assert.Equal(t, 1, svc.lists)
}

func TestSearchContentHintsMissingParentRow(t *testing.T) {
	page := service.ContentSearchResult{Matches: []db.ContentMatch{{SessionID: "kid"}}}
	svc := &rowService{search: expandedOnly(page), rows: map[string]db.Session{
		// The lookup skips the trashed parent, so its row never arrives.
		"kid": {ID: "kid", RelationshipType: "subagent", UserMessageCount: 1, IsAutomated: true, ParentSessionIDs: []string{"trashed"}},
	}}
	req := service.ContentSearchRequest{Pattern: "marker", IncludeChildren: true}
	hints := service.SearchContentHints(t.Context(), svc, req, &service.ContentSearchResult{}, true, nil)
	assert.Equal(t, []service.ContentSearchHint{
		{Code: "excluded_combined", Message: "at least 1 match is hidden by default", RetryWith: map[string]any{"include_one_shot": true, "include_automated": true}},
	}, hints)
}

func TestSearchContentHintsShortHybridPage(t *testing.T) {
	one := service.ContentSearchResult{Matches: []db.ContentMatch{{SessionID: "one"}}}
	auto := service.ContentSearchResult{Matches: []db.ContentMatch{{SessionID: "auto"}}}
	svc := &rowService{
		rows: map[string]db.Session{"one": {ID: "one", UserMessageCount: 1}, "auto": {ID: "auto", UserMessageCount: 2, IsAutomated: true}},
		search: func(req service.ContentSearchRequest) service.ContentSearchResult {
			switch {
			case req.IncludeOneShot && req.IncludeAutomated:
				return one
			case req.IncludeAutomated:
				return auto
			}
			return service.ContentSearchResult{}
		},
	}
	// The shared hybrid page is short but missed the automated match, as when
	// one-shot rows used up the keyword batches.
	req := service.ContentSearchRequest{Pattern: "marker", Mode: "hybrid"}
	hints := service.SearchContentHints(t.Context(), svc, req, &service.ContentSearchResult{}, true, nil)
	assert.Equal(t, []service.ContentSearchHint{
		{Code: "excluded_one_shot", Message: "at least 1 match is in one-shot sessions, hidden by default", RetryWith: map[string]any{"include_one_shot": true}},
		{Code: "excluded_automated", Message: "at least 1 match is in automated sessions, hidden by default", RetryWith: map[string]any{"include_automated": true}},
	}, hints)
	assert.Equal(t, 2, svc.searches)
}

func TestSearchContentHintsRejoiningParents(t *testing.T) {
	// Each level lists both sessions of the level above, so the number of
	// parent paths doubles per level while the session count grows by two.
	const levels = 40
	rows := map[string]db.Session{"root": {ID: "root", UserMessageCount: 1}}
	above := []string{"root"}
	for i := range levels {
		level := []string{fmt.Sprintf("a%d", i), fmt.Sprintf("b%d", i)}
		for _, id := range level {
			rows[id] = db.Session{ID: id, RelationshipType: "subagent", UserMessageCount: 2, ParentSessionIDs: above}
		}
		above = level
	}
	rows["kid"] = db.Session{ID: "kid", RelationshipType: "subagent", UserMessageCount: 2, ParentSessionIDs: above}
	page := service.ContentSearchResult{Matches: []db.ContentMatch{{SessionID: "kid"}}}
	svc := &rowService{search: expandedOnly(page), rows: rows}
	req := service.ContentSearchRequest{Pattern: "marker", IncludeChildren: true}
	done := make(chan []service.ContentSearchHint, 1)
	go func() {
		done <- service.SearchContentHints(t.Context(), svc, req, &service.ContentSearchResult{}, true, nil)
	}()
	select {
	case hints := <-done:
		assert.Equal(t, []service.ContentSearchHint{
			{Code: "excluded_one_shot", Message: "1 match is in one-shot sessions, hidden by default", RetryWith: map[string]any{"include_one_shot": true}},
		}, hints)
	case <-time.After(10 * time.Second):
		require.FailNow(t, "classifying one match walked every parent path")
	}
}
