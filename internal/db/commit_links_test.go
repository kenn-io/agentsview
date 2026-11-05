package db

import (
	"encoding/json/v2"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
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
	after, err := d.SessionCommitLinks(t.Context(), "claude:commit")
	require.NoError(t, err)
	require.Len(t, after.Commits, 1)
	link := after.Commits[0]
	assert.Equal(t, "claude:commit", link.SessionID)
	assert.Equal(t, "abc1234", link.SHA)
	assert.Equal(t, 2, link.MessageOrdinal)
	assert.Equal(t, 0, link.CallIndex)
	assert.Equal(t, "commit-call", link.ToolUseID)
	assert.False(t, link.Ambiguous)
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

func TestCommitLinksParsing(t *testing.T) {
	for _, input := range []string{
		`{"command":["bash","-lc","git commit -m fix"]}`,
		`{"command":["git","commit","-m","fix"]}`,
		`{"command":["git","-C","/path with spaces","commit","-m","x"]}`,
	} {
		t.Run(input, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "codex:commit", "project-a")
			messages := commitMessages("codex:commit", input, "[main abc1234] Fix parser")
			messages[2].ToolCalls[0].ToolName = "shell"
			insertMessages(t, d, messages...)
			got, err := d.SessionCommitLinks(t.Context(), "codex:commit")
			require.NoError(t, err)
			require.Len(t, got.Commits, 1)
			assert.Equal(t, "abc1234", got.Commits[0].SHA)
			assert.Equal(t, "main", got.Commits[0].Branch)
			assert.Equal(t, "Fix parser", got.Commits[0].Subject)
		})
	}
	for _, tt := range []struct {
		name              string
		results, commands []string
		count             int
	}{
		{"commit message forms", []string{"[main abc1234] Fix parser"}, []string{"git commit -m fix", "git commit -m \"$(cat <<'EOF'\nFix parser\nEOF\n)\""}, 1},
		{"absolute git", []string{"[main abc1234] Fix parser"}, []string{"/usr/bin/git commit -m fix"}, 1},
		{"shell wrapper", []string{"[main abc1234] Fix parser"}, []string{"bash -lc 'git commit -m fix'"}, 1},
		{"python", []string{"[main abc1234] Fix parser"}, []string{`python3 -c 'subprocess.run(["git","commit","-m","fix"])'`}, 1},
		{"failed", []string{"nothing to commit"}, []string{"git commit -m fix"}, 0},
		{"printed", []string{"[main abc1234] Fix parser"}, []string{"cat out.txt"}, 0},
		{"printed commit documentation", []string{"[main abc1234] Fix parser"}, []string{"cat docs/git-commit.md"}, 0},
		{"searched commit documentation", []string{"[main 463dc4f] Example commit"}, []string{"grep -n 'git commit' docs/x.md; sed -n 20,24p docs/x.md"}, 0},
		{"shown commit documentation", []string{"[main abc1234] Fix parser"}, []string{"git show HEAD:docs/commit.md"}, 0},
		{"log grep", []string{"[main abc1234] Fix parser"}, []string{"git log --grep=commit"}, 0},
		{"commit filename", []string{"[main abc1234] Fix parser"}, []string{"git commit.md"}, 0},
		{"whitespace prefix", []string{"[main abc1234] Fix parser"}, []string{"git add .\ngit commit -m fix", "cat <<'EOF'\nIt's fixed\nEOF\ngit commit -m fix", "GIT_X=1 git commit -m fix"}, 1},
		{"cherry-pick", []string{"[main abc1234] Fix parser"}, []string{"git cherry-pick abc1234"}, 0},
		{"multiple", []string{"[main (root-commit) abc1234] First\n[main def5678] Second\n[main def5678] Second"}, []string{"git commit -m one; git commit -m two"}, 2},
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

func TestCommitLinksQuietReadback(t *testing.T) {
	type readbackCase struct {
		name, command, output, sha, subject string
		tool, input, repository, cwd        string
	}
	cases := []readbackCase{
		{name: "rev parse", command: "git commit -q -m fix && git rev-parse HEAD", output: "abc1234\n", sha: "abc1234", repository: "/work", cwd: "/work"},
		{name: "rev parse CRLF", command: "git commit -q -m fix && git rev-parse HEAD", output: "abc1234\r\n", sha: "abc1234", repository: "/work", cwd: "/work"},
		{name: "double quoted option text", command: `git commit -q -m "Fix --dry-run handling" && git rev-parse HEAD`, output: "abc1234\n", sha: "abc1234", repository: "/work", cwd: "/work"},
		{name: "single quoted option text", command: "git commit -q -m 'Fix --dry-run handling' && git rev-parse HEAD", output: "abc1234\n", sha: "abc1234", repository: "/work", cwd: "/work"},
		{name: "dry run with option text", command: `git commit -q --dry-run -m "Fix --dry-run handling" && git rev-parse HEAD`, output: "abc1234\n", repository: "/work", cwd: "/work"},
		{name: "log oneline", command: "git commit --quiet -m fix && git log --oneline -1", output: "abc1234 Fix parser\n", sha: "abc1234", subject: "Fix parser", repository: "/work", cwd: "/work"},
		{name: "log reverse options", command: "git commit -q -m fix && git log -1 --oneline", output: "abc1234 Fix parser\n", sha: "abc1234", subject: "Fix parser", repository: "/work", cwd: "/work"},
		{name: "commit other repo", command: "git -C /other commit -q -m fix && git rev-parse HEAD", output: "abc1234", repository: "/work", cwd: "/work"},
		{name: "readback other repo", command: "git commit -q -m fix && git -C /other rev-parse HEAD", output: "abc1234", repository: "/work", cwd: "/work"},
		{name: "no verify", command: "git commit -n -q -m fix && git rev-parse HEAD", output: "abc1234", sha: "abc1234", repository: "/work", cwd: "/work"},
		{name: "fallback", command: "git commit -q || true && git rev-parse HEAD", output: "abc1234", repository: "/work", cwd: "/work"},
		{name: "printed", command: "echo 'git commit -q && git rev-parse HEAD'", output: "abc1234", repository: "/work", cwd: "/work"},
		{name: "quoted", command: "printf '%s' \"git commit -q && git rev-parse HEAD\"", output: "abc1234", repository: "/work", cwd: "/work"},
		{name: "multiple shas", command: "git commit -q && git rev-parse HEAD", output: "abc1234\ndef5678 Other", repository: "/work", cwd: "/work"},
	}
	for _, option := range []string{"-z", "--dr", "--dry", "--dry-", "--dry-r", "--dry-ru", "--dry-run", "--sh", "--sho", "--shor", "--short", "--po", "--por", "--porc", "--porce", "--porcel", "--porcela", "--porcelai", "--porcelain", "--l", "--lo", "--lon", "--long", "--nu", "--nul", "--null", "'--dry-run'", `"--dry-run"`, "'--short'", `"--porcelain"`, "'--l'", `"-z"`} {
		cases = append(cases, readbackCase{name: option, command: "git commit -q " + option + " && git rev-parse HEAD", output: "abc1234\n"})
	}
	cases = append(cases, readbackCase{
		name: "code mode", tool: "exec",
		input:  `text(await tools.exec_command({cmd: "git commit -q -m \"fix\" && git rev-parse HEAD", workdir: "/repo"}));`,
		output: `{"output":"abc1234\n"}`, sha: "abc1234", repository: "/repo", cwd: "/work",
	})
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "s1", "project-a", func(s *Session) { s.Cwd = tt.cwd })
			input, err := json.Marshal(map[string]string{"command": tt.command})
			require.NoError(t, err)
			messages := commitMessages("s1", string(input), tt.output)
			if tt.tool != "" {
				messages[2].ToolCalls[0].ToolName = tt.tool
				messages[2].ToolCalls[0].InputJSON = tt.input
			}
			insertMessages(t, d, messages...)
			got, err := d.SessionCommitLinks(t.Context(), "s1")
			require.NoError(t, err)
			if tt.sha == "" {
				assert.Empty(t, got.Commits)
				return
			}
			require.Len(t, got.Commits, 1)
			assert.Equal(t, tt.sha, got.Commits[0].SHA)
			assert.Equal(t, tt.subject, got.Commits[0].Subject)
			assert.Empty(t, got.Commits[0].Branch)
			assert.Equal(t, tt.repository, got.Commits[0].Repository)
		})
	}
	t.Run("summary takes precedence", func(t *testing.T) {
		d := testDB(t)
		insertSession(t, d, "s1", "project-a")
		insertMessages(t, d, commitMessages("s1", `{"command":"git commit -q && git rev-parse HEAD"}`, "[main abc1234] Fix parser\ndef5678")...)
		got, err := d.SessionCommitLinks(t.Context(), "s1")
		require.NoError(t, err)
		require.Len(t, got.Commits, 1)
		assert.Equal(t, "abc1234", got.Commits[0].SHA)
		assert.Equal(t, "main", got.Commits[0].Branch)
	})
}

