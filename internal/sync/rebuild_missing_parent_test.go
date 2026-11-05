package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestRebuildKeepsHealthyHistoryWhenCodexParentIsMissing(t *testing.T) {
	const parent = "11111111-1111-4111-8111-111111111111"
	const child = "22222222-2222-4222-8222-222222222222"
	const healthy = "33333333-3333-4333-8333-333333333333"
	const timestamp = "2026-09-01T10:00:00Z"
	root := t.TempDir()
	parentPath := filepath.Join(root, "rollout-2026-09-01T10-00-00-"+parent+".jsonl")
	childPath := filepath.Join(root, "rollout-2026-09-01T11-00-00-"+child+".jsonl")
	healthyPath := filepath.Join(root, "rollout-2026-09-01T12-00-00-"+healthy+".jsonl")
	parentContent := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(parent, "/work/project", "user", timestamp),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5", "parent-turn", timestamp),
		testjsonl.CodexMsgJSON("assistant", "parent answer", timestamp),
		testjsonl.CodexTokenCountJSON(timestamp, 100, 40, 0),
	)
	require.NoError(t, os.WriteFile(parentPath, []byte(parentContent), 0o600))
	require.NoError(t, os.WriteFile(childPath, []byte(testjsonl.JoinJSONL(
		testjsonl.CodexForkedSessionMetaJSON(child, parent, "/work/project", "user", timestamp),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5", "parent-turn", timestamp),
		testjsonl.CodexMsgJSON("assistant", "parent answer", timestamp),
		testjsonl.CodexTokenCountJSON(timestamp, 100, 40, 0),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", timestamp),
		testjsonl.CodexMsgJSON("assistant", "child answer", timestamp),
		testjsonl.CodexTokenCountJSON(timestamp, 20, 5, 0),
	)), 0o600))
	require.NoError(t, os.WriteFile(healthyPath, []byte(testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(healthy, "/work/project", "user", timestamp),
		testjsonl.CodexMsgJSON("assistant", "healthy answer", timestamp),
	)), 0o600))
	database := openTestDB(t)
	var retry WatchBatch
	engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Machine: "local", DisableFilesystemProjectDiscovery: true, OnRebuildDeferred: func(batch WatchBatch) { retry = batch }})
	t.Cleanup(engine.Close)
	initial := engine.SyncAll(t.Context(), nil)
	require.Zero(t, initial.Deferred)
	require.NoError(t, database.SetSessionDataVersion(t.Context(), "codex:"+child, 17))
	require.NoError(t, os.Remove(parentPath))
	engine.Close()
	engine = NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Machine: "local", DisableFilesystemProjectDiscovery: true, OnRebuildDeferred: func(batch WatchBatch) { retry = batch }})
	t.Cleanup(engine.Close)
	partialCtx, cancel := context.WithCancel(t.Context())
	partial := engine.ResyncAll(partialCtx, func(p Progress) {
		if p.Phase == PhaseSyncing && p.SessionsDone > 0 {
			cancel()
		}
	})
	cancel()
	require.True(t, partial.Aborted)
	assert.Equal(t, []string{"child answer"}, messageContents(t, database, "codex:"+child))
	assert.Empty(t, retry.Paths)
	rebuilt := engine.ResyncAll(t.Context(), nil)
	require.False(t, rebuilt.Aborted)
	require.True(t, rebuilt.ArchiveRebuilt)
	assert.Equal(t, 1, rebuilt.Deferred)
	assert.Equal(t, []string{"healthy answer"}, messageContents(t, database, "codex:"+healthy))
	assert.Equal(t, []string{"parent answer"}, messageContents(t, database, "codex:"+parent))
	assert.Equal(t, []string{"child answer"}, messageContents(t, database, "codex:"+child))
	assert.Less(t, database.GetSessionDataVersion(t.Context(), "codex:"+child), db.CurrentDataVersion())
	assert.Equal(t, []string{childPath}, retry.Paths)
	assert.True(t, engine.StartupReconciled())
	usageBefore, err := database.GetSessionUsage(t.Context(), "codex:"+child, true)
	require.NoError(t, err)
	require.NotNil(t, usageBefore)
	require.Equal(t, 5, usageBefore.TotalOutputTokens)
	require.NoError(t, engine.TrySyncSessionWithSubagentsContext(t.Context(), "codex:"+child))
	usageAfter, err := database.GetSessionUsage(t.Context(), "codex:"+child, true)
	require.NoError(t, err)
	require.NotNil(t, usageAfter)
	assert.Equal(t, 5, usageAfter.TotalOutputTokens)
	assert.Equal(t, []string{"child answer"}, messageContents(t, database, "codex:"+child))
	appendFile(t, childPath, testjsonl.JoinJSONL(
		testjsonl.CodexTurnContextWithIDJSON("gpt-5", "later-turn", timestamp),
		testjsonl.CodexMsgJSON("assistant", "later answer", timestamp),
	))
	again := engine.SyncAll(t.Context(), nil)
	assert.Equal(t, 1, again.Deferred)
	assert.Less(t, database.GetSessionDataVersion(t.Context(), "codex:"+child), db.CurrentDataVersion())
	assert.Equal(t, []string{"child answer", "later answer"}, messageContents(t, database, "codex:"+child))
	require.NoError(t, os.WriteFile(parentPath, []byte(parentContent), 0o600))
	recovered := engine.SyncAll(t.Context(), nil)
	require.Zero(t, recovered.Deferred)
	assert.Equal(t, []string{"child answer", "later answer"}, messageContents(t, database, "codex:"+child))
	assert.Equal(t, db.CurrentDataVersion(), database.GetSessionDataVersion(t.Context(), "codex:"+child))
	require.NoError(t, os.Remove(parentPath))
	require.NoError(t, engine.TrySyncSessionWithSubagentsContext(t.Context(), "codex:"+child))
	assert.Less(t, database.GetSessionDataVersion(t.Context(), "codex:"+child), db.CurrentDataVersion())
	assert.Equal(t, []string{"child answer", "later answer"}, messageContents(t, database, "codex:"+child))
	require.NoError(t, os.WriteFile(parentPath, []byte(parentContent), 0o600))
	assert.Zero(t, engine.SyncAll(t.Context(), nil).Deferred)
	assert.Equal(t, db.CurrentDataVersion(), database.GetSessionDataVersion(t.Context(), "codex:"+child))
}

