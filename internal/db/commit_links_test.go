package db

import (
	"encoding/json/v2"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	gitlog "go.kenn.io/agentsview/internal/db/git"
)

func commitMessages(id, input, result string) []Message {
	var events []ToolResultEvent
	if result != "" {
		events = []ToolResultEvent{{Source: "tool_result", Content: result}}
	}
	return []Message{
		userMsg(id, 0, "Record the parser fix"),
		{SessionID: id, Ordinal: 1, Role: "user", IsSystem: true, Content: "system context"},
		{SessionID: id, Ordinal: 2, Role: "assistant", Timestamp: tsZero, HasToolUse: true, ToolCalls: []ToolCall{{SessionID: id, ToolName: "Bash", Category: "Bash", ToolUseID: "commit-call", InputJSON: input, ResultContent: result, ResultEvents: events}}},
	}
}

func TestCommitLinksNonCommitCandidatesSkipResultReadsAndDelete(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "candidate", "project-a")
	insertMessages(t, d, commitMessages("candidate", `{"command":"git log -p --grep=commit"}`, "[main abc1234] Example commit")[2])
	conn, err := d.getWriter().Conn(t.Context())
	require.NoError(t, err)
	defer conn.Close()
	// Reject result reads and link deletion for candidates that cannot create commits.
	require.NoError(t, conn.Raw(func(raw any) error {
		raw.(*sqlite3.SQLiteConn).RegisterAuthorizer(func(op int, table, column, _ string) int {
			if op == sqlite3.SQLITE_READ && ((table == "tool_calls" && column == "result_content") || (table == "tool_result_events" && column == "content")) {
				return sqlite3.SQLITE_DENY
			}
			if op == sqlite3.SQLITE_DELETE && table == "commit_links" {
				return sqlite3.SQLITE_DENY
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	}))
	defer func() {
		require.NoError(t, conn.Raw(func(raw any) error {
			raw.(*sqlite3.SQLiteConn).RegisterAuthorizer(nil)
			return nil
		}))
	}()
	tx, err := conn.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	require.NoError(t, refreshSessionCommitLinksTx(tx, "candidate"))
}

func TestCommitLinksAppendWithoutCandidatesSkipsInputScan(t *testing.T) {
	for _, input := range []string{`{"command":"true"}`, `{"command":"git log --grep=commit"}`} {
		t.Run(input, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "candidate", "project-a")
			insertMessages(t, d, commitMessages("candidate", `{"command":"git commit -m fix"}`, "[main abc1234] Fix parser")...)
			conn, err := d.getWriter().Conn(t.Context())
			require.NoError(t, err)
			var sqlite *sqlite3.SQLiteConn
			require.NoError(t, conn.Raw(func(raw any) error {
				sqlite = raw.(*sqlite3.SQLiteConn)
				sqlite.RegisterAuthorizer(func(op int, table, column, _ string) int {
					if op == sqlite3.SQLITE_READ && table == "tool_calls" && column == "input_json" {
						return sqlite3.SQLITE_DENY
					}
					return sqlite3.SQLITE_OK
				})
				return nil
			}))
			require.NoError(t, conn.Close())
			defer sqlite.RegisterAuthorizer(nil)
			message := commitMessages("candidate", input, "")[2]
			message.Ordinal = 3
			_, err = d.WriteSessionIncremental(t.Context(), "candidate", []Message{message}, IncrementalSessionUpdate{MsgCount: 4, NextOrdinal: 4})
			require.NoError(t, err)
			links, err := d.SessionCommitLinks(t.Context(), "candidate")
			require.NoError(t, err)
			require.Len(t, links.Commits, 1)
			assert.Equal(t, "abc1234", links.Commits[0].SHA)
		})
	}
}

