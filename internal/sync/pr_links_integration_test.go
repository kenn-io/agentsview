package sync_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
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
		testjsonl.ClaudeAssistantJSON([]map[string]any{{"type": "tool_use", "id": "call", "name": "Bash", "input": map[string]any{"command": "gh pr view 42 -R example/repo"}}}, tsZeroS1),
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
		testjsonl.CodexMsgJSON("user", "Finish the work", tsEarlyS1),
	))
	require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)
	appendClaudeSplitLines(t, path, testjsonl.CodexFunctionCallWithCallIDJSON("exec_command", "call1", map[string]any{"cmd": "gh pr merge 42 -R example/repo"}, tsEarlyS5))
	env.engine.SyncPaths([]string{path})
	filter, err := db.ParsePRFilter("example/repo#42")
	require.NoError(t, err)
	page, err := env.db.ListSessions(t.Context(), db.SessionFilter{PR: filter})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	assert.Equal(t, "codex:"+id, page.Sessions[0].ID)

	appendClaudeSplitLines(t, path, testjsonl.CodexFunctionCallWithCallIDJSON("exec_command", "call2", map[string]any{"cmd": "gh pr view 43 --json title"}, "2024-01-01T10:00:06Z"))
	env.engine.SyncPaths([]string{path})
	assert.ElementsMatch(t, []string{"https://github.com/example/repo/pull/42", "https://github.com/example/repo/pull/43"}, storedPRURLs(t, env.db, "codex:"+id))
}

func TestClaudeCommandPRUsesCheckoutRemote(t *testing.T) {
	testutil.IsolateGit(t)
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".git", "config"), []byte("[remote \"origin\"]\n url = https://github.com/example/repo.git\n"), 0o600))
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)
	env.writeClaudeSession(t, "proj", "checkout-pr.jsonl", testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("Finish the work", tsEarly, root),
		testjsonl.ClaudeAssistantJSON([]map[string]any{{"type": "tool_use", "id": "call", "name": "Bash", "input": map[string]any{"command": "gh pr view 42 --json title"}}}, tsEarlyS1),
	))
	require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)
	filter, err := db.ParsePRFilter("example/repo#42")
	require.NoError(t, err)
	page, err := env.db.ListSessions(t.Context(), db.SessionFilter{PR: filter})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	assert.Equal(t, "checkout-pr", page.Sessions[0].ID)
}

func TestCodeModeCommandPRUsesItsWorkdir(t *testing.T) {
	testutil.IsolateGit(t)
	base := t.TempDir()
	for _, tc := range []struct{ dir, config string }{
		{"first", "[remote \"origin\"]\n url = https://github.com/example/first.git\n"},
		{"second", "[remote \"origin\"]\n url = https://github.com/example/second.git\n"},
		{"ambiguous", "[remote \"origin\"]\n url = https://github.com/example/fork.git\n[remote \"upstream\"]\n url = https://github.com/example/upstream.git\n"},
	} {
		dir := filepath.Join(base, tc.dir, ".git")
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "config"), []byte(tc.config), 0o600))
	}
	env := setupSingleAgentTestEnv(t, parser.AgentCodex)
	const id = "01900000-0000-7000-8000-000000000091"
	code := fmt.Sprintf(`text(await tools.exec_command({cmd:"gh pr view 41",workdir:%q})); text(await tools.exec_command({cmd:"gh pr view 42",workdir:%q})); text(await tools.exec_command({cmd:"gh pr view 44",workdir:%q})); text(await tools.exec_command({cmd:"gh pr view 45",workdir:%q}))`, filepath.Join(base, "first"), filepath.Join(base, "second"), filepath.Join(base, "ambiguous"), filepath.Join(base, "missing"))
	path := env.writeCodexSession(t, filepath.Join("2024", "01", "15"), "rollout-20240115-"+id+".jsonl", testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(id, filepath.Join(base, "first"), "user", tsEarly),
		testjsonl.CodexMsgJSON("user", "Review changes", tsEarlyS1),
		testjsonl.CodexFunctionCallWithCallIDJSON("exec", "first", code, tsEarlyS5),
	))
	require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)
	assert.ElementsMatch(t, []string{"https://github.com/example/first/pull/41", "https://github.com/example/second/pull/42"}, storedPRURLs(t, env.db, "codex:"+id))
	code = fmt.Sprintf(`text(await tools.exec_command({cmd:"gh pr view 43",workdir:%q}))`, filepath.Join(base, "second"))
	appendClaudeSplitLines(t, path, testjsonl.CodexFunctionCallWithCallIDJSON("exec", "second", code, "2024-01-01T10:00:06Z"))
	env.engine.SyncPaths([]string{path})
	assert.ElementsMatch(t, []string{"https://github.com/example/first/pull/41", "https://github.com/example/second/pull/42", "https://github.com/example/second/pull/43"}, storedPRURLs(t, env.db, "codex:"+id))
}

