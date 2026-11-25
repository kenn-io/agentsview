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

func TestUsageOnlyResyncPreservesPRLinkHistoryAndUncertainty(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentCodex)
	env.db.SetArchiveContent(config.ArchiveContentUsage)
	const id = "01900000-0000-7000-8000-000000000095"
	root := t.TempDir()
	meta := testjsonl.CodexSessionMetaWithFieldsJSON(id, root, "user", tsEarly, map[string]any{"git": map[string]any{"repository_url": "https://github.com/example/repo.git"}})
	initial := testjsonl.JoinJSONL(meta,
		testjsonl.CodexMsgJSON("user", "Review", tsEarlyS1),
		testjsonl.CodexFunctionCallWithCallIDJSON("exec_command", "first", map[string]any{"cmd": "gh pr view 41 -R github.com/example/repo; git remote set-url origin https://git.example/example/repo.git"}, tsEarlyS5))
	path := env.writeCodexSession(t, "2024/01/15", "rollout-20240115-"+id+".jsonl", initial)
	require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)
	assert.Equal(t, []string{"https://github.com/example/repo/pull/41"}, storedPRURLs(t, env.db, "codex:"+id))
	rewritten := testjsonl.JoinJSONL(meta,
		testjsonl.CodexMsgJSON("user", "Review", tsEarlyS1),
		testjsonl.CodexFunctionCallWithCallIDJSON("exec_command", "second", map[string]any{"cmd": "gh pr view 42 -R example/repo; gh pr view 43 -R git.example/example/repo"}, tsEarlyS5))
	require.NoError(t, os.WriteFile(path, []byte(rewritten), 0o600))
	stats := env.engine.ResyncAll(t.Context(), nil)
	require.False(t, stats.Aborted, "%v", stats.Warnings)
	assert.ElementsMatch(t, []string{"https://github.com/example/repo/pull/41", "https://git.example/example/repo/pull/43"}, storedPRURLs(t, env.db, "codex:"+id))
}

func TestUsageOnlyResyncUsesRecordedCheckout(t *testing.T) {
	testutil.IsolateGit(t)
	for _, tc := range []struct {
		name, initial, host string
	}{
		{"single", "[remote \"origin\"]\n url = https://git-a.example/example/repo.git\n", "git-a.example"},
		{"resolved fork", "[remote \"origin\"]\n url = https://git-a.example/example/repo.git\n[remote \"upstream\"]\n url = https://git-c.example/example/repo.git\n", "git-a.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupSingleAgentTestEnv(t, parser.AgentClaude)
			env.db.SetArchiveContent(config.ArchiveContentUsage)
			root := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0o700))
			configPath := filepath.Join(root, ".git", "config")
			writeRemote := func(host string) {
				t.Helper()
				require.NoError(t, os.WriteFile(configPath, []byte("[remote \"origin\"]\n url = https://"+host+"/example/repo.git\n"), 0o600))
			}
			require.NoError(t, os.WriteFile(configPath, []byte(tc.initial), 0o600))
			env.writeClaudeSession(t, "proj", "recorded-checkout.jsonl", testjsonl.JoinJSONL(
				testjsonl.ClaudeUserJSON("Review", tsZero, root),
				`{"type":"assistant","timestamp":"2024-01-01T00:00:01Z","message":{"content":[{"type":"tool_use","id":"call","name":"Bash","input":{"command":"gh pr view 42 -R example/repo"}}]}}`,
			))
			require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)
			want := []string{"https://" + tc.host + "/example/repo/pull/42"}
			require.Equal(t, want, storedPRURLs(t, env.db, "recorded-checkout"))
			writeRemote("git-b.example")
			env.engine.Close()
			engine := sync.NewEngine(t.Context(), env.db, sync.EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {env.claudeDir}}, Machine: "local"})
			t.Cleanup(engine.Close)
			stats := engine.ResyncAll(t.Context(), nil)
			require.False(t, stats.Aborted, "%v", stats.Warnings)
			assert.Equal(t, want, storedPRURLs(t, env.db, "recorded-checkout"))
		})
	}
}

func TestUsageOnlyParseDiffMultiRemoteOrigin(t *testing.T) {
	testutil.IsolateGit(t)
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)
	env.db.SetArchiveContent(config.ArchiveContentUsage)
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".git", "config"), []byte("[remote \"origin\"]\n url = https://ghe.example/fork/repo.git\n[remote \"upstream\"]\n url = https://ghe.example/org/repo.git\n"), 0o600))
	env.writeClaudeSession(t, "proj", "multi-remote.jsonl", testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("Review", tsZero, root),
		`{"type":"assistant","timestamp":"2024-01-01T00:00:01Z","message":{"content":[{"type":"tool_use","id":"call","name":"Bash","input":{"command":"gh pr view 42 -R org/repo"}}]}}`,
	))
	require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)
	snapshots, err := env.db.ListSessionProjectIdentitySnapshotsByID(t.Context(), []string{"multi-remote"})
	require.NoError(t, err)
	require.Equal(t, 2, snapshots["multi-remote"].RemoteCandidateCount)
	require.Equal(t, "https://ghe.example/fork/repo.git", snapshots["multi-remote"].GitRemote)
	link, ok := parser.NewPRLink("https://ghe.example/org/repo/pull/42", "org/repo", 42)
	require.True(t, ok)
	encoded := db.EncodePRLinks([]db.PRLink{link})
	mutateDB(t, env, `UPDATE sessions SET structured_pr_links=?,pr_links=? WHERE id=?`, encoded, encoded, "multi-remote")
	report := runParseDiff(t, env, sync.ParseDiffOptions{})
	assert.Zero(t, report.FieldCounts[sync.FieldPRLinks])
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

