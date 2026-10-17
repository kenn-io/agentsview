package service_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

func timedMsg(sid string, ordinal int, role, content, ts string) db.Message {
	return db.Message{
		SessionID:     sid,
		Ordinal:       ordinal,
		Role:          role,
		Content:       content,
		ContentLength: len(content),
		Timestamp:     ts,
	}
}

func newInputTreeReader(t *testing.T) (service.InputTreeReader, *directTestEnv) {
	t.Helper()
	svc, env := newDirectTestSvc(t)
	reader, ok := svc.(service.InputTreeReader)
	require.True(t, ok, "direct backend should build input trees")
	return reader, env
}

func seedForkSession(
	t *testing.T, d *db.DB, id, parentID, relationship, startedAt string,
) {
	t.Helper()
	dbtest.SeedSession(t, d, id, "p1", func(s *db.Session) {
		s.ParentSessionID = &parentID
		s.RelationshipType = relationship
		s.StartedAt = &startedAt
	})
}

// seedNestedForkFamily seeds a session whose rewound branches were kept as
// forks: fork-a branched after the second root input, and fork-b branched
// inside fork-a after its first input. A sub-agent is not part of the family
// and stays out of the input tree.
func seedNestedForkFamily(t *testing.T, d *db.DB) {
	t.Helper()
	dbtest.SeedSession(t, d, "root", "p1", func(s *db.Session) {
		s.StartedAt = new("2026-07-01T10:00:00Z")
	})
	seedForkSession(t, d, "fork-a", "root", "fork", "2026-07-01T10:04:00Z")
	seedForkSession(t, d, "fork-b", "fork-a", "fork", "2026-07-01T10:06:00Z")
	seedForkSession(t, d, "helper", "root", "subagent", "2026-07-01T10:01:30Z")
	dbtest.SeedMessages(t, d,
		timedMsg("root", 0, "user", "plan the feature", "2026-07-01T10:00:00Z"),
		timedMsg("root", 1, "assistant", "plan", "2026-07-01T10:01:00Z"),
		timedMsg("root", 2, "user", "write the parser", "2026-07-01T10:02:00Z"),
		timedMsg("root", 3, "assistant", "parser", "2026-07-01T10:03:00Z"),
		timedMsg("root", 4, "user", "<command-name>/clear</command-name>", "2026-07-01T10:19:00Z"),
		timedMsg("root", 5, "user", "use a streaming parser", "2026-07-01T10:20:00Z"),
		timedMsg("root", 6, "assistant", "streaming", "2026-07-01T10:21:00Z"),
		timedMsg("fork-a", 0, "user", "use a recursive parser", "2026-07-01T10:04:00Z"),
		timedMsg("fork-a", 1, "assistant", "recursive", "2026-07-01T10:05:00Z"),
		timedMsg("fork-a", 2, "user", "add tests", "2026-07-01T10:10:00Z"),
		timedMsg("fork-a", 3, "assistant", "tests", "2026-07-01T10:11:00Z"),
		timedMsg("fork-b", 0, "user", "add benchmarks", "2026-07-01T10:06:00Z"),
		timedMsg("fork-b", 1, "assistant", "benchmarks", "2026-07-01T10:07:00Z"),
		timedMsg("helper", 0, "user", "search the code", "2026-07-01T10:01:30Z"),
	)
}

type inputTreeRow struct {
	parent  int
	session string
	ordinal int
	preview string
	ends    []string
}

func inputTreeRows(tree *service.InputTree) []inputTreeRow {
	rows := make([]inputTreeRow, 0, len(tree.Nodes))
	for _, node := range tree.Nodes {
		rows = append(rows, inputTreeRow{
			parent:  node.Parent,
			session: node.SessionID,
			ordinal: node.Ordinal,
			preview: node.Preview,
			ends:    node.EndSessionIDs,
		})
	}
	return rows
}

func activeOrdinals(tree *service.InputTree) map[string]int {
	out := map[string]int{}
	for _, node := range tree.Nodes {
		if node.ActiveOrdinal != nil {
			out[node.Preview] = *node.ActiveOrdinal
		}
	}
	return out
}

// activePath lists the previews of the inputs on the active session's path,
// in tree order.
func activePath(tree *service.InputTree) []string {
	out := []string{}
	for _, node := range tree.Nodes {
		if node.OnActivePath {
			out = append(out, node.Preview)
		}
	}
	return out
}