func TestRebuildMissingParentRemainsRetryableAcrossRestart(t *testing.T) {
	const parent = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	const child = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	const timestamp = "2026-09-01T10:00:00Z"
	for _, stagedMin := range []int64{1, 1 << 30} {
		t.Run(fmt.Sprintf("staging-%d", stagedMin), func(t *testing.T) {
			root := t.TempDir()
			childPath := filepath.Join(root, "rollout-2026-09-01T11-00-00-"+child+".jsonl")
			parentPath := filepath.Join(root, "rollout-2026-09-01T10-00-00-"+parent+".jsonl")
			require.NoError(t, os.WriteFile(childPath, []byte(testjsonl.JoinJSONL(
				testjsonl.CodexForkedSessionMetaJSON(child, parent, "/work/project", "user", timestamp),
				testjsonl.CodexTurnContextWithIDJSON("gpt-5", "parent-turn", timestamp),
				testjsonl.CodexMsgJSON("assistant", "replayed answer", timestamp),
				testjsonl.CodexTokenCountJSON(timestamp, 100, 40, 0),
				testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", timestamp),
				testjsonl.CodexMsgJSON("assistant", "child answer", timestamp),
				testjsonl.CodexTokenCountJSON(timestamp, 20, 5, 0),
			)), 0o600))
			database := openTestDB(t)
			cfg := EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Machine: "local", DisableFilesystemProjectDiscovery: true, StagedCodexParseMinBytes: stagedMin}
			engine := NewEngine(t.Context(), database, cfg)
			stats := engine.ResyncAll(t.Context(), nil)
			require.True(t, stats.ArchiveRebuilt)
			assert.Equal(t, 1, stats.Deferred)
			assert.Contains(t, stats.Warnings,
				"resync: 1 session needs retry; why: codex:"+
					child+" ("+childPath+"): could not read the Codex session it was forked from ("+parent+")",
			)
			assert.Equal(t, []string{"replayed answer", "child answer"}, messageContents(t, database, "codex:"+child))
			usage, err := database.GetSessionUsage(t.Context(), "codex:"+child, true)
			require.NoError(t, err)
			require.NotNil(t, usage)
			assert.Zero(t, usage.TotalOutputTokens)
			assert.Less(t, database.GetSessionDataVersion(t.Context(), "codex:"+child), db.CurrentDataVersion())
			engine.Close()
			engine = NewEngine(t.Context(), database, cfg)
			t.Cleanup(engine.Close)
			assert.Equal(t, 1, engine.SyncAll(t.Context(), nil).Deferred)
			assert.Equal(t, []string{"replayed answer", "child answer"}, messageContents(t, database, "codex:"+child))
			appendFile(t, childPath, testjsonl.JoinJSONL(
				testjsonl.CodexTurnContextWithIDJSON("gpt-5", "later-turn", timestamp),
				testjsonl.CodexMsgJSON("assistant", "later answer", timestamp),
				testjsonl.CodexTokenCountJSON(timestamp, 20, 7, 0),
			))
			assert.Equal(t, 1, engine.SyncAll(t.Context(), nil).Deferred)
			// Available appends stay visible while the parent is unresolved.
			assert.Equal(t, []string{"replayed answer", "child answer", "later answer"}, messageContents(t, database, "codex:"+child))
			require.NoError(t, engine.TrySyncSessionWithSubagentsContext(t.Context(), "codex:"+child))
			assert.Equal(t, []string{"replayed answer", "child answer", "later answer"}, messageContents(t, database, "codex:"+child))
			usage, err = database.GetSessionUsage(t.Context(), "codex:"+child, true)
			require.NoError(t, err)
			require.NotNil(t, usage)
			assert.Zero(t, usage.TotalOutputTokens)
			engine.Close()
			engine = NewEngine(t.Context(), database, cfg)
			t.Cleanup(engine.Close)
			assert.Equal(t, 1, engine.SyncAll(t.Context(), nil).Deferred)
			assert.Equal(t, []string{"replayed answer", "child answer", "later answer"}, messageContents(t, database, "codex:"+child))
			require.NoError(t, os.WriteFile(parentPath, []byte(testjsonl.JoinJSONL(
				testjsonl.CodexSessionMetaJSON(parent, "/work/project", "user", timestamp),
				testjsonl.CodexTurnContextWithIDJSON("gpt-5", "parent-turn", timestamp),
				testjsonl.CodexMsgJSON("assistant", "replayed answer", timestamp),
			)), 0o600))
			recovered := engine.SyncAll(t.Context(), nil)
			assert.Zero(t, recovered.Deferred)
			assert.Equal(t, []string{"child answer", "later answer"}, messageContents(t, database, "codex:"+child))
			usage, err = database.GetSessionUsage(t.Context(), "codex:"+child, true)
			require.NoError(t, err)
			require.NotNil(t, usage)
			assert.Equal(t, 12, usage.TotalOutputTokens)
			assert.Equal(t, db.CurrentDataVersion(), database.GetSessionDataVersion(t.Context(), "codex:"+child))
		})
	}
}

