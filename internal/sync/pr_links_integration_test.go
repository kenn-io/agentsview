package sync_test

import (
	"encoding/json"
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
	env := setupSingleAgentTestEnv(t, parser.AgentCodex)
	const id = "01900000-0000-7000-8000-000000000042"
	path := env.writeCodexSession(t, filepath.Join("2024", "01", "15"), "rollout-20240115-"+id+".jsonl", testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaWithFieldsJSON(id, "/tmp/project", "user", tsEarly, map[string]any{"git": map[string]any{"repository_url": "https://github.com/example/repo.git"}}),
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
	appendClaudeSplitLines(t, path, testjsonl.CodexFunctionCallOutputJSON("call2", json.RawMessage(`"other/repo#43"`), "2024-01-01T10:00:07Z"))
	env.engine.SyncPaths([]string{path})
	assert.Equal(t, []string{"https://github.com/example/repo/pull/42", "https://github.com/example/repo/pull/43"}, storedPRURLs(t, env.db, "codex:"+id))
	other := "01900000-0000-7000-8000-000000000053"
	otherPath := env.writeCodexSession(t, filepath.Join("2024", "01", "15"), "rollout-20240115-"+other+".jsonl", testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(other, "/tmp/project", "user", tsEarly),
		testjsonl.CodexMsgJSON("user", "other", tsEarlyS1),
	))
	env.engine.SyncPaths([]string{otherPath})
	before, err := env.db.GetAllMessages(t.Context(), "codex:"+id)
	require.NoError(t, err)
	require.NotEmpty(t, before)
	appendClaudeSplitLines(t, path, testjsonl.CodexFunctionCallWithCallIDJSON("exec_command", "call3", map[string]any{"cmd": "gh pr view 42 -R example/repo"}, "2024-01-01T10:00:08Z"))
	env.engine.SyncPaths([]string{path})
	after, err := env.db.GetAllMessages(t.Context(), "codex:"+id)
	require.NoError(t, err)
	require.NotEmpty(t, after)
	assert.Equal(t, before[0].ID, after[0].ID)
}

func TestCommandPRFilterSeparatesRepositories(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentCodex)
	ids := []string{"01900000-0000-7000-8000-000000000051", "01900000-0000-7000-8000-000000000052"}
	for i, repo := range []string{"example/alpha", "example/beta"} {
		env.writeCodexSession(t, filepath.Join("2024", "01", "15"), "rollout-20240115-"+ids[i]+".jsonl", testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON(ids[i], "/tmp/project", "user", tsEarly),
			testjsonl.CodexMsgJSON("user", "Finish the work", tsEarlyS1),
			testjsonl.CodexFunctionCallWithCallIDJSON("exec_command", "call", map[string]any{"cmd": "gh pr merge 42 -R " + repo}, tsEarlyS5),
		))
	}
	require.Equal(t, 2, env.engine.SyncAll(t.Context(), nil).Synced)
	filter, err := db.ParsePRFilter("example/alpha#42")
	require.NoError(t, err)
	page, err := env.db.ListSessions(t.Context(), db.SessionFilter{PR: filter})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	assert.Equal(t, "codex:"+ids[0], page.Sessions[0].ID)
}

func TestClaudeCommandPRUsesCheckoutRemote(t *testing.T) {
	testutil.IsolateGit(t)
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".git", "config"), []byte("[remote \"origin\"]\n url = https://github.com/example/repo.git\n"), 0600))
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)
	var checkoutPath string
	for _, tt := range []struct {
		id    string
		input map[string]any
	}{
		{"checkout-pr", map[string]any{"command": "gh pr view 42 --json title"}},
		{"other-dir-pr", map[string]any{"command": "gh pr view 42", "workdir": "/tmp/elsewhere"}},
	} {
		p := env.writeClaudeSession(t, "proj", tt.id+".jsonl", testjsonl.JoinJSONL(
			testjsonl.ClaudeUserJSON("Finish the work", tsEarly, root),
			testjsonl.ClaudeAssistantJSON([]map[string]any{{"type": "tool_use", "id": "call", "name": "Bash", "input": tt.input}}, tsEarlyS1),
		))
		if tt.id == "checkout-pr" {
			checkoutPath = p
		}
	}
	require.Equal(t, 2, env.engine.SyncAll(t.Context(), nil).Synced)
	filter, err := db.ParsePRFilter("example/repo#42")
	require.NoError(t, err)
	page, err := env.db.ListSessions(t.Context(), db.SessionFilter{PR: filter})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	assert.Equal(t, "checkout-pr", page.Sessions[0].ID)
	appendClaudeSplitLines(t, checkoutPath, testjsonl.ClaudeAssistantJSON([]map[string]any{{"type": "tool_use", "id": "call2", "name": "Bash", "input": map[string]any{"command": "gh pr view 43 -R example/repo"}}}, tsEarlyS5))
	env.engine.SyncPaths([]string{checkoutPath})
	assert.ElementsMatch(t, []string{"https://github.com/example/repo/pull/42", "https://github.com/example/repo/pull/43"}, storedPRURLs(t, env.db, "checkout-pr"))
}