func TestDirectBackend_InputTreeBranchesWhereForksDiverge(t *testing.T) {
	t.Parallel()
	reader, env := newInputTreeReader(t)
	seedNestedForkFamily(t, env.db)

	for _, activeID := range []string{"root", "fork-a", "fork-b"} {
		tree, err := reader.InputTree(t.Context(), activeID)
		require.NoError(t, err)
		require.NotNil(t, tree)

		assert.Equal(t, activeID, tree.ActiveSessionID)
		assert.Equal(t, "root", tree.RootSessionID)
		assert.False(t, tree.Truncated)
		sessionIDs := make([]string, 0, len(tree.Sessions))
		for _, s := range tree.Sessions {
			sessionIDs = append(sessionIDs, s.ID)
		}
		assert.Equal(t, []string{"root", "fork-a", "fork-b"}, sessionIDs)
		assert.Equal(t, []inputTreeRow{
			{parent: -1, session: "root", ordinal: 0, preview: "plan the feature", ends: []string{}},
			{parent: 0, session: "root", ordinal: 2, preview: "write the parser", ends: []string{}},
			{parent: 1, session: "root", ordinal: 5, preview: "use a streaming parser", ends: []string{"root"}},
			{parent: 1, session: "fork-a", ordinal: 0, preview: "use a recursive parser", ends: []string{}},
			{parent: 3, session: "fork-a", ordinal: 2, preview: "add tests", ends: []string{"fork-a"}},
			{parent: 3, session: "fork-b", ordinal: 0, preview: "add benchmarks", ends: []string{"fork-b"}},
		}, inputTreeRows(tree), "active %s", activeID)
	}
}

