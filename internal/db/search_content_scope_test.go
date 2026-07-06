package db

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedSubagentSession inserts a subagent child session (a sidebar-child
// relationship, excluded by default from session lists) with the given
// messages for scope-filter tests.
func seedSubagentSession(
	t *testing.T, d *DB, id, parent, project string, msgs [][2]string,
) {
	t.Helper()
	insertSession(t, d, id, project, func(s *Session) {
		s.Agent = "claude"
		s.UserMessageCount = 2
		s.ParentSessionID = Ptr(parent)
		s.RelationshipType = "subagent"
	})
	var out []Message
	for i, rc := range msgs {
		out = append(out, Message{
			SessionID: id, Ordinal: i, Role: rc[0],
			Content: rc[1], Timestamp: "2026-05-20T12:00:0" + itoa(i) + "Z",
		})
	}
	require.NoError(t, d.ReplaceSessionMessages(id, out), "ReplaceSessionMessages")
}

// seedScopeFixture seeds one top-level session ("top") and one subagent
// child ("sub"), both matching the FTS pattern "zebra", and returns a
// searcher whose hits cover both (the subagent hit flagged Subordinate).
func seedScopeFixture(t *testing.T, d *DB) *fakeVectorSearcher {
	t.Helper()
	seedSearchSession(t, d, "top", "proj", [][2]string{
		{"user", "zebra question at top level"},
	})
	seedSubagentSession(t, d, "sub", "top", "proj", [][2]string{
		{"user", "zebra prompt inside the subagent"},
	})
	return &fakeVectorSearcher{hits: []VectorHit{
		{SessionID: "sub", Ordinal: 0, Subordinate: true, Score: 0.9,
			Snippet: "zebra prompt inside the subagent"},
		{SessionID: "top", Ordinal: 0, Score: 0.5,
			Snippet: "zebra question at top level"},
	}}
}

func matchSessionIDs(page ContentSearchPage) []string {
	ids := make([]string, 0, len(page.Matches))
	for _, m := range page.Matches {
		ids = append(ids, m.SessionID)
	}
	return ids
}

// requireHybridReady skips hybrid-mode subtests when FTS5 is unavailable.
func requireHybridReady(t *testing.T, d *DB, mode string) {
	t.Helper()
	if mode == "hybrid" && !d.HasFTS() {
		t.Skip("fts5 not available")
	}
}

// TestSearchContentScopeDefaultAllIncludesSubordinate is the precedence
// rule's critical test: with the default scope ("all") a subagent-session
// unit IS returned by semantic and hybrid search even though
// IncludeChildren is false — include_children must not hide subordinate
// units from either leg in these modes.
func TestSearchContentScopeDefaultAllIncludesSubordinate(t *testing.T) {
	for _, mode := range []string{"semantic", "hybrid"} {
		t.Run(mode, func(t *testing.T) {
			d := testDB(t)
			requireHybridReady(t, d, mode)
			d.SetVectorSearcher(seedScopeFixture(t, d))

			page, err := d.SearchContent(context.Background(), ContentSearchFilter{
				Pattern: "zebra", Mode: mode, Limit: 50,
			})
			require.NoError(t, err, "SearchContent")
			ids := matchSessionIDs(page)
			assert.Contains(t, ids, "sub",
				"subordinate unit must be visible with the default scope despite IncludeChildren=false")
			assert.Contains(t, ids, "top")
		})
	}
}

// TestSearchContentScopeTopExcludesSubordinate pins scope=top: subordinate
// units are excluded entirely from both modes.
func TestSearchContentScopeTopExcludesSubordinate(t *testing.T) {
	for _, mode := range []string{"semantic", "hybrid"} {
		t.Run(mode, func(t *testing.T) {
			d := testDB(t)
			requireHybridReady(t, d, mode)
			d.SetVectorSearcher(seedScopeFixture(t, d))

			page, err := d.SearchContent(context.Background(), ContentSearchFilter{
				Pattern: "zebra", Mode: mode, Scope: "top", Limit: 50,
			})
			require.NoError(t, err, "SearchContent")
			assert.Equal(t, []string{"top"}, matchSessionIDs(page))
		})
	}
}

// TestSearchContentScopeSubordinateOnly pins scope=subordinate: only
// subordinate units are returned.
func TestSearchContentScopeSubordinateOnly(t *testing.T) {
	for _, mode := range []string{"semantic", "hybrid"} {
		t.Run(mode, func(t *testing.T) {
			d := testDB(t)
			requireHybridReady(t, d, mode)
			d.SetVectorSearcher(seedScopeFixture(t, d))

			page, err := d.SearchContent(context.Background(), ContentSearchFilter{
				Pattern: "zebra", Mode: mode, Scope: "subordinate", Limit: 50,
			})
			require.NoError(t, err, "SearchContent")
			assert.Equal(t, []string{"sub"}, matchSessionIDs(page))
		})
	}
}

// TestSearchContentScopeSupersedesIncludeChildren pins that explicit
// include_children (either value) does not change the semantic-mode unit
// universe: scope governs visibility, so false and true return the same
// session set.
func TestSearchContentScopeSupersedesIncludeChildren(t *testing.T) {
	d := testDB(t)
	searcher := seedScopeFixture(t, d)
	d.SetVectorSearcher(searcher)

	withoutChildren, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "zebra", Mode: "semantic", IncludeChildren: false, Limit: 50,
	})
	require.NoError(t, err, "SearchContent IncludeChildren=false")
	withChildren, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "zebra", Mode: "semantic", IncludeChildren: true, Limit: 50,
	})
	require.NoError(t, err, "SearchContent IncludeChildren=true")

	assert.Equal(t, matchSessionIDs(withChildren), matchSessionIDs(withoutChildren),
		"include_children must be superseded in semantic mode")
	assert.Contains(t, matchSessionIDs(withoutChildren), "sub")
}