func TestRebuildDeferredFilteredAndExcludedSources(t *testing.T) {
	const parent = "11111111-1111-4111-8111-111111111111"
	const child = "22222222-2222-4222-8222-222222222222"
	const filtered = "33333333-3333-4333-8333-333333333333"
	const timestamp = "2026-09-01T10:00:00Z"
	root := t.TempDir()
	claudeRoot := t.TempDir()
	parentPath := filepath.Join(root, "rollout-2026-09-01T10-00-00-"+parent+".jsonl")
	childPath := filepath.Join(root, "rollout-2026-09-01T11-00-00-"+child+".jsonl")
	filteredPath := filepath.Join(root, "rollout-2026-09-01T12-00-00-"+filtered+".jsonl")
	require.NoError(t, os.WriteFile(parentPath, []byte(testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(parent, "/work/project", "user", timestamp),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5", "parent-turn", timestamp),
		testjsonl.CodexMsgJSON("assistant", "parent history", timestamp),
	)), 0o600))
	require.NoError(t, os.WriteFile(childPath, []byte(testjsonl.JoinJSONL(
		testjsonl.CodexForkedSessionMetaJSON(child, parent, "/work/project", "user", timestamp),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", timestamp),
		testjsonl.CodexMsgJSON("assistant", "retained child", timestamp),
	)), 0o600))
	database := openTestDB(t)
	cfg := EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}, parser.AgentClaude: {claudeRoot}}, Machine: "local", IncludeCwdPrefixes: []string{"/work"}, DisableFilesystemProjectDiscovery: true}
	engine := NewEngine(t.Context(), database, cfg)
	t.Cleanup(engine.Close)
	require.Zero(t, engine.SyncAll(t.Context(), nil).Deferred)
	count, err := database.FileBackedSessionCount(t.Context())
	require.NoError(t, err)
	require.Positive(t, count)
	require.NoError(t, os.Remove(parentPath))
	require.NoError(t, os.WriteFile(filteredPath, []byte(testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(filtered, "/other/project", "user", timestamp),
		testjsonl.CodexMsgJSON("assistant", "filtered", timestamp),
	)), 0o600))
	usageDir := filepath.Join(claudeRoot, "project")
	require.NoError(t, os.MkdirAll(usageDir, 0o700))
	usagePath := filepath.Join(usageDir, "usage-probe.jsonl")
	usageCmd := "<command-name>/usage</command-name>\n<command-message>usage</command-message>\n<command-args></command-args>"
	require.NoError(t, os.WriteFile(usagePath, []byte(testjsonl.ClaudeUserJSON(usageCmd, timestamp, "/work/project")+"\n"), 0o600))
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{ID: "usage-probe", Agent: "claude", Machine: "local", Project: "project", Cwd: "/work/project", FilePath: &usagePath}))
	stats := engine.ResyncAll(t.Context(), nil)
	require.True(t, stats.ArchiveRebuilt, "zero-write rebuild should account for deferred, filtered and excluded files: %+v", stats)
	assert.False(t, stats.Aborted)
	assert.Equal(t, 1, stats.Deferred)
	assert.Equal(t, []string{"retained child"}, messageContents(t, database, "codex:"+child))
}