func TestCommitLinksRepositoryPathStyle(t *testing.T) {
	type repositoryCase struct{ cwd, command, repository string }
	for _, tt := range []struct {
		name  string
		cases []repositoryCase
	}{
		{"POSIX absolute", []repositoryCase{{"/work", "cd /repo && git commit -m fix", "/repo"}}},
		{"tilde", []repositoryCase{{"/work", "cd ~/x && git commit -m fix", "~/x"}}},
		{"nonleading cd", []repositoryCase{{"/work", "git add . && cd /elsewhere && git commit -m fix", "/work"}}},
		{"quoted directory", []repositoryCase{{"/work", `git -C "/tmp/my repo" commit -m fix`, "/tmp/my repo"}}},
		{"quoted operators", []repositoryCase{{"/work", `git -C "/tmp/a;b" commit -m "x && y"`, "/tmp/a;b"}}},
		{"directory after global options", []repositoryCase{{"/work", `git -c 'user.name=Example User' --no-pager -C /repo commit -m fix`, "/repo"}}},
		{"successive directories with global options", []repositoryCase{{"/work", `git -C root -c 'user.name=-C decoy' --no-pager -C 'child repo' commit -m fix`, "/work/root/child repo"}}},
		{"absolute directory resets previous directory", []repositoryCase{{"/work", `git -C root -C /repo -C child commit -m fix`, "/repo/child"}}},
		{"separate git invocations", []repositoryCase{{"/work", `git -C /other add . && git -C root -C child commit -m fix`, "/work/root/child"}}},
		{"directory text in message", []repositoryCase{{"/work", `git commit -m "Document git -C /other commit"`, "/work"}}},
		{"relative directory", []repositoryCase{{"/work", "cd sub && git -C child commit -m fix", "/work/sub/child"}}},
		{"global directory option", []repositoryCase{{"/work", `git --git-dir=/repo/.git commit -m fix`, "/work"}}},
		{"POSIX relative", []repositoryCase{{"/work", "cd sub && git -C ../child commit -m fix", "/work/child"}}},
		{"Windows absolute", []repositoryCase{{`C:\work`, `cd D:\repo && git commit -m fix`, `D:\repo`}, {"C:/work", "cd D:/repo && git commit -m fix", "D:/repo"}}},
		{"Windows relative", []repositoryCase{{`C:\work`, `cd sub && git -C child commit -m fix`, `C:\work\sub\child`}, {`C:\work`, `git -C sub\child commit -m fix`, `C:\work\sub\child`}}},
		{"Windows parent", []repositoryCase{{`C:\work`, `git -C ..\repo commit -m fix`, `C:\repo`}}},
		{"Windows rooted", []repositoryCase{{`C:\work`, `git -C \repo commit -m fix`, `C:\repo`}}},
		{"Windows slash relative", []repositoryCase{{"C:/work", "git -C sub commit -m fix", "C:/work/sub"}}},
		{"Windows UNC", []repositoryCase{{`C:\work`, `cd \\server\share\repo && git commit -m fix`, `\\server\share\repo`}}},
		{"Windows UNC relative", []repositoryCase{{`\\server\share\repo`, `git -C ..\child commit -m fix`, `\\server\share\child`}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, c := range tt.cases {
				d := testDB(t)
				insertSession(t, d, "s1", "project-a", func(s *Session) { s.Cwd = c.cwd })
				input, err := json.Marshal(map[string]string{"command": c.command})
				require.NoError(t, err)
				insertMessages(t, d, commitMessages("s1", string(input), "[main abc1234] Fix parser")...)
				got, err := d.SessionCommitLinks(t.Context(), "s1")
				require.NoError(t, err)
				require.Len(t, got.Commits, 1)
				assert.Equal(t, c.repository, got.Commits[0].Repository)
			}
		})
	}
	type callResult struct{ input, result string }
	for _, tt := range []struct {
		name, repository string
		calls            []callResult
	}{
		{"Codex JavaScript backtick", "/work", []callResult{{"text(await tools.exec_command({cmd:`git commit -m fix`}))", "[main abc1234] Fix parser"}}},
		{"Codex JavaScript backtick leading cd", "/repo", []callResult{{"text(await tools.exec_command({cmd:`cd /repo && git commit -m fix`}))", "[main abc1234] Fix parser"}}},
		{"Codex JavaScript workdir", "/repo", []callResult{{`text(await tools.exec_command({cmd:"git commit -m fix",workdir:"/repo"}))`, `[{"type":"input_text","text":"{\"output\":\"[main abc1234] Fix parser\\n\"}"}]`}}},
		{"Codex JavaScript directory text in subject", "/repo", []callResult{{`text(await tools.exec_command({cmd:"git commit -m \"Document cwd: '/wrong'\"",workdir:"/repo"}))`, "[main abc1234] Document cwd: '/wrong'"}}},
		{"Codex JavaScript leading cd", "/repo", []callResult{{`text(await tools.exec_command({cmd:"cd /repo && git commit -m fix"}))`, "[main abc1234] Fix parser"}}},
		{"Codex JavaScript escaped command", "/my repo", []callResult{{`text(await tools.exec_command({cmd:"cd \"/my repo\" && git commit -m fix",workdir:"/work"}))`, "[main abc1234] Fix parser"}}},
		{"JSON workdir", "/repo", []callResult{{`{"cmd":"git commit -m fix","workdir":"/repo"}`, "[main abc1234] Fix parser"}}},
		{"JSON cwd", "/work/sub", []callResult{{`{"cmd":"git commit -m fix","cwd":"sub"}`, "[main abc1234] Fix parser"}}},
		{"JavaScript cwd", "/repo", []callResult{{`text(await tools.exec_command({cmd:'git commit -m fix',cwd:'/repo'}))`, `{"output":"[main abc1234] Fix parser\n"}`}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, call := range tt.calls {
				d := testDB(t)
				insertSession(t, d, "s1", "project-a", func(s *Session) { s.Cwd = "/work" })
				messages := commitMessages("s1", call.input, call.result)
				if !strings.HasPrefix(call.input, "{") {
					messages[2].ToolCalls[0].ToolName = "exec"
				}
				insertMessages(t, d, messages...)
				got, err := d.SessionCommitLinks(t.Context(), "s1")
				require.NoError(t, err)
				require.Len(t, got.Commits, 1)
				assert.Equal(t, tt.repository, got.Commits[0].Repository)
			}
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
		insertMessages(t, d, commitMessages(id, `{"cmd":"git commit -m fix"}`, "[main abc123456] Fix parser")...)
	}
	oversized := make([]string, 201)
	for i := range oversized {
		oversized[i] = "abc1234"
	}
	for _, shas := range [][]string{{"short"}, {"abc123z"}, oversized} {
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
	t.Run("uppercase full and short lookup", func(t *testing.T) {
		d := testDB(t)
		insertSession(t, d, "claude:commit", "project-a")
		insertMessages(t, d, commitMessages("claude:commit", `{"command":"git commit -m fix"}`, "[main abc1234] Fix parser")...)
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
	})
}

func TestCommitLinksLookupAndLifecycle(t *testing.T) {
	d := testDB(t)
	for _, id := range []string{"s1", "s2"} {
		insertSession(t, d, id, "project-a")
		insertMessages(t, d, commitMessages(id, `{"cmd":"git commit -m fix"}`, "[main abc123456] Fix parser")...)
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
			_, err := fresh.CopyTrashedDataFrom(t.Context(), old.Path())
			require.NoError(t, err)
			_, err = fresh.CopyOrphanedDataFrom(t.Context(), old.Path())
			require.NoError(t, err)
			require.NoError(t, fresh.CopyExcludedSessionsFrom(t.Context(), old.Path()))
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

func TestCommitLinksPromptLimit(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "project-a")
	messages := commitMessages("s1", `{"command":"git commit -m fix"}`, "[main abc1234] Fix parser")
	messages[0].Content = strings.Repeat("é", 1100)
	insertMessages(t, d, messages...)
	got, err := d.SessionCommitLinks(t.Context(), "s1")
	require.NoError(t, err)
	require.Len(t, got.Commits, 1)
	require.NotNil(t, got.Commits[0].Turn)
	assert.Equal(t, strings.Repeat("é", 1000), got.Commits[0].Turn.Text)
}

func TestCommitLinksWritePolicy(t *testing.T) {
	for _, tt := range []struct {
		name   string
		policy config.ArchiveContent
	}{
		{"full", config.ArchiveContentFull},
		{"transcripts", config.ArchiveContentTranscripts},
		{"usage", config.ArchiveContentUsage},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "s1", "project-a")
			messages := commitMessages("s1", `{"command":"git commit -m fix"}`, "[main abc1234] Fix parser")
			insertMessages(t, d, messages...)
			before, err := d.SessionCommitLinks(t.Context(), "s1")
			require.NoError(t, err)
			require.Len(t, before.Commits, 1)
			d.SetArchiveContent(tt.policy)
			insertSession(t, d, "s2", "project-a")
			insertMessages(t, d, commitMessages("s2", `{"command":"git commit -m fix"}`, "[main abc1234] Fix parser")...)
			initial, err := d.SessionCommitLinks(t.Context(), "s2")
			require.NoError(t, err)
			if tt.policy == config.ArchiveContentFull {
				require.Len(t, initial.Commits, 1)
			} else {
				assert.Empty(t, initial.Commits)
			}
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "s1", messages))
			got, err := d.SessionCommitLinks(t.Context(), "s1")
			require.NoError(t, err)
			if tt.policy == config.ArchiveContentFull {
				require.Len(t, got.Commits, 1)
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
}

func TestCommitLinksNativePollOwnership(t *testing.T) {
	call := func(tool, id, input, result string) Message {
		m := commitMessages("s1", input, result)[2]
		m.ToolCalls[0].ToolName = tool
		m.ToolCalls[0].ToolUseID = id
		if tool == "wait" {
			m.ToolCalls[0].Category = "Other"
		}
		return m
	}

	for _, tt := range []struct {
		name, initial, intermediate, final string
		codeMode                           bool
		count                              int
	}{
		{"JSON process", `{"session_id":7,"output":""}`, `{"session_id":7,"output":"still running"}`, "Chunk ID: end\nWall time: 1 seconds\nProcess exited with code 0\nFinal output:\n[main abc1234] Second commit\n", false, 1},
		{"text process", "Chunk ID: abc\nWall time: 1 seconds\nProcess running with session ID 7\nFinal output:\n", "Chunk ID: def\nWall time: 1 seconds\nProcess running with session ID 7\nFinal output:\nstill running", "Chunk ID: end\nWall time: 1 seconds\nProcess exited with code 0\nFinal output:\n[main abc1234] Second commit\n", false, 1},
		{"wrapped text process", `[{"type":"input_text","text":"Process running with session ID 7\nFinal output:\n"}]`, `[{"type":"input_text","text":"Process running with session ID 7\nFinal output:\nstill running"}]`, "Chunk ID: end\nWall time: 1 seconds\nProcess exited with code 0\nFinal output:\n[main abc1234] Second commit\n", false, 1},
		{"partial summary", `{"session_id":7,"output":"[main 123abcd] First commit\n"}`, `{"session_id":7,"output":"still running"}`, "Chunk ID: end\nWall time: 1 seconds\nProcess exited with code 0\nFinal output:\n[main abc1234] Second commit\n", false, 2},
		{"code mode partial summary", `{"session_id":7,"output":"[main 123abcd] First commit\n"}`, `{"session_id":7,"output":"still running"}`, "Chunk ID: end\nWall time: 1 seconds\nProcess exited with code 0\nFinal output:\n[main abc1234] Second commit\n", true, 2},
		{"code mode JSON", `{"session_id":7,"output":""}`, `{"session_id":7,"output":"still running"}`, `{"output":"[main abc1234] Fix parser\n"}`, true, 1},
		{"code mode wrapped JSON", `{"session_id":7,"output":""}`, `{"session_id":7,"output":"still running"}`, `[{"type":"input_text","text":"{\"output\":\"[main abc1234] Fix parser\\n\"}"}]`, true, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "s1", "project-a", func(s *Session) { s.Cwd = "/work" })
			messages := commitMessages("s1", `{"cmd":"git commit -m first && git commit -m second","workdir":"/repo"}`, tt.initial)
			messages[2].ToolCalls[0].ToolName = "exec_command"
			if tt.codeMode {
				messages[2].ToolCalls[0].ToolName = "exec"
				messages[2].ToolCalls[0].InputJSON = `text(await tools.exec_command({cmd:"git commit -m first && git commit -m second",workdir:"/repo"}))`
			}
			messages = append(messages, userMsg("s1", 3, "Later prompt"))
			for i, result := range []struct{ input, output string }{
				{`{"session_id":7,"chars":""}`, tt.intermediate},
				{`{"session_id":8,"chars":""}`, `{"output":""}`},
				{`{"session_id":9,"chars":""}`, `{"output":"[main def5678] Unrelated commit\n"}`},
				{`{"session_id":7,"chars":""}`, tt.final},
			} {
				poll := call("write_stdin", "poll", result.input, result.output)
				poll.Ordinal = 4 + i
				poll.Timestamp = "2026-01-02T00:00:00Z"
				if tt.codeMode {
					poll.ToolCalls[0].ToolName = "exec"
					poll.ToolCalls[0].InputJSON = "text(await tools.write_stdin(" + result.input + "))"
				}
				messages = append(messages, poll)
			}
			insertMessages(t, d, messages[:5]...)
			if tt.count == 1 {
				before, err := d.SessionCommitLinks(t.Context(), "s1")
				require.NoError(t, err)
				assert.Empty(t, before.Commits)
			}
			insertMessages(t, d, messages[5:]...)
			got, err := d.SessionCommitLinks(t.Context(), "s1")
			require.NoError(t, err)
			require.Len(t, got.Commits, tt.count)
			link := got.Commits[tt.count-1]
			assert.Equal(t, "abc1234", link.SHA)
			assert.Equal(t, 2, link.MessageOrdinal)
			assert.Equal(t, 0, link.CallIndex)
			assert.Equal(t, "commit-call", link.ToolUseID)
			assert.Equal(t, tsZero, link.CommittedAt)
			assert.Equal(t, "/repo", link.Repository)
			require.NotNil(t, link.Turn)
			assert.Equal(t, "Record the parser fix", link.Turn.Text)
		})
	}
	type ownedCommit struct {
		sha, owner, repository, prompt, timestamp string
		ordinal                                   int
	}
	type ownershipCase struct {
		name       string
		cwd        string
		beforePoll bool
		messages   []Message
		want       []ownedCommit
	}
	cases := []ownershipCase{
		{
			name: "quiet readback after poll", cwd: "/work", beforePoll: true,
			messages: []Message{
				call("exec_command", "commit-call", `{"cmd":"git commit -q -m fix && git rev-parse HEAD","workdir":"/repo"}`, `{"session_id":7,"output":""}`),
				call("write_stdin", "poll", `{"session_id":7}`, "abc1234\n"),
			},
			want: []ownedCommit{{"abc1234", "commit-call", "/repo", "Record the parser fix", tsZero, 2}},
		},
		{
			name:     "complete batch result",
			messages: []Message{call("exec", "commit-call", `text(await tools.exec_command({cmd:"git commit -m first"})); text(await tools.exec_command({cmd:"git commit -m second"}))`, `[{"type":"input_text","text":"[main abc1234] First\n"},{"type":"input_text","text":"[main def5678] Second\n"}]`)},
			want: []ownedCommit{
				{"abc1234", "commit-call", "", "Record the parser fix", tsZero, 2},
				{"def5678", "commit-call", "", "Record the parser fix", tsZero, 2},
			},
		},
		{
			name: "running batch with native polls",
			messages: []Message{
				call("exec", "commit-call", `text(await tools.exec_command({cmd:"git commit -m first"})); text(await tools.exec_command({cmd:"git commit -m second"}))`, `[{"type":"input_text","text":"{\"session_id\":7,\"output\":\"\"}"},{"type":"input_text","text":"{\"session_id\":8,\"output\":\"\"}"}]`),
				call("write_stdin", "poll", `{"session_id":7}`, `{"output":"[main abc1234] First\n"}`),
				call("write_stdin", "poll", `{"session_id":8}`, `{"output":"[main def5678] Second\n"}`),
			},
			want: []ownedCommit{
				{"abc1234", "commit-call", "", "Record the parser fix", tsZero, 2},
				{"def5678", "commit-call", "", "Record the parser fix", tsZero, 2},
			},
		},
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
			{"type": "input_text", "text": `{"output":"[main def5678] Second\n"}`},
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
				call("write_stdin", "poll", `{"session_id":7}`, `{"output":"[main abc1234] First\n"}`),
			},
			want: []ownedCommit{
				{"abc1234", "commit-call", "/first", "Record the parser fix", tsZero, 2},
				{"def5678", "second-call", "/second", "Record the second fix", "2026-01-02T03:04:05Z", 4},
			},
		})
	}
	cases = append(cases, ownershipCase{
		name: "cell wait g26:48",
		messages: []Message{
			call("exec", "commit-call", `text(await tools.exec_command({cmd:"git commit -m fix",workdir:"/repo"}))`, "Script running with cell ID g26:48"),
			call("wait", "poll", `{"cell_id":"g26:48"}`, "Script running with cell ID g26:48"),
			call("wait", "poll", `{"cell_id":"g26:48"}`, "[main abc1234] Fix parser"),
		},
		want: []ownedCommit{{"abc1234", "commit-call", "/repo", "Record the parser fix", tsZero, 2}},
	})
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "s1", "project-a", func(s *Session) { s.Cwd = tt.cwd })
			messages := commitMessages("s1", "", "")[:2]
			for i, message := range tt.messages {
				message.Ordinal = 2 + i
				messages = append(messages, message)
			}
			if tt.beforePoll {
				insertMessages(t, d, messages[:3]...)
				before, err := d.SessionCommitLinks(t.Context(), "s1")
				require.NoError(t, err)
				assert.Empty(t, before.Commits)
				insertMessages(t, d, messages[3:]...)
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
}

func TestCommitLinkProjectSurvivesMoveDeletionAndResync(t *testing.T) {
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
	retained, err := fresh.RetainedCommitLinkRows(t.Context())
	require.NoError(t, err)
	require.Len(t, retained, 1)
	assert.Equal(t, "project-b", retained[0].Project)
	assert.Equal(t, "abc1234", retained[0].SHA)
}
