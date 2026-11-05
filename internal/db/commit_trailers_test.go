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
	for _, differentMetadata := range []bool{false, true} {
		t.Run(map[bool]string{false: "same metadata", true: "different metadata"}[differentMetadata], func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "codex:abc", "project-a", func(s *Session) { s.Agent = "codex" })
			sha := strings.Repeat("a", 40)
			commit := gitlog.TrailerCommit{SHA: sha, Sessions: []string{"claude:codex:abc"}, CommittedAt: tsZero}
			commits := []gitlog.TrailerCommit{commit}
			commit.Sessions = []string{"codex:abc"}
			if differentMetadata {
				commit.Branch = "feature"
				commit.Subject = "Updated subject"
			}
			commits = append(commits, commit)
			require.NoError(t, d.StoreTrailerCommitLinks(t.Context(), "repo", commits))

			rows, err := d.SessionCommitLinkRows(t.Context(), "codex:abc")
			require.NoError(t, err)
			assert.Len(t, rows, 1)
			snapshot, err := d.LoadSessionMirrorSnapshot(t.Context(), "codex:abc")
			require.NoError(t, err)
			assert.Len(t, snapshot.CommitLinks, 1)
			list, err := d.SessionCommitLinks(t.Context(), "codex:abc")
			require.NoError(t, err)
			require.Len(t, list.Commits, 1)
			assert.Equal(t, sha, list.Commits[0].SHA)
			found, err := d.CommitSessionLinks(t.Context(), []string{sha})
			require.NoError(t, err)
			require.Len(t, found.Commits[0].Links, 1)
			assert.Equal(t, "codex:abc", found.Commits[0].Links[0].SessionID)
		})
	}
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
			exact := commitMessages("session-a", `{"command":"git commit -m fix"}`, "[main "+sha+"] Exact")[2]
			exact.Ordinal = 5
			insertMessages(t, d, exact)
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
				{SessionID: "session-a", Ordinal: 1, Role: "user", Content: "Within tolerance", Timestamp: "2026-10-30T10:14:00Z"},
				{SessionID: "session-a", Ordinal: 2, Role: "user", Content: "Too late", Timestamp: "2026-10-30T10:16:00Z"},
				{SessionID: "session-a", Ordinal: 3, Role: "user", Content: "System", IsSystem: true, Timestamp: "2026-10-30T10:14:00Z"},
				{SessionID: "session-a", Ordinal: 4, Role: "user", Content: "Tool result", SourceSubtype: "tool_result", Timestamp: "2026-10-30T10:14:00Z"},
			},
		},
		{
			name: "no qualifying prompt", owner: "codex:thread", agent: "codex", trailer: "codex:thread", committedAt: "2026-10-30T10:10:00Z",
			messages: []Message{
				{SessionID: "codex:thread", Ordinal: 0, Role: "user", Content: "Too late", Timestamp: "2026-10-30T10:16:00Z"},
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
			assert.Equal(t, "Within tolerance", list.Commits[0].Turn.Text)
		})
	}
}

func TestTrailerCommitLinksTranscriptRewrite(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "session-a", "project-a")
	insertMessages(t, d, commitMessages("session-a", `{"command":"git commit -m fix"}`, "[main abc1234] Exact")...)
	require.NoError(t, d.StoreTrailerCommitLinks(t.Context(), "repo", []gitlog.TrailerCommit{{
		SHA: "abc1234" + strings.Repeat("0", 33), CommittedAt: tsZero, Sessions: []string{"claude:session-a"},
	}}))
	// A full transcript replacement removes exact evidence, preserving trailers.
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "session-a", []Message{{SessionID: "session-a", Ordinal: 0, Role: "user", Content: "Rewritten", Timestamp: tsZero}}))
	list, err := d.SessionCommitLinks(t.Context(), "session-a")
	require.NoError(t, err)
	require.Len(t, list.Commits, 1)
	l := list.Commits[0]
	assert.Equal(t, "trailer", l.Source)
	assert.Equal(t, -1, l.CallIndex)
	assert.Empty(t, l.ToolUseID)
}
