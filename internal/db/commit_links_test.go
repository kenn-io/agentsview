package db

import (
	"encoding/json/v2"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
)

func commitMessages(id, input, result string) []Message {
	return []Message{
		userMsg(id, 0, "Record the parser fix"),
		{SessionID: id, Ordinal: 1, Role: "user", IsSystem: true, Content: "system context"},
		{SessionID: id, Ordinal: 2, Role: "assistant", Timestamp: tsZero, HasToolUse: true, ToolCalls: []ToolCall{{SessionID: id, ToolName: "Bash", Category: "Bash", ToolUseID: "commit-call", InputJSON: input, ResultContent: result, ResultEvents: []ToolResultEvent{{Source: "tool_result", Content: result}}}}},
	}
}

func TestCommitLinksLateResult(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "claude:commit", "project-a")
	insertMessages(t, d, commitMessages("claude:commit", `{"command":"git commit -m fix"}`, "")...)
	before, err := d.SessionCommitLinks(t.Context(), "claude:commit")
	require.NoError(t, err)
	assert.Empty(t, before.Commits)
	_, err = d.WriteSessionIncremental(t.Context(), "claude:commit", nil, IncrementalSessionUpdate{
		MsgCount: 3, NextOrdinal: 3, SubagentLinks: []ToolCallSubagentLink{{ToolUseID: "commit-call", HasResult: true, ResultContent: "[main abc1234] Fix parser", ResultContentLen: len("[main abc1234] Fix parser")}},
	})
	require.NoError(t, err)
	found, err := d.CommitSessionLinks(t.Context(), []string{"ABC12340000000000000000000000000000000000", "abc1234"})
	require.NoError(t, err)
	require.Len(t, found.Commits, 2)
	for _, match := range found.Commits {
		require.Len(t, match.Links, 1)
		l := match.Links[0]
		assert.Equal(t, "claude:commit", l.SessionID)
		assert.Equal(t, "abc1234", l.SHA)
		assert.Equal(t, 2, l.MessageOrdinal)
		assert.Equal(t, 0, l.CallIndex)
		assert.Equal(t, "commit-call", l.ToolUseID)
		assert.Equal(t, "transcript", l.Source)
		assert.Equal(t, tsZero, l.CommittedAt)
		assert.False(t, l.Ambiguous)
		require.NotNil(t, l.Turn)
		assert.Equal(t, 0, l.Turn.Ordinal)
		assert.Equal(t, "Record the parser fix", l.Turn.Text)
	}
}

