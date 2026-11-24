package sync_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
	"go.kenn.io/agentsview/internal/testutil"
)

func storedPRURLs(t *testing.T, d *db.DB, id string) []string {
	t.Helper()
	sess, err := d.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, sess)
	urls := make([]string, 0, len(sess.PRLinks))
	for _, link := range sess.PRLinks {
		urls = append(urls, link.URL)
	}
	return urls
}

func TestIncrementalSync_ClaudePRLinkAppends(t *testing.T) {
	env := setupTestEnv(t)
	prLink := func(n string) string {
		return `{"type":"pr-link","prNumber":` + n +
			`,"prUrl":"https://github.com/owner/repo/pull/` + n +
			`","prRepository":"owner/repo","timestamp":"2024-01-01T00:00:02Z"}`
	}
	initial := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("Open a PR", tsZero),
		testjsonl.ClaudeAssistantJSON("Opened", tsZeroS1),
		prLink("1"),
	)
	path := env.writeClaudeSession(t, "proj", "pr-link-append.jsonl", initial)
	require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)
	assert.Equal(t,
		[]string{"https://github.com/owner/repo/pull/1"},
		storedPRURLs(t, env.db, "pr-link-append"))

	// A new link reaches the session through a full reparse.
	appendClaudeSplitLines(t, path, prLink("2"))
	env.engine.SyncPaths([]string{path})
	assert.Equal(t, []string{
		"https://github.com/owner/repo/pull/1",
		"https://github.com/owner/repo/pull/2",
	}, storedPRURLs(t, env.db, "pr-link-append"))
	assertSessionMessageCount(t, env.db, "pr-link-append", 2)
}

func TestParseDiffIgnoresLauncherParents(t *testing.T) {
	env := setupFocusedTestEnv(t, parser.AgentClaude, parser.AgentGemini)
	env.writeClaudeSession(t, "test-proj", "pd-prs.jsonl", testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("Review", tsZero),
		testjsonl.ClaudeAssistantJSON("Opened", tsZeroS1),
	))
	env.writeGeminiSession(t,
		filepath.Join("tmp", "annhash", "chats", "session-001.json"),
		parseDiffGeminiContent("pd-worker", "annhash"))
	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 2, Synced: 2})

	// A launcher-supplied parent on a full-replace agent's session is not
	// parser drift.
	_, err := env.db.SetSessionExternalParent(
		t.Context(), "gemini:pd-worker", "pd-prs")
	require.NoError(t, err)
	report := runParseDiff(t, env, sync.ParseDiffOptions{})
	assert.Equal(t, sync.ParseDiffTotals{Examined: 2, Identical: 2}, report.Totals)
}

func TestCodexPromptPRLinks(t *testing.T) {
	const id = "019f0000-0000-7000-8000-000000000004"
	env := setupTestEnv(t)
	env.writeCodexSession(t, "2026/07/29", "rollout-2026-07-29T14-55-00-"+id+".jsonl", testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(id, "/workspace/project", "user", tsZero),
		testjsonl.CodexMsgJSON("user", "$pr https://github.com/owner/repo/pull/123", tsZero),
	))
	require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)
	assert.Equal(t, []string{"https://github.com/owner/repo/pull/123"}, storedPRURLs(t, env.db, "codex:"+id))
}

func TestIncrementalSync_PromptPRLinks(t *testing.T) {
	env := setupTestEnv(t)
	path := env.writeClaudeSession(t, "proj", "prompt-append.jsonl", testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("Start", tsZero),
		testjsonl.ClaudeAssistantJSON("Working", tsZeroS1),
	))
	require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)
	appendClaudeSplitLines(t, path, testjsonl.ClaudeUserJSON("https://github.com/owner/repo/pull/1", "2024-01-01T00:00:02Z"))
	env.engine.SyncPaths([]string{path})
	sess, err := env.db.GetSessionFull(t.Context(), "prompt-append")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.True(t, sess.LastWriteIncremental)
	assert.Equal(t, []string{"https://github.com/owner/repo/pull/1"}, storedPRURLs(t, env.db, "prompt-append"))
}