func TestCommitLinksIncrementalCandidates(t *testing.T) {
	for _, tt := range []struct {
		name, tool, input, initial string
		late, replace, subagent    bool
	}{
		{name: "commit", tool: "Bash", input: `{"command":"git commit -m fix"}`},
		{name: "shell poll", tool: "write_stdin", input: `{"session_id":7}`},
		{name: "code poll", tool: "exec", input: `text(await tools.write_stdin({session_id:7}))`},
		{name: "cell poll", tool: "wait", input: `{"cell_id":"cell-a"}`, initial: "Script running with cell ID cell-a"},
		{name: "late commit", tool: "Bash", input: `{"command":"git commit -m fix"}`, late: true},
		{name: "late subagent result", tool: "Bash", input: `{"command":"git commit -m fix"}`, late: true, subagent: true},
		{name: "late poll", tool: "write_stdin", input: `{"session_id":7}`, late: true},
		{name: "replacement", tool: "Bash", input: `{"command":"true"}`, initial: "[main abc1234] Fix parser", replace: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "candidate", "project-a")
			initial := tt.initial
			if initial == "" {
				initial = "Process running with session ID 7"
			}
			if !tt.subagent {
				insertMessages(t, d, commitMessages("candidate", `{"command":"git commit -m fix"}`, initial)...)
			}
			message := commitMessages("candidate", tt.input, "[main def5678] Next fix")[2]
			message.Ordinal = 3
			message.ToolCalls[0].ToolName = tt.tool
			message.ToolCalls[0].ToolUseID = "next-call"
			update := IncrementalSessionUpdate{MsgCount: 4, NextOrdinal: 4}
			if tt.subagent {
				message.Ordinal = 2
				message.ToolCalls[0].CallIndex = 1
				message.ToolCalls[0].ToolUseID = "commit-call"
				message.ToolCalls = append([]ToolCall{{SessionID: "candidate", ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"true"}`}}, message.ToolCalls...)
				update = IncrementalSessionUpdate{MsgCount: 3, NextOrdinal: 3}
			}
			messages := []Message{message}
			if tt.subagent {
				messages = commitMessages("candidate", "", "")[:2]
				messages = append(messages, message)
			}
			if tt.late {
				call := &message.ToolCalls[len(message.ToolCalls)-1]
				call.ResultContent = ""
				call.ResultEvents = nil
				insertMessages(t, d, messages...)
				messages = nil
				before, err := d.SessionCommitLinks(t.Context(), "candidate")
				require.NoError(t, err)
				assert.Empty(t, before.Commits)
				if tt.subagent {
					update.SubagentLinks = []ToolCallSubagentLink{{ToolUseID: "commit-call", HasResult: true, ResultContent: "[main def5678] Next fix", ResultContentLen: len("[main def5678] Next fix")}}
				} else {
					update.ToolCallResultUpdates = []ToolCallResultUpdate{{ToolUseID: "next-call", Position: ToolCallPosition{MessageOrdinal: 3}, Events: []ToolResultEvent{{Source: "tool_result", Content: "[main def5678] Next fix"}}}}
				}
			}
			if tt.replace {
				messages[0].Ordinal = 2
				update.ReplaceFromOrdinal = &messages[0].Ordinal
			}
			_, err := d.WriteSessionIncremental(t.Context(), "candidate", messages, update)
			require.NoError(t, err)
			links, err := d.SessionCommitLinks(t.Context(), "candidate")
			require.NoError(t, err)
			if tt.replace {
				assert.Empty(t, links.Commits)
			} else {
				require.Len(t, links.Commits, 1)
				link := links.Commits[0]
				assert.Equal(t, "def5678", link.SHA)
				assert.Equal(t, "candidate", link.SessionID)
				if tt.subagent {
					assert.Equal(t, 2, link.MessageOrdinal)
					assert.Equal(t, 1, link.CallIndex)
					assert.Equal(t, "commit-call", link.ToolUseID)
				}
				assert.False(t, link.Ambiguous)
			}
		})
	}
}

func TestCommitLinksParsing(t *testing.T) {
	for _, tt := range []struct {
		name              string
		results, commands []string
		count             int
	}{
		{"commit message forms", []string{"[main abc1234] Fix parser"}, []string{"git commit -m fix", "git commit -m \"$(cat <<'EOF'\nFix parser\nEOF\n)\""}, 1},
		{"absolute git", []string{"[main abc1234] Fix parser"}, []string{"/usr/bin/git commit -m fix"}, 1},
		{"shell wrapper", []string{"[main abc1234] Fix parser"}, []string{"bash -lc 'git commit -m fix'"}, 1},
		{"failed", []string{"nothing to commit"}, []string{"git commit -m fix"}, 0},
		{"printed", []string{"[main abc1234] Fix parser"}, []string{"cat out.txt"}, 0},
		{"printed commit documentation", []string{"[main abc1234] Fix parser"}, []string{"cat docs/git-commit.md"}, 0},
		{"searched commit documentation", []string{"[main 463dc4f] Example commit"}, []string{"grep -n 'git commit' docs/x.md; sed -n 20,24p docs/x.md"}, 0},
		{"shown commit documentation", []string{"[main abc1234] Fix parser"}, []string{"git show HEAD:docs/commit.md"}, 0},
		{"log grep", []string{"[main abc1234] Fix parser"}, []string{"git log --grep=commit"}, 0},
		{"commit filename", []string{"[main abc1234] Fix parser"}, []string{"git commit.md"}, 0},
		{"whitespace prefix", []string{"[main abc1234] Fix parser"}, []string{"git add .\ngit commit -m fix", "cat <<'EOF'\nIt's fixed\nEOF\ngit commit -m fix", "GIT_X=1 git commit -m fix"}, 1},
		{"cherry-pick", []string{"[main abc1234] Fix parser"}, []string{"git cherry-pick abc1234"}, 0},
		{"multiple", []string{"[main (root-commit) abc1234] First\n[main def5678] Second"}, []string{"git commit -m one; git commit -m two"}, 2},
		{"global option", []string{"[main abc1234] Fix parser"}, []string{`git -c user.name=Example --no-pager commit -m fix`, `git -c 'user.name=Example User' commit -m fix`}, 1},
		{"flag-only global options", []string{"[main abc1234] Fix parser"}, []string{"git --no-advice commit -m fix", "git -p commit -m fix", "git -P commit -m fix"}, 1},
		{"output delta", []string{`{"session_id":null,"state":"completed","exit_code":0,"output_delta":"[main abc1234] Fix parser\n"}`, `{"session_id":"shell-7","state":"completed","exit_code":0,"output_delta":"[main abc1234] Fix parser\n"}`, `{"session_id":7,"state":"completed","exit_code":0,"output_delta":"[main abc1234] Fix parser\n"}`}, []string{"git commit -m fix"}, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, result := range tt.results {
				for _, command := range tt.commands {
					d := testDB(t)
					insertSession(t, d, "s1", "project-a", func(s *Session) { s.Cwd = "/work" })
					input, err := json.Marshal(map[string]string{"command": command})
					require.NoError(t, err)
					insertMessages(t, d, commitMessages("s1", string(input), result)...)
					got, err := d.SessionCommitLinks(t.Context(), "s1")
					require.NoError(t, err)
					require.Len(t, got.Commits, tt.count)
					if tt.count == 1 {
						assert.Equal(t, "abc1234", got.Commits[0].SHA)
						assert.Equal(t, "main", got.Commits[0].Branch)
						assert.Equal(t, "Fix parser", got.Commits[0].Subject)
					}
				}
			}
		})
	}
}

func TestCommitLinksCodeModeFixedCommand(t *testing.T) {
	for _, tt := range []struct {
		name, input string
		count       int
	}{
		{"double quoted", `text(await tools.exec_command({cmd:"git commit -m \"Document tools.exec_command\"",workdir:"/repo"}))`, 1},
		{"single quoted", `text(await tools.exec_command({cmd:'git commit -m fix',workdir:'/repo'}))`, 1},
		{"template literal", "text(await tools.exec_command({cmd:`git commit -m fix`,workdir:\"/repo\"}))", 1},
		{"loop", `for (let i = 0; i < 1; i++) text(await tools.exec_command({cmd:"git commit -m fix",workdir:"/repo"}))`, 1},
		{"interpolated", "text(await tools.exec_command({cmd:`git commit -m ${subject}`,workdir:\"/repo\"}))", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "s1", "project-a", func(s *Session) { s.Cwd = "/work" })
			messages := commitMessages("s1", tt.input, `[{"type":"input_text","text":"{\"output\":\"[main abc1234] Fix parser\\n\"}"}]`)
			messages[2].ToolCalls[0].ToolName = "exec"
			insertMessages(t, d, messages...)
			got, err := d.SessionCommitLinks(t.Context(), "s1")
			require.NoError(t, err)
			require.Len(t, got.Commits, tt.count)
			if tt.count == 1 {
				assert.Equal(t, "abc1234", got.Commits[0].SHA)
				assert.Equal(t, "/repo", got.Commits[0].Repository)
			}
		})
	}
}

func TestCommitLinksRepositoryPathStyle(t *testing.T) {
	for _, tt := range []struct {
		name, cwd, command, repository string
	}{
		{"POSIX absolute", "/work", "cd /repo && git commit -m fix", "/repo"},
		{"tilde", "/work", "cd ~/x && git commit -m fix", "~/x"},
		{"nonleading cd", "/work", "git add . && cd /elsewhere && git commit -m fix", "/elsewhere"},
		{"nonleading relative cd", "/work", "git add . && cd repo && git commit -m fix", "/work/repo"},
		{"last cd before commit", "/work", "cd /first && cd /repo && git commit -m fix && cd /later", "/repo"},
		{"cd after commit", "/work", "git commit -m fix && cd /later", "/work"},
		{"cd after semicolon", "/work", "git add .; cd '/my repo'; git commit -m fix", "/my repo"},
		{"cd after newline", "/work", "git add .\ncd /repo\ngit commit -m fix", "/repo"},
		{"quoted directory", "/work", `git -C "/tmp/my repo" commit -m fix`, "/tmp/my repo"},
		{"quoted operators", "/work", `git -C "/tmp/a;b" commit -m "x && y"`, "/tmp/a;b"},
		{"directory after global options", "/work", `git -c 'user.name=Example User' --no-pager -C /repo commit -m fix`, "/repo"},
		{"successive directories with global options", "/work", `git -C root -c 'user.name=-C decoy' --no-pager -C 'child repo' commit -m fix`, "/work/root/child repo"},
		{"absolute directory resets previous directory", "/work", `git -C root -C /repo -C child commit -m fix`, "/repo/child"},
		{"separate git invocations", "/work", `git -C /other add . && git -C root -C child commit -m fix`, "/work/root/child"},
		{"directory text in message", "/work", `git commit -m "Document git -C /other commit"`, "/work"},
		{"relative directory", "/work", "cd sub && git -C child commit -m fix", "/work/sub/child"},
		{"global directory option", "/work", `git --git-dir=/repo/.git commit -m fix`, "/work"},
		{"POSIX relative", "/work", "cd sub && git -C ../child commit -m fix", "/work/child"},
		{"Windows absolute", `C:\work`, `cd D:\repo && git commit -m fix`, `D:\repo`},
		{"Windows absolute slashes", "C:/work", "cd D:/repo && git commit -m fix", "D:/repo"},
		{"Windows relative", `C:\work`, `cd sub && git -C child commit -m fix`, `C:\work\sub\child`},
		{"Windows relative git directory", `C:\work`, `git -C sub\child commit -m fix`, `C:\work\sub\child`},
		{"Windows parent", `C:\work`, `git -C ..\repo commit -m fix`, `C:\repo`},
		{"Windows rooted", `C:\work`, `git -C \repo commit -m fix`, `C:\repo`},
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
	for _, tt := range []struct {
		name, repository, input, result string
	}{
		{"JSON workdir", "/repo", `{"cmd":"git commit -m fix","workdir":"/repo"}`, "[main abc1234] Fix parser"},
		{"JSON cwd", "/work/sub", `{"cmd":"git commit -m fix","cwd":"sub"}`, "[main abc1234] Fix parser"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "s1", "project-a", func(s *Session) { s.Cwd = "/work" })
			messages := commitMessages("s1", tt.input, tt.result)
			messages[2].ToolCalls[0].ToolName = "exec_command"
			insertMessages(t, d, messages...)
			got, err := d.SessionCommitLinks(t.Context(), "s1")
			require.NoError(t, err)
			require.Len(t, got.Commits, 1)
			assert.Equal(t, tt.repository, got.Commits[0].Repository)
		})
	}
}

func TestCommitLinksPurgeExcludedPages(t *testing.T) {
	for _, permanent := range []bool{false, true} {
		name := "purge"
		if permanent {
			name = "permanent delete"
		}
		t.Run(name, func(t *testing.T) {
			d := testDB(t)
			const thread = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
			const page = thread + "_bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
			for _, id := range []string{thread, page, "retained"} {
				insertSession(t, d, id, "project-a", func(s *Session) { s.Agent = "codex" })
				insertMessages(t, d, commitMessages(id, `{"command":"git commit -m fix"}`, "[main abc1234] Fix parser")...)
			}
			if permanent {
				require.NoError(t, d.SoftDeleteSession(t.Context(), thread))
				_, err := d.getWriter().ExecContext(t.Context(), `UPDATE sessions SET trash_includes_codex_pages=1 WHERE id=?`, thread)
				require.NoError(t, err)
				require.NoError(t, d.DeleteSession(t.Context(), thread))

				got, err := d.CommitSessionLinks(t.Context(), []string{"abc1234"})
				require.NoError(t, err)
				require.Len(t, got.Commits[0].Links, 3)
				for _, link := range got.Commits[0].Links[:2] {
					session, err := d.GetSession(t.Context(), link.SessionID)
					require.NoError(t, err)
					assert.Nil(t, session)
				}
			} else {
				_, err := d.getWriter().ExecContext(t.Context(), `INSERT INTO excluded_sessions(id, include_codex_pages) VALUES (?, 1)`, thread)
				require.NoError(t, err)
				require.NoError(t, d.PurgeExcludedSessions(t.Context()))
				deleted, err := d.SessionCommitLinks(t.Context(), page)
				require.NoError(t, err)
				assert.Empty(t, deleted.Commits)
				got, err := d.SessionCommitLinks(t.Context(), "retained")
				require.NoError(t, err)
				require.Len(t, got.Commits, 1)
			}
		})
	}
}

func TestCommitLinksAmbiguity(t *testing.T) {
	d := testDB(t)
	for _, id := range []string{"s1", "s2"} {
		insertSession(t, d, id, "project-a")
		insertMessages(t, d, commitMessages(id, `{"command":"git commit -m fix"}`, "[main abc123456] Fix parser")...)
	}
	oversized := make([]string, 201)
	for i := range oversized {
		oversized[i] = "abc1234"
	}
	for _, shas := range [][]string{{"short"}, {"abc123z"}, {"6f84"}, oversized} {
		_, err := d.CommitSessionLinks(t.Context(), shas)
		require.ErrorIs(t, err, ErrInvalidCommitSHA)
	}
	shared, err := d.CommitSessionLinks(t.Context(), []string{"abc1234", "deadbee"})
	require.NoError(t, err)
	require.Len(t, shared.Commits, 2)
	require.Len(t, shared.Commits[0].Links, 2)
	assert.False(t, shared.Commits[0].Links[0].Ambiguous)
	assert.Empty(t, shared.Commits[1].Links)
	insertSession(t, d, "s3", "project-a")
	insertMessages(t, d, commitMessages("s3", `{"command":"git commit -m other"}`, "[main abc1234ff] Other fix")...)
	clash, err := d.CommitSessionLinks(t.Context(), []string{"abc1234"})
	require.NoError(t, err)
	require.Len(t, clash.Commits, 1)
	require.Len(t, clash.Commits[0].Links, 3)
	for _, l := range clash.Commits[0].Links {
		assert.True(t, l.Ambiguous)
	}
	assert.ElementsMatch(t, []string{"abc123456", "abc123456", "abc1234ff"}, []string{clash.Commits[0].Links[0].SHA, clash.Commits[0].Links[1].SHA, clash.Commits[0].Links[2].SHA})

	t.Run("compatible SHA prefixes", func(t *testing.T) {
		d := testDB(t)
		insertSession(t, d, "s1", "project-a")
		insertMessages(t, d, commitMessages("s1", `{"command":"git commit -m fix"}`, "[main abc1234] Fix parser")...)
		found, err := d.CommitSessionLinks(t.Context(), []string{"ABC1234000000000000000000000000000000000", "abc1234"})
		require.NoError(t, err)
		require.Len(t, found.Commits, 2)
		for _, match := range found.Commits {
			require.Len(t, match.Links, 1)
			link := match.Links[0]
			assert.Equal(t, "abc1234", link.SHA)
			assert.Equal(t, "transcript", link.Source)
			assert.False(t, link.Ambiguous)
		}
		for _, sha := range []string{"abc1234ff7", "abc1234012345678901234567890123456789012"} {
			d := testDB(t)
			insertSession(t, d, "s1", "project-a")
			insertMessages(t, d, commitMessages("s1", `{"command":"git commit -m fix"}`, "[main abc1234] Fix parser")...)
			insertSession(t, d, "s2", "project-b")
			insertMessages(t, d, commitMessages("s2", `{"command":"git commit -m other"}`, "[main "+sha+"] Other fix")...)

			found, err := d.CommitSessionLinks(t.Context(), []string{sha})
			require.NoError(t, err)
			require.Len(t, found.Commits, 1)
			require.Len(t, found.Commits[0].Links, 2)
			assert.Equal(t, "abc1234", found.Commits[0].Links[0].SHA)
			assert.Equal(t, sha, found.Commits[0].Links[1].SHA)
			for _, link := range found.Commits[0].Links {
				assert.False(t, link.Ambiguous, "stored SHA %s", link.SHA)
			}
		}
	})
}

func TestCommitLinksLookupAndLifecycle(t *testing.T) {
	d := testDB(t)
	for _, id := range []string{"s1", "s2"} {
		insertSession(t, d, id, "project-a")
		insertMessages(t, d, commitMessages(id, `{"command":"git commit -m fix"}`, "[main abc123456] Fix parser")...)
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
	assert.Equal(t, "s1", deleted.Commits[0].Links[0].SessionID)
	assert.Equal(t, "abc123456", deleted.Commits[0].Links[0].SHA)
	assert.True(t, deleted.Commits[0].Links[0].Missing)
	assert.Nil(t, deleted.Commits[0].Links[0].Turn)
	assert.Equal(t, "s2", deleted.Commits[0].Links[1].SessionID)
	assert.False(t, deleted.Commits[0].Links[1].Missing)
	assert.NotNil(t, deleted.Commits[0].Links[1].Turn)
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "s2", []Message{userMsg("s2", 0, "Rewritten prompt")}))
	rewritten, err := d.SessionCommitLinks(t.Context(), "s2")
	require.NoError(t, err)
	assert.Empty(t, rewritten.Commits)
}

func TestCommitLinksDeleteParserExcludedSessions(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s3", "project-a")
	insertMessages(t, d, commitMessages("s3", `{"command":"git commit -m other"}`, "[main abc1234ff] Other fix")...)
	_, err := d.DeleteParserExcludedSessions(t.Context(), []string{"s3"})
	require.NoError(t, err)
	var count int
	require.NoError(t, d.getReader().QueryRowContext(t.Context(), `SELECT count(*) FROM commit_links WHERE session_id='s3'`).Scan(&count))
	assert.Zero(t, count)
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
	insertSession(t, old, "written", "project-a")
	for _, id := range []string{"orphan", "trash", "deleted"} {
		insertSession(t, old, id, "project-a")
		insertMessages(t, old, commitMessages(id, `{"command":"git commit -m fix"}`, "[main abc1234] Fix parser")...)
	}
	_, err := old.StoreTrailerCommitLinks(t.Context(), "gone", []gitlog.TrailerCommit{{SHA: strings.Repeat("a", 40), Sessions: []string{"claude:orphan"}, CommittedAt: tsZero}})
	require.NoError(t, err)
	require.NoError(t, old.SoftDeleteSession(t.Context(), "trash"))
	require.NoError(t, old.DeleteSession(t.Context(), "deleted"))
	for _, policy := range []config.ArchiveContent{config.ArchiveContentFull, config.ArchiveContentTranscripts, config.ArchiveContentUsage} {
		t.Run(string(policy), func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "s1", "project-a")
			messages := commitMessages("s1", `{"command":"git commit -m fix"}`, "[main abc1234] Fix parser")
			insertMessages(t, d, messages...)
			before, err := d.SessionCommitLinks(t.Context(), "s1")
			require.NoError(t, err)
			require.Len(t, before.Commits, 1)
			d.SetArchiveContent(policy)
			insertSession(t, d, "s2", "project-a")
			insertMessages(t, d, commitMessages("s2", `{"command":"git commit -m fix"}`, "[main abc1234] Fix parser")...)
			initial, err := d.SessionCommitLinks(t.Context(), "s2")
			require.NoError(t, err)
			if policy == config.ArchiveContentFull {
				require.Len(t, initial.Commits, 1)
			} else {
				assert.Empty(t, initial.Commits)
			}
			_, err = d.StoreTrailerCommitLinks(t.Context(), "repo", []gitlog.TrailerCommit{{SHA: strings.Repeat("b", 40), Sessions: []string{"claude:s2"}, CommittedAt: tsZero}})
			require.NoError(t, err)
			trailers, err := d.CommitSessionLinks(t.Context(), []string{strings.Repeat("b", 40)})
			require.NoError(t, err)
			require.Len(t, trailers.Commits, 1)
			if policy.UsageOnly() {
				assert.Empty(t, trailers.Commits[0].Links)
			} else {
				require.Len(t, trailers.Commits[0].Links, 1)
				assert.Equal(t, "trailer", trailers.Commits[0].Links[0].Source)
			}
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "s1", messages))
			got, err := d.SessionCommitLinks(t.Context(), "s1")
			require.NoError(t, err)
			if policy == config.ArchiveContentFull {
				require.Len(t, got.Commits, 1)
			} else {
				assert.Empty(t, got.Commits)
			}

			fresh := testDB(t)
			if policy.UsageOnly() {
				// Downgrade an archive that already holds evidence for a session being copied.
				_, err := fresh.StoreTrailerCommitLinks(t.Context(), "repo", []gitlog.TrailerCommit{{SHA: strings.Repeat("b", 40), Sessions: []string{"claude:written"}, CommittedAt: tsZero}})
				require.NoError(t, err)
				var rows int
				require.NoError(t, fresh.getReader().QueryRowContext(t.Context(), "SELECT count(*) FROM commit_trailers").Scan(&rows))
				require.Equal(t, 1, rows)
			}
			fresh.SetArchiveContent(policy)
			_, err = fresh.CopyTrashedDataFrom(t.Context(), old.Path())
			require.NoError(t, err)
			_, err = fresh.CopyOrphanedDataFrom(t.Context(), old.Path())
			require.NoError(t, err)
			require.NoError(t, fresh.CopyExcludedSessionsFrom(t.Context(), old.Path()))
			require.NoError(t, fresh.CopyCommitTrailersFrom(t.Context(), old.Path()))
			if policy.UsageOnly() {
				written, err := fresh.SessionCommitLinks(t.Context(), "written")
				require.NoError(t, err)
				assert.Empty(t, written.Commits)
				var rows int
				require.NoError(t, fresh.getReader().QueryRowContext(t.Context(), "SELECT count(*) FROM commit_trailers").Scan(&rows))
				assert.Zero(t, rows)
			}
			trailers, err = fresh.CommitSessionLinks(t.Context(), []string{strings.Repeat("a", 40)})
			require.NoError(t, err)
			if !policy.UsageOnly() {
				require.Len(t, trailers.Commits[0].Links, 1)
				assert.Equal(t, "orphan", trailers.Commits[0].Links[0].SessionID)
				assert.Equal(t, "trailer", trailers.Commits[0].Links[0].Source)
			} else {
				assert.Empty(t, trailers.Commits[0].Links)
			}
			copied, err := fresh.CommitSessionLinks(t.Context(), []string{"abc1234"})
			require.NoError(t, err)
			if policy == config.ArchiveContentFull {
				require.Len(t, copied.Commits[0].Links, 3)
				assert.True(t, copied.Commits[0].Links[0].Missing)
				assert.False(t, copied.Commits[0].Links[1].Missing)
				assert.True(t, copied.Commits[0].Links[2].Missing)
			} else {
				assert.Empty(t, copied.Commits[0].Links)
			}
		})
	}
}

func TestCommitLinksTurn(t *testing.T) {
	for _, tt := range []struct {
		name, prompt, context, want string
		isSystem                    bool
	}{
		{"legacy system context", "Record the parser fix", "<goal_context>Continue the current task</goal_context>", "Record the parser fix", false},
		{"prompt limit", strings.Repeat("é", 1100), "system context", strings.Repeat("é", 1000), true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "s1", "project-a")
			messages := commitMessages("s1", `{"command":"git commit -m fix"}`, "[main abc1234] Fix parser")
			messages[0].Content = tt.prompt
			messages[1].Content = tt.context
			messages[1].IsSystem = tt.isSystem
			insertMessages(t, d, messages...)
			got, err := d.SessionCommitLinks(t.Context(), "s1")
			require.NoError(t, err)
			require.Len(t, got.Commits, 1)
			require.NotNil(t, got.Commits[0].Turn)
			assert.Equal(t, 0, got.Commits[0].Turn.Ordinal)
			assert.Equal(t, tt.want, got.Commits[0].Turn.Text)
		})
	}
}

func TestCommitLinksDedupAndRepeats(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "project-a")
	messages := commitMessages("s1", `{"command":"git commit -m fix"}`, "[main abc1234] Fix parser\r\n[main def5678] Other fix\r\n[main def5678] Other fix\r\n")
	repeat := commitMessages("s1", `{"command":"git commit -m repeat"}`, "[main abc1234] Fix parser")[2]
	repeat.Ordinal = 3
	repeat.ToolCalls[0].ToolUseID = "repeat"
	messages = append(messages, repeat)
	insertMessages(t, d, messages...)
	got, err := d.SessionCommitLinks(t.Context(), "s1")
	require.NoError(t, err)
	require.Len(t, got.Commits, 2)
	assert.Equal(t, "Fix parser", got.Commits[0].Subject)
	assert.Equal(t, 2, got.Commits[0].MessageOrdinal)
	assert.Equal(t, "Other fix", got.Commits[1].Subject)
}

func TestCommitLinksSummaryRef(t *testing.T) {
	for _, tt := range []struct {
		ref  string
		want bool
	}{
		{"main", true},
		{"feature/parser", true},
		{"detached HEAD", true},
		{"HEAD -> x, origin/x", false},
		{"tag: v1", false},
		{"feature[old]", false},
		{"feature~1", false},
		{"feature^", false},
		{"feature?", false},
		{"feature*", false},
		{`feature\old`, false},
	} {
		t.Run(tt.ref, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "s1", "project-a")
			insertMessages(t, d, commitMessages("s1", `{"command":"git commit -m fix; git log --format=custom -1"}`, "["+tt.ref+" abc1234] Fix parser\n")...)
			got, err := d.SessionCommitLinks(t.Context(), "s1")
			require.NoError(t, err)
			if tt.want {
				require.Len(t, got.Commits, 1)
				assert.Equal(t, tt.ref, got.Commits[0].Branch)
			} else {
				assert.Empty(t, got.Commits)
			}
		})
	}
}

func TestCommitLinksNativePollOwnership(t *testing.T) {
	call := func(tool, id, input, result string) Message {
		// Code-mode shell results carry JSON inside input_text blocks.
		if tool == "exec" && strings.HasPrefix(result, "{") {
			blocks, err := json.Marshal([]map[string]string{{"type": "input_text", "text": result}})
			require.NoError(t, err)
			result = string(blocks)
		}
		m := commitMessages("s1", input, result)[2]
		m.ToolCalls[0].ToolName = tool
		m.ToolCalls[0].ToolUseID = id
		if tool == "wait" {
			m.ToolCalls[0].Category = "Other"
		}
		return m
	}

	type ownedCommit struct {
		sha, owner, repository, prompt, timestamp string
		ordinal                                   int
	}
	type ownershipCase struct {
		name        string
		cwd         string
		messages    []Message
		want        []ownedCommit
		splitAt     int
		emptyBefore bool
	}
	cases := []ownershipCase{
		{
			name: "single code mode execution",
			messages: []Message{
				call("exec", "commit-call", `text(await tools.exec_command({cmd:"git commit -m fix"}))`, `{"session_id":7,"output":"[main abc1234] Fix parser\n"}`),
				call("write_stdin", "poll", `{"session_id":7}`, "[main def5678] Second\n"),
			},
			want: []ownedCommit{
				{"abc1234", "commit-call", "", "Record the parser fix", tsZero, 2},
				{"def5678", "commit-call", "", "Record the parser fix", tsZero, 2},
			},
		},
		{
			name: "single code mode poll without output ID",
			messages: []Message{
				call("exec_command", "commit-call", `{"cmd":"git commit -m fix"}`, `{"session_id":7,"output":""}`),
				call("exec", "poll", `text(await tools.write_stdin({session_id:7}))`, `{"output":"[main abc1234] Fix parser\n"}`),
			},
			want: []ownedCommit{{"abc1234", "commit-call", "", "Record the parser fix", tsZero, 2}},
		},
		{
			name:     "failed commit with unrelated printed summary",
			messages: []Message{call("exec", "commit-call", `const readCommand = "cat example.txt"; text(await tools.exec_command({cmd:"git commit -m fix",workdir:"/repo"})); text(await tools.exec_command({cmd:readCommand,workdir:"/other"}))`, `[{"type":"input_text","text":"{\"output\":\"nothing to commit\\n\"}"},{"type":"input_text","text":"{\"output\":\"[main abc1234] Example\\n\"}"}]`)},
		},
		{
			name: "single poll mixed with unrelated command",
			messages: []Message{
				call("exec_command", "commit-call", `{"cmd":"git commit -m first","workdir":"/first"}`, `{"session_id":7,"output":""}`),
				call("exec", "mixed-call", `text(await tools.write_stdin({session_id:7})); text(await tools.exec_command({cmd:"cat example.txt"}))`, `[{"type":"input_text","text":"{\"session_id\":7,\"output\":\"[main abc1234] First\\n\"}"},{"type":"input_text","text":"{\"output\":\"[main def5678] Example\\n\"}"}]`),
			},
			want: []ownedCommit{{"abc1234", "commit-call", "/first", "Record the parser fix", tsZero, 2}},
		},
		{
			name: "single poll mixed with unidentified output",
			messages: []Message{
				call("exec_command", "commit-call", `{"cmd":"git commit -m first","workdir":"/first"}`, `{"session_id":7,"output":""}`),
				call("exec", "mixed-call", `text(await tools.write_stdin({session_id:7})); text(await tools.exec_command({cmd:"cat example.txt"}))`, `[{"type":"input_text","text":"{\"output\":\"[main abc1234] First\\n\"}"},{"type":"input_text","text":"{\"output\":\"[main def5678] Example\\n\"}"}]`),
			},
		},
		{
			name: "running batch with native polls",
			messages: []Message{
				call("exec", "commit-call", `const secondCommand = "git commit -m second"; text(await tools.exec_command({cmd:"git commit -m first"})); text(await tools.exec_command({cmd:secondCommand}))`, `[{"type":"input_text","text":"{\"session_id\":7,\"output\":\"\"}"},{"type":"input_text","text":"{\"session_id\":8,\"output\":\"\"}"}]`),
				call("write_stdin", "poll-first", `{"session_id":7}`, "[main abc1234] First\n"),
				call("write_stdin", "poll-second", `{"session_id":8}`, "[main def5678] Second\n"),
			},
		},
	}
	cases = append(cases, ownershipCase{
		name: "reused process ID",
		messages: []Message{
			call("exec_command", "first", `{"cmd":"git commit -m first","workdir":"/first"}`, `{"session_id":7,"output":""}`),
			call("exec", "batch-poll", `text(await tools.write_stdin({session_id:7})); text(await tools.write_stdin({session_id:8}))`, `{"session_id":7,"output":"[main abc1234] First\n"}`),
			userMsg("s1", 4, "Record the second fix"),
			call("exec_command", "second", `{"cmd":"git commit -m second","workdir":"/second"}`, `{"session_id":7,"output":""}`),
			call("write_stdin", "final-poll", `{"session_id":7}`, "[main def5678] Second\n"),
		},
		want: []ownedCommit{
			{"abc1234", "first", "/first", "Record the parser fix", tsZero, 2},
			{"def5678", "second", "/second", "Record the second fix", tsZero, 5},
		},
	})

	for _, tt := range []struct {
		name, initial, intermediate, final string
		codeMode                           bool
		count                              int
	}{
		{"code mode process", `{"session_id":7,"output":""}`, `{"session_id":7,"output":"still running"}`, `{"session_id":7,"output":"[main abc1234] Fix parser\n"}`, true, 1},
		{"text process", "Chunk ID: abc\nWall time: 1 seconds\nProcess running with session ID 7\nFinal output:\n", "Chunk ID: def\nWall time: 1 seconds\nProcess running with session ID 7\nFinal output:\nstill running", "Chunk ID: end\nWall time: 1 seconds\nProcess exited with code 0\nFinal output:\n[main abc1234] Second commit\n", false, 1},
		{"wrapped text process", `[{"type":"input_text","text":"Process running with session ID 7\nFinal output:\n"}]`, `[{"type":"input_text","text":"Process running with session ID 7\nFinal output:\nstill running"}]`, "Chunk ID: end\nWall time: 1 seconds\nProcess exited with code 0\nFinal output:\n[main abc1234] Second commit\n", false, 1},
		{"partial summary", "Process running with session ID 7\nFinal output:\n[main 123abcd] First commit\n", "Process running with session ID 7\nFinal output:\nstill running", "Process exited with code 0\nFinal output:\n[main abc1234] Second commit\n", false, 2},
		{"code mode partial summary", `{"session_id":7,"output":"[main 123abcd] First commit\n"}`, `{"session_id":7,"output":"still running"}`, `{"session_id":7,"output":"[main abc1234] Second commit\n"}`, true, 2},
	} {
		tool, input := "exec_command", `{"cmd":"git commit -m first && git commit -m second","workdir":"/repo"}`
		messages := []Message{call(tool, "commit-call", input, tt.initial)}
		messages = append(messages, userMsg("s1", 3, "Later prompt"))
		for i, result := range []struct{ input, output string }{
			{`{"session_id":7,"chars":""}`, tt.intermediate},
			{`{"session_id":8,"chars":""}`, ""},
			{`{"session_id":9,"chars":""}`, "[main def5678] Unrelated commit\n"},
			{`{"session_id":7,"chars":""}`, tt.final},
		} {
			tool := "write_stdin"
			if tt.codeMode {
				tool = "exec"
				result.input = "text(await tools.write_stdin(" + result.input + "))"
				if i == 1 || i == 2 {
					payload, err := json.Marshal(map[string]string{"output": result.output})
					require.NoError(t, err)
					result.output = string(payload)
				}
			}
			poll := call(tool, "poll-"+strconv.Itoa(i), result.input, result.output)
			poll.Ordinal = 4 + i
			poll.Timestamp = "2026-01-02T00:00:00Z"
			messages = append(messages, poll)
		}
		want := []ownedCommit{}
		if tt.count == 2 {
			want = append(want, ownedCommit{"123abcd", "commit-call", "/repo", "Record the parser fix", tsZero, 2})
		}
		want = append(want, ownedCommit{"abc1234", "commit-call", "/repo", "Record the parser fix", tsZero, 2})
		cases = append(cases, ownershipCase{name: tt.name, cwd: "/work", messages: messages, want: want, splitAt: 5, emptyBefore: tt.count == 1})
	}
	for _, output := range []struct {
		name, result string
		want         []ownedCommit
	}{
		{"identified poll batch", `[{"type":"input_text","text":"{\"session_id\":8,\"output\":\"[main def5678] Second\\n\"}"},{"type":"input_text","text":"{\"session_id\":7,\"output\":\"[main abc1234] First\\n\"}"}]`, []ownedCommit{{"abc1234", "commit-call", "/first", "Record the parser fix", tsZero, 2}, {"def5678", "second-call", "/second", "Record the second fix", tsZero, 4}}},
		{"unidentified poll batch", `[{"type":"input_text","text":"{\"output\":\"[main def5678] Second\\n\"}"},{"type":"input_text","text":"{\"output\":\"[main abc1234] First\\n\"}"}]`, nil},
	} {
		cases = append(cases, ownershipCase{
			name: output.name,
			messages: []Message{
				call("exec_command", "commit-call", `{"cmd":"git commit -m first","workdir":"/first"}`, `{"session_id":7,"output":""}`),
				userMsg("s1", 3, "Record the second fix"),
				call("exec_command", "second-call", `{"cmd":"git commit -m second","workdir":"/second"}`, `{"session_id":8,"output":""}`),
				call("exec", "poll", `text(await tools.write_stdin({session_id:7})); text(await tools.write_stdin({session_id:8}))`, output.result),
			},
			want: output.want,
		})
	}
	for _, summary := range []struct{ name, output string }{
		{"summary in mixed call", "[main abc1234] First\n"},
		{"summary in later poll", "still running"},
	} {
		firstOutput, err := json.Marshal(map[string]any{"session_id": 7, "output": summary.output})
		require.NoError(t, err)
		result, err := json.Marshal([]map[string]string{
			{"type": "input_text", "text": string(firstOutput)},
			{"type": "input_text", "text": `{"session_id":8,"output":"[main def5678] Second\n"}`},
		})
		require.NoError(t, err)
		mixed := call("exec", "second-call", `text(await tools.write_stdin({session_id:7})); text(await tools.exec_command({cmd:"git commit -m second",workdir:"/second"}))`, string(result))
		mixed.Timestamp = "2026-01-02T03:04:05Z"
		cases = append(cases, ownershipCase{
			name: summary.name,
			messages: []Message{
				call("exec_command", "commit-call", `{"cmd":"git commit -m first","workdir":"/first"}`, `{"session_id":7,"output":""}`),
				userMsg("s1", 3, "Record the second fix"),
				mixed,
				call("write_stdin", "poll-first", `{"session_id":7}`, "[main abc1234] First\n"),
			},
			want: []ownedCommit{
				{"abc1234", "commit-call", "/first", "Record the parser fix", tsZero, 2},
			},
		})
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "s1", "project-a", func(s *Session) { s.Cwd = tt.cwd })
			messages := commitMessages("s1", "", "")[:2]
			for i, message := range tt.messages {
				message.Ordinal = 2 + i
				messages = append(messages, message)
			}
			if tt.splitAt > 0 {
				insertMessages(t, d, messages[:tt.splitAt]...)
				if tt.emptyBefore {
					before, err := d.SessionCommitLinks(t.Context(), "s1")
					require.NoError(t, err)
					assert.Empty(t, before.Commits)
				}
				insertMessages(t, d, messages[tt.splitAt:]...)
			} else {
				insertMessages(t, d, messages...)
			}
			got, err := d.SessionCommitLinks(t.Context(), "s1")
			require.NoError(t, err)
			require.Len(t, got.Commits, len(tt.want))
			for i, want := range tt.want {
				link := got.Commits[i]
				assert.Equal(t, want.sha, link.SHA)
				assert.Equal(t, want.owner, link.ToolUseID)
				assert.Equal(t, want.ordinal, link.MessageOrdinal)
				assert.Equal(t, 0, link.CallIndex)
				assert.Equal(t, want.repository, link.Repository)
				assert.Equal(t, want.timestamp, link.CommittedAt)
				require.NotNil(t, link.Turn)
				assert.Equal(t, want.prompt, link.Turn.Text)
			}
		})
	}
}

func TestCommitLinkMirrorRows(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "project-a")
	insertMessages(t, d, commitMessages("s1", `{"command":"git commit -m fix"}`, "[main abc1234] Fix parser")...)
	require.NoError(t, d.SoftDeleteSession(t.Context(), "s1"))
	rows, err := d.SessionCommitLinkRows(t.Context(), "s1")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.NotNil(t, rows[0].TurnOrdinal)
	assert.Equal(t, 0, *rows[0].TurnOrdinal)
	assert.Equal(t, "Record the parser fix", rows[0].TurnText)
	assert.Equal(t, "project-a", rows[0].Project)
	retained, err := d.RetainedCommitLinkRows(t.Context())
	require.NoError(t, err)
	assert.Empty(t, retained)
	snapshot, err := d.LoadSessionMirrorSnapshot(t.Context(), "s1")
	require.NoError(t, err)
	assert.Equal(t, rows, snapshot.CommitLinks)
	require.NoError(t, d.DeleteSession(t.Context(), "s1"))
	retained, err = d.RetainedCommitLinkRows(t.Context())
	require.NoError(t, err)
	require.Len(t, retained, 1)
	assert.Equal(t, "abc1234", retained[0].SHA)
	assert.Nil(t, retained[0].TurnOrdinal)
	assert.Empty(t, retained[0].TurnText)

	old := testDB(t)
	insertSession(t, old, "s1", "project-a")
	insertMessages(t, old, commitMessages("s1", `{"command":"git commit -m fix"}`, "[main abc1234] Fix parser")...)
	session, err := old.GetSession(t.Context(), "s1")
	require.NoError(t, err)
	session.Project = "project-b"
	require.NoError(t, old.UpsertSession(t.Context(), *session))
	require.NoError(t, old.DeleteSession(t.Context(), "s1"))

	fresh := testDB(t)
	require.NoError(t, fresh.CopyExcludedSessionsFrom(t.Context(), old.Path()))
	retained, err = fresh.RetainedCommitLinkRows(t.Context())
	require.NoError(t, err)
	require.Len(t, retained, 1)
	assert.Equal(t, "project-b", retained[0].Project)
	assert.Equal(t, "abc1234", retained[0].SHA)
}

func TestTrailerCommitTurnRecency(t *testing.T) {
	for _, tt := range []struct {
		name, authorTime, activityTime string
		wantOrdinal                    int
		wantTurn                       *CommitTurn
	}{
		{"agent commit", "2026-01-02T10:04:30Z", "2026-01-02T12:00:00Z", 1, &CommitTurn{Heuristic: true, Ordinal: 0, Text: "Fix the parser"}},
		{"five minutes", "2026-01-02T12:05:00Z", "2026-01-02T12:00:00Z", 3, &CommitTurn{Heuristic: true, Ordinal: 2, Text: "Push and open a pull request"}},
		{"five minutes rounded up", "2026-01-02T12:06:00Z", "2026-01-02T12:01:00Z", 3, &CommitTurn{Heuristic: true, Ordinal: 2, Text: "Push and open a pull request"}},
		{"over five minutes", "2026-01-02T12:05:01Z", "2026-01-02T12:00:00Z", -1, nil},
		{"before session", "2026-01-02T09:59:59Z", "2026-01-02T12:00:00Z", -1, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "editor", "project-a")
			insertMessages(t, d,
				Message{SessionID: "editor", Ordinal: 0, Role: "user", Content: "Fix the parser", Timestamp: "2026-01-02T10:00:00Z"},
				Message{SessionID: "editor", Ordinal: 1, Role: "assistant", Content: "Fixed the parser", Timestamp: "2026-01-02T10:04:00Z"},
				Message{SessionID: "editor", Ordinal: 2, Role: "user", Content: "Push and open a pull request", Timestamp: "2026-01-02T11:59:00Z"},
				Message{SessionID: "editor", Ordinal: 3, Role: "assistant", Content: "Opened the pull request", Timestamp: tt.activityTime},
			)
			sha := strings.Repeat("a", 40)
			_, err := d.StoreTrailerCommitLinks(t.Context(), "repo", []gitlog.TrailerCommit{{SHA: sha, Sessions: []string{"claude:editor"}, CommittedAt: tt.authorTime}})
			require.NoError(t, err)
			session, err := d.SessionCommitLinks(t.Context(), "editor")
			require.NoError(t, err)
			require.Len(t, session.Commits, 1)
			commits, err := d.CommitSessionLinks(t.Context(), []string{sha})
			require.NoError(t, err)
			require.Len(t, commits.Commits, 1)
			require.Len(t, commits.Commits[0].Links, 1)
			for _, link := range []CommitLink{session.Commits[0], commits.Commits[0].Links[0]} {
				assert.Equal(t, "trailer", link.Source)
				assert.Equal(t, tt.wantOrdinal, link.MessageOrdinal)
				assert.Equal(t, tt.wantTurn, link.Turn)
			}
		})
	}
}

func TestTrailerCommitTranscriptPrefix(t *testing.T) {
	const firstSHA = "abc1234000000000000000000000000000000000"
	const secondSHA = "abc1234111111111111111111111111111111111"
	for _, tt := range []struct {
		name          string
		transcriptSHA string
		collision     bool
		wantSHAs      []string
	}{
		{"prefix", "abc1234", false, []string{"abc1234"}},
		{"exact", firstSHA, false, []string{firstSHA}},
		{"later prefix collision", "abc1234", true, []string{"abc1234"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "editor", "project-a")
			insertMessages(t, d, commitMessages("editor", `{"command":"git commit -m fix"}`, "[main "+tt.transcriptSHA+"] Fix parser")...)
			commits := []gitlog.TrailerCommit{
				{SHA: firstSHA, Sessions: []string{"claude:editor"}, CommittedAt: tsZero},
			}
			if tt.collision {
				commits = append(commits, gitlog.TrailerCommit{SHA: secondSHA, Sessions: []string{"claude:editor"}, CommittedAt: tsZero})
			}
			_, err := d.StoreTrailerCommitLinks(t.Context(), "repo", commits)
			require.NoError(t, err)

			session, err := d.SessionCommitLinks(t.Context(), "editor")
			require.NoError(t, err)
			var shas []string
			for _, link := range session.Commits {
				shas = append(shas, link.SHA)
			}
			assert.ElementsMatch(t, tt.wantSHAs, shas)
			for _, link := range session.Commits {
				if link.SHA == tt.transcriptSHA {
					assert.Equal(t, "transcript", link.Source)
				}
			}
			found, err := d.CommitSessionLinks(t.Context(), []string{"abc1234"})
			require.NoError(t, err)
			require.Len(t, found.Commits, 1)
			require.Len(t, found.Commits[0].Links, 1)
			shas = nil
			for _, link := range found.Commits[0].Links {
				assert.False(t, link.Ambiguous)
				assert.Equal(t, "editor", link.SessionID)
				shas = append(shas, link.SHA)
				assert.Equal(t, "transcript", link.Source)
			}
			assert.ElementsMatch(t, tt.wantSHAs, shas)
			// Replacing the transcript preserves its independent trailer evidence.
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "editor", []Message{{SessionID: "editor", Ordinal: 0, Role: "user", Content: "Rewritten", Timestamp: tsZero}}))
			session, err = d.SessionCommitLinks(t.Context(), "editor")
			require.NoError(t, err)
			wantTrailers := []string{firstSHA}
			if tt.collision {
				wantTrailers = append(wantTrailers, secondSHA)
			}
			shas = nil
			for _, link := range session.Commits {
				shas = append(shas, link.SHA)
				assert.Equal(t, "trailer", link.Source)
				assert.Equal(t, -1, link.CallIndex)
				assert.Empty(t, link.ToolUseID)
			}
			assert.ElementsMatch(t, wantTrailers, shas)
		})
	}
}

func TestTrailerCommitUpsert(t *testing.T) {
	for _, tt := range []struct {
		name                       string
		branches, repositories     []string
		wantBranch, wantRepository string
		unchangedAfter             int
	}{
		{name: "first branch", branches: []string{"", "feature", "main", ""}, repositories: []string{"repo", "repo", "repo", "repo"}, wantBranch: "feature", wantRepository: "repo", unchangedAfter: 2},
		{name: "first repository", branches: []string{"", "", "", ""}, repositories: []string{"/clone-a", "/clone-b", "/clone-a", "/clone-b"}, wantRepository: "/clone-a", unchangedAfter: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "editor", "project-a")
			c := gitlog.TrailerCommit{SHA: strings.Repeat("a", 40), Sessions: []string{"claude:editor"}, CommittedAt: tsZero}
			for i, repo := range tt.repositories {
				_, err := d.getWriter().ExecContext(t.Context(), `UPDATE sessions SET local_modified_at=? WHERE id='editor'`, tsZero)
				require.NoError(t, err)
				c.Branch = tt.branches[i]
				stored, err := d.StoreTrailerCommitLinks(t.Context(), repo, []gitlog.TrailerCommit{c})
				require.NoError(t, err)
				if i >= tt.unchangedAfter {
					assert.Zero(t, stored)
					links, err := d.SessionCommitLinks(t.Context(), "editor")
					require.NoError(t, err)
					require.Len(t, links.Commits, 1)
					assert.Equal(t, tt.wantBranch, links.Commits[0].Branch)
					assert.Equal(t, tt.wantRepository, links.Commits[0].Repository)
					var modified string
					require.NoError(t, d.getReader().QueryRowContext(t.Context(), `SELECT local_modified_at FROM sessions WHERE id='editor'`).Scan(&modified))
					assert.Equal(t, tsZero, modified)
				} else {
					assert.Equal(t, 1, stored)
				}
			}
			stored, err := d.StoreTrailerCommitLinks(t.Context(), tt.wantRepository, nil)
			require.NoError(t, err)
			assert.Zero(t, stored)
			links, err := d.SessionCommitLinks(t.Context(), "editor")
			require.NoError(t, err)
			require.Len(t, links.Commits, 1)
			assert.Equal(t, c.SHA, links.Commits[0].SHA)
			assert.Equal(t, tt.wantBranch, links.Commits[0].Branch)
			assert.Equal(t, tt.wantRepository, links.Commits[0].Repository)
		})
	}
}

func TestTrailerCommitProjectSurvivesPurge(t *testing.T) {
	for _, tt := range []struct {
		name       string
		storeAfter bool
	}{
		{name: "stored before purge"},
		{name: "stored after purge", storeAfter: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const id = "editor"
			d := testDB(t)
			insertSession(t, d, id, "project-a")
			if tt.storeAfter {
				require.NoError(t, d.DeleteSession(t.Context(), id))
			} else {
				insertMessages(t, d, userMsg(id, 0, "Save the edit"))
			}
			_, err := d.StoreTrailerCommitLinks(t.Context(), "repo", []gitlog.TrailerCommit{{SHA: strings.Repeat("a", 40), Sessions: []string{"claude:editor"}, CommittedAt: tsZero}})
			require.NoError(t, err)
			if tt.storeAfter {
				links, err := d.SessionCommitLinks(t.Context(), id)
				require.NoError(t, err)
				assert.Empty(t, links.Commits)
				matches, err := d.CommitSessionLinks(t.Context(), []string{strings.Repeat("a", 40)})
				require.NoError(t, err)
				require.Len(t, matches.Commits, 1)
				assert.Empty(t, matches.Commits[0].Links)
				rows, err := d.RetainedCommitLinkRows(t.Context())
				require.NoError(t, err)
				assert.Empty(t, rows, "a trailer without project evidence cannot be scoped for publication")
				return
			}
			rows, err := d.SessionCommitLinkRows(t.Context(), id)
			require.NoError(t, err)
			require.Len(t, rows, 1)
			snapshot, err := d.LoadSessionMirrorSnapshot(t.Context(), id)
			require.NoError(t, err)
			assert.Equal(t, rows, snapshot.CommitLinks)
			session, err := d.GetSession(t.Context(), id)
			require.NoError(t, err)
			session.Project = "project-b"
			require.NoError(t, d.UpsertSession(t.Context(), *session))
			require.NoError(t, d.DeleteSession(t.Context(), id))
			for _, archive := range []*DB{d, testDB(t)} {
				if archive != d {
					require.NoError(t, archive.CopyExcludedSessionsFrom(t.Context(), d.Path()))
					require.NoError(t, archive.CopyCommitTrailersFrom(t.Context(), d.Path()))
				}
				rows, err := archive.RetainedCommitLinkRows(t.Context())
				require.NoError(t, err)
				require.Len(t, rows, 1)
				assert.Equal(t, "project-b", rows[0].Project)
				assert.Equal(t, "trailer", rows[0].Source)
				assert.True(t, rows[0].Missing)
				assert.Nil(t, rows[0].TurnOrdinal)
				assert.Empty(t, rows[0].TurnText)
			}
		})
	}
}
