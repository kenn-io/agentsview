package db

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gitlog "go.kenn.io/agentsview/internal/db/git"
)

func TestTrailerCommitLinksSessionResolution(t *testing.T) {
	for _, tt := range []struct {
		name, agent, owner string
		trailers           []string
	}{
		{"freebuff", "freebuff", "freebuff:project-a:123", []string{"freebuff:project-a:123"}},
		{"codebuff", "codebuff", "codebuff:project-a:123", []string{"codebuff:project-a:123"}},
		{"native colon", "codex", "codex:session:page", []string{"codex:session:page"}},
		{"claude native colon", "claude", "session:page", []string{"claude:session:page"}},
		{"duplicate owner identities", "codex", "codex:abc", []string{"claude:codex:abc", "codex:abc"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, tt.owner, "project-a", func(s *Session) { s.Agent = tt.agent })
			sha := strings.Repeat("a", 40)
			_, err := d.StoreTrailerCommitLinks(t.Context(), "repo", []gitlog.TrailerCommit{{
				SHA: sha, Sessions: tt.trailers, CommittedAt: tsZero,
			}})
			require.NoError(t, err)
			found, err := d.CommitSessionLinks(t.Context(), []string{sha})
			require.NoError(t, err)
			require.Len(t, found.Commits, 1)
			require.Len(t, found.Commits[0].Links, 1)
			assert.Equal(t, tt.owner, found.Commits[0].Links[0].SessionID)
			assert.False(t, found.Commits[0].Links[0].Missing)
			list, err := d.SessionCommitLinks(t.Context(), tt.owner)
			require.NoError(t, err)
			require.Len(t, list.Commits, 1)
			assert.Equal(t, sha, list.Commits[0].SHA)
			assert.Equal(t, "trailer", list.Commits[0].Source)
		})
	}
}

func TestTrailerCommitLinksMissingOwner(t *testing.T) {
	d := testDB(t)
	owner := "codex:session"
	insertSession(t, d, owner, "project-a", func(s *Session) { s.Agent = "codex" })
	sha := strings.Repeat("a", 40)
	_, err := d.StoreTrailerCommitLinks(t.Context(), "repo", []gitlog.TrailerCommit{{SHA: sha, Sessions: []string{"codex:session"}, CommittedAt: tsZero}})
	require.NoError(t, err)
	check := func(missing bool) {
		t.Helper()
		found, err := d.CommitSessionLinks(t.Context(), []string{sha})
		require.NoError(t, err)
		require.Len(t, found.Commits[0].Links, 1)
		assert.Equal(t, owner, found.Commits[0].Links[0].SessionID)
		assert.Equal(t, missing, found.Commits[0].Links[0].Missing)
		list, err := d.SessionCommitLinks(t.Context(), owner)
		require.NoError(t, err)
		require.Len(t, list.Commits, 1)
		assert.Equal(t, sha, list.Commits[0].SHA)
		assert.Equal(t, missing, list.Commits[0].Missing)
		if missing {
			assert.Nil(t, list.Commits[0].Turn)
		}
	}
	check(false)
	require.NoError(t, d.SoftDeleteSession(t.Context(), owner))
	check(true)
	_, err = d.RestoreSession(t.Context(), owner)
	require.NoError(t, err)
	check(false)
	require.NoError(t, d.DeleteSession(t.Context(), owner))
	check(true)
}

func TestTrailerCommitLinksPrompt(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "session-a", "project-a")
	insertMessages(t, d,
		Message{SessionID: "session-a", Ordinal: 0, Role: "user", Content: "Before", Timestamp: "2026-10-30T10:00:00Z"},
		Message{SessionID: "session-a", Ordinal: 1, Role: "assistant", HasToolUse: true, ToolCalls: []ToolCall{{ToolName: "apply_patch", Category: "Edit"}}, Timestamp: "2026-10-30T10:01:00Z"},
		Message{SessionID: "session-a", Ordinal: 2, Role: "user", Content: "At author time", Timestamp: "2026-10-30T10:10:00Z"},
		Message{SessionID: "session-a", Ordinal: 3, Role: "user", Content: "System", IsSystem: true, Timestamp: "2026-10-30T10:10:00Z"},
		Message{SessionID: "session-a", Ordinal: 4, Role: "user", Content: "Tool result", SourceSubtype: "tool_result", Timestamp: "2026-10-30T10:10:00Z"},
		Message{SessionID: "session-a", Ordinal: 5, Role: "user", Content: "Next task", Timestamp: "2026-10-30T10:12:00Z"},
	)
	sha := strings.Repeat("a", 40)
	_, err := d.StoreTrailerCommitLinks(t.Context(), "repo", []gitlog.TrailerCommit{{SHA: sha, CommittedAt: "2026-10-30T10:10:00Z", Sessions: []string{"claude:session-a"}}})
	require.NoError(t, err)
	list, err := d.SessionCommitLinks(t.Context(), "session-a")
	require.NoError(t, err)
	require.Len(t, list.Commits, 1)
	require.NotNil(t, list.Commits[0].Turn)
	assert.True(t, list.Commits[0].Turn.Heuristic)
	assert.Equal(t, "At author time", list.Commits[0].Turn.Text)
}

func TestTrailerCommitLinksOrdering(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "session-a", "project-a")
	prompt := userMsg("session-a", 0, "Save the edit")
	prompt.Timestamp = "2026-10-30T10:00:00Z"
	commit := commitMessages("session-a", `{"command":"git commit -m fix"}`, "[main abc1234] Exact")[2]
	commit.Ordinal = 1
	commit.Timestamp = "2026-10-30T10:30:00Z"
	insertMessages(t, d, prompt, commit)
	sha := strings.Repeat("b", 40)
	_, err := d.StoreTrailerCommitLinks(t.Context(), "repo", []gitlog.TrailerCommit{{
		SHA: sha, CommittedAt: "2026-10-30T10:34:00Z", Sessions: []string{"claude:session-a"},
	}})
	require.NoError(t, err)

	list, err := d.SessionCommitLinks(t.Context(), "session-a")
	require.NoError(t, err)
	require.Len(t, list.Commits, 2)
	assert.Equal(t, "abc1234", list.Commits[0].SHA)
	assert.Equal(t, sha, list.Commits[1].SHA)
	assert.Equal(t, "trailer", list.Commits[1].Source)
	assert.Equal(t, 1, list.Commits[1].MessageOrdinal)
}
