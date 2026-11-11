package sync

import (
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

func TestSessionPushSharedSourceImportsUnreservedMember(t *testing.T) {
	d := openTestDB(t)
	receipt := db.SessionPushReceipt{SessionID: "reserved", SourceOrigin: "source", PayloadHash: strings.Repeat("a", 64)}
	write := db.SessionBatchWrite{Session: db.Session{ID: "reserved", Agent: string(parser.AgentVibe), Machine: "source", MessageCount: 1, UserMessageCount: 1}, Messages: []db.Message{{SessionID: "reserved", Ordinal: 0, Role: "user", Content: "accepted snapshot"}}, ReplaceMessages: true}
	_, err := d.ApplySessionPush(t.Context(), receipt, write)
	require.NoError(t, err)
	engine := NewEngine(t.Context(), d, EngineConfig{Machine: "receiver"})
	t.Cleanup(engine.Close)
	jobs := make(chan syncJob, 1)
	jobs <- syncJob{agent: parser.AgentVibe, path: "fixture-source", excludedSessionIDs: []string{"reserved"}, results: []parser.ParseResult{{Session: parser.ParsedSession{ID: "canonical", Agent: parser.AgentVibe, Machine: "receiver"}, Messages: []parser.ParsedMessage{{Role: parser.RoleUser, Content: "canonical content"}}}}}
	close(jobs)
	stats := engine.collectAndBatch(t.Context(), jobs, 1, 1, nil, syncWriteDefault)
	assert.Zero(t, stats.Failed)
	reserved, err := d.GetAllMessages(t.Context(), "reserved")
	require.NoError(t, err)
	require.Len(t, reserved, 1)
	assert.Equal(t, "accepted snapshot", reserved[0].Content)
	member, err := d.GetAllMessages(t.Context(), "canonical")
	require.NoError(t, err)
	require.Len(t, member, 1)
	assert.Equal(t, "canonical content", member[0].Content)
}

func TestSessionPushContributorMergePreservesRebuildDecision(t *testing.T) {
	for name, merge := range map[string]func(*SyncStats, SyncStats){"contributors": mergeSyncStats, "reconciliation": mergeReconciliationSyncStats} {
		t.Run(name, func(t *testing.T) {
			var aggregate SyncStats
			merge(&aggregate, SyncStats{TotalSessions: 1, Failed: 1, filesOK: 1, filesDiscovered: 1, nonContainerDiscovered: 1, deferredPreservedIDs: []string{"deferred"}, deferredPreservedFiles: 1, preservedRetryFailures: 1})
			merge(&aggregate, SyncStats{TotalSessions: 2, Skipped: 2, filesOK: 1, filesDiscovered: 1, nonContainerDiscovered: 1, sessionPushReservedFiles: 1})
			assert.Empty(t, resyncSwapAbortReason(aggregate, 1, 0), "known reserved/deferred outcomes must survive aggregation")
			assert.Equal(t, 1, aggregate.sessionPushReservedFiles)
			assert.Equal(t, []string{"deferred"}, aggregate.deferredPreservedIDs)
		})
	}
}

func TestSessionPushMixedReservedAndDeferredSource(t *testing.T) {
	archive, replacement := openTestDB(t), openTestDB(t)
	_, err := archive.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{Session: db.Session{ID: "deferred", Agent: string(parser.AgentAntigravityCLI), Machine: "local", MessageCount: 1}, Messages: []db.Message{{SessionID: "deferred", Role: "user", Content: "saved history"}}, ReplaceMessages: true}})
	require.NoError(t, err)
	receipt := db.SessionPushReceipt{SessionID: "reserved", SourceOrigin: "source", PayloadHash: strings.Repeat("a", 64)}
	write := db.SessionBatchWrite{Session: db.Session{ID: "reserved", Agent: string(parser.AgentAntigravityCLI), Machine: "source", MessageCount: 1}, Messages: []db.Message{{SessionID: "reserved", Role: "user", Content: "snapshot"}}, ReplaceMessages: true, DataVersion: db.CurrentDataVersion()}
	_, err = replacement.ApplySessionPush(t.Context(), receipt, write)
	require.NoError(t, err)
	engine := NewEngine(t.Context(), replacement, EngineConfig{Machine: "local"})
	engine.archiveStore = archive
	t.Cleanup(engine.Close)
	jobs := make(chan syncJob, 1)
	jobs <- syncJob{agent: parser.AgentAntigravityCLI, path: "fixture-source", retrySessionIDs: map[string]bool{"deferred": true}, results: []parser.ParseResult{{Session: parser.ParsedSession{ID: "reserved", Agent: parser.AgentAntigravityCLI}}, {Session: parser.ParsedSession{ID: "deferred", Agent: parser.AgentAntigravityCLI}}}}
	close(jobs)
	stats := engine.collectAndBatch(t.Context(), jobs, 1, 1, nil, syncWriteDefault)
	assert.Equal(t, 1, stats.deferredPreservedFiles)
	assert.Equal(t, []string{"deferred"}, stats.deferredPreservedIDs)
	assert.Empty(t, resyncSwapAbortReason(stats, 0, 0))
	assert.Equal(t, db.CurrentDataVersion(), replacement.GetSessionDataVersion(t.Context(), "reserved"))
}

