package db

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gitlog "go.kenn.io/agentsview/internal/db/git"
)

func TestTrailerCommitLinksArchiveOwner(t *testing.T) {
	d := testDB(t)
	_, err := d.EnsureInstallationIdentity(t.Context(), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	require.NoError(t, err)
	write := archiveProvenanceWrite(t, d)
	result, err := d.WriteSessionBatchContext(t.Context(), []SessionBatchWrite{write})
	require.NoError(t, err)
	require.Empty(t, result.Errors)
	sha := strings.Repeat("a", 40)
	require.NoError(t, d.StoreTrailerCommitLinks(t.Context(), "repo", []gitlog.TrailerCommit{{
		SHA: sha, Sessions: []string{"codex:session"}, CommittedAt: tsZero,
	}}))
	found, err := d.CommitSessionLinks(t.Context(), []string{sha})
	require.NoError(t, err)
	require.Len(t, found.Commits, 1)
	require.Len(t, found.Commits[0].Links, 1)
	assert.Equal(t, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb~codex:session", found.Commits[0].Links[0].SessionID)
	list, err := d.SessionCommitLinks(t.Context(), write.Session.ID)
	require.NoError(t, err)
	require.Len(t, list.Commits, 1)
	assert.Equal(t, sha, list.Commits[0].SHA)
}

func TestTrailerCommitLinksSessionResolution(t *testing.T) {
	for _, tt := range []struct {
		name, trailer, agent, want string
		ids                        []string
	}{
		{"claude namespace", "claude:session", "claude", "host-a~session", []string{"host-a~session"}},
		{"literal ID", "codex:session_a", "codex", "host-a~codex:session_a", []string{"host-a~codex:session_a", "host-b~codex:sessionXa"}},
		{"native colon", "codex:session:page", "codex", "host-a~codex:session:page", []string{"host-a~codex:session:page"}},
		{"claude native colon", "claude:session:page", "claude", "host-a~session:page", []string{"host-a~session:page"}},
		{"exact owner", "codex:session", "codex", "codex:session", []string{"codex:session", "host-a~codex:session"}},
		{"ambiguous owners", "codex:session", "codex", "", []string{"host-a~codex:session", "host-b~codex:session"}},
		{"different agent", "claude:session", "codex", "", []string{"host-a~codex:session"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			for _, id := range tt.ids {
				insertSession(t, d, id, "project-a", func(s *Session) { s.Agent = tt.agent })
			}
			sha := strings.Repeat("a", 40)
			require.NoError(t, d.StoreTrailerCommitLinks(t.Context(), "repo", []gitlog.TrailerCommit{{
				SHA: sha, Sessions: []string{tt.trailer}, CommittedAt: tsZero,
			}}))
			found, err := d.CommitSessionLinks(t.Context(), []string{sha})
			require.NoError(t, err)
			require.Len(t, found.Commits, 1)
			if tt.want == "" {
				assert.Empty(t, found.Commits[0].Links)
			} else {
				require.Len(t, found.Commits[0].Links, 1)
				assert.Equal(t, tt.want, found.Commits[0].Links[0].SessionID)
				list, err := d.SessionCommitLinks(t.Context(), tt.want)
				require.NoError(t, err)
				require.Len(t, list.Commits, 1)
				assert.Equal(t, sha, list.Commits[0].SHA)
			}
		})
	}
}

func TestTrailerCommitLinksOwnerAvailableAfterScan(t *testing.T) {
	for _, state := range []string{"not imported", "ambiguous"} {
		t.Run(state, func(t *testing.T) {
			d := testDB(t)
			owner := "host-a~codex:session"
			if state != "not imported" {
				insertSession(t, d, owner, "project-a", func(s *Session) { s.Agent = "codex" })
			}
			if state == "ambiguous" {
				insertSession(t, d, "host-b~codex:session", "project-a", func(s *Session) { s.Agent = "codex" })
			}
			sha := strings.Repeat("a", 40)
			require.NoError(t, d.StoreTrailerCommitLinks(t.Context(), "repo", []gitlog.TrailerCommit{{
				SHA: sha, Sessions: []string{"codex:session"}, CommittedAt: tsZero,
			}}))
			found, err := d.CommitSessionLinks(t.Context(), []string{sha})
			require.NoError(t, err)
			require.Len(t, found.Commits, 1)
			assert.Empty(t, found.Commits[0].Links)

			switch state {
			case "not imported":
				insertSession(t, d, owner, "project-a", func(s *Session) { s.Agent = "codex" })
			case "ambiguous":
				owner = "codex:session"
				insertSession(t, d, owner, "project-a", func(s *Session) { s.Agent = "codex" })
			}
			// The saved Git evidence must become visible without another scan.
			found, err = d.CommitSessionLinks(t.Context(), []string{sha})
			require.NoError(t, err)
			require.Len(t, found.Commits[0].Links, 1)
			assert.Equal(t, owner, found.Commits[0].Links[0].SessionID)
			list, err := d.SessionCommitLinks(t.Context(), owner)
			require.NoError(t, err)
			require.Len(t, list.Commits, 1)
			assert.Equal(t, sha, list.Commits[0].SHA)
		})
	}
}

func TestTrailerCommitLinksMissingOwner(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			d := testDB(t)
			owner := "host-a~session"
			if agent == "codex" {
				owner = "host-a~codex:session"
			}
			insertSession(t, d, owner, "project-a", func(s *Session) { s.Agent = agent })
			sha := strings.Repeat("a", 40)
			require.NoError(t, d.StoreTrailerCommitLinks(t.Context(), "repo", []gitlog.TrailerCommit{{SHA: sha, Sessions: []string{agent + ":session"}, CommittedAt: tsZero}}))
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
			active, err := d.FindSessionIDsByRawSuffix(t.Context(), "session", 2)
			require.NoError(t, err)
			assert.Empty(t, active)
			_, err = d.RestoreSession(t.Context(), owner)
			require.NoError(t, err)
			check(false)
			require.NoError(t, d.DeleteSession(t.Context(), owner))
			check(true)
		})
	}
}