func TestIncrementalStructuredLinkAlsoInPrompt(t *testing.T) {
	env := setupTestEnv(t)
	path := env.writeClaudeSession(t, "proj", "shared-pr.jsonl", testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("Review https://github.com/owner/repo/pull/1", tsZero),
		testjsonl.ClaudeAssistantJSON("Working", tsZeroS1),
		`{"type":"pr-link","prNumber":2,"prUrl":"https://github.com/owner/repo/pull/2","prRepository":"owner/repo","timestamp":"2024-01-01T00:00:02Z"}`,
	))
	require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)
	appendClaudeSplitLines(t, path, `{"type":"pr-link","prNumber":1,"prUrl":"https://github.com/owner/repo/pull/1","prRepository":"owner/repo","timestamp":"2024-01-01T00:00:03Z"}`)
	env.engine.SyncPaths([]string{path})
	links, err := env.db.GetSessionStructuredPRLinks(t.Context(), "shared-pr")
	require.NoError(t, err)
	require.Len(t, links, 2)
	assert.Equal(t, "https://github.com/owner/repo/pull/2", links[0].URL)
	assert.Equal(t, "https://github.com/owner/repo/pull/1", links[1].URL)
}

func TestParseDiffDetectsPRLinkChanges(t *testing.T) {
	env := setupTestEnv(t)
	env.writeClaudeSession(t, "proj", "changed-pr.jsonl", testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("Review https://github.com/owner/repo/pull/1", tsZero),
		testjsonl.ClaudeAssistantJSON("Working", tsZeroS1),
		`{"type":"pr-link","prNumber":2,"prUrl":"https://github.com/owner/repo/pull/2","prRepository":"owner/repo","timestamp":"2024-01-01T00:00:02Z"}`,
	))
	require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)
	report := runParseDiff(t, env, sync.ParseDiffOptions{})
	assert.Equal(t, 1, report.Totals.Identical)
	mutateDB(t, env, `UPDATE sessions SET pr_links='' WHERE id='changed-pr'`)
	report = runParseDiff(t, env, sync.ParseDiffOptions{})
	assert.Equal(t, 1, report.Totals.Changed)
	assert.Equal(t, 1, report.FieldCounts[sync.FieldPRLinks])
}

func TestCodexCommandPRLinksOnAppend(t *testing.T) {
	root := t.TempDir()
	env := setupSingleAgentTestEnv(t, parser.AgentCodex)
	const id = "01900000-0000-7000-8000-000000000042"
	path := env.writeCodexSession(t, filepath.Join("2024", "01", "15"), "rollout-20240115-"+id+".jsonl", testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaWithFieldsJSON(id, root, "user", tsEarly, nil),
		testjsonl.CodexMsgJSON("user", "Review https://github.com/example/repo/pull/42", tsEarlyS1),
	))
	require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)
	assert.Equal(t, []string{"https://github.com/example/repo/pull/42"}, storedPRURLs(t, env.db, "codex:"+id))
	appendClaudeSplitLines(t, path, testjsonl.JoinJSONL(
		testjsonl.CodexFunctionCallWithCallIDJSON("exec_command", "call2", map[string]any{"cmd": `gh pr comment --body "$BODY" 43 -R github.com/example/repo`}, "2024-01-01T10:00:06Z"),
	))
	env.engine.SyncPaths([]string{path})
	sess, err := env.db.GetSessionFull(t.Context(), "codex:"+id)
	require.NoError(t, err)
	require.True(t, sess.LastWriteIncremental)
	assert.ElementsMatch(t, []string{"https://github.com/example/repo/pull/42", "https://github.com/example/repo/pull/43"}, storedPRURLs(t, env.db, "codex:"+id))
}

