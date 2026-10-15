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
// inside fork-a after its first input. A sub-agent and a continuation of the
// root are not forks and stay out of the input tree.
func seedNestedForkFamily(t *testing.T, d *db.DB) {
	t.Helper()
	dbtest.SeedSession(t, d, "root", "p1", func(s *db.Session) {
		s.StartedAt = new("2026-07-01T10:00:00Z")
	})
	seedForkSession(t, d, "fork-a", "root", "fork", "2026-07-01T10:04:00Z")
	seedForkSession(t, d, "fork-b", "fork-a", "fork", "2026-07-01T10:06:00Z")
	seedForkSession(t, d, "helper", "root", "subagent", "2026-07-01T10:01:30Z")
	seedForkSession(t, d, "later", "root", "continuation", "2026-07-01T11:00:00Z")
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
		timedMsg("later", 0, "user", "continue later", "2026-07-01T11:00:00Z"),
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
	}{
		{
			activeID: "root",
			want: map[string]int{
				"plan the feature":       0,
				"write the parser":       2,
				"use a streaming parser": 5,
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
		},
	}
	for _, tt := range tests {
		t.Run(tt.activeID, func(t *testing.T) {
			t.Parallel()
			tree, err := reader.InputTree(t.Context(), tt.activeID)
			require.NoError(t, err)
			require.NotNil(t, tree)
			assert.Equal(t, tt.want, activeOrdinals(tree))

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