func TestCommitLinksParsing(t *testing.T) {
	for _, tt := range []struct {
		name, command, result, repository string
		count                             int
	}{
		{"commit", "git commit -m fix", "[main abc1234] Fix parser", "/work", 1},
		{"env", "GIT_X=1 git commit -m fix", "[main abc1234] Fix parser", "/work", 1},
		{"absolute git", "/usr/bin/git commit -m fix", "[main abc1234] Fix parser", "/work", 1},
		{"shell wrapper", "bash -lc 'git commit -m fix'", "[main abc1234] Fix parser", "/work", 1},
		{"python", `python3 -c 'subprocess.run(["git","commit","-m","fix"])'`, "[main abc1234] Fix parser", "/work", 1},
		{"heredoc apostrophe", "cat <<'EOF'\nIt's fixed\nEOF\ngit commit -m fix", "[main abc1234] Fix parser", "/work", 1},
		{"tilde", "cd ~/x && git commit -m fix", "[main abc1234] Fix parser", "~/x", 1},
		{"nonleading cd", "git add . && cd /elsewhere && git commit -m fix", "[main abc1234] Fix parser", "/work", 1},
		{"quiet", "git commit -q -m fix", "", "", 0},
		{"quiet HEAD", "git commit -q -m fix && git rev-parse HEAD", "abc1234000000000000000000000000000000000\n", "/work", 1},
		{"failed quiet HEAD", "git commit -q -m fix; git rev-parse HEAD", "nothing to commit\nabc1234000000000000000000000000000000000\n", "", 0},
		{"failed quiet log", "git commit -q -m fix; git log -1 --oneline", "nothing to commit\nabc1234 Previous commit\n", "", 0},
		{"quiet earlier readback", "git rev-parse HEAD; git commit -q -m fix", "abc1234000000000000000000000000000000000\n", "", 0},
		{"quiet recovered failure", "git commit -q -m fix || true && git log -1 --oneline", "abc1234 Previous commit\n", "", 0},
		{"quiet quoted separator", `git commit -q -m 'fix && git rev-parse HEAD'; git rev-parse HEAD`, "abc1234000000000000000000000000000000000\n", "", 0},
		{"quiet log", "git commit -q -m fix && git log -1 --oneline", "abc1234 Fix parser\n", "/work", 1},
		{"quiet log reordered", "git commit --quiet -m fix && git log --oneline -1", "abc1234 Fix parser\n", "/work", 1},
		{"quiet log n", "git commit -q -m fix && git log -n 1 --oneline", "abc1234 Fix parser\n", "/work", 1},
		{"quiet log oneline n", "git commit -q -m fix && git log --oneline -n 1", "abc1234 Fix parser\n", "/work", 1},
		{"quiet unrelated SHA", "git commit -q -m fix && cat sha.txt", "abc1234000000000000000000000000000000000\n", "", 0},
		{"quiet ancestor", "git commit -q -m fix && git rev-parse HEAD~1", "abc1234000000000000000000000000000000000\n", "", 0},
		{"quiet log many", "git commit -q -m fix && git log -3 --oneline", "abc1234 Fix parser\n", "", 0},
		{"quiet multiple lines", "git commit -q -m fix && git log -1 --oneline", "abc1234 Fix parser\ndef5678 Other commit\n", "", 0},
		{"failed", "git commit -m fix", "nothing to commit", "", 0},
		{"printed", "cat out.txt", "[main abc1234] Fix parser", "", 0},
		{"printed commit documentation", "cat docs/git-commit.md", "[main abc1234] Fix parser", "", 0},
		{"shown commit documentation", "git show HEAD:docs/commit.md", "[main abc1234] Fix parser", "", 0},
		{"log grep", "git log --grep=commit", "[main abc1234] Fix parser", "", 0},
		{"commit filename", "git commit.md", "[main abc1234] Fix parser", "", 0},
		{"quoted directory", `git -C "/tmp/my repo" commit -m fix`, "[main abc1234] Fix parser", "/tmp/my repo", 1},
		{"cd", "cd /repo && git commit -m fix", "[main abc1234] Fix parser", "/repo", 1},
		{"newline", "git add .\ngit commit -m fix", "[main abc1234] Fix parser", "/work", 1},
		{"heredoc", "git commit -m \"$(cat <<'EOF'\nFix parser\nEOF\n)\"", "[main abc1234] Fix parser", "/work", 1},
		{"cherry-pick", "git cherry-pick abc1234", "[main abc1234] Fix parser", "", 0},
		{"multiple", "git commit -m one; git commit -m two", "[main (root-commit) abc1234] First\n[main def5678] Second\n[main def5678] Second", "/work", 2},
		{"quoted operators", `git -C "/tmp/a;b" commit -m "x && y"`, "[feature/x abc1234] Fix parser", "/tmp/a;b", 1},
		{"relative directory", "cd sub && git -C child commit -m fix", "[main abc1234] Fix parser", "/work/sub/child", 1},
		{"global option", `git -c user.name=Example --no-pager commit -m fix`, "[main abc1234] Fix parser", "/work", 1},
		{"quoted global option", `git -c 'user.name=Example User' commit -m fix`, "[main abc1234] Fix parser", "/work", 1},
		{"global directory option", `git --git-dir=/repo/.git commit -m fix`, "[main abc1234] Fix parser", "/work", 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "s1", "project-a", func(s *Session) { s.Cwd = "/work" })
			input, err := json.Marshal(map[string]string{"command": tt.command})
			require.NoError(t, err)
			insertMessages(t, d, commitMessages("s1", string(input), tt.result)...)
			got, err := d.SessionCommitLinks(t.Context(), "s1")
			require.NoError(t, err)
			require.Len(t, got.Commits, tt.count)
			if tt.count > 0 {
				assert.Equal(t, tt.repository, got.Commits[0].Repository)
				if strings.HasPrefix(tt.name, "quiet") {
					assert.Empty(t, got.Commits[0].Branch)
				}
			}
		})
	}

	for _, tt := range []struct{ name, input, result, repository string }{
		{"Codex JavaScript workdir", `text(await tools.exec_command({cmd:"git commit -m fix",workdir:"/repo"}))`, `[{"type":"input_text","text":"{\"output\":\"[main abc1234] Fix parser\\n\"}"}]`, "/repo"},
		{"JSON workdir", `{"cmd":"git commit -m fix","workdir":"/repo"}`, "[main abc1234] Fix parser", "/repo"},
		{"JSON cwd", `{"cmd":"git commit -m fix","cwd":"sub"}`, "[main abc1234] Fix parser", "/work/sub"},
		{"JavaScript cwd", `text(await tools.exec_command({cmd:'git commit -m fix',cwd:'/repo'}))`, `{"output":"[main abc1234] Fix parser\n"}`, "/repo"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "s1", "project-a", func(s *Session) { s.Cwd = "/work" })
			insertMessages(t, d, commitMessages("s1", tt.input, tt.result)...)
			got, err := d.SessionCommitLinks(t.Context(), "s1")
			require.NoError(t, err)
			require.Len(t, got.Commits, 1)
			assert.Equal(t, tt.repository, got.Commits[0].Repository)
		})
	}
}

