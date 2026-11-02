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
		{"quoted directory", `git -C "/tmp/my repo" commit -m fix`, "[main abc1234] Fix parser", "/tmp/my repo", 1},
		{"cd", "cd /repo && git commit -m fix", "[main abc1234] Fix parser", "/repo", 1},
		{"newline", "git add .\ngit commit -m fix", "[main abc1234] Fix parser", "/work", 1},
		{"heredoc", "git commit -m \"$(cat <<'EOF'\nFix parser\nEOF\n)\"", "[main abc1234] Fix parser", "/work", 1},
		{"cherry-pick", "git cherry-pick abc1234", "[main abc1234] Fix parser", "", 0},
		{"multiple", "git commit -m one; git commit -m two", "[main (root-commit) abc1234] First\n[main def5678] Second\n[main def5678] Second", "/work", 2},
		{"quoted operators", `git -C "/tmp/a;b" commit -m "x && y"`, "[feature/x abc1234] Fix parser", "/tmp/a;b", 1},
		{"relative directory", "cd sub && git -C child commit -m fix", "[main abc1234] Fix parser", "/work/sub/child", 1},
		{"global option", `git -c user.name=Example --no-pager commit -m fix`, "[main abc1234] Fix parser", "/work", 1},
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
			// An unmatched poll owns its summary; use a distinct SHA so it
			// doesn't hide the paired poll through first-occurrence dedup.
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
			require.Len(t, got.Commits, 2)
			link := got.Commits[0]
			assert.Equal(t, "abc1234", link.SHA)
			assert.Equal(t, 2, link.MessageOrdinal)
			assert.Equal(t, "commit-call", link.ToolUseID)
			assert.Equal(t, tsZero, link.CommittedAt)
			assert.Equal(t, "/repo", link.Repository)
			require.NotNil(t, link.Turn)
			assert.Equal(t, "Record the parser fix", link.Turn.Text)
			assert.Equal(t, 6, got.Commits[1].MessageOrdinal)
		})
	}
}