// TestSearchContentScopeStillAppliesSessionFilters guards that lifting the
// child exclusion in semantic mode does not lift the other session
// predicates: project, automated, and one-shot filtering still drop hits.
func TestSearchContentScopeStillAppliesSessionFilters(t *testing.T) {
	seedExtra := func(t *testing.T, d *DB) *fakeVectorSearcher {
		t.Helper()
		searcher := seedScopeFixture(t, d)
		// ReplaceSessionMessages recomputes is_automated from the stored
		// transcript, so the automated session must genuinely classify as
		// automated: one user message with a recognized automation prefix.
		autoContent := "You are a code reviewer. Find the zebra issue."
		insertSession(t, d, "auto", "proj", func(s *Session) {
			s.Agent = "claude"
			s.UserMessageCount = 1
			s.FirstMessage = Ptr(autoContent)
		})
		require.NoError(t, d.ReplaceSessionMessages("auto", []Message{
			{SessionID: "auto", Ordinal: 0, Role: "user",
				Content: autoContent, Timestamp: "2026-05-20T12:00:00Z"},
		}))
		insertSession(t, d, "oneshot", "proj", func(s *Session) {
			s.Agent = "claude"
			s.UserMessageCount = 1
		})
		require.NoError(t, d.ReplaceSessionMessages("oneshot", []Message{
			{SessionID: "oneshot", Ordinal: 0, Role: "user",
				Content: "zebra one-shot", Timestamp: "2026-05-20T12:00:00Z"},
		}))
		insertSession(t, d, "elsewhere", "otherproj", func(s *Session) {
			s.Agent = "claude"
			s.UserMessageCount = 2
		})
		require.NoError(t, d.ReplaceSessionMessages("elsewhere", []Message{
			{SessionID: "elsewhere", Ordinal: 0, Role: "user",
				Content: "zebra elsewhere", Timestamp: "2026-05-20T12:00:00Z"},
		}))
		searcher.hits = append(searcher.hits,
			VectorHit{SessionID: "auto", Ordinal: 0, Score: 0.4, Snippet: "zebra from automation"},
			VectorHit{SessionID: "oneshot", Ordinal: 0, Score: 0.3, Snippet: "zebra one-shot"},
			VectorHit{SessionID: "elsewhere", Ordinal: 0, Score: 0.2, Snippet: "zebra elsewhere"},
		)
		return searcher
	}

	t.Run("defaults drop automated one-shot and other projects", func(t *testing.T) {
		d := testDB(t)
		d.SetVectorSearcher(seedExtra(t, d))
		page, err := d.SearchContent(context.Background(), ContentSearchFilter{
			Pattern: "zebra", Mode: "semantic", Project: "proj", Limit: 50,
		})
		require.NoError(t, err, "SearchContent")
		assert.ElementsMatch(t, []string{"top", "sub"}, matchSessionIDs(page))
	})

	t.Run("opt-ins restore automated and one-shot", func(t *testing.T) {
		d := testDB(t)
		d.SetVectorSearcher(seedExtra(t, d))
		page, err := d.SearchContent(context.Background(), ContentSearchFilter{
			Pattern: "zebra", Mode: "semantic", Project: "proj",
			IncludeAutomated: true, IncludeOneShot: true, Limit: 50,
		})
		require.NoError(t, err, "SearchContent")
		assert.ElementsMatch(t, []string{"top", "sub", "auto", "oneshot"},
			matchSessionIDs(page))
	})
}

// TestSearchContentFTSModeIncludeChildrenUnchanged is the regression guard
// for the non-semantic paths: mode "fts" keeps today's include_children
// semantics — a subagent child is hidden by default and reachable only via
// IncludeChildren=true.
func TestSearchContentFTSModeIncludeChildrenUnchanged(t *testing.T) {
	d := testDB(t)
	if !d.HasFTS() {
		t.Skip("fts5 not available")
	}
	d.SetVectorSearcher(seedScopeFixture(t, d))

	base := ContentSearchFilter{
		Pattern: "zebra", Mode: "fts", Sources: []string{"messages"}, Limit: 50,
	}
	page, err := d.SearchContent(context.Background(), base)
	require.NoError(t, err, "SearchContent fts default")
	assert.Equal(t, []string{"top"}, matchSessionIDs(page),
		"fts mode must keep excluding sidebar children by default")

	base.IncludeChildren = true
	page, err = d.SearchContent(context.Background(), base)
	require.NoError(t, err, "SearchContent fts include_children")
	assert.ElementsMatch(t, []string{"top", "sub"}, matchSessionIDs(page))
}