func TestDirectBackend_InputTreeActiveOrdinalsMatchMessageView(t *testing.T) {
	t.Parallel()
	reader, env := newInputTreeReader(t)
	seedNestedForkFamily(t, env.db)
	svc := reader.(service.SessionService)

	tests := []struct {
		activeID string
		want     map[string]int
		wantPath []string
	}{
		{
			activeID: "root",
			want: map[string]int{
				"plan the feature":       0,
				"write the parser":       2,
				"use a streaming parser": 5,
			},
			wantPath: []string{
				"plan the feature", "write the parser", "use a streaming parser",
			},
		},
		{
			activeID: "fork-b",
			want: map[string]int{
				"plan the feature":       -7,
				"write the parser":       -5,
				"use a recursive parser": -3,
				"add benchmarks":         0,
			},
			wantPath: []string{
				"plan the feature", "write the parser",
				"use a recursive parser", "add benchmarks",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.activeID, func(t *testing.T) {
			t.Parallel()
			tree, err := reader.InputTree(t.Context(), tt.activeID)
			require.NoError(t, err)
			require.NotNil(t, tree)
			assert.Equal(t, tt.want, activeOrdinals(tree))
			assert.Equal(t, tt.wantPath, activePath(tree))

			// The ordinals must address the same inputs the message view
			// and its input outline show for the active session.
			outline, err := svc.InputOutline(t.Context(), tt.activeID, true)
			require.NoError(t, err)
			fromOutline := map[string]int{}
			for _, item := range outline.Items {
				fromOutline[item.Preview] = item.Ordinal
			}
			assert.Equal(t, fromOutline, activeOrdinals(tree))
		})
	}
}

func TestDirectBackend_InputTreeForkFromLastInputEndsParentInside(
	t *testing.T,
) {
	t.Parallel()
	reader, env := newInputTreeReader(t)
	dbtest.SeedSession(t, env.db, "base", "p1", func(s *db.Session) {
		s.StartedAt = new("2026-07-01T09:00:00Z")
	})
	seedForkSession(t, env.db, "branch", "base", "fork", "2026-07-01T09:30:00Z")
	dbtest.SeedMessages(t, env.db,
		timedMsg("base", 0, "user", "first", "2026-07-01T09:00:00Z"),
		timedMsg("base", 1, "assistant", "ok", "2026-07-01T09:01:00Z"),
		timedMsg("base", 2, "user", "second", "2026-07-01T09:02:00Z"),
		timedMsg("base", 3, "assistant", "ok", "2026-07-01T09:03:00Z"),
		timedMsg("branch", 0, "user", "explore", "2026-07-01T09:30:00Z"),
	)

	tree, err := reader.InputTree(t.Context(), "branch")
	require.NoError(t, err)
	require.NotNil(t, tree)
	assert.Equal(t, []inputTreeRow{
		{parent: -1, session: "base", ordinal: 0, preview: "first", ends: []string{}},
		{parent: 0, session: "base", ordinal: 2, preview: "second", ends: []string{"base"}},
		{parent: 1, session: "branch", ordinal: 0, preview: "explore", ends: []string{"branch"}},
	}, inputTreeRows(tree))
}

// seedContinuedFork seeds a fork that moved to a new session, as a
// background handoff does: side and main are forks of base, and moved
// continues main. moved's first message is the task notification that
// arrived after the handoff, which is not an input.
func seedContinuedFork(t *testing.T, d *db.DB) {
	t.Helper()
	dbtest.SeedSession(t, d, "base", "p1", func(s *db.Session) {
		s.StartedAt = new("2026-07-01T09:00:00Z")
	})
	seedForkSession(t, d, "side", "base", "fork", "2026-07-01T09:30:00Z")
	seedForkSession(t, d, "main", "base", "fork", "2026-07-01T09:40:00Z")
	seedForkSession(t, d, "moved", "main", "continuation", "2026-07-01T12:00:00Z")
	dbtest.SeedMessages(t, d,
		timedMsg("base", 0, "user", "summarize the run", "2026-07-01T09:00:00Z"),
		timedMsg("base", 1, "assistant", "summary", "2026-07-01T09:01:00Z"),
		timedMsg("base", 2, "user", "check the queue", "2026-07-01T09:10:00Z"),
		timedMsg("base", 3, "assistant", "queue", "2026-07-01T09:11:00Z"),
		timedMsg("side", 0, "user", "explain the metric", "2026-07-01T09:30:00Z"),
		timedMsg("main", 0, "user", "stop the old job", "2026-07-01T09:40:00Z"),
		timedMsg("main", 1, "assistant", "stopped", "2026-07-01T09:41:00Z"),
		timedMsg("main", 2, "user", "try the faster setting", "2026-07-01T10:00:00Z"),
		timedMsg("main", 3, "assistant", "running", "2026-07-01T10:01:00Z"),
		timedMsg("moved", 0, "user",
			"<task-notification>done</task-notification>", "2026-07-01T12:00:00Z"),
		timedMsg("moved", 1, "assistant", "it is faster", "2026-07-01T12:01:00Z"),
		timedMsg("moved", 2, "user", "report the results", "2026-07-01T12:30:00Z"),
	)
}

func TestDirectBackend_InputTreeContinuationExtendsItsParentPath(
	t *testing.T,
) {
	t.Parallel()
	reader, env := newInputTreeReader(t)
	seedContinuedFork(t, env.db)

	for _, activeID := range []string{"base", "side", "main", "moved"} {
		tree, err := reader.InputTree(t.Context(), activeID)
		require.NoError(t, err)
		require.NotNil(t, tree)

		assert.Equal(t, "base", tree.RootSessionID, "active %s", activeID)
		sessionIDs := make([]string, 0, len(tree.Sessions))
		for _, s := range tree.Sessions {
			sessionIDs = append(sessionIDs, s.ID)
		}
		assert.Equal(t, []string{"base", "side", "main", "moved"}, sessionIDs,
			"active %s", activeID)
		assert.Equal(t, []inputTreeRow{
			{parent: -1, session: "base", ordinal: 0, preview: "summarize the run", ends: []string{}},
			{parent: 0, session: "base", ordinal: 2, preview: "check the queue", ends: []string{"base"}},
			{parent: 1, session: "side", ordinal: 0, preview: "explain the metric", ends: []string{"side"}},
			{parent: 1, session: "main", ordinal: 0, preview: "stop the old job", ends: []string{}},
			{parent: 3, session: "main", ordinal: 2, preview: "try the faster setting", ends: []string{"main"}},
			{parent: 4, session: "moved", ordinal: 2, preview: "report the results", ends: []string{"moved"}},
		}, inputTreeRows(tree), "active %s", activeID)
	}
}

func TestDirectBackend_InputTreeActivePathThroughContinuation(
	t *testing.T,
) {
	t.Parallel()
	reader, env := newInputTreeReader(t)
	seedContinuedFork(t, env.db)
	svc := reader.(service.SessionService)

	tests := []struct {
		activeID     string
		wantOrdinals map[string]int
		wantPath     []string
	}{
		{
			// The continuation's message view shows only its own messages,
			// so the inputs it carries on from main have no view ordinal.
			activeID:     "moved",
			wantOrdinals: map[string]int{"report the results": 2},
			wantPath: []string{
				"summarize the run", "check the queue",
				"stop the old job", "try the faster setting", "report the results",
			},
		},
		{
			activeID: "main",
			wantOrdinals: map[string]int{
				"summarize the run":      -5,
				"check the queue":        -3,
				"stop the old job":       0,
				"try the faster setting": 2,
			},
			wantPath: []string{
				"summarize the run", "check the queue", "stop the old job", "try the faster setting",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.activeID, func(t *testing.T) {
			t.Parallel()
			tree, err := reader.InputTree(t.Context(), tt.activeID)
			require.NoError(t, err)
			require.NotNil(t, tree)
			assert.Equal(t, tt.wantOrdinals, activeOrdinals(tree))
			assert.Equal(t, tt.wantPath, activePath(tree))

			outline, err := svc.InputOutline(t.Context(), tt.activeID, true)
			require.NoError(t, err)
			fromOutline := map[string]int{}
			for _, item := range outline.Items {
				fromOutline[item.Preview] = item.Ordinal
			}
			assert.Equal(t, fromOutline, activeOrdinals(tree))
		})
	}
}

func TestDirectBackend_InputTreeContinuationBranchesWhereItTookOver(
	t *testing.T,
) {
	t.Parallel()
	reader, env := newInputTreeReader(t)
	dbtest.SeedSession(t, env.db, "base", "p1", func(s *db.Session) {
		s.StartedAt = new("2026-07-01T09:00:00Z")
	})
	seedForkSession(t, env.db, "moved", "base", "continuation", "2026-07-01T10:00:00Z")
	dbtest.SeedMessages(t, env.db,
		timedMsg("base", 0, "user", "first", "2026-07-01T09:00:00Z"),
		timedMsg("base", 1, "assistant", "ok", "2026-07-01T09:01:00Z"),
		timedMsg("base", 2, "user", "second", "2026-07-01T09:10:00Z"),
		timedMsg("base", 3, "assistant", "ok", "2026-07-01T09:11:00Z"),
		// base was resumed after moved had taken over.
		timedMsg("base", 4, "user", "back again", "2026-07-01T11:00:00Z"),
		timedMsg("moved", 0, "user", "carry on", "2026-07-01T10:00:00Z"),
	)

	tree, err := reader.InputTree(t.Context(), "moved")
	require.NoError(t, err)
	require.NotNil(t, tree)
	assert.Equal(t, []inputTreeRow{
		{parent: -1, session: "base", ordinal: 0, preview: "first", ends: []string{}},
		{parent: 0, session: "base", ordinal: 2, preview: "second", ends: []string{}},
		{parent: 1, session: "base", ordinal: 4, preview: "back again", ends: []string{"base"}},
		{parent: 1, session: "moved", ordinal: 0, preview: "carry on", ends: []string{"moved"}},
	}, inputTreeRows(tree))
}

func TestDirectBackend_InputTreeForkOfContinuationInheritsThroughIt(
	t *testing.T,
) {
	t.Parallel()
	reader, env := newInputTreeReader(t)
	svc := reader.(service.SessionService)
	dbtest.SeedSession(t, env.db, "first", "p1", func(s *db.Session) {
		s.StartedAt = new("2026-07-01T09:00:00Z")
	})
	seedForkSession(t, env.db, "second", "first", "continuation", "2026-07-01T10:00:00Z")
	seedForkSession(t, env.db, "retry", "second", "fork", "2026-07-01T10:05:00Z")
	dbtest.SeedMessages(t, env.db,
		timedMsg("first", 0, "user", "plan", "2026-07-01T09:00:00Z"),
		timedMsg("first", 1, "assistant", "ok", "2026-07-01T09:01:00Z"),
		timedMsg("second", 0, "user", "build", "2026-07-01T10:00:00Z"),
		timedMsg("second", 1, "assistant", "ok", "2026-07-01T10:01:00Z"),
		timedMsg("second", 2, "user", "test", "2026-07-01T10:10:00Z"),
		timedMsg("retry", 0, "user", "test again", "2026-07-01T10:05:00Z"),
	)

	for _, activeID := range []string{"first", "second", "retry"} {
		tree, err := reader.InputTree(t.Context(), activeID)
		require.NoError(t, err)
		require.NotNil(t, tree)
		assert.Equal(t, "first", tree.RootSessionID, "active %s", activeID)
		assert.Equal(t, []inputTreeRow{
			{parent: -1, session: "first", ordinal: 0, preview: "plan", ends: []string{"first"}},
			{parent: 0, session: "second", ordinal: 0, preview: "build", ends: []string{}},
			{parent: 1, session: "second", ordinal: 2, preview: "test", ends: []string{"second"}},
			{parent: 1, session: "retry", ordinal: 0, preview: "test again", ends: []string{"retry"}},
		}, inputTreeRows(tree), "active %s", activeID)
	}

	// retry's message view shows the part of second it forked from, but not
	// the session second continues.
	tree, err := reader.InputTree(t.Context(), "retry")
	require.NoError(t, err)
	require.NotNil(t, tree)
	assert.Equal(t, map[string]int{"build": -3, "test again": 0}, activeOrdinals(tree))
	assert.Equal(t, []string{"plan", "build", "test again"}, activePath(tree))
	outline, err := svc.InputOutline(t.Context(), "retry", true)
	require.NoError(t, err)
	fromOutline := map[string]int{}
	for _, item := range outline.Items {
		fromOutline[item.Preview] = item.Ordinal
	}
	assert.Equal(t, fromOutline, activeOrdinals(tree))
}

func TestDirectBackend_InputTreeWithoutForksIsOnePath(t *testing.T) {
	t.Parallel()
	reader, env := newInputTreeReader(t)
	dbtest.SeedSession(t, env.db, "solo", "p1")
	dbtest.SeedMessages(t, env.db,
		timedMsg("solo", 0, "user", "one", "2026-07-01T09:00:00Z"),
		timedMsg("solo", 1, "assistant", "ok", "2026-07-01T09:01:00Z"),
		timedMsg("solo", 2, "user", "two", "2026-07-01T09:02:00Z"),
	)

	tree, err := reader.InputTree(t.Context(), "solo")
	require.NoError(t, err)
	require.NotNil(t, tree)
	assert.Equal(t, "solo", tree.RootSessionID)
	assert.Equal(t, []inputTreeRow{
		{parent: -1, session: "solo", ordinal: 0, preview: "one", ends: []string{}},
		{parent: 0, session: "solo", ordinal: 2, preview: "two", ends: []string{"solo"}},
	}, inputTreeRows(tree))
}

func TestDirectBackend_InputTreeMissingSession(t *testing.T) {
	t.Parallel()
	reader, _ := newInputTreeReader(t)

	tree, err := reader.InputTree(t.Context(), "missing")
	require.NoError(t, err)
	assert.Nil(t, tree)
}

func TestDirectBackend_InputTreeStopsAtForkCycle(t *testing.T) {
	t.Parallel()
	reader, env := newInputTreeReader(t)
	seedForkSession(t, env.db, "cycle-a", "cycle-b", "fork", "2026-07-01T09:00:00Z")
	seedForkSession(t, env.db, "cycle-b", "cycle-a", "fork", "2026-07-01T09:05:00Z")
	dbtest.SeedMessages(t, env.db,
		timedMsg("cycle-a", 0, "user", "a", "2026-07-01T09:00:00Z"),
		timedMsg("cycle-b", 0, "user", "b", "2026-07-01T09:05:00Z"),
	)

	tree, err := reader.InputTree(t.Context(), "cycle-a")
	require.NoError(t, err)
	require.NotNil(t, tree)
	assert.True(t, tree.Truncated)
	assert.Equal(t, "cycle-b", tree.RootSessionID)
	assert.Len(t, tree.Sessions, 2)
}

func TestDirectBackend_InputTreeKeepsActiveForkPastSessionLimit(
	t *testing.T,
) {
	t.Parallel()
	reader, env := newInputTreeReader(t)
	dbtest.SeedSession(t, env.db, "wide", "p1", func(s *db.Session) {
		s.StartedAt = new("2026-07-01T00:00:00Z")
	})
	msgs := []db.Message{
		timedMsg("wide", 0, "user", "start", "2026-07-01T00:00:00Z"),
	}
	const forks = 205
	for i := range forks {
		id := fmt.Sprintf("wide-fork-%03d", i)
		ts := time.Date(2026, 7, 1, 1, 0, i, 0, time.UTC).Format(time.RFC3339)
		seedForkSession(t, env.db, id, "wide", "fork", ts)
		msgs = append(msgs, timedMsg(id, 0, "user", "try "+id, ts))
	}
	dbtest.SeedMessages(t, env.db, msgs...)

	lastFork := fmt.Sprintf("wide-fork-%03d", forks-1)
	tree, err := reader.InputTree(t.Context(), lastFork)
	require.NoError(t, err)
	require.NotNil(t, tree)

	assert.True(t, tree.Truncated)
	assert.Len(t, tree.Sessions, 200)
	ended := map[string]bool{}
	for _, node := range tree.Nodes {
		for _, id := range node.EndSessionIDs {
			ended[id] = true
		}
	}
	assert.True(t, ended[lastFork], "the active fork keeps its leaf")
	assert.Len(t, ended, 200)
}
