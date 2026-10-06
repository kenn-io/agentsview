package sync_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func appendToFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(content)
	require.NoError(t, err)
	require.NoError(t, f.Close())
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

	// Claude Code repeats the record after later turns. A repeat of a
	// stored link must not force a full reparse: the malformed line is
	// only counted by a full parse.
	appendToFile(t, path, "not json\n"+prLink("1")+"\n")
	env.engine.SyncPaths([]string{path})
	stored, err := env.db.GetSessionFull(t.Context(), "pr-link-append")
	require.NoError(t, err)
	assert.Equal(t, 0, stored.ParserMalformedLines)
	assert.True(t, stored.LastWriteIncremental)

	// A new link reaches the session through a full reparse.
	appendToFile(t, path, prLink("2")+"\n")
	env.engine.SyncPaths([]string{path})
	assert.Equal(t, []string{
		"https://github.com/owner/repo/pull/1",
		"https://github.com/owner/repo/pull/2",
	}, storedPRURLs(t, env.db, "pr-link-append"))
	assertSessionMessageCount(t, env.db, "pr-link-append", 2)
}

func TestParseDiffAnnotations(t *testing.T) {
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
		t.Context(), "gemini:pd-worker", "pd-prs", "")
	require.NoError(t, err)
	report := runParseDiff(t, env, sync.ParseDiffOptions{})
	assert.Equal(t, sync.ParseDiffTotals{Examined: 2, Identical: 2}, report.Totals)

	// Missing pull request links on a Claude session are real drift, not
	// incremental-append history.
	mutateDB(t, env, "UPDATE sessions SET pr_links = '' WHERE id = ?", "pd-prs")
	report = runParseDiff(t, env, sync.ParseDiffOptions{})
	assert.Equal(t, sync.ParseDiffTotals{Examined: 2, Identical: 1, Changed: 1},
		report.Totals)
	assert.True(t, report.HasFailures())
}