// seedOneShotSubagentFixture seeds a normal top-level session ("top"), a
// one-shot subagent child ("sub1": exactly one user message, the shape
// nearly all non-automated subagent transcripts have), and a top-level
// one-shot ("solo"), all matching "zebra", plus a searcher covering all
// three (sub1 subordinate).
func seedOneShotSubagentFixture(t *testing.T, d *DB) *fakeVectorSearcher {
	t.Helper()
	seedSearchSession(t, d, "top", "proj", [][2]string{
		{"user", "zebra question at top level"},
	})
	insertSession(t, d, "sub1", "proj", func(s *Session) {
		s.Agent = "claude"
		s.UserMessageCount = 1
		s.ParentSessionID = Ptr("top")
		s.RelationshipType = "subagent"
	})
	require.NoError(t, d.ReplaceSessionMessages("sub1", []Message{
		{SessionID: "sub1", Ordinal: 0, Role: "user",
			Content: "zebra prompt for the subagent", Timestamp: "2026-05-20T12:00:00Z"},
	}), "ReplaceSessionMessages sub1")
	insertSession(t, d, "solo", "proj", func(s *Session) {
		s.Agent = "claude"
		s.UserMessageCount = 1
	})
	require.NoError(t, d.ReplaceSessionMessages("solo", []Message{
		{SessionID: "solo", Ordinal: 0, Role: "user",
			Content: "zebra one-shot at top level", Timestamp: "2026-05-20T12:00:00Z"},
	}), "ReplaceSessionMessages solo")
	return &fakeVectorSearcher{hits: []VectorHit{
		{SessionID: "sub1", Ordinal: 0, Subordinate: true, Score: 0.9,
			Snippet: "zebra prompt for the subagent"},
		{SessionID: "top", Ordinal: 0, Score: 0.5,
			Snippet: "zebra question at top level"},
		{SessionID: "solo", Ordinal: 0, Score: 0.4,
			Snippet: "zebra one-shot at top level"},
	}}
}

// TestSearchContentOneShotSubagentVisibleInSemanticModes pins the child
// carve-out from the one-shot gate: a subagent session with exactly one
// user message IS returned by semantic and hybrid under default filters
// (scope=all), while a TOP-LEVEL one-shot stays excluded.
func TestSearchContentOneShotSubagentVisibleInSemanticModes(t *testing.T) {
	for _, mode := range []string{"semantic", "hybrid"} {
		t.Run(mode, func(t *testing.T) {
			d := testDB(t)
			requireHybridReady(t, d, mode)
			d.SetVectorSearcher(seedOneShotSubagentFixture(t, d))

			page, err := d.SearchContent(context.Background(), ContentSearchFilter{
				Pattern: "zebra", Mode: mode, Limit: 50,
			})
			require.NoError(t, err, "SearchContent")
			ids := matchSessionIDs(page)
			assert.Contains(t, ids, "sub1",
				"one-shot subagent unit must survive the one-shot gate in %s mode", mode)
			assert.Contains(t, ids, "top")
			assert.NotContains(t, ids, "solo",
				"top-level one-shot must keep being excluded by default")
		})
	}
}

// TestSearchContentFTSModeOneShotSubagentStillExcluded guards the untouched
// path: mode "fts" with default filters keeps excluding the one-shot
// subagent session (both as a child and as a one-shot).
func TestSearchContentFTSModeOneShotSubagentStillExcluded(t *testing.T) {
	d := testDB(t)
	if !d.HasFTS() {
		t.Skip("fts5 not available")
	}
	d.SetVectorSearcher(seedOneShotSubagentFixture(t, d))

	page, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "zebra", Mode: "fts", Sources: []string{"messages"}, Limit: 50,
	})
	require.NoError(t, err, "SearchContent fts")
	assert.Equal(t, []string{"top"}, matchSessionIDs(page),
		"fts mode must keep today's one-shot and child exclusions")
}

// TestSearchContentScopeInvalidRejected pins the db-side backstop: an
// unknown scope value is a SearchInputError for both modes.
func TestSearchContentScopeInvalidRejected(t *testing.T) {
	for _, mode := range []string{"semantic", "hybrid"} {
		t.Run(mode, func(t *testing.T) {
			d := testDB(t)
			d.SetVectorSearcher(&fakeVectorSearcher{})

			_, err := d.SearchContent(context.Background(), ContentSearchFilter{
				Pattern: "zebra", Mode: mode, Scope: "bogus",
			})
			require.Error(t, err)
			var inputErr *SearchInputError
			assert.True(t, errors.As(err, &inputErr),
				"expected *SearchInputError, got %T: %v", err, err)
		})
	}
}