func TestCommandPRLinksSurviveArchiveContentPolicy(t *testing.T) {
	for _, policy := range []config.ArchiveContent{config.ArchiveContentFull, config.ArchiveContentTranscripts, config.ArchiveContentUsage} {
		for _, bulk := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/bulk=%t", policy, bulk), func(t *testing.T) {
				env := setupSingleAgentTestEnv(t, parser.AgentClaude)
				env.db.SetArchiveContent(policy)
				if bulk {
					env.engine.Close()
					env.engine = sync.NewEngine(t.Context(), env.db, sync.EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {env.claudeDir}}, Machine: "local", ArchiveContent: policy, DiscardPendingWritesOnCancel: true})
					t.Cleanup(env.engine.Close)
				}
				path := env.writeClaudeSession(t, "proj", "policy-pr.jsonl", testjsonl.JoinJSONL(
					testjsonl.ClaudeUserJSON("Review the changes", tsEarly),
					testjsonl.ClaudeAssistantJSON([]map[string]any{{"type": "tool_use", "id": "call", "name": "Bash", "input": map[string]any{"command": "gh pr view 42 -R example/repo"}}}, tsEarlyS1),
				))
				require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)
				assert.Equal(t, []string{"https://github.com/example/repo/pull/42"}, storedPRURLs(t, env.db, "policy-pr"))
				structured, err := env.db.GetSessionStructuredPRLinks(t.Context(), "policy-pr")
				require.NoError(t, err)
				assert.Empty(t, structured)
				appendClaudeSplitLines(t, path, testjsonl.ClaudeAssistantJSON([]map[string]any{{"type": "tool_use", "id": "call2", "name": "Bash", "input": map[string]any{"command": "gh pr view 43 -R example/repo"}}}, tsEarlyS5))
				env.engine.SyncPaths([]string{path})
				assert.ElementsMatch(t, []string{"https://github.com/example/repo/pull/42", "https://github.com/example/repo/pull/43"}, storedPRURLs(t, env.db, "policy-pr"))
				structured, err = env.db.GetSessionStructuredPRLinks(t.Context(), "policy-pr")
				require.NoError(t, err)
				assert.Empty(t, structured)
				if policy == config.ArchiveContentUsage {
					require.NoError(t, env.db.RenameSession(t.Context(), "policy-pr", nil))
					assert.ElementsMatch(t, []string{"https://github.com/example/repo/pull/42", "https://github.com/example/repo/pull/43"}, storedPRURLs(t, env.db, "policy-pr"))
				}
			})
		}
	}
}

func TestStagedCommandPRLinksSurviveTranscriptPolicy(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentCodex)
	env.db.SetArchiveContent(config.ArchiveContentTranscripts)
	env.engine.Close()
	env.engine = sync.NewEngine(t.Context(), env.db, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {env.codexDir}},
		Machine:   "local", ArchiveContent: config.ArchiveContentTranscripts, StagedCodexParseMinBytes: 1,
	})
	t.Cleanup(env.engine.Close)
	const id = "01900000-0000-7000-8000-000000000098"
	env.writeCodexSession(t, filepath.Join("2024", "01", "15"), "rollout-20240115-"+id+".jsonl", testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(id, "/tmp/project", "user", tsEarly),
		testjsonl.CodexMsgJSON("user", "Review the changes", tsEarlyS1),
		testjsonl.CodexFunctionCallWithCallIDJSON("exec_command", "call", map[string]any{"cmd": "gh pr view 42 -R example/repo"}, tsEarlyS5),
	))
	require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)
	assert.Equal(t, []string{"https://github.com/example/repo/pull/42"}, storedPRURLs(t, env.db, "codex:"+id))
	structured, err := env.db.GetSessionStructuredPRLinks(t.Context(), "codex:"+id)
	require.NoError(t, err)
	assert.Empty(t, structured)
}