func TestCommitLinksRepositoryPathStyle(t *testing.T) {
	for _, tt := range []struct{ name, cwd, command, repository string }{
		{"POSIX absolute", "/work", "cd /repo && git commit -m fix", "/repo"},
		{"POSIX relative", "/work", "cd sub && git -C ../child commit -m fix", "/work/child"},
		{"Windows absolute", `C:\work`, `cd D:\repo && git commit -m fix`, `D:\repo`},
		{"Windows relative", `C:\work`, `cd sub && git -C child commit -m fix`, `C:\work\sub\child`},
		{"Windows backslash relative", `C:\work`, `git -C sub\child commit -m fix`, `C:\work\sub\child`},
		{"Windows parent", `C:\work`, `git -C ..\repo commit -m fix`, `C:\repo`},
		{"Windows rooted", `C:\work`, `git -C \repo commit -m fix`, `C:\repo`},
		{"Windows slash drive", "C:/work", "cd D:/repo && git commit -m fix", "D:/repo"},
		{"Windows slash relative", "C:/work", "git -C sub commit -m fix", "C:/work/sub"},
		{"Windows UNC", `C:\work`, `cd \\server\share\repo && git commit -m fix`, `\\server\share\repo`},
		{"Windows UNC relative", `\\server\share\repo`, `git -C ..\child commit -m fix`, `\\server\share\child`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "s1", "project-a", func(s *Session) { s.Cwd = tt.cwd })
			input, err := json.Marshal(map[string]string{"command": tt.command})
			require.NoError(t, err)
			insertMessages(t, d, commitMessages("s1", string(input), "[main abc1234] Fix parser")...)
			got, err := d.SessionCommitLinks(t.Context(), "s1")
			require.NoError(t, err)
			require.Len(t, got.Commits, 1)
			assert.Equal(t, tt.repository, got.Commits[0].Repository)
		})
	}
}

func TestCommitLinksPurgeExcludedPages(t *testing.T) {
	d := testDB(t)
	const thread = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const page = thread + "_bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	for _, id := range []string{page, "retained"} {
		insertSession(t, d, id, "project-a")
		insertMessages(t, d, commitMessages(id, `{"command":"git commit -m fix"}`, "[main abc1234] Fix parser")...)
	}
	_, err := d.getWriter().ExecContext(t.Context(), `INSERT INTO excluded_sessions(id, include_codex_pages) VALUES (?, 1)`, thread)
	require.NoError(t, err)
	require.NoError(t, d.PurgeExcludedSessions(t.Context()))
	deleted, err := d.SessionCommitLinks(t.Context(), page)
	require.NoError(t, err)
	require.Len(t, deleted.Commits, 1)
	assert.True(t, deleted.Commits[0].Missing)
	assert.Nil(t, deleted.Commits[0].Turn)
	got, err := d.SessionCommitLinks(t.Context(), "retained")
	require.NoError(t, err)
	require.Len(t, got.Commits, 1)
}