func TestCommandPRLinksSurviveArchiveContentPolicy(t *testing.T) {
	for _, policy := range []config.ArchiveContent{config.ArchiveContentTranscripts, config.ArchiveContentUsage} {
		t.Run(string(policy), func(t *testing.T) {
			env := setupSingleAgentTestEnv(t, parser.AgentClaude)
			env.db.SetArchiveContent(policy)
			env.writeClaudeSession(t, "proj", "policy-pr.jsonl", testjsonl.JoinJSONL(
				testjsonl.ClaudeUserJSON("Review the changes", tsEarly),
				testjsonl.ClaudeAssistantJSON([]map[string]any{{"type": "tool_use", "id": "call", "name": "Bash", "input": map[string]any{"command": "gh pr view 42 -R example/repo"}}}, tsEarlyS1),
			))
			require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)
			assert.Equal(t, []string{"https://github.com/example/repo/pull/42"}, storedPRURLs(t, env.db, "policy-pr"))
			structured, err := env.db.GetSessionStructuredPRLinks(t.Context(), "policy-pr")
			require.NoError(t, err)
			require.Len(t, structured, 1)
			assert.Equal(t, "https://github.com/example/repo/pull/42", structured[0].URL)
		})
	}
}

func TestParseDiffDetectsCommandPRLinks(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)
	env.writeClaudeSession(t, "proj", "diff-tool-pr.jsonl", testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("Review the changes", tsEarly),
		testjsonl.ClaudeAssistantJSON([]map[string]any{{"type": "tool_use", "id": "call", "name": "Bash", "input": map[string]any{"command": "gh pr view 42 -R example/repo"}}}, tsEarlyS1),
	))
	require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)
	report := runParseDiff(t, env, sync.ParseDiffOptions{})
	assert.Equal(t, 1, report.Totals.Identical)
	mutateDB(t, env, `UPDATE sessions SET pr_links='' WHERE id='diff-tool-pr'`)
	report = runParseDiff(t, env, sync.ParseDiffOptions{})
	assert.Equal(t, 1, report.FieldCounts[sync.FieldPRLinks])
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
}

func TestPRFilterCommandAliasAndRedirection(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentCodex)
	for _, tc := range []struct{ id, command string }{
		{"01900000-0000-7000-8000-000000000061", "gh pr co 42 -R example/alpha"},
		{"01900000-0000-7000-8000-000000000062", "true > /no/such/directory/output && gh pr view 42 -R example/alpha"},
		{"01900000-0000-7000-8000-000000000063", "gh pr view 42 -R example/beta"},
	} {
		env.writeCodexSession(t, filepath.Join("2024", "01", "15"), "rollout-20240115-"+tc.id+".jsonl", testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON(tc.id, "/tmp/project", "user", tsEarly),
			testjsonl.CodexMsgJSON("user", "Review the work", tsEarlyS1),
			testjsonl.CodexFunctionCallWithCallIDJSON("exec_command", "call", map[string]any{"cmd": tc.command}, tsEarlyS5),
		))
	}
	require.Equal(t, 3, env.engine.SyncAll(t.Context(), nil).Synced)
	filter, err := db.ParsePRFilter("example/alpha#42")
	require.NoError(t, err)
	page, err := env.db.ListSessions(t.Context(), db.SessionFilter{PR: filter})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	assert.Equal(t, "codex:01900000-0000-7000-8000-000000000061", page.Sessions[0].ID)
}