func TestCodexPRLinksCheckoutHost(t *testing.T) {
	testutil.IsolateGit(t)
	for _, policy := range []config.ArchiveContent{config.ArchiveContentFull, config.ArchiveContentUsage} {
		for _, source := range []string{"recorded", "checkout"} {
			t.Run(string(policy)+"/"+source, func(t *testing.T) {
				env := setupSingleAgentTestEnv(t, parser.AgentCodex)
				env.db.SetArchiveContent(policy)
				const id = "01900000-0000-7000-8000-000000000091"
				root := t.TempDir()
				fields := map[string]any{}
				if source == "recorded" {
					fields["git"] = map[string]any{"repository_url": "https://git.example/example/repo.git"}
				} else {
					require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0o700))
					require.NoError(t, os.WriteFile(filepath.Join(root, ".git", "config"), []byte("[remote \"origin\"]\n url = https://git.example/example/repo.git\n"), 0o600))
				}
				path := env.writeCodexSession(t, filepath.Join("2024", "01", "15"), "rollout-20240115-"+id+".jsonl", testjsonl.JoinJSONL(
					testjsonl.CodexSessionMetaWithFieldsJSON(id, root, "user", tsEarly, fields),
					testjsonl.CodexMsgJSON("user", "Review changes", tsEarlyS1),
					testjsonl.CodexFunctionCallWithCallIDJSON("exec_command", "call1", map[string]any{"cmd": "gh pr view 42 -R example/repo"}, tsEarlyS5),
				))
				require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)
				assert.Equal(t, []string{"https://git.example/example/repo/pull/42"}, storedPRURLs(t, env.db, "codex:"+id))
				if policy == config.ArchiveContentFull {
					report := runParseDiff(t, env, sync.ParseDiffOptions{})
					assert.Equal(t, 1, report.Totals.Identical)
				}
				appendClaudeSplitLines(t, path, testjsonl.CodexFunctionCallWithCallIDJSON("exec_command", "call2", map[string]any{"cmd": "gh pr view 43 -R example/repo"}, "2024-01-01T10:00:06Z"))
				env.engine.SyncPaths([]string{path})
				sess, err := env.db.GetSessionFull(t.Context(), "codex:"+id)
				require.NoError(t, err)
				require.True(t, sess.LastWriteIncremental)
				assert.Equal(t, []string{"https://git.example/example/repo/pull/42", "https://git.example/example/repo/pull/43"}, storedPRURLs(t, env.db, "codex:"+id))
			})
		}
	}
	t.Run("ambiguous/full", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(root, ".git", "config"), []byte("[remote \"origin\"]\n url = https://git.example/example/repo.git\n[remote \"upstream\"]\n url = https://github.com/example/other.git\n"), 0o600))
		env := setupSingleAgentTestEnv(t, parser.AgentCodex)
		env.db.SetArchiveContent(config.ArchiveContentFull)
		const id = "01900000-0000-7000-8000-000000000081"
		path := env.writeCodexSession(t, filepath.Join("2024", "01", "15"), "rollout-20240115-"+id+".jsonl", testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaWithFieldsJSON(id, root, "user", tsEarly, nil),
			testjsonl.CodexMsgJSON("user", "Review changes", tsEarlyS1),
			testjsonl.CodexFunctionCallWithCallIDJSON("exec_command", "call", map[string]any{"cmd": "gh pr view 42 -R example/repo"}, tsEarlyS5),
		))
		require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)
		assert.Equal(t, []string{"https://github.com/example/repo/pull/42"}, storedPRURLs(t, env.db, "codex:"+id))
		report := runParseDiff(t, env, sync.ParseDiffOptions{})
		assert.Equal(t, 1, report.Totals.Identical)
		appendClaudeSplitLines(t, path, testjsonl.CodexFunctionCallWithCallIDJSON("exec_command", "append", map[string]any{"cmd": "gh pr view 43 -R example/repo"}, "2024-01-01T10:00:06Z"))
		env.engine.SyncPaths([]string{path})
		assert.Equal(t, []string{"https://github.com/example/repo/pull/42", "https://github.com/example/repo/pull/43"}, storedPRURLs(t, env.db, "codex:"+id))
	})
}

func TestClineToolPRLinks(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "data", "sessions", "team-pr")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "team-pr.json"), []byte(`{"session_id":"team-pr","cwd":"/workspace"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "team-pr.messages.json"), []byte(`{"messages":[{"id":"m1","role":"user","content":[{"type":"text","text":"Delegate review"}],"ts":1000},{"id":"m2","role":"assistant","content":[{"type":"tool_use","id":"call","name":"team_run_task","input":{"task":"Review https://github.com/example/repo/pull/42"}},{"type":"tool_use","id":"call2","name":"execute_command","input":{"commands":["gh pr view 43 -R example/repo"]}}],"ts":2000}]}`), 0o600))
	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCline: {root}}, Machine: "local"})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	assert.Equal(t, []string{"https://github.com/example/repo/pull/42", "https://github.com/example/repo/pull/43"}, storedPRURLs(t, database, "cline:team-pr"))
}