func TestCommitLinksDeleteCodexThread(t *testing.T) {
	d := testDB(t)
	const thread = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const page = thread + "_bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	for _, id := range []string{thread, page} {
		insertSession(t, d, id, "project-a", func(s *Session) { s.Agent = "codex" })
		insertMessages(t, d, commitMessages(id, `{"command":"git commit -m fix"}`, "[main abc1234] Fix parser")...)
	}
	require.NoError(t, d.SoftDeleteSession(t.Context(), thread))
	_, err := d.getWriter().ExecContext(t.Context(), `UPDATE sessions SET trash_includes_codex_pages=1 WHERE id=?`, thread)
	require.NoError(t, err)
	require.NoError(t, d.DeleteSession(t.Context(), thread))
	got, err := d.CommitSessionLinks(t.Context(), []string{"abc1234"})
	require.NoError(t, err)
	require.Len(t, got.Commits[0].Links, 2)
	for _, link := range got.Commits[0].Links {
		assert.True(t, link.Missing)
		assert.Nil(t, link.Turn)
		session, err := d.GetSession(t.Context(), link.SessionID)
		require.NoError(t, err)
		assert.Nil(t, session)
	}
}

func TestCommitLinksQuietMetadata(t *testing.T) {
	for _, tt := range []struct{ name, command, result, branch, subject string }{
		{"HEAD", `git commit -q -m "Fix parser" && git rev-parse HEAD`, "abc1234000000000000000000000000000000000\n", "feature/parser", "Fix parser"},
		{"single quotes", `git commit -q -m 'Fix parser' && git rev-parse HEAD`, "abc1234000000000000000000000000000000000\n", "feature/parser", "Fix parser"},
		{"quoted subject", `git commit -q -m 'Fix "parser"' && git rev-parse HEAD`, "abc1234000000000000000000000000000000000\n", "feature/parser", `Fix "parser"`},
		{"escaped quotes", `git commit -q -m "Fix \"parser\"" && git rev-parse HEAD`, "abc1234000000000000000000000000000000000\n", "feature/parser", `Fix "parser"`},
		{"own message", `echo -m unrelated; git commit -q -m fix && git rev-parse HEAD`, "abc1234000000000000000000000000000000000\n", "feature/parser", "fix"},
		{"first message", "git commit -q -m 'Fix parser\n\nDetails' -m 'More details' && git rev-parse HEAD", "abc1234000000000000000000000000000000000\n", "feature/parser", "Fix parser"},
		{"readback subject", `git commit -q -m fix && git log -1 --oneline`, "abc1234 Fix parser\n", "feature/parser", "Fix parser"},
		{"no branch", `git commit -q -m fix && git rev-parse HEAD`, "abc1234000000000000000000000000000000000\n", "", "fix"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "s1", "project-a", func(s *Session) { s.GitBranch = tt.branch })
			input, err := json.Marshal(map[string]string{"command": tt.command})
			require.NoError(t, err)
			insertMessages(t, d, commitMessages("s1", string(input), tt.result)...)
			got, err := d.SessionCommitLinks(t.Context(), "s1")
			require.NoError(t, err)
			require.Len(t, got.Commits, 1)
			assert.Equal(t, tt.branch, got.Commits[0].Branch)
			assert.Equal(t, tt.subject, got.Commits[0].Subject)
		})
	}
}