func TestCodexCommandPRLinksOnSync(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentCodex)
	cases := []struct{ id, command, url string }{
		{"01900000-0000-7000-8000-000000000011", "gh pr view 11 -R example/repo --json title", "https://github.com/example/repo/pull/11"},
		{"01900000-0000-7000-8000-000000000012", "gh pr merge 12 -R example/repo --squash", "https://github.com/example/repo/pull/12"},
		{"01900000-0000-7000-8000-000000000013", "gh pr comment https://github.com/example/repo/pull/13 --body done", "https://github.com/example/repo/pull/13"},
		{"01900000-0000-7000-8000-000000000014", "gh pr view 14 -R example/repo", "https://github.com/example/repo/pull/14"},
	}
	for _, tc := range cases {
		env.writeCodexSession(t, filepath.Join("2024", "01", "15"), "rollout-20240115-"+tc.id+".jsonl", testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaWithFieldsJSON(tc.id, t.TempDir(), "user", tsEarly, nil),
			testjsonl.CodexMsgJSON("user", "Review changes", tsEarlyS1),
			testjsonl.CodexFunctionCallWithCallIDJSON("exec_command", "call", map[string]any{"cmd": tc.command}, tsEarlyS5),
		))
	}
	require.Equal(t, 4, env.engine.SyncAll(t.Context(), nil).Synced)
	for _, tc := range cases {
		assert.Equal(t, []string{tc.url}, storedPRURLs(t, env.db, "codex:"+tc.id))
	}
}

func TestCodexWrappedCommandPRFilter(t *testing.T) {
	cases := []struct {
		id, name string
		input    map[string]any
		number   int
	}{
		{"01900000-0000-7000-8000-000000000041", "exec_command", map[string]any{"cmd": "command gh pr view 41 -R example/repo"}, 41},
		{"01900000-0000-7000-8000-000000000042", "exec_command", map[string]any{"cmd": "gh -R example/repo pr view 42"}, 42},
		{"01900000-0000-7000-8000-000000000043", "shell", map[string]any{"command": []string{"bash", "-lc", "gh pr view 43 -R example/repo"}}, 43},
		{"01900000-0000-7000-8000-000000000044", "shell", map[string]any{"command": "bash -lc 'gh pr view 44 -R example/repo'"}, 44},
		{"01900000-0000-7000-8000-000000000045", "exec_command", map[string]any{"cmd": "timeout 60 gh pr checks 45 -R example/repo --watch"}, 45},
	}
	for _, policy := range []config.ArchiveContent{config.ArchiveContentFull, config.ArchiveContentTranscripts} {
		t.Run(string(policy), func(t *testing.T) {
			env := setupSingleAgentTestEnv(t, parser.AgentCodex)
			env.db.SetArchiveContent(policy)
			for _, tc := range cases {
				env.writeCodexSession(t, filepath.Join("2024", "01", "15"), "rollout-20240115-"+tc.id+".jsonl", testjsonl.JoinJSONL(
					testjsonl.CodexSessionMetaWithFieldsJSON(tc.id, t.TempDir(), "user", tsEarly, nil),
					testjsonl.CodexMsgJSON("user", "Review changes", tsEarlyS1),
					testjsonl.CodexFunctionCallWithCallIDJSON(tc.name, "call", tc.input, tsEarlyS5),
				))
			}
			require.Equal(t, len(cases), env.engine.SyncAll(t.Context(), nil).Synced)
			for _, tc := range cases {
				id := "codex:" + tc.id
				page, err := env.db.ListSessions(t.Context(), db.SessionFilter{PR: db.PRFilter{Repository: "example/repo", Number: tc.number}})
				require.NoError(t, err)
				require.Len(t, page.Sessions, 1)
				assert.Equal(t, id, page.Sessions[0].ID)
				if policy == config.ArchiveContentTranscripts {
					messages, err := env.db.GetAllMessages(t.Context(), id)
					require.NoError(t, err)
					for _, message := range messages {
						for _, call := range message.ToolCalls {
							assert.Empty(t, call.InputJSON)
						}
					}
				}
			}
		})
	}
}

