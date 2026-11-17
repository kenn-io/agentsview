package sync_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
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

func TestParseDiffIgnoresLauncherParentsButDetectsMissingPRLinks(t *testing.T) {
	env := setupFocusedTestEnv(t, parser.AgentClaude, parser.AgentGemini)
	env.writeClaudeSession(t, "test-proj", "pd-prs.jsonl", testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("Open a PR", tsZero),
		testjsonl.ClaudeAssistantJSON("Opened", tsZeroS1),
		`{"type":"pr-link","prNumber":1,"prUrl":"https://github.com/owner/repo/pull/1","prRepository":"owner/repo","timestamp":"2024-01-01T00:00:02Z"}`,
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

	// Missing pull request links on a Claude session are real drift, not
	// incremental-append history.
	mutateDB(t, env, "UPDATE sessions SET pr_links = '' WHERE id = ?", "pd-prs")
	report = runParseDiff(t, env, sync.ParseDiffOptions{})
	assert.Equal(t, sync.ParseDiffTotals{Examined: 2, Identical: 1, Changed: 1},
		report.Totals)
}

func TestCodexPromptPRLinkFilter(t *testing.T) {
	const id = "019f0000-0000-7000-8000-000000000004"
	env := setupTestEnv(t)
	env.writeCodexSession(t, "2026/07/29", "rollout-2026-07-29T14-55-00-"+id+".jsonl", testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(id, "/workspace/project", "user", tsZero),
		testjsonl.CodexMsgJSON("user", "$pr https://github.com/owner/repo/%70ull/123", tsZero),
	))
	require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)
	filter, err := db.ParsePRFilter("owner/repo#123")
	require.NoError(t, err)
	page, err := env.db.ListSessions(t.Context(), db.SessionFilter{PR: filter})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	assert.Equal(t, "codex:"+id, page.Sessions[0].ID)
}

func TestIncrementalSync_PromptPRLinks(t *testing.T) {
	env := setupTestEnv(t)
	path := env.writeClaudeSession(t, "proj", "prompt-append.jsonl", testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("Start", tsZero),
		testjsonl.ClaudeAssistantJSON("Working", tsZeroS1),
	))
	require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)
	appendClaudeSplitLines(t, path, testjsonl.ClaudeUserJSON("https://github.com/owner/repo/%70ull/1", "2024-01-01T00:00:02Z"))
	env.engine.SyncPaths([]string{path})
	assert.Equal(t, []string{"https://github.com/owner/repo/pull/1"}, storedPRURLs(t, env.db, "prompt-append"))
	appendClaudeSplitLines(t, path, testjsonl.ClaudeUserJSON("Again https://github.com/owner/repo/pull/1", "2024-01-01T00:00:03Z"))
	env.engine.SyncPaths([]string{path})
	sess, err := env.db.GetSessionFull(t.Context(), "prompt-append")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.True(t, sess.LastWriteIncremental)
	assert.Equal(t, []string{"https://github.com/owner/repo/pull/1"}, storedPRURLs(t, env.db, "prompt-append"))
}

func TestParseDiffDetectsRemovedStructuredPRLink(t *testing.T) {
	env := setupTestEnv(t)
	env.writeClaudeSession(t, "proj", "removed-pr.jsonl", testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("Review https://github.com/owner/repo/pull/1", tsZero),
		testjsonl.ClaudeAssistantJSON("Working", tsZeroS1),
	))
	require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)
	mutateDB(t, env, `UPDATE sessions SET structured_pr_links='[{"url":"https://github.com/owner/repo/pull/2","host":"github.com","repository":"owner/repo","number":2}]' WHERE id='removed-pr'`)
	report := runParseDiff(t, env, sync.ParseDiffOptions{})
	assert.Equal(t, 1, report.Totals.Changed)
}

func TestIncrementalStructuredLinkAlsoInPrompt(t *testing.T) {
	env := setupTestEnv(t)
	path := env.writeClaudeSession(t, "proj", "shared-pr.jsonl", testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("Review https://github.com/owner/repo/pull/1", tsZero),
		testjsonl.ClaudeAssistantJSON("Working", tsZeroS1),
		`{"type":"pr-link","prNumber":2,"prUrl":"https://github.com/owner/repo/pull/2","prRepository":"owner/repo","timestamp":"2024-01-01T00:00:02Z"}`,
	))
	require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)
	assert.Equal(t, []string{"https://github.com/owner/repo/pull/2", "https://github.com/owner/repo/pull/1"}, storedPRURLs(t, env.db, "shared-pr"))
	appendClaudeSplitLines(t, path, `{"type":"pr-link","prNumber":1,"prUrl":"https://github.com/owner/repo/pull/1","prRepository":"owner/repo","timestamp":"2024-01-01T00:00:03Z"}`)
	env.engine.SyncPaths([]string{path})
	require.NoError(t, env.db.ReplaceSessionMessages(t.Context(), "shared-pr", []db.Message{{SessionID: "shared-pr", Role: "user", Content: "Different prompt"}}))
	assert.Equal(t, []string{"https://github.com/owner/repo/pull/2", "https://github.com/owner/repo/pull/1"}, storedPRURLs(t, env.db, "shared-pr"))
}

func TestParseDiffDetectsObsoletePromptPRLink(t *testing.T) {
	env := setupTestEnv(t)
	env.writeClaudeSession(t, "proj", "obsolete-pr.jsonl", testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("Review https://github.com/owner/repo/pull/1", tsZero),
		testjsonl.ClaudeAssistantJSON("Working", tsZeroS1),
	))
	require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)
	mutateDB(t, env, `UPDATE sessions SET pr_links='[{"url":"https://github.com/owner/repo/pull/1","host":"github.com","repository":"owner/repo","number":1},{"url":"https://github.com/owner/repo/pull/2","host":"github.com","repository":"owner/repo","number":2}]' WHERE id='obsolete-pr'`)
	report := runParseDiff(t, env, sync.ParseDiffOptions{})
	assert.Equal(t, 1, report.Totals.Changed)
}
