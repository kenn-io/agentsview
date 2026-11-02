package db

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	gitlog "go.kenn.io/agentsview/internal/db/git"
)

func TestTrailerCommitLinksAuthorDateAfterAmend(t *testing.T) {
	skipIfNoGit(t)
	repo := t.TempDir()
	statsInitRepoAt(t, repo)
	statsRunGit(t, repo, []string{"GIT_AUTHOR_DATE=2026-01-01T12:10:00+02:00", "GIT_COMMITTER_DATE=2026-01-01T12:10:00+02:00"}, "commit", "--allow-empty", "-qm", "Editor\n\nAgent-Session: claude:session-a")
	d := testDB(t)
	insertSession(t, d, "session-a", "project-a")
	insertMessages(t, d,
		Message{SessionID: "session-a", Ordinal: 0, Role: "user", Content: "Write the change", Timestamp: "2026-01-01T10:00:00Z"},
		Message{SessionID: "session-a", Ordinal: 1, Role: "user", Content: "Later task", Timestamp: "2026-02-01T10:00:00Z"},
	)
	for _, phase := range []string{"original", "amended"} {
		t.Run(phase, func(t *testing.T) {
			if phase == "amended" {
				statsRunGit(t, repo, []string{"GIT_COMMITTER_DATE=2026-03-01T10:00:00Z"}, "commit", "--amend", "--no-edit", "--allow-empty", "-q")
			}
			commits, err := gitlog.ScanCommitTrailers(t.Context(), repo, nil)
			require.NoError(t, err)
			require.Len(t, commits, 1)
			require.NoError(t, d.StoreTrailerCommitLinks(t.Context(), repo, commits))
			found, err := d.CommitSessionLinks(t.Context(), []string{commits[0].SHA})
			require.NoError(t, err)
			require.Len(t, found.Commits, 1)
			require.Len(t, found.Commits[0].Links, 1)
			link := found.Commits[0].Links[0]
			assert.Equal(t, "2026-01-01T10:10:00Z", link.CommittedAt)
			require.NotNil(t, link.Turn)
			assert.True(t, link.Turn.Heuristic)
			assert.Equal(t, "Write the change", link.Turn.Text)
		})
	}
}

func TestTrailerCommitLinksPromptAndPrecedence(t *testing.T) {
	for _, sha := range []string{"abc1234", "abc1234" + strings.Repeat("0", 33)} {
		for _, trailerFirst := range []bool{false, true} {
			t.Run(sha+strconv.FormatBool(trailerFirst), func(t *testing.T) {
				d := testDB(t)
				insertSession(t, d, "session-a", "project-a")
				insertSession(t, d, "codex:thread", "project-a")
				insertSession(t, d, "thread", "project-a")
				messages := []Message{
					{SessionID: "session-a", Ordinal: 0, Role: "user", Content: "Before", Timestamp: "2026-10-30T10:00:00Z"},
					{SessionID: "session-a", Ordinal: 1, Role: "user", Content: "Within tolerance", Timestamp: "2026-10-30T10:14:00Z"},
					{SessionID: "session-a", Ordinal: 2, Role: "user", Content: "Too late", Timestamp: "2026-10-30T10:16:00Z"},
					{SessionID: "session-a", Ordinal: 3, Role: "user", Content: "System", IsSystem: true, Timestamp: "2026-10-30T10:14:00Z"},
					{SessionID: "session-a", Ordinal: 4, Role: "user", Content: "Tool result", SourceSubtype: "tool_result", Timestamp: "2026-10-30T10:14:00Z"},
				}
				insertMessages(t, d, messages...)
				c := gitlog.TrailerCommit{SHA: "abc1234" + strings.Repeat("0", 33), Branch: "main", Subject: "Editor", CommittedAt: "2026-10-30T10:10:00Z", Sessions: []string{"claude:session-a", "codex:thread", "unknown:thread"}}
				if trailerFirst {
					require.NoError(t, d.StoreTrailerCommitLinks(t.Context(), "repo", []gitlog.TrailerCommit{c}))
				}
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
						assert.Nil(t, l.Turn)
					}
				}
				// A full transcript replacement removes exact evidence, preserving trailers.
				require.NoError(t, d.ReplaceSessionMessages(t.Context(), "session-a", messages))
				list, err := d.SessionCommitLinks(t.Context(), "session-a")
				require.NoError(t, err)
				require.Len(t, list.Commits, 1)
				l := list.Commits[0]
				assert.Equal(t, "trailer", l.Source)
				assert.Equal(t, -1, l.CallIndex)
				assert.Empty(t, l.ToolUseID)
				require.NotNil(t, l.Turn)
				assert.True(t, l.Turn.Heuristic)
				assert.Equal(t, "Within tolerance", l.Turn.Text)
			})
		}
	}
}

func TestTrailerCommitLinksRebuildPolicies(t *testing.T) {
	for _, policy := range []config.ArchiveContent{config.ArchiveContentFull, config.ArchiveContentTranscripts, config.ArchiveContentUsage} {
		t.Run(string(policy), func(t *testing.T) {
			old := testDB(t)
			insertSession(t, old, "retained", "project-a")
			require.NoError(t, old.StoreTrailerCommitLinks(t.Context(), "gone", []gitlog.TrailerCommit{{SHA: strings.Repeat("a", 40), Sessions: []string{"claude:retained"}, CommittedAt: tsZero}}))
			fresh := testDB(t)
			fresh.SetArchiveContent(policy)
			require.NoError(t, fresh.CopyExcludedSessionsFrom(old.Path()))
			insertSession(t, fresh, "retained", "project-a")
			list, err := fresh.SessionCommitLinks(t.Context(), "retained")
			require.NoError(t, err)
			require.Len(t, list.Commits, 1)
			assert.Equal(t, "trailer", list.Commits[0].Source)
		})
	}
}