func TestTrailerCommitLinksRetiredOwnerAmbiguity(t *testing.T) {
	for _, state := range []string{"trashed", "excluded"} {
		t.Run(state, func(t *testing.T) {
			d := testDB(t)
			for _, id := range []string{"host-a~codex:session", "host-b~codex:session"} {
				insertSession(t, d, id, "project-a", func(s *Session) { s.Agent = "codex" })
			}
			if state == "trashed" {
				require.NoError(t, d.SoftDeleteSession(t.Context(), "host-b~codex:session"))
			} else {
				require.NoError(t, d.DeleteSession(t.Context(), "host-b~codex:session"))
			}
			sha := strings.Repeat("a", 40)
			require.NoError(t, d.StoreTrailerCommitLinks(t.Context(), "repo", []gitlog.TrailerCommit{{SHA: sha, Sessions: []string{"codex:session"}, CommittedAt: tsZero}}))
			found, err := d.CommitSessionLinks(t.Context(), []string{sha})
			require.NoError(t, err)
			assert.Empty(t, found.Commits[0].Links)
			list, err := d.SessionCommitLinks(t.Context(), "host-a~codex:session")
			require.NoError(t, err)
			assert.Empty(t, list.Commits)
			insertSession(t, d, "codex:session", "project-a", func(s *Session) { s.Agent = "codex" })
			require.NoError(t, d.DeleteSession(t.Context(), "codex:session"))
			found, err = d.CommitSessionLinks(t.Context(), []string{sha})
			require.NoError(t, err)
			require.Len(t, found.Commits[0].Links, 1)
			assert.Equal(t, "codex:session", found.Commits[0].Links[0].SessionID)
			assert.True(t, found.Commits[0].Links[0].Missing)
		})
	}
}

func TestTrailerCommitLinksLookupWork(t *testing.T) {
	for _, lookup := range []string{"sha", "session", "empty SHA list"} {
		t.Run(lookup, func(t *testing.T) {
			d := testDB(t)
			const owner = "host-a~codex:session"
			insertSession(t, d, owner, "project-a", func(s *Session) { s.Agent = "codex" })
			sha := strings.Repeat("a", 40)
			require.NoError(t, d.StoreTrailerCommitLinks(t.Context(), "repo", []gitlog.TrailerCommit{{SHA: sha, Sessions: []string{"codex:session"}, CommittedAt: tsZero}}))
			// Count SQLite session reads during query preparation, avoiding timing thresholds.
			d.reader.Load().SetMaxOpenConns(1)
			reads := 0
			conn, err := d.getReader().Conn(t.Context())
			require.NoError(t, err)
			require.NoError(t, conn.Raw(func(raw any) error {
				raw.(*diagnosticReaderConn).RegisterAuthorizer(func(op int, table, _, _ string) int {
					if op == sqlite3.SQLITE_READ && table == "sessions" {
						reads++
					}
					return sqlite3.SQLITE_OK
				})
				return nil
			}))
			require.NoError(t, conn.Close())
			read := func() int {
				t.Helper()
				reads = 0
				switch lookup {
				case "sha":
					found, err := d.CommitSessionLinks(t.Context(), []string{sha[:7], sha})
					require.NoError(t, err)
					require.Len(t, found.Commits, 2)
					for _, match := range found.Commits {
						require.Len(t, match.Links, 1)
						assert.Equal(t, owner, match.Links[0].SessionID)
					}
				case "session":
					found, err := d.SessionCommitLinks(t.Context(), owner)
					require.NoError(t, err)
					require.Len(t, found.Commits, 1)
					assert.Equal(t, sha, found.Commits[0].SHA)
				case "empty SHA list":
					found, err := d.CommitSessionLinks(t.Context(), nil)
					require.NoError(t, err)
					assert.Empty(t, found.Commits)
				}
				return reads
			}
			baseline := read()
			var unrelated []gitlog.TrailerCommit
			for i := range 30 {
				unrelated = append(unrelated, gitlog.TrailerCommit{SHA: strings.Repeat("b", 40), Sessions: []string{fmt.Sprintf("codex:unrelated-%d", i)}, CommittedAt: tsZero})
			}
			require.NoError(t, d.StoreTrailerCommitLinks(t.Context(), "repo", unrelated))
			assert.LessOrEqual(t, read(), baseline, "unrelated trailer owners must not add session lookups")
		})
	}
}

func TestTrailerCommitLinksPromptAndPrecedence(t *testing.T) {
	for _, sha := range []string{"abc1234", "abc1234" + strings.Repeat("0", 33)} {
		t.Run(sha, func(t *testing.T) {
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
