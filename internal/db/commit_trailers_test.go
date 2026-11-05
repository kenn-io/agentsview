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
		name, trailer, agent, owner string
	}{
		{"claude", "claude:session", "claude", "session"},
		{"codex", "codex:session_a", "codex", "codex:session_a"},
		{"freebuff", "freebuff:project-a:123", "freebuff", "freebuff:project-a:123"},
		{"codebuff", "codebuff:project-a:123", "codebuff", "codebuff:project-a:123"},
		{"native colon", "codex:session:page", "codex", "codex:session:page"},
		{"claude native colon", "claude:session:page", "claude", "session:page"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, tt.owner, "project-a", func(s *Session) { s.Agent = tt.agent })
			sha := strings.Repeat("a", 40)
			require.NoError(t, d.StoreTrailerCommitLinks(t.Context(), "repo", []gitlog.TrailerCommit{{
				SHA: sha, Sessions: []string{tt.trailer}, CommittedAt: tsZero,
			}}))
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

func TestTrailerCommitLinksOwnerAvailableAfterScan(t *testing.T) {
	d := testDB(t)
	owner := "codex:session"
	sha := strings.Repeat("a", 40)
	require.NoError(t, d.StoreTrailerCommitLinks(t.Context(), "repo", []gitlog.TrailerCommit{{
		SHA: sha, Sessions: []string{"codex:session"}, CommittedAt: tsZero,
	}}))
	found, err := d.CommitSessionLinks(t.Context(), []string{sha})
	require.NoError(t, err)
	require.Len(t, found.Commits, 1)
	assert.Empty(t, found.Commits[0].Links)
	insertSession(t, d, owner, "project-a", func(s *Session) { s.Agent = "codex" })
	// The saved Git evidence must become visible without another scan.
	found, err = d.CommitSessionLinks(t.Context(), []string{sha})
	require.NoError(t, err)
	require.Len(t, found.Commits[0].Links, 1)
	assert.Equal(t, owner, found.Commits[0].Links[0].SessionID)
	list, err := d.SessionCommitLinks(t.Context(), owner)
	require.NoError(t, err)
	require.Len(t, list.Commits, 1)
	assert.Equal(t, sha, list.Commits[0].SHA)
}

func TestTrailerCommitLinksDuplicateOwnerIdentities(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "codex:abc", "project-a", func(s *Session) { s.Agent = "codex" })
	sha := strings.Repeat("a", 40)
	require.NoError(t, d.StoreTrailerCommitLinks(t.Context(), "repo", []gitlog.TrailerCommit{{
		SHA: sha, Sessions: []string{"claude:codex:abc", "codex:abc"}, CommittedAt: tsZero,
	}}))
	found, err := d.CommitSessionLinks(t.Context(), []string{sha})
	require.NoError(t, err)
	require.Len(t, found.Commits, 1)
	require.Len(t, found.Commits[0].Links, 1)
	assert.Equal(t, "codex:abc", found.Commits[0].Links[0].SessionID)
}

func TestTrailerCommitLinksMissingOwner(t *testing.T) {
	d := testDB(t)
	owner := "codex:session"
	insertSession(t, d, owner, "project-a", func(s *Session) { s.Agent = "codex" })
	sha := strings.Repeat("a", 40)
	require.NoError(t, d.StoreTrailerCommitLinks(t.Context(), "repo", []gitlog.TrailerCommit{{SHA: sha, Sessions: []string{"codex:session"}, CommittedAt: tsZero}}))
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
	_, err := d.RestoreSession(t.Context(), owner)
	require.NoError(t, err)
	check(false)
	require.NoError(t, d.DeleteSession(t.Context(), owner))
	check(true)
}

