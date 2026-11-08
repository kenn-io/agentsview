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

func TestSessionPushRealRebuildPreservesSnapshotAgainstNativeCollision(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "example")
	require.NoError(t, os.MkdirAll(project, 0o700))
	path := filepath.Join(project, "reserved.jsonl")
	content := testjsonl.NewSessionBuilder().AddClaudeUser("2026-01-01T00:00:00Z", "different native snapshot").String()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	d := openTestDB(t)
	receipt := db.SessionPushReceipt{SessionID: "reserved", SourceOrigin: "source-archive", PayloadHash: strings.Repeat("a", 64)}
	write := db.SessionBatchWrite{Session: db.Session{ID: "reserved", Agent: "claude", Machine: "source-archive", Project: "example", MessageCount: 1, UserMessageCount: 1}, Messages: []db.Message{{SessionID: "reserved", Ordinal: 0, Role: "user", Content: "accepted snapshot"}}, ReplaceMessages: true, DataVersion: db.CurrentDataVersion()}
	_, err := d.ApplySessionPush(t.Context(), receipt, write)
	require.NoError(t, err)
	engine := NewEngine(t.Context(), d, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}, Machine: "receiver"})
	t.Cleanup(engine.Close)
	stats := engine.ResyncAll(t.Context(), nil)
	require.False(t, stats.Aborted, "rebuild aborted: %v", stats.Warnings)
	messages, err := d.GetAllMessages(t.Context(), "reserved")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "accepted snapshot", messages[0].Content)
	session, err := d.GetSessionFull(t.Context(), "reserved")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Nil(t, session.FilePath)
	created, err := d.ApplySessionPush(t.Context(), receipt, write)
	require.NoError(t, err)
	assert.False(t, created)
}

func TestSessionPushRebuildSkipClassificationPreservesFailureGuard(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stats SyncStats
		abort bool
	}{
		{"reserved", SyncStats{TotalSessions: 1, Skipped: 1, filesOK: 1, nonContainerDiscovered: 1, sessionPushReservedFiles: 1}, false},
		{"real failure", SyncStats{TotalSessions: 2, Skipped: 1, Failed: 1, filesOK: 1, nonContainerDiscovered: 2, sessionPushReservedFiles: 1}, true},
		{"unexplained empty", SyncStats{TotalSessions: 2, Skipped: 2, filesOK: 2, nonContainerDiscovered: 2, sessionPushReservedFiles: 1}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason := resyncSwapAbortReason(tc.stats, 1, 0)
			if tc.abort {
				assert.NotEmpty(t, reason)
			} else {
				assert.Empty(t, reason)
			}
		})
	}
}

func TestSessionPushSharedSourceImportsUnreservedMember(t *testing.T) {
	d := openTestDB(t)
	receipt := db.SessionPushReceipt{SessionID: "reserved", SourceOrigin: "source", PayloadHash: strings.Repeat("a", 64)}
	write := db.SessionBatchWrite{Session: db.Session{ID: "reserved", Agent: "cursor", Machine: "source", MessageCount: 1, UserMessageCount: 1}, Messages: []db.Message{{SessionID: "reserved", Ordinal: 0, Role: "user", Content: "accepted snapshot"}}, ReplaceMessages: true}
	_, err := d.ApplySessionPush(t.Context(), receipt, write)
	require.NoError(t, err)
	engine := NewEngine(t.Context(), d, EngineConfig{Machine: "receiver"})
	t.Cleanup(engine.Close)
	jobs := make(chan syncJob, 1)
	job := syncJob{agent: parser.AgentCursor, path: "fixture-source"}
	job.results = []parser.ParseResult{
		{Session: parser.ParsedSession{ID: "reserved", Agent: parser.AgentCursor, Machine: "receiver"}, Messages: []parser.ParsedMessage{{Role: parser.RoleUser, Content: "replacement"}}},
		{Session: parser.ParsedSession{ID: "independent", Agent: parser.AgentCursor, Machine: "receiver"}, Messages: []parser.ParsedMessage{{Role: parser.RoleUser, Content: "independent content"}}},
	}
	jobs <- job
	close(jobs)
	stats := engine.collectAndBatch(t.Context(), jobs, 1, 1, nil, syncWriteDefault)
	assert.Zero(t, stats.Failed)
	reserved, err := d.GetAllMessages(t.Context(), "reserved")
	require.NoError(t, err)
	require.Len(t, reserved, 1)
	assert.Equal(t, "accepted snapshot", reserved[0].Content)
	independent, err := d.GetAllMessages(t.Context(), "independent")
	require.NoError(t, err)
	require.Len(t, independent, 1)
	assert.Equal(t, "independent content", independent[0].Content)
}

func TestSessionPushReservedExclusionDoesNotBlockSourceMember(t *testing.T) {
	d := openTestDB(t)
	receipt := db.SessionPushReceipt{SessionID: "reserved", SourceOrigin: "source", PayloadHash: strings.Repeat("a", 64)}
	write := db.SessionBatchWrite{Session: db.Session{ID: "reserved", Agent: "vibe", Machine: "source", MessageCount: 1, UserMessageCount: 1}, Messages: []db.Message{{SessionID: "reserved", Ordinal: 0, Role: "user", Content: "accepted snapshot"}}, ReplaceMessages: true}
	_, err := d.ApplySessionPush(t.Context(), receipt, write)
	require.NoError(t, err)
	engine := NewEngine(t.Context(), d, EngineConfig{Machine: "receiver"})
	t.Cleanup(engine.Close)
	jobs := make(chan syncJob, 1)
	job := syncJob{agent: parser.AgentVibe, path: "fixture-source"}
	job.excludedSessionIDs = []string{"reserved"}
	job.results = []parser.ParseResult{{Session: parser.ParsedSession{ID: "canonical", Agent: parser.AgentVibe, Machine: "receiver"}, Messages: []parser.ParsedMessage{{Role: parser.RoleUser, Content: "canonical content"}}}}
	jobs <- job
	close(jobs)
	stats := engine.collectAndBatch(t.Context(), jobs, 1, 1, nil, syncWriteDefault)
	assert.Zero(t, stats.Failed)
	reserved, err := d.GetAllMessages(t.Context(), "reserved")
	require.NoError(t, err)
	require.Len(t, reserved, 1)
	assert.Equal(t, "accepted snapshot", reserved[0].Content)
	canonical, err := d.GetAllMessages(t.Context(), "canonical")
	require.NoError(t, err)
	require.Len(t, canonical, 1)
	assert.Equal(t, "canonical content", canonical[0].Content)
}