func TestUsageOnlyCleanupRetainsTaskAndCommandPRLinks(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)
	env.writeClaudeSession(t, "proj", "usage-links.jsonl", testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("Review https://github.com/example/repo/pull/44", tsEarly),
		testjsonl.ClaudeAssistantJSON([]map[string]any{
			{"type": "tool_use", "id": "command", "name": "Bash", "input": map[string]any{"command": "gh pr view 42 -R example/repo"}},
			{"type": "tool_use", "id": "task", "name": "Task", "input": map[string]any{"prompt": "Review https://github.com/example/repo/pull/43"}},
		}, tsEarlyS1),
	))
	require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)
	env.db.SetArchiveContent(config.ArchiveContentUsage)
	require.NoError(t, env.db.RenameSession(t.Context(), "usage-links", nil))
	assert.ElementsMatch(t, []string{"https://github.com/example/repo/pull/42", "https://github.com/example/repo/pull/43"}, storedPRURLs(t, env.db, "usage-links"))
	structured, err := env.db.GetSessionStructuredPRLinks(t.Context(), "usage-links")
	require.NoError(t, err)
	assert.Empty(t, structured)
}

func TestIncrementalPromptThenCommandSurvivesUsageOnly(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentCodex)
	const id = "01900000-0000-7000-8000-000000000092"
	path := env.writeCodexSession(t, filepath.Join("2024", "01", "15"), "rollout-20240115-"+id+".jsonl", testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(id, "/tmp/project", "user", tsEarly),
		testjsonl.CodexMsgJSON("user", "Review https://github.com/example/repo/pull/42", tsEarlyS1),
	))
	require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)
	appendClaudeSplitLines(t, path, testjsonl.CodexFunctionCallWithCallIDJSON("exec_command", "call", map[string]any{"cmd": "gh pr view 42 -R example/repo"}, tsEarlyS5))
	env.engine.SyncPaths([]string{path})
	assert.Equal(t, []string{"https://github.com/example/repo/pull/42"}, storedPRURLs(t, env.db, "codex:"+id))
	env.db.SetArchiveContent(config.ArchiveContentUsage)
	require.NoError(t, env.db.RenameSession(t.Context(), "codex:"+id, nil))
	assert.Equal(t, []string{"https://github.com/example/repo/pull/42"}, storedPRURLs(t, env.db, "codex:"+id))
}

func TestAmbiguousCheckoutRemoteDoesNotInferPR(t *testing.T) {
	testutil.IsolateGit(t)
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".git", "config"), []byte("[remote \"origin\"]\n url = https://github.com/example/fork.git\n[remote \"upstream\"]\n url = https://github.com/example/upstream.git\n"), 0o600))
	env := setupSingleAgentTestEnv(t, parser.AgentCodex)
	var paths []string
	for _, tc := range []struct {
		id       string
		recorded bool
	}{
		{"01900000-0000-7000-8000-000000000081", false},
		{"01900000-0000-7000-8000-000000000082", true},
	} {
		fields := map[string]any{}
		if tc.recorded {
			fields["git"] = map[string]any{"repository_url": "https://github.com/example/fork.git"}
		}
		paths = append(paths, env.writeCodexSession(t, filepath.Join("2024", "01", "15"), "rollout-20240115-"+tc.id+".jsonl", testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaWithFieldsJSON(tc.id, root, "user", tsEarly, fields),
			testjsonl.CodexMsgJSON("user", "Review changes", tsEarlyS1),
			testjsonl.CodexFunctionCallWithCallIDJSON("exec_command", "call", map[string]any{"cmd": "gh pr view 42"}, tsEarlyS5),
		)))
	}
	require.Equal(t, 2, env.engine.SyncAll(t.Context(), nil).Synced)
	for _, id := range []string{"01900000-0000-7000-8000-000000000081", "01900000-0000-7000-8000-000000000082"} {
		assert.Empty(t, storedPRURLs(t, env.db, "codex:"+id))
	}
	for _, path := range paths {
		appendClaudeSplitLines(t, path, testjsonl.CodexFunctionCallWithCallIDJSON("exec_command", "append", map[string]any{"cmd": "gh pr view 43"}, "2024-01-01T10:00:06Z"))
	}
	env.engine.SyncPaths(paths)
	for _, id := range []string{"01900000-0000-7000-8000-000000000081", "01900000-0000-7000-8000-000000000082"} {
		assert.Empty(t, storedPRURLs(t, env.db, "codex:"+id))
	}
}