func TestCommandPRCheckoutRemote(t *testing.T) {
	testutil.IsolateGit(t)
	for _, tc := range []struct {
		name, config, recorded string
		agent                  parser.AgentType
		want                   bool
		parseDiff              bool
	}{
		{"single Claude", "[remote \"origin\"]\n url = https://github.com/example/repo.git\n", "", parser.AgentClaude, true, true},
		{"ambiguous", "[remote \"origin\"]\n url = https://github.com/example/repo.git\n[remote \"upstream\"]\n url = https://github.com/example/other.git\n", "", parser.AgentCodex, false, false},
		{"ambiguous recorded", "[remote \"origin\"]\n url = https://github.com/example/repo.git\n[remote \"upstream\"]\n url = https://github.com/example/other.git\n", "https://github.com/example/repo.git", parser.AgentCodex, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(root, ".git", "config"), []byte(tc.config), 0o600))
			env := setupSingleAgentTestEnv(t, tc.agent)
			const id = "01900000-0000-7000-8000-000000000081"
			var path, sessionID string
			if tc.agent == parser.AgentClaude {
				sessionID = "checkout-pr"
				path = env.writeClaudeSession(t, "proj", sessionID+".jsonl", testjsonl.JoinJSONL(
					testjsonl.ClaudeUserJSON("Finish the work", tsEarly, root),
					testjsonl.ClaudeAssistantJSON([]map[string]any{{"type": "tool_use", "id": "call", "name": "Bash", "input": map[string]any{"command": "gh pr view 42"}}}, tsEarlyS1),
				))
			} else {
				sessionID = "codex:" + id
				fields := map[string]any{}
				if tc.recorded != "" {
					fields["git"] = map[string]any{"repository_url": tc.recorded}
				}
				path = env.writeCodexSession(t, filepath.Join("2024", "01", "15"), "rollout-20240115-"+id+".jsonl", testjsonl.JoinJSONL(
					testjsonl.CodexSessionMetaWithFieldsJSON(id, root, "user", tsEarly, fields),
					testjsonl.CodexMsgJSON("user", "Review changes", tsEarlyS1),
					testjsonl.CodexFunctionCallWithCallIDJSON("exec_command", "call", map[string]any{"cmd": "gh pr view 42"}, tsEarlyS5),
				))
			}
			require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)
			if tc.want {
				assert.Equal(t, []string{"https://github.com/example/repo/pull/42"}, storedPRURLs(t, env.db, sessionID))
				if tc.parseDiff {
					report := runParseDiff(t, env, sync.ParseDiffOptions{})
					assert.Equal(t, 1, report.Totals.Identical)
				}
			} else {
				assert.Empty(t, storedPRURLs(t, env.db, sessionID))
				appendClaudeSplitLines(t, path, testjsonl.CodexFunctionCallWithCallIDJSON("exec_command", "append", map[string]any{"cmd": "gh pr view 43"}, "2024-01-01T10:00:06Z"))
				env.engine.SyncPaths([]string{path})
				assert.Empty(t, storedPRURLs(t, env.db, sessionID))
			}
		})
	}
}

func TestClineTeamTaskPRLinks(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "data", "sessions", "team-pr")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "team-pr.json"), []byte(`{"session_id":"team-pr","cwd":"/workspace"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "team-pr.messages.json"), []byte(`{"messages":[{"id":"m1","role":"user","content":[{"type":"text","text":"Delegate review"}],"ts":1000},{"id":"m2","role":"assistant","content":[{"type":"tool_use","id":"call","name":"team_run_task","input":{"task":"Review https://github.com/example/repo/pull/42"}}],"ts":2000}]}`), 0o600))
	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCline: {root}}, Machine: "local"})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	assert.Equal(t, []string{"https://github.com/example/repo/pull/42"}, storedPRURLs(t, database, "cline:team-pr"))
}

func TestClineCommandArrayPRLinks(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "data", "sessions", "command-pr")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "command-pr.json"), []byte(`{"session_id":"command-pr","cwd":"/workspace"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "command-pr.messages.json"), []byte(`{"messages":[{"id":"m1","role":"user","content":[{"type":"text","text":"Review changes"}],"ts":1000},{"id":"m2","role":"assistant","content":[{"type":"tool_use","id":"call","name":"execute_command","input":{"commands":["gh pr view 42 -R example/repo","gh pr view 43 -R example/repo"]}}],"ts":2000}]}`), 0o600))
	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCline: {root}}, Machine: "local"})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	assert.Equal(t, []string{"https://github.com/example/repo/pull/42", "https://github.com/example/repo/pull/43"}, storedPRURLs(t, database, "cline:command-pr"))
	for _, n := range []int{42, 43} {
		page, err := database.ListSessions(t.Context(), db.SessionFilter{PR: db.PRFilter{Repository: "example/repo", Number: n}})
		require.NoError(t, err)
		require.Len(t, page.Sessions, 1)
		assert.Equal(t, "cline:command-pr", page.Sessions[0].ID)
	}
}
