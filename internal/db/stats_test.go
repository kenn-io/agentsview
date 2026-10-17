package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHasVisibleSession(t *testing.T) {
	for _, tc := range []struct {
		name    string
		session *Session
		parent  bool
		want    bool
	}{
		{name: "empty session", session: &Session{}},
		{name: "orphan subagent", session: &Session{MessageCount: 1, RelationshipType: "subagent", ParentSessionID: new("missing")}, want: true},
		{name: "orphan fork", session: &Session{MessageCount: 1, RelationshipType: "fork", ParentSessionID: new("missing")}, want: true},
		{name: "child of empty root", session: &Session{MessageCount: 1, RelationshipType: "subagent", ParentSessionID: new("parent")}, parent: true},
		{name: "automated", session: &Session{MessageCount: 1, IsAutomated: true}},
		{name: "trash", session: &Session{MessageCount: 1, DeletedAt: new("2026-01-01T00:00:00Z")}},
		{name: "root", session: &Session{MessageCount: 1}, want: true},
		{name: "continuation", session: &Session{MessageCount: 1, RelationshipType: "continuation"}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			if tc.parent {
				insertSession(t, d, "parent", "project-a", func(s *Session) { s.MessageCount = 0 })
			}
			if tc.session != nil {
				insertSession(t, d, "session-one", "project-a", func(s *Session) {
					s.MessageCount = tc.session.MessageCount
					s.RelationshipType = tc.session.RelationshipType
					s.ParentSessionID = tc.session.ParentSessionID
					s.IsAutomated = tc.session.IsAutomated
				})
				if tc.session.DeletedAt != nil {
					require.NoError(t, d.SoftDeleteSession(t.Context(), "session-one"))
				}
			}
			got, err := d.HasVisibleSession(t.Context())
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			index, err := d.GetSidebarSessionIndex(t.Context(), SessionFilter{ExcludeAutomated: true})
			require.NoError(t, err)
			assert.Equal(t, tc.want, len(index.Sessions) > 0)
		})
	}
}

func TestFileBackedSessionCount_ExcludesNonDevinNonFileBackedAgents(
	t *testing.T,
) {
	d := testDB(t)
	ctx := t.Context()

	// Insert a claude-ai session (non-file-backed).
	insertSession(t, d, "claude-ai:test-1", "claude.ai",
		func(s *Session) { s.Agent = "claude-ai" })

	// Insert a warp session (non-Devin, non-file-backed).
	insertSession(t, d, "warp:test-1", "myproject",
		func(s *Session) { s.Agent = "warp" })

	// Insert a devin session (provider-backed local source; FileBased=false but
	// still protected by resync counting).
	insertSession(t, d, "devin:test-1", "myproject",
		func(s *Session) { s.Agent = "devin" })

	// Insert a claude session (file-backed).
	insertSession(t, d, "test-file-session", "myproject")

	count, err := d.FileBackedSessionCount(ctx)
	require.NoError(t, err, "FileBackedSessionCount")
	assert.Equal(t, 2, count,
		"FileBackedSessionCount should include claude plus devin, but exclude other non-file-backed agents")
}

func TestFileBackedSessionCountForRebuildOwner_IcodemateExclusions(
	t *testing.T,
) {
	d := testDB(t)
	ctx := t.Context()

	containerPath := "/home/user/.local/share/icodemate/icodemate.db"
	cliPath := "/home/user/.icodemate/cli/projects/proj/abc.jsonl"
	insertSession(t, d, "icodemate:container-1", "proj",
		func(s *Session) {
			s.Agent = "icodemate"
			s.FilePath = &containerPath
		})
	insertSession(t, d, "icodemate:cli-1", "proj",
		func(s *Session) {
			s.Agent = "icodemate"
			s.FilePath = &cliPath
		})
	insertSession(t, d, "claude-1", "proj")

	tests := []struct {
		name          string
		keepJSONLRows bool
		want          int
	}{
		{
			name:          "enabled provider keeps CLI transcript rows protected",
			keepJSONLRows: true,
			want:          2,
		},
		{
			name:          "disabled provider excludes every icodemate row",
			keepJSONLRows: false,
			want:          1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			count, err := d.FileBackedSessionCountForRebuildOwner(
				ctx, defaultMachine, nil, []RebuildAgentExclusion{
					{Agent: "icodemate", KeepJSONLRows: tt.keepJSONLRows},
				},
			)
			require.NoError(t, err)
			assert.Equal(t, tt.want, count)
		})
	}
}