func TestTrailerCommitLinksPrecedence(t *testing.T) {
	for _, sha := range []string{"abc1234", "abc1234" + strings.Repeat("0", 33)} {
		t.Run(sha, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "session-a", "project-a")
			insertSession(t, d, "codex:thread", "project-a", func(s *Session) { s.Agent = "codex" })
			c := gitlog.TrailerCommit{SHA: "abc1234" + strings.Repeat("0", 33), Branch: "main", Subject: "Editor", CommittedAt: "2026-10-30T10:10:00Z", Sessions: []string{"claude:session-a", "codex:thread", "unknown:thread"}}
			insertMessages(t, d, commitMessages("session-a", `{"command":"git commit -m fix"}`, "[main "+sha+"] Exact")...)
			require.NoError(t, d.StoreTrailerCommitLinks(t.Context(), "repo", []gitlog.TrailerCommit{c}))
			found, err := d.CommitSessionLinks(t.Context(), []string{"abc1234"})
			require.NoError(t, err)
			require.Len(t, found.Commits[0].Links, 2)
			for _, l := range found.Commits[0].Links {
				assert.False(t, l.Ambiguous)
				if l.SessionID == "session-a" {
					assert.Equal(t, "transcript", l.Source)
					assert.Equal(t, sha, l.SHA)
				} else {
					assert.Equal(t, "codex:thread", l.SessionID)
					assert.Equal(t, "trailer", l.Source)
				}
			}
			// Replacing the transcript preserves its independent trailer evidence.
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "session-a", []Message{{SessionID: "session-a", Ordinal: 0, Role: "user", Content: "Rewritten", Timestamp: tsZero}}))
			list, err := d.SessionCommitLinks(t.Context(), "session-a")
			require.NoError(t, err)
			require.Len(t, list.Commits, 1)
			assert.Equal(t, c.SHA, list.Commits[0].SHA)
			assert.Equal(t, "trailer", list.Commits[0].Source)
			assert.Equal(t, -1, list.Commits[0].CallIndex)
			assert.Empty(t, list.Commits[0].ToolUseID)
		})
	}
}

func TestTrailerCommitLinksPrompt(t *testing.T) {
	for _, tt := range []struct {
		name, owner, agent, trailer, committedAt string
		messages                                 []Message
		wantPrompt                               bool
	}{
		{
			wantPrompt: true, name: "qualifying prompt", owner: "session-a", agent: "claude", trailer: "claude:session-a", committedAt: "2026-10-30T10:10:00Z",
			messages: []Message{
				{SessionID: "session-a", Ordinal: 0, Role: "user", Content: "Before", Timestamp: "2026-10-30T10:00:00Z"},
				{SessionID: "session-a", Ordinal: 1, Role: "user", Content: "System", IsSystem: true, Timestamp: "2026-10-30T10:05:00Z"},
				{SessionID: "session-a", Ordinal: 2, Role: "user", Content: "Tool result", SourceSubtype: "tool_result", Timestamp: "2026-10-30T10:08:00Z"},
				{SessionID: "session-a", Ordinal: 3, Role: "user", Content: "Next task", Timestamp: "2026-10-30T10:12:00Z"},
				{SessionID: "session-a", Ordinal: 4, Role: "user", Content: "Too late", Timestamp: "2026-10-30T10:16:00Z"},
			},
		},
		{
			name: "no qualifying prompt", owner: "codex:thread", agent: "codex", trailer: "codex:thread", committedAt: "2026-10-30T10:10:00Z",
			messages: []Message{
				{SessionID: "codex:thread", Ordinal: 0, Role: "user", Content: "Too late", Timestamp: "2026-10-30T10:12:00Z"},
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, tt.owner, "project-a", func(s *Session) { s.Agent = tt.agent })
			insertMessages(t, d, tt.messages...)
			sha := strings.Repeat("a", 40)
			require.NoError(t, d.StoreTrailerCommitLinks(t.Context(), "repo", []gitlog.TrailerCommit{{SHA: sha, CommittedAt: tt.committedAt, Sessions: []string{tt.trailer}}}))
			if !tt.wantPrompt {
				found, err := d.CommitSessionLinks(t.Context(), []string{sha})
				require.NoError(t, err)
				require.Len(t, found.Commits[0].Links, 1)
				assert.Nil(t, found.Commits[0].Links[0].Turn)
				return
			}
			list, err := d.SessionCommitLinks(t.Context(), tt.owner)
			require.NoError(t, err)
			require.Len(t, list.Commits, 1)
			require.NotNil(t, list.Commits[0].Turn)
			assert.True(t, list.Commits[0].Turn.Heuristic)
			assert.Equal(t, "Before", list.Commits[0].Turn.Text)
		})
	}
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
	require.NoError(t, d.StoreTrailerCommitLinks(t.Context(), "repo", []gitlog.TrailerCommit{{
		SHA: sha, CommittedAt: "2026-10-30T10:45:00Z", Sessions: []string{"claude:session-a"},
	}}))

	list, err := d.SessionCommitLinks(t.Context(), "session-a")
	require.NoError(t, err)
	require.Len(t, list.Commits, 2)
	assert.Equal(t, "abc1234", list.Commits[0].SHA)
	assert.Equal(t, sha, list.Commits[1].SHA)
	assert.Equal(t, "trailer", list.Commits[1].Source)
	assert.Equal(t, 1, list.Commits[1].MessageOrdinal)
	require.NotNil(t, list.Commits[1].Turn)
	assert.Equal(t, 0, list.Commits[1].Turn.Ordinal)
	assert.Equal(t, "Save the edit", list.Commits[1].Turn.Text)
	assert.True(t, list.Commits[1].Turn.Heuristic)
}