func TestCommitLinksLookupAndLifecycle(t *testing.T) {
	d := testDB(t)
	for _, id := range []string{"s1", "s2"} {
		insertSession(t, d, id, "project-a")
		insertMessages(t, d, commitMessages(id, `{"cmd":"git commit -m fix"}`, "[main abc123456] Fix parser")...)
	}
	shared, err := d.CommitSessionLinks(t.Context(), []string{"abc1234", "deadbee"})
	require.NoError(t, err)
	require.Len(t, shared.Commits[0].Links, 2)
	assert.False(t, shared.Commits[0].Links[0].Ambiguous)
	assert.Empty(t, shared.Commits[1].Links)
	insertSession(t, d, "s3", "project-a")
	insertMessages(t, d, commitMessages("s3", `{"command":"git commit -m other"}`, "[main abc1234ff] Other fix")...)
	clash, err := d.CommitSessionLinks(t.Context(), []string{"abc1234"})
	require.NoError(t, err)
	require.Len(t, clash.Commits[0].Links, 3)
	for _, l := range clash.Commits[0].Links {
		assert.True(t, l.Ambiguous)
	}
	for _, shas := range [][]string{{"short"}, {"abc123z"}, make([]string, 201)} {
		_, err := d.CommitSessionLinks(t.Context(), shas)
		require.ErrorIs(t, err, ErrInvalidCommitSHA)
	}
	require.NoError(t, d.SoftDeleteSession(t.Context(), "s1"))
	trash, err := d.SessionCommitLinks(t.Context(), "s1")
	require.NoError(t, err)
	require.Len(t, trash.Commits, 1)
	assert.True(t, trash.Commits[0].Missing)
	assert.Nil(t, trash.Commits[0].Turn)
	_, err = d.RestoreSession(t.Context(), "s1")
	require.NoError(t, err)
	restored, err := d.SessionCommitLinks(t.Context(), "s1")
	require.NoError(t, err)
	assert.False(t, restored.Commits[0].Missing)
	assert.NotNil(t, restored.Commits[0].Turn)
	require.NoError(t, d.DeleteSession(t.Context(), "s1"))
	deleted, err := d.CommitSessionLinks(t.Context(), []string{"abc123456"})
	require.NoError(t, err)
	require.Len(t, deleted.Commits[0].Links, 2)
	assert.True(t, deleted.Commits[0].Links[0].Missing)
	assert.Nil(t, deleted.Commits[0].Links[0].Turn)
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "s2", []Message{userMsg("s2", 0, "Rewritten prompt")}))
	rewritten, err := d.SessionCommitLinks(t.Context(), "s2")
	require.NoError(t, err)
	assert.Empty(t, rewritten.Commits)
}

func TestCommitLinksBackfill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	d, err := Open(t.Context(), path)
	require.NoError(t, err)
	insertSession(t, d, "s1", "project-a")
	insertMessages(t, d, commitMessages("s1", `{"command":"git commit -m fix"}`, "[main abc1234] Fix parser")...)
	_, err = d.getWriter().Exec(t.Context(), `DELETE FROM commit_links`)
	require.NoError(t, err)
	_, err = d.getWriter().Exec(t.Context(), `DELETE FROM stats WHERE key=?`, commitLinksBackfillKey)
	require.NoError(t, err)
	require.NoError(t, d.Close())
	d, err = Open(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	got, err := d.SessionCommitLinks(t.Context(), "s1")
	require.NoError(t, err)
	require.Len(t, got.Commits, 1)
	assert.Equal(t, "abc1234", got.Commits[0].SHA)
}

func TestCommitLinksResyncAndPolicy(t *testing.T) {
	old := testDB(t)
	for _, id := range []string{"orphan", "trash", "deleted"} {
		insertSession(t, old, id, "project-a")
		insertMessages(t, old, commitMessages(id, `{"command":"git commit -m fix"}`, "[main abc1234] Fix parser")...)
	}
	require.NoError(t, old.SoftDeleteSession(t.Context(), "trash"))
	require.NoError(t, old.DeleteSession(t.Context(), "deleted"))
	for _, policy := range []config.ArchiveContent{config.ArchiveContentFull, config.ArchiveContentTranscripts, config.ArchiveContentUsage} {
		t.Run(string(policy), func(t *testing.T) {
			fresh := testDB(t)
			fresh.SetArchiveContent(policy)
			_, err := fresh.CopyTrashedDataFrom(old.Path())
			require.NoError(t, err)
			_, err = fresh.CopyOrphanedDataFrom(old.Path())
			require.NoError(t, err)
			require.NoError(t, fresh.CopyExcludedSessionsFrom(old.Path()))
			got, err := fresh.CommitSessionLinks(t.Context(), []string{"abc1234"})
			require.NoError(t, err)
			if policy == config.ArchiveContentFull {
				require.Len(t, got.Commits[0].Links, 3)
				assert.True(t, got.Commits[0].Links[0].Missing)
				assert.False(t, got.Commits[0].Links[1].Missing)
				assert.True(t, got.Commits[0].Links[2].Missing)
			} else {
				assert.Empty(t, got.Commits[0].Links)
			}
		})
	}
}