func writeOrphanCodexFork(t *testing.T, root, child, parent string) string {
	t.Helper()
	const timestamp = "2026-09-01T10:00:00Z"
	path := filepath.Join(root, "rollout-2026-09-01T11-00-00-"+child+".jsonl")
	require.NoError(t, os.WriteFile(path, []byte(testjsonl.JoinJSONL(
		testjsonl.CodexForkedSessionMetaJSON(child, parent, "/work/project", "user", timestamp),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", timestamp),
		testjsonl.CodexMsgJSON("assistant", "available child history", timestamp),
	)), 0o600))
	return path
}

// A row without messages has no history to protect. A fork with a missing
// parent must still import its fallback transcript over such a placeholder.
func TestMissingParentForkImportsOverPlaceholderRows(t *testing.T) {
	const parent = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	const child = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	const id = "codex:" + child
	for _, seed := range []string{"recall-import", "pending-content"} {
		for _, route := range []string{"sync", "rebuild", "single"} {
			t.Run(seed+"/"+route, func(t *testing.T) {
				root := t.TempDir()
				path := writeOrphanCodexFork(t, root, child, parent)
				database := openTestDB(t)
				if seed == "recall-import" {
					entry := fmt.Sprintf(`{"candidate_id":"c1","type":"fact","scope":"project","title":"Imported fact","body":"An imported memory.","project":"project","agent":"codex","session_id":%q,"label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":0,"ordinal_end":0}}`, id)
					imported, err := database.ImportAcceptedRecallEntriesJSONL(t.Context(), strings.NewReader(entry+"\n"))
					require.NoError(t, err)
					require.Equal(t, 1, imported.Imported)
				} else {
					_, err := database.UpsertSessionPendingContent(t.Context(), db.Session{
						ID: id, Agent: "codex", Machine: "local", Project: "project",
						FilePath: &path, Cwd: "/work/project", MessageCount: 1,
					})
					require.NoError(t, err)
				}
				placeholder, err := database.GetSession(t.Context(), id)
				require.NoError(t, err)
				require.NotNil(t, placeholder, "the seed must create a row without messages")
				require.Empty(t, messageContents(t, database, id))
				engine := NewEngine(t.Context(), database, EngineConfig{
					AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}},
					Machine:   "local", DisableFilesystemProjectDiscovery: true,
				})
				t.Cleanup(engine.Close)
				switch route {
				case "sync":
					stats := engine.SyncAll(t.Context(), nil)
					assert.Equal(t, 1, stats.Deferred)
				case "rebuild":
					stats := engine.ResyncAll(t.Context(), nil)
					require.True(t, stats.ArchiveRebuilt)
					assert.Equal(t, 1, stats.Deferred)
				case "single":
					require.NoError(t, engine.TrySyncSessionWithSubagentsContext(t.Context(), id))
				}
				assert.Equal(t, []string{"available child history"}, messageContents(t, database, id))
				assert.Less(t, database.GetSessionDataVersion(t.Context(), id), db.CurrentDataVersion(),
					"the fallback import must stay marked for retry")
				if seed == "recall-import" {
					entry, err := database.GetRecallEntry(t.Context(), "c1")
					require.NoError(t, err)
					require.NotNil(t, entry, "the imported Recall entry must survive the fallback import")
					assert.Equal(t, id, entry.SourceSessionID)
				}
			})
		}
	}
}

