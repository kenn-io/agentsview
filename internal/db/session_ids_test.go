package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListSessionsIDs(t *testing.T) {
	d := testDB(t)
	for _, id := range []string{"codex:shared", "node-a~codex:shared", "node-b~codex:shared", "node-a~codex:shared-extra", "node-a~codex:SHARED", "node-a~codex:percent_%", "node-a~codex:percent_xy", "unrelated"} {
		insertSession(t, d, id, "app", func(s *Session) { s.Machine = "node-a"; s.MessageCount = 2; s.UserMessageCount = 1 })
	}
	insertSession(t, d, "node-c~codex:shared", "other", func(s *Session) {
		s.MessageCount = 0
		s.IsAutomated = true
		s.RelationshipType = "subagent"
		s.ParentSessionID = new("unrelated")
	})
	insertSession(t, d, "node-deleted~codex:shared", "app", func(s *Session) { s.DeletedAt = new("2026-09-01T12:00:00Z") })
	require.NoError(t, d.SoftDeleteSession(t.Context(), "node-deleted~codex:shared"))
	cases := []struct {
		name    string
		ids     []string
		project string
		want    []string
	}{
		{"raw copies", []string{"codex:shared"}, "", []string{"codex:shared", "node-a~codex:shared", "node-b~codex:shared", "node-c~codex:shared"}},
		{"qualified exact", []string{"node-a~codex:shared"}, "", []string{"node-a~codex:shared"}},
		{"overlap duplicate unknown", []string{"codex:shared", "node-a~codex:shared", "codex:shared", "missing"}, "app", []string{"codex:shared", "node-a~codex:shared", "node-b~codex:shared"}},
		{"unknown", []string{"missing"}, "", []string{}},
		{"empty selection", []string{}, "", []string{}},
		{"literal wildcards", []string{"codex:percent_%"}, "", []string{"node-a~codex:percent_%"}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			page, err := d.ListSessions(t.Context(), SessionFilter{IDs: tt.ids, Project: tt.project, IncludeChildren: true, ExcludeOneShot: true, ExcludeAutomated: true})
			require.NoError(t, err)
			got := make([]string, 0, len(page.Sessions))
			for _, s := range page.Sessions {
				got = append(got, s.ID)
			}
			assert.ElementsMatch(t, tt.want, got)
			assert.Equal(t, len(tt.want), page.Total)
		})
	}
	t.Run("machine intersects directly", func(t *testing.T) {
		page, err := d.ListSessions(t.Context(), SessionFilter{IDs: []string{"codex:shared"}, Machine: "node-a"})
		require.NoError(t, err)
		assert.Len(t, page.Sessions, 3)
	})
	t.Run("pagination", func(t *testing.T) {
		var got []string
		cursor := ""
		for {
			page, err := d.ListSessions(t.Context(), SessionFilter{IDs: []string{"codex:shared"}, Limit: 1, Cursor: cursor})
			require.NoError(t, err)
			for _, s := range page.Sessions {
				got = append(got, s.ID)
			}
			cursor = page.NextCursor
			if cursor == "" {
				break
			}
			require.LessOrEqual(t, len(got), 4)
		}
		assert.ElementsMatch(t, []string{"codex:shared", "node-a~codex:shared", "node-b~codex:shared", "node-c~codex:shared"}, got)
	})
}