func TestCommitLinksPromptLimitAndPolicy(t *testing.T) {
	for _, policy := range []config.ArchiveContent{config.ArchiveContentFull, config.ArchiveContentTranscripts, config.ArchiveContentUsage} {
		t.Run(string(policy), func(t *testing.T) {
			d := testDB(t)
			d.SetArchiveContent(policy)
			insertSession(t, d, "s1", "project-a")
			messages := commitMessages("s1", `{"command":"git commit -m fix"}`, "[main abc1234] Fix parser")
			messages[0].Content = strings.Repeat("é", 1100)
			insertMessages(t, d, messages...)
			got, err := d.SessionCommitLinks(t.Context(), "s1")
			require.NoError(t, err)
			if policy == config.ArchiveContentFull {
				require.Len(t, got.Commits, 1)
				require.NotNil(t, got.Commits[0].Turn)
				assert.Equal(t, strings.Repeat("é", 1000), got.Commits[0].Turn.Text)
			} else {
				assert.Empty(t, got.Commits)
			}
		})
	}
}

func TestCommitLinksDedupAndRepeats(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "project-a")
	messages := commitMessages("s1", `{"command":"git commit -m fix"}`, "[main abc1234] Fix parser\r\n[main def5678] Other fix\r\n")
	// Blank and tool-result user messages must not replace the real prompt.
	messages = append(messages, Message{SessionID: "s1", Ordinal: 3, Role: "user", Content: " "}, Message{SessionID: "s1", Ordinal: 4, Role: "user", SourceSubtype: "tool_result", Content: "Tool output"})
	repeat := commitMessages("s1", `{"command":"git commit -m repeat; tail commit.log"}`, "[main abc1234] Fix parser\n[main 123abcd] Third fix")[2]
	repeat.Ordinal = 5
	repeat.ToolCalls[0].ToolUseID = "repeat"
	messages = append(messages, repeat)
	insertMessages(t, d, messages...)
	var raw string
	require.NoError(t, d.getReader().QueryRowContext(t.Context(), `SELECT COALESCE(result_content,'') FROM tool_calls WHERE session_id='s1' ORDER BY message_id LIMIT 1`).Scan(&raw))
	assert.Empty(t, raw)
	got, err := d.SessionCommitLinks(t.Context(), "s1")
	require.NoError(t, err)
	require.Len(t, got.Commits, 3)
	assert.Equal(t, "Fix parser", got.Commits[0].Subject)
	assert.Equal(t, 2, got.Commits[0].MessageOrdinal)
	require.NotNil(t, got.Commits[0].Turn)
	assert.Equal(t, 0, got.Commits[0].Turn.Ordinal)
	assert.Equal(t, "Other fix", got.Commits[1].Subject)
	require.NotNil(t, got.Commits[2].Turn)
	assert.Equal(t, 0, got.Commits[2].Turn.Ordinal)
	_, err = d.DeleteParserExcludedSessions(t.Context(), []string{"s1"})
	require.NoError(t, err)
	var count int
	require.NoError(t, d.getReader().QueryRowContext(t.Context(), `SELECT count(*) FROM commit_links WHERE session_id='s1'`).Scan(&count))
	assert.Zero(t, count)
}

func TestCommitLinksPolled(t *testing.T) {
	for _, result := range []string{
		`{"output":"[main abc1234] Fix parser\n"}`,
		`[{"type":"input_text","text":"{\"output\":\"[main abc1234] Fix parser\\n\"}"}]`,
	} {
		t.Run(result, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "s1", "project-a", func(s *Session) { s.Cwd = "/work" })
			messages := commitMessages("s1", `text(await tools.exec_command({cmd:"git commit -m fix",workdir:"/repo"}))`, `{"session_id":7,"output":""}`)
			messages = append(messages, userMsg("s1", 3, "A later prompt"))
			for i, input := range []string{`text(await tools.write_stdin({session_id:8,chars:""}))`, `text(await tools.write_stdin({session_id:7,chars:""}))`, `text(await tools.write_stdin({session_id:9,chars:""}))`} {
				poll := commitMessages("s1", input, result)[2]
				poll.Ordinal = 4 + i
				poll.Timestamp = "2026-01-02T00:00:00Z"
				poll.ToolCalls[0].ToolUseID = "poll"
				messages = append(messages, poll)
			}
			// An unmatched poll must not manufacture a commit link.
			messages[4].ToolCalls[0].ResultContent = `{"output":""}`
			messages[4].ToolCalls[0].ResultEvents[0].Content = messages[4].ToolCalls[0].ResultContent
			messages[6].ToolCalls[0].ResultContent = strings.ReplaceAll(result, "abc1234", "def5678")
			messages[6].ToolCalls[0].ResultEvents[0].Content = messages[6].ToolCalls[0].ResultContent
			insertMessages(t, d, messages[:5]...)
			before, err := d.SessionCommitLinks(t.Context(), "s1")
			require.NoError(t, err)
			assert.Empty(t, before.Commits)
			insertMessages(t, d, messages[5:]...)
			got, err := d.SessionCommitLinks(t.Context(), "s1")
			require.NoError(t, err)
			require.Len(t, got.Commits, 1)
			link := got.Commits[0]
			assert.Equal(t, "abc1234", link.SHA)
			assert.Equal(t, 2, link.MessageOrdinal)
			assert.Equal(t, "commit-call", link.ToolUseID)
			assert.Equal(t, tsZero, link.CommittedAt)
			assert.Equal(t, "/repo", link.Repository)
			require.NotNil(t, link.Turn)
			assert.Equal(t, "Record the parser fix", link.Turn.Text)
		})
	}
}

