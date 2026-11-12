package db

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// activityCutoff is ten minutes before the fixtures' 2024-06-15T12:00:00Z "now".
var activityCutoff = time.Date(2024, 6, 15, 11, 50, 0, 0, time.UTC)

// epochCutoff predates every fixture, so it excludes all sessions with known activity.
var epochCutoff = time.Unix(0, 0).UTC()

func TestSearchContentExcludeActiveSince(t *testing.T) {
	t.Run("fills page before pagination", func(t *testing.T) {
		d := testDB(t)
		for i := range 7 {
			id := fmt.Sprintf("session-%d", i)
			seedSearchSession(t, d, id, "project-a", [][2]string{{"user", "docker-compose.test.yml"}})
			ended := "2024-06-15T10:00:00Z"
			if i < 3 {
				ended = "2024-06-15T11:59:00Z"
			}
			_, err := d.getWriter().Exec(t.Context(), "UPDATE sessions SET ended_at = ? WHERE id = ?", ended, id)
			require.NoError(t, err)
		}
		filter := ContentSearchFilter{
			Pattern: "docker-compose.test.yml", Limit: 3, ExcludeActiveSince: activityCutoff,
		}
		page, err := d.SearchContent(t.Context(), filter)
		require.NoError(t, err)
		require.Len(t, page.Matches, 3)
		require.NotZero(t, page.NextCursor)
		ids := make([]string, 0, 4)
		for _, match := range page.Matches {
			ids = append(ids, match.SessionID)
		}
		filter.Cursor = page.NextCursor
		page, err = d.SearchContent(t.Context(), filter)
		require.NoError(t, err)
		require.Len(t, page.Matches, 1)
		assert.Zero(t, page.NextCursor)
		ids = append(ids, page.Matches[0].SessionID)
		assert.ElementsMatch(t, []string{"session-3", "session-4", "session-5", "session-6"}, ids)
	})
	d := testDB(t)
	fixtures := []struct {
		id, ended, started string
	}{
		{"active", "2024-06-15T11:59:00Z", "2024-06-15T10:00:00Z"},
		{"offset-active", "2024-06-15T07:59:00-04:00", ""},
		{"started-active", "", "2024-06-15T11:59:00Z"},
		{"boundary", "2024-06-15T07:50:00-04:00", ""},
		{"started-boundary", "", "2024-06-15T11:50:00Z"},
		{"created-active", "", ""},
		{"idle", "2024-06-15T10:00:00Z", ""},
		{"malformed", "invalid", "2024-06-15T11:59:00Z"},
		{"unknown", "", ""},
	}
	hits := make([]VectorHit, 0, len(fixtures))
	for _, f := range fixtures {
		seedSearchSession(t, d, f.id, "project-a", [][2]string{{"user", "needle"}})
		_, err := d.getWriter().Exec(t.Context(),
			"UPDATE sessions SET ended_at = ?, started_at = ?, created_at = ? WHERE id = ?",
			f.ended, f.started, "2024-06-15T11:59:00Z", f.id)
		require.NoError(t, err)
		if f.id == "unknown" {
			_, err = d.getWriter().Exec(t.Context(), "UPDATE sessions SET created_at = '' WHERE id = ?", f.id)
			require.NoError(t, err)
		}
		hits = append(hits, VectorHit{SessionID: f.id, Ordinal: 0, Score: 0.9, Snippet: "needle"})
	}
	d.SetVectorSearcher(&fakeVectorSearcher{hits: hits})
	for _, mode := range []string{"substring", "regex", "fts", "terms", "semantic", "hybrid"} {
		t.Run(mode, func(t *testing.T) {
			cutoffs := []time.Time{activityCutoff, epochCutoff}
			if mode == "substring" {
				cutoffs = append(cutoffs,
					activityCutoff.In(time.FixedZone("UTC+2", 2*60*60)),
					activityCutoff.In(time.FixedZone("UTC-5", -5*60*60)))
			}
			for _, cutoff := range cutoffs {
				t.Run(cutoff.Format(time.RFC3339), func(t *testing.T) {
					page, err := d.SearchContent(t.Context(), ContentSearchFilter{
						Pattern: "needle", Mode: mode, Limit: 6,
						ExcludeActiveSince: cutoff,
					})
					require.NoError(t, err)
					ids := make([]string, 0, len(page.Matches))
					for _, match := range page.Matches {
						ids = append(ids, match.SessionID)
					}
					want := []string{"boundary", "started-boundary", "idle", "malformed", "unknown"}
					if cutoff.Equal(epochCutoff) {
						want = []string{"malformed", "unknown"}
					}
					assert.ElementsMatch(t, want, ids)
				})
			}
		})
	}
}

