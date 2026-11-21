package sync_test

import (
	"os"
	"path/filepath"
	"strconv"
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

func TestClineCommandArrayPRFilter(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "data", "sessions", "pr-commands")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pr-commands.json"), []byte(`{"session_id":"pr-commands","cwd":"/workspace"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pr-commands.messages.json"), []byte(`{"messages":[{"id":"m1","role":"user","content":[{"type":"text","text":"Review changes"}],"ts":1000},{"id":"m2","role":"assistant","content":[{"type":"tool_use","id":"call","name":"execute_command","input":{"commands":["gh pr view 41 -R example/other","gh pr view 42 -R example/repo"]}}],"ts":2000}]}`), 0o600))
	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCline: {root}}, Machine: "local"})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	filter, err := db.ParsePRFilter("example/repo#42")
	require.NoError(t, err)
	page, err := database.ListSessions(t.Context(), db.SessionFilter{PR: filter})
	require.NoError(t, err)
	if assert.Len(t, page.Sessions, 1) {
		assert.Equal(t, "cline:pr-commands", page.Sessions[0].ID)
	}
}

func TestRetainedForkCommandPRLinks(t *testing.T) {
	const parent = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	const child = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	for _, policy := range []config.ArchiveContent{config.ArchiveContentFull, config.ArchiveContentTranscripts} {
		for _, stage := range []int64{1, 1 << 30} {
			t.Run(string(policy)+"/"+strconv.FormatInt(stage, 10), func(t *testing.T) {
				env := setupSingleAgentTestEnv(t, parser.AgentCodex)
				env.db.SetArchiveContent(policy)
				env.engine.Close()
				env.engine = sync.NewEngine(t.Context(), env.db, sync.EngineConfig{
					AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {env.codexDir}},
					Machine:   "local", ArchiveContent: policy, StagedCodexParseMinBytes: stage,
				})
				t.Cleanup(env.engine.Close)
				meta := testjsonl.JoinJSONL(testjsonl.CodexForkedSessionMetaJSON(child, parent, "/work/project", "user", tsEarly))
				old := testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "old-turn", tsEarly), testjsonl.CodexFunctionCallWithCallIDJSON("exec_command", "call1", map[string]any{"cmd": "gh pr view 42 -R example/repo"}, tsEarlyS1))
				fresh := testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "new-turn", tsEarly), testjsonl.CodexFunctionCallWithCallIDJSON("exec_command", "call2", map[string]any{"cmd": "echo done"}, tsEarlyS5))
				path := env.writeCodexSession(t, "2024/01/15", "rollout-20240115-"+child+".jsonl", meta+old)
				require.Zero(t, env.engine.SyncAll(t.Context(), nil).Failed)
				want := []string{"https://github.com/example/repo/pull/42"}
				require.Equal(t, want, storedPRURLs(t, env.db, "codex:"+child))
				require.NoError(t, os.WriteFile(path, []byte(meta+fresh), 0o600))
				require.NoError(t, env.engine.TrySyncSessionWithSubagentsContext(t.Context(), "codex:"+child))
				messages, err := env.db.GetAllMessages(t.Context(), "codex:"+child)
				require.NoError(t, err)
				require.Len(t, messages, 2)
				assert.Equal(t, want, storedPRURLs(t, env.db, "codex:"+child))
				require.Zero(t, env.engine.ResyncAll(t.Context(), nil).Failed)
				assert.Equal(t, want, storedPRURLs(t, env.db, "codex:"+child))
			})
		}
	}
}

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
	testutil.IsolateGit(t)
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".git", "config"), []byte("[remote \"origin\"]\n url = https://github.com/example/repo.git\n"), 0o600))
	env := setupSingleAgentTestEnv(t, parser.AgentCodex)
	const id = "01900000-0000-7000-8000-000000000042"
	path := env.writeCodexSession(t, filepath.Join("2024", "01", "15"), "rollout-20240115-"+id+".jsonl", testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaWithFieldsJSON(id, root, "user", tsEarly, map[string]any{"git": map[string]any{"repository_url": "https://github.com/example/repo.git"}}),
		testjsonl.CodexMsgJSON("user", "Review https://github.com/example/repo/pull/42", tsEarlyS1),
	))
	require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)
	assert.Equal(t, []string{"https://github.com/example/repo/pull/42"}, storedPRURLs(t, env.db, "codex:"+id))
	appendClaudeSplitLines(t, path, testjsonl.JoinJSONL(
		testjsonl.CodexFunctionCallWithCallIDJSON("exec_command", "call1", map[string]any{"cmd": "gh pr merge 42 -R example/repo"}, tsEarlyS5),
		testjsonl.CodexFunctionCallWithCallIDJSON("exec_command", "call2", map[string]any{"cmd": "gh pr view 43 -R example/repo"}, "2024-01-01T10:00:06Z"),
	))
	env.engine.SyncPaths([]string{path})
	assert.ElementsMatch(t, []string{"https://github.com/example/repo/pull/42", "https://github.com/example/repo/pull/43"}, storedPRURLs(t, env.db, "codex:"+id))
}