func TestCommitLinksNativePollOwnership(t *testing.T) {
	for _, tt := range []struct {
		name, initial, intermediate string
		codeMode                    bool
	}{
		{"JSON process", `{"session_id":7,"output":""}`, `{"session_id":7,"output":"still running"}`, false},
		{"text process", "Chunk ID: abc\nWall time: 1 seconds\nProcess running with session ID 7\nFinal output:\n", "Chunk ID: def\nWall time: 1 seconds\nProcess running with session ID 7\nFinal output:\nstill running", false},
		{"wrapped text process", `[{"type":"input_text","text":"Process running with session ID 7\nFinal output:\n"}]`, `[{"type":"input_text","text":"Process running with session ID 7\nFinal output:\nstill running"}]`, false},
		{"partial summary", `{"session_id":7,"output":"[main 123abcd] First commit\n"}`, `{"session_id":7,"output":"still running"}`, false},
		{"code mode partial summary", `{"session_id":7,"output":"[main 123abcd] First commit\n"}`, `{"session_id":7,"output":"still running"}`, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "s1", "project-a")
			messages := commitMessages("s1", `{"cmd":"git commit -m first && git commit -m second","workdir":"/repo"}`, tt.initial)
			messages[2].ToolCalls[0].ToolName = "exec_command"
			if tt.codeMode {
				messages[2].ToolCalls[0].ToolName = "exec"
				messages[2].ToolCalls[0].InputJSON = `text(await tools.exec_command({cmd:"git commit -m first && git commit -m second",workdir:"/repo"}))`
			}
			messages = append(messages, userMsg("s1", 3, "Later prompt"))
			for i, output := range []string{tt.intermediate, "Chunk ID: end\nWall time: 1 seconds\nProcess exited with code 0\nFinal output:\n[main abc1234] Second commit\n"} {
				poll := commitMessages("s1", `{"session_id":7,"chars":""}`, output)[2]
				poll.Ordinal = 4 + i
				poll.Timestamp = "2026-01-02T00:00:00Z"
				poll.ToolCalls[0].ToolName = "write_stdin"
				if tt.codeMode {
					poll.ToolCalls[0].ToolName = "exec"
					poll.ToolCalls[0].InputJSON = `text(await tools.write_stdin({session_id:7,chars:""}))`
				}
				poll.ToolCalls[0].ToolUseID = "poll"
				messages = append(messages, poll)
			}
			insertMessages(t, d, messages...)
			got, err := d.CommitSessionLinks(t.Context(), []string{"abc1234"})
			require.NoError(t, err)
			require.Len(t, got.Commits[0].Links, 1)
			link := got.Commits[0].Links[0]
			assert.Equal(t, 2, link.MessageOrdinal)
			assert.Equal(t, 0, link.CallIndex)
			assert.Equal(t, "commit-call", link.ToolUseID)
			assert.Equal(t, tsZero, link.CommittedAt)
			assert.Equal(t, "/repo", link.Repository)
			require.NotNil(t, link.Turn)
			assert.Equal(t, "Record the parser fix", link.Turn.Text)
		})
	}
}