func TestSearchContentExcludeActiveChildBeforeLimit(t *testing.T) {
	d := testDB(t)
	seedSearchSession(t, d, "parent", "project-a", [][2]string{{"user", "parent text"}})
	seedSearchSession(t, d, "child", "child-project", [][2]string{{"user", "needle"}})
	seedSearchSession(t, d, "independent", "project-a", [][2]string{{"user", "needle"}})
	_, err := d.getWriter().Exec(t.Context(), "UPDATE sessions SET parent_session_id = 'parent', relationship_type = 'subagent' WHERE id = 'child'")
	require.NoError(t, err)
	_, err = d.getWriter().Exec(t.Context(), "UPDATE messages SET timestamp = '2024-06-15T11:59:00Z' WHERE session_id = 'child'")
	require.NoError(t, err)
	_, err = d.getWriter().Exec(t.Context(), "UPDATE messages SET timestamp = '2024-06-15T10:00:00Z' WHERE session_id = 'independent'")
	require.NoError(t, err)
	d.SetVectorSearcher(&fakeVectorSearcher{hits: []VectorHit{
		{SessionID: "child", Ordinal: 0, Score: 0.9, Snippet: "needle"},
		{SessionID: "independent", Ordinal: 0, Score: 0.8, Snippet: "needle"},
	}})
	for _, tc := range []struct {
		name, parentEnd, childEnd, project string
	}{
		{"idle parent active child", "2024-06-15T10:00:00Z", "2024-06-15T11:59:00Z", "project-a"},
		{"idle parent active child without project", "2024-06-15T10:00:00Z", "2024-06-15T11:59:00Z", ""},
		{"active parent idle child", "2024-06-15T11:59:00Z", "2024-06-15T10:00:00Z", "project-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := d.getWriter().Exec(t.Context(), "UPDATE sessions SET ended_at = CASE id WHEN 'parent' THEN ? WHEN 'child' THEN ? ELSE '2024-06-15T10:00:00Z' END", tc.parentEnd, tc.childEnd)
			require.NoError(t, err)
			modes := []string{"substring", "regex", "fts"}
			if tc.project == "" {
				modes = []string{"semantic", "hybrid"}
			}
			for _, mode := range modes {
				t.Run(mode, func(t *testing.T) {
					page, err := d.SearchContent(t.Context(), ContentSearchFilter{
						Pattern: "needle", Mode: mode, Limit: 1, IncludeChildren: true,
						Project: tc.project, ExcludeActiveSince: activityCutoff,
					})
					require.NoError(t, err)
					require.Len(t, page.Matches, 1)
					if tc.name == "active parent idle child" {
						assert.Equal(t, "child", page.Matches[0].SessionID)
					} else {
						assert.Equal(t, "independent", page.Matches[0].SessionID)
					}
				})
			}
		})
	}
}

// activityVectorSearcher models chunk candidates rolling up into fewer document hits.
type activityVectorSearcher struct {
	fakeVectorSearcher
	limits       []int
	chunksPerHit int
}

func (s *activityVectorSearcher) PrepareSemanticSearch(context.Context, string) (VectorSearchPage, int, error) {
	return func(_ context.Context, k int) ([]VectorHit, bool, error) {
		s.limits = append(s.limits, k)
		hits, err := s.page(k / max(s.chunksPerHit, 1))
		return hits, len(s.hits)*max(s.chunksPerHit, 1) < k, err
	}, 0, nil
}