func TestSessionPushRebuildWithoutSources(t *testing.T) {
	d := openTestDB(t)
	receipt := db.SessionPushReceipt{SessionID: "pushed", SourceOrigin: "source", PayloadHash: strings.Repeat("a", 64)}
	write := db.SessionBatchWrite{Session: db.Session{ID: "pushed", Agent: "claude", Machine: "source", MessageCount: 1, UserMessageCount: 1}, Messages: []db.Message{{SessionID: "pushed", Ordinal: 0, Role: "user", Content: "accepted snapshot"}}, ReplaceMessages: true, DataVersion: db.CurrentDataVersion()}
	_, err := d.ApplySessionPush(t.Context(), receipt, write)
	require.NoError(t, err)
	engine := NewEngine(t.Context(), d, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {t.TempDir()}}, Machine: "receiver"})
	t.Cleanup(engine.Close)
	stats := engine.ResyncAll(t.Context(), nil)
	require.False(t, stats.Aborted, "rebuild aborted: %v", stats.Warnings)
	require.True(t, stats.ArchiveRebuilt)
	messages, err := d.GetAllMessages(t.Context(), "pushed")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "accepted snapshot", messages[0].Content)
}

func TestSessionPushRebuildMixedReservedAndFilteredFiles(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	require.NoError(t, os.MkdirAll(project, 0o700))
	d := openTestDB(t)
	for _, item := range []struct{ id, cwd string }{{"allowed", "/allowed/project"}, {"filtered", "/outside/project"}} {
		content := testjsonl.NewSessionBuilder().AddClaudeUser("2026-01-01T00:00:00Z", "native", item.cwd).String()
		require.NoError(t, os.WriteFile(filepath.Join(project, item.id+".jsonl"), []byte(content), 0o600))
		receipt := db.SessionPushReceipt{SessionID: item.id, SourceOrigin: "source", PayloadHash: strings.Repeat("a", 64)}
		write := db.SessionBatchWrite{Session: db.Session{ID: item.id, Agent: "claude", Machine: "source", MessageCount: 1, UserMessageCount: 1}, Messages: []db.Message{{SessionID: item.id, Role: "user", Content: "snapshot"}}, ReplaceMessages: true, DataVersion: db.CurrentDataVersion()}
		_, err := d.ApplySessionPush(t.Context(), receipt, write)
		require.NoError(t, err)
	}
	path := filepath.Join(project, "native.jsonl")
	content := testjsonl.NewSessionBuilder().AddClaudeUser("2026-01-01T00:00:00Z", "native", "/outside/project").String()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	_, err := d.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{Session: db.Session{ID: "native", Agent: "claude", Machine: "local", Cwd: "/outside/project", FilePath: &path, MessageCount: 1, UserMessageCount: 1}, Messages: []db.Message{{SessionID: "native", Role: "user", Content: "archived"}}, ReplaceMessages: true, DataVersion: db.CurrentDataVersion()}})
	require.NoError(t, err)
	engine := NewEngine(t.Context(), d, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}, Machine: "local", IncludeCwdPrefixes: []string{"/allowed"}})
	t.Cleanup(engine.Close)
	stats := engine.ResyncAll(t.Context(), nil)
	require.False(t, stats.Aborted, "rebuild aborted: %v", stats.Warnings)
	require.True(t, stats.ArchiveRebuilt)
	for _, id := range []string{"allowed", "filtered", "native"} {
		messages, err := d.GetAllMessages(t.Context(), id)
		require.NoError(t, err)
		require.Len(t, messages, 1)
		if id == "native" {
			assert.Equal(t, "archived", messages[0].Content)
		} else {
			assert.Equal(t, "snapshot", messages[0].Content)
			session, err := d.GetSessionFull(t.Context(), id)
			require.NoError(t, err)
			require.NotNil(t, session)
			assert.Nil(t, session.FilePath)
			receipt := db.SessionPushReceipt{SessionID: id, SourceOrigin: "source", PayloadHash: strings.Repeat("a", 64)}
			write := db.SessionBatchWrite{Session: db.Session{ID: id, Agent: "claude", Machine: "source", MessageCount: 1, UserMessageCount: 1}, Messages: []db.Message{{SessionID: id, Role: "user", Content: "snapshot"}}, ReplaceMessages: true, DataVersion: db.CurrentDataVersion()}
			created, err := d.ApplySessionPush(t.Context(), receipt, write)
			require.NoError(t, err)
			assert.False(t, created)
		}
	}
}

func TestSessionPushRebuildClaudeDAGPreservesSnapshot(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	project := filepath.Join(root, "project-a")
	require.NoError(t, os.MkdirAll(project, 0o755))
	path := filepath.Join(project, "forked.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(newClaudeDAGBuilder(true).String()), 0o644))
	receipt := db.SessionPushReceipt{SessionID: "forked-i", SourceOrigin: "source", PayloadHash: strings.Repeat("a", 64)}
	write := db.SessionBatchWrite{Session: db.Session{ID: "forked-i", Agent: "claude", Machine: "source", MessageCount: 1}, Messages: []db.Message{{SessionID: "forked-i", Ordinal: 0, Role: "assistant", Content: "accepted snapshot"}}, ReplaceMessages: true, DataVersion: db.CurrentDataVersion()}
	_, err := database.ApplySessionPush(t.Context(), receipt, write)
	require.NoError(t, err)
	engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}, Machine: "local"})
	t.Cleanup(engine.Close)
	rebuilt := engine.ResyncAll(t.Context(), nil)
	require.True(t, rebuilt.ArchiveRebuilt)
	require.Zero(t, rebuilt.Failed)
	main, err := database.GetSessionFull(t.Context(), "forked")
	require.NoError(t, err)
	require.NotNil(t, main)
	messages, err := database.GetAllMessages(t.Context(), "forked-i")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "accepted snapshot", messages[0].Content)
}
