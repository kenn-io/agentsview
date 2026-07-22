package db

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHasActiveSessionSourceBelow(t *testing.T) {
	database := testDB(t)
	base := filepath.Join(t.TempDir(), "sessions%_用户")
	activePath := filepath.Join(base, "nested", "active.jsonl")
	deletedPath := filepath.Join(base, "deleted", "gone.jsonl")
	siblingPath := base + "-sibling" + string(filepath.Separator) + "other.jsonl"
	otherAgentDir := filepath.Join(base, "only-other-agent")
	otherAgentPath := filepath.Join(otherAgentDir, "session.jsonl")

	for _, session := range []struct {
		id, agent, path string
	}{
		{id: "active", agent: "codex", path: activePath},
		{id: "deleted", agent: "codex", path: deletedPath},
		{id: "sibling", agent: "codex", path: siblingPath},
		{id: "other-agent", agent: "claude", path: otherAgentPath},
	} {
		path := session.path
		insertSession(t, database, session.id, "project", func(s *Session) {
			s.Agent = session.agent
			s.FilePath = &path
		})
	}
	require.NoError(t, database.SoftDeleteSession("deleted"))

	tests := []struct {
		name, agent, path string
		want              bool
	}{
		{name: "active descendant", agent: "codex", path: base, want: true},
		{name: "deleted descendant", agent: "codex", path: filepath.Join(base, "deleted")},
		{name: "sibling prefix", agent: "codex", path: base + "-sib"},
		{name: "other agent", agent: "codex", path: otherAgentDir},
		{name: "path itself is not below", agent: "codex", path: activePath},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := database.HasActiveSessionSourceBelow(tc.agent, tc.path)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	var positivePlan []string
	for _, tc := range []struct {
		name, path string
		wantRows   []int
	}{
		{name: "positive prefix", path: base, wantRows: []int{1}},
		{name: "negative prefix", path: filepath.Join(base, "absent")},
	} {
		t.Run(tc.name+" query shape", func(t *testing.T) {
			lower, upper := activeSessionSourceBounds(tc.path)
			rows, err := database.getReader().Query(
				hasActiveSessionSourceBelowQuery,
				"codex", lower, upper,
			)
			require.NoError(t, err)
			var gotRows []int
			for rows.Next() {
				var one int
				require.NoError(t, rows.Scan(&one))
				gotRows = append(gotRows, one)
			}
			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
			assert.Equal(t, tc.wantRows, gotRows,
				"the prefix probe must return at most its one sentinel row")

			planRows, err := database.getReader().Query(
				"EXPLAIN QUERY PLAN "+hasActiveSessionSourceBelowQuery,
				"codex", lower, upper,
			)
			require.NoError(t, err)
			var plan []string
			for planRows.Next() {
				var id, parent, unused int
				var detail string
				require.NoError(t, planRows.Scan(&id, &parent, &unused, &detail))
				plan = append(plan, detail)
			}
			require.NoError(t, planRows.Err())
			require.NoError(t, planRows.Close())
			assert.Condition(t, func() bool {
				return strings.Contains(strings.Join(plan, "\n"),
					"idx_sessions_agent_file_path_active (agent=? AND file_path>? AND file_path<?)")
			}, "expected indexed agent/path range seek, plans: %v", plan)
			if tc.wantRows != nil {
				positivePlan = append([]string(nil), plan...)
			} else {
				assert.Equal(t, positivePlan, plan,
					"positive and negative probes must use the same index seek")
			}
		})
	}
}