func TestSearchContentActivityCandidateExpansion(t *testing.T) {
	d := testDB(t)
	for _, fixture := range []struct {
		id, content string
		count       int
	}{
		{"active", "unrelated", 6400},
		{"idle", "older text", 6403},
	} {
		messages := make([]Message, 0, fixture.count)
		for i := range fixture.count {
			messages = append(messages, Message{Ordinal: i, Role: "user", Content: fixture.content, Timestamp: "2024-06-15T10:00:00Z"})
		}
		seedUnitSession(t, d, fixture.id, func(s *Session) { s.Project = "project-a" }, messages)
	}
	_, err := d.getWriter().Exec(t.Context(), "UPDATE sessions SET ended_at = CASE id WHEN 'active' THEN '2024-06-15T11:59:00Z' ELSE '2024-06-15T10:00:00Z' END")
	require.NoError(t, err)
	eligible := []VectorHit{
		{SessionID: "idle", Ordinal: 0, OrdinalStart: 0, OrdinalEnd: 0},
		{SessionID: "idle", Ordinal: 1, OrdinalStart: 1, OrdinalEnd: 1},
		{SessionID: "idle", Ordinal: 2, OrdinalStart: 2, OrdinalEnd: 2},
	}
	for _, tc := range []struct {
		name                     string
		prefix                   VectorHit
		count, chunks            int
		cutoff                   time.Time
		project                  string
		wantOrdinals, wantLimits []int
	}{
		{"boundary", VectorHit{SessionID: "active"}, 200, 1, activityCutoff, "", []int{0, 1, 2}, []int{200, 400}},
		{"collapsed chunks", VectorHit{SessionID: "active"}, 1, 200, activityCutoff, "", []int{0, 1, 2}, []int{200, 400, 800}},
		{"fetch budget exhausted", VectorHit{SessionID: "active"}, 6400, 1, activityCutoff, "", nil, []int{200, 400, 800, 1600, 3200, 4096}},
		{"all active", VectorHit{SessionID: "active"}, 200, 1, epochCutoff, "", nil, []int{200, 400}},
		{"sparse project without activity rejection", VectorHit{SessionID: "idle", Ordinal: 3}, 6400, 1, activityCutoff, "project-b", nil, []int{200}},
	} {
		modes := []string{"semantic"}
		if tc.name == "boundary" {
			modes = append(modes, "hybrid")
		}
		for _, mode := range modes {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				hits := make([]VectorHit, 0, tc.count+len(eligible))
				for i := range tc.count {
					hit := tc.prefix
					hit.Ordinal += i
					hit.OrdinalStart, hit.OrdinalEnd = hit.Ordinal, hit.Ordinal
					hits = append(hits, hit)
				}
				hits = append(hits, eligible...)
				searcher := &activityVectorSearcher{hits: hits, chunksPerHit: tc.chunks}
				d.SetVectorSearcher(searcher)
				page, err := d.SearchContent(t.Context(), ContentSearchFilter{
					Pattern: "absent keyword", Mode: mode, Limit: 3, ExcludeActiveSince: tc.cutoff, Project: tc.project,
				})
				require.NoError(t, err)
				ordinals := make([]int, 0, len(page.Matches))
				for _, match := range page.Matches {
					ordinals = append(ordinals, match.Ordinal)
				}
				assert.Equal(t, tc.wantLimits, searcher.limits)
				assert.ElementsMatch(t, tc.wantOrdinals, ordinals)
			})
		}
	}
}

func TestSelectSurvivingVectorHitsErrors(t *testing.T) {
	failure := errors.New("search failed")
	for _, tc := range []struct {
		name                 string
		searchErr, lookupErr error
		cancelAfterLookup    bool
	}{
		{"search", failure, nil, false},
		{"session lookup", nil, failure, false},
		{"canceled during expansion", nil, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			searcher := &fakeVectorSearcher{err: tc.searchErr}
			if tc.lookupErr != nil {
				searcher.hits = []VectorHit{{SessionID: "session"}}
			}
			_, err := SelectSurvivingVectorHits(ctx, ContentSearchFilter{Limit: 3, ExcludeActiveSince: activityCutoff}, searcher, 200,
				func(_ context.Context, _ ContentSearchFilter, sessionIDs []string) (map[string]bool, error) {
					if tc.lookupErr != nil {
						assert.Equal(t, []string{"session"}, sessionIDs)
					}
					if tc.cancelAfterLookup {
						cancel()
					}
					return nil, tc.lookupErr
				})
			if tc.cancelAfterLookup {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.ErrorIs(t, err, failure)
			}
			assert.Equal(t, 1, searcher.calls)
		})
	}
}