// A startup pass whose only unfinished work is a queued fork retry lets the
// daemon continue startup work such as draining attached archive sources.
func TestStartupWithMissingParentReportsRetryingCompletion(t *testing.T) {
	for _, queued := range []bool{true, false} {
		t.Run(fmt.Sprintf("retry queued=%v", queued), func(t *testing.T) {
			root := t.TempDir()
			path := writeOrphanCodexFork(t, root,
				"cccccccc-cccc-4ccc-8ccc-cccccccccccc", "dddddddd-dddd-4ddd-8ddd-dddddddddddd")
			var reconciled SyncStats
			var retry WatchBatch
			cfg := EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}},
				Machine:   "local", DisableFilesystemProjectDiscovery: true,
				OnStartupReconciled: func(stats SyncStats, err error) {
					if queued {
						assert.NoError(t, err)
					} else {
						assert.Error(t, err)
					}
					reconciled = stats
				},
			}
			if queued {
				cfg.OnRebuildDeferred = func(batch WatchBatch) { retry = batch }
			}
			engine := NewEngine(t.Context(), openTestDB(t), cfg)
			t.Cleanup(engine.Close)
			stats := engine.SyncAll(t.Context(), nil)
			require.Equal(t, 1, stats.Deferred)
			assert.False(t, reconciled.ProcessingComplete())
			assert.Equal(t, queued, reconciled.ProcessingCompleteOrRetrying())
			if queued {
				assert.Equal(t, []string{path}, retry.Paths)
			}
		})
	}
}

// A row without messages can still hold saved usage. A fork fallback must not
// replace that usage while its parent is missing, but available messages import.
func TestMissingParentForkKeepsMessagelessRowWithUsage(t *testing.T) {
	const parent = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	const child = "ffffffff-ffff-4fff-8fff-ffffffffffff"
	const id = "codex:" + child
	root := t.TempDir()
	path := writeOrphanCodexFork(t, root, child, parent)
	database := openTestDB(t)
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{
		ID: id, Agent: "codex", Machine: "local", Project: "project",
		FilePath: &path, Cwd: "/work/project",
	}))
	require.NoError(t, database.ReplaceSessionUsageEvents(t.Context(), id, []db.UsageEvent{
		{SessionID: id, Source: "saved", Model: "gpt-5", InputTokens: 70, OutputTokens: 30},
	}))
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}},
		Machine:   "local", DisableFilesystemProjectDiscovery: true,
	})
	t.Cleanup(engine.Close)

	stats := engine.SyncAll(t.Context(), nil)

	assert.Equal(t, 1, stats.Deferred)
	assert.Equal(t, []string{"available child history"}, messageContents(t, database, id))
	events, err := database.GetUsageEvents(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, "saved", events[0].Source)
	assert.Equal(t, 30, events[0].OutputTokens)
}