func TestCommandPRLinksSurviveArchiveContentPolicy(t *testing.T) {
	for _, policy := range []config.ArchiveContent{config.ArchiveContentFull, config.ArchiveContentTranscripts, config.ArchiveContentUsage} {
		t.Run(string(policy), func(t *testing.T) {
			env := setupSingleAgentTestEnv(t, parser.AgentClaude)
			env.db.SetArchiveContent(policy)
			path := env.writeClaudeSession(t, "proj", "policy-pr.jsonl", testjsonl.JoinJSONL(
				testjsonl.ClaudeUserJSON("Review the changes", tsEarly),
				testjsonl.ClaudeAssistantJSON([]map[string]any{
					{"type": "tool_use", "id": "call", "name": "Bash", "input": map[string]any{"command": "gh pr view 42 -R example/repo"}},
					{"type": "tool_use", "id": "task", "name": "Task", "input": map[string]any{"prompt": "Review https://github.com/example/repo/pull/44"}},
				}, tsEarlyS1),
			))
			require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)
			assert.ElementsMatch(t, []string{"https://github.com/example/repo/pull/42", "https://github.com/example/repo/pull/44"}, storedPRURLs(t, env.db, "policy-pr"))
			structured, err := env.db.GetSessionStructuredPRLinks(t.Context(), "policy-pr")
			require.NoError(t, err)
			assert.Empty(t, structured)
			appendClaudeSplitLines(t, path, testjsonl.ClaudeAssistantJSON([]map[string]any{{"type": "tool_use", "id": "call2", "name": "Bash", "input": map[string]any{"command": "gh pr view 43 -R example/repo"}}}, tsEarlyS5))
			env.engine.SyncPaths([]string{path})
			want := []string{"https://github.com/example/repo/pull/42", "https://github.com/example/repo/pull/43", "https://github.com/example/repo/pull/44"}
			assert.ElementsMatch(t, want, storedPRURLs(t, env.db, "policy-pr"))
			structured, err = env.db.GetSessionStructuredPRLinks(t.Context(), "policy-pr")
			require.NoError(t, err)
			assert.Empty(t, structured)
			if policy == config.ArchiveContentFull {
				env.db.SetArchiveContent(config.ArchiveContentUsage)
				require.NoError(t, env.db.RenameSession(t.Context(), "policy-pr", nil))
				assert.ElementsMatch(t, want, storedPRURLs(t, env.db, "policy-pr"))
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
	}{
		{"single Claude", "[remote \"origin\"]\n url = https://github.com/example/repo.git\n", "", parser.AgentClaude, true},
		{"ambiguous", "[remote \"origin\"]\n url = https://github.com/example/repo.git\n[remote \"upstream\"]\n url = https://github.com/example/other.git\n", "", parser.AgentCodex, false},
		{"ambiguous recorded", "[remote \"origin\"]\n url = https://github.com/example/repo.git\n[remote \"upstream\"]\n url = https://github.com/example/other.git\n", "https://github.com/example/repo.git", parser.AgentCodex, false},
		{"case insensitive", "[remote \"origin\"]\n url = https://github.com/example/repo.git\n", "git@github.com:Example/Repo.git", parser.AgentCodex, true},
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
					testjsonl.ClaudeAssistantJSON([]map[string]any{{"type": "tool_use", "id": "call", "name": "Bash", "input": map[string]any{"command": "gh pr view 42 --json title"}}}, tsEarlyS1),
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
				filter, err := db.ParsePRFilter("example/repo#42")
				require.NoError(t, err)
				page, err := env.db.ListSessions(t.Context(), db.SessionFilter{PR: filter})
				require.NoError(t, err)
				require.Len(t, page.Sessions, 1)
				assert.Equal(t, sessionID, page.Sessions[0].ID)
			} else {
				assert.Empty(t, storedPRURLs(t, env.db, sessionID))
				appendClaudeSplitLines(t, path, testjsonl.CodexFunctionCallWithCallIDJSON("exec_command", "append", map[string]any{"cmd": "gh pr view 43"}, "2024-01-01T10:00:06Z"))
				env.engine.SyncPaths([]string{path})
				assert.Empty(t, storedPRURLs(t, env.db, sessionID))
			}
		})
	}
}

func TestCommandPRUsesChangedCheckoutAfterRestart(t *testing.T) {
	testutil.IsolateGit(t)
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0o700))
	gitConfig := filepath.Join(root, ".git", "config")
	require.NoError(t, os.WriteFile(gitConfig, []byte("[remote \"origin\"]\n url = https://github.com/example/first.git\n"), 0o600))
	env := setupSingleAgentTestEnv(t, parser.AgentCodex)
	const id = "01900000-0000-7000-8000-000000000100"
	path := env.writeCodexSession(t, filepath.Join("2024", "01", "15"), "rollout-20240115-"+id+".jsonl", testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(id, root, "user", tsEarly),
		testjsonl.CodexMsgJSON("user", "Review", tsEarlyS1),
	))
	require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)
	require.NoError(t, os.WriteFile(gitConfig, []byte("[remote \"origin\"]\n url = https://github.com/example/second.git\n"), 0o600))
	env.engine.Close()
	env.engine = sync.NewEngine(t.Context(), env.db, sync.EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {env.codexDir}}, Machine: "local"})
	t.Cleanup(env.engine.Close)
	appendClaudeSplitLines(t, path, testjsonl.CodexFunctionCallWithCallIDJSON("exec_command", "call", map[string]any{"cmd": "gh pr view 42"}, tsEarlyS5))
	env.engine.SyncPaths([]string{path})
	assert.Equal(t, []string{"https://github.com/example/second/pull/42"}, storedPRURLs(t, env.db, "codex:"+id))
}
