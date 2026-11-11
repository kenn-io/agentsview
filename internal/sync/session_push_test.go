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

func TestSessionPushSharedSourceImportsUnreservedMember(t *testing.T) {
	for _, tc := range []struct {
		name            string
		agent           parser.AgentType
		excluded        bool
		member, content string
	}{
		{"reserved member", parser.AgentCursor, false, "independent", "independent content"},
		{"excluded member", parser.AgentVibe, true, "canonical", "canonical content"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := openTestDB(t)
			receipt := db.SessionPushReceipt{SessionID: "reserved", SourceOrigin: "source", PayloadHash: strings.Repeat("a", 64)}
			write := db.SessionBatchWrite{Session: db.Session{ID: "reserved", Agent: string(tc.agent), Machine: "source", MessageCount: 1, UserMessageCount: 1}, Messages: []db.Message{{SessionID: "reserved", Ordinal: 0, Role: "user", Content: "accepted snapshot"}}, ReplaceMessages: true}
			_, err := d.ApplySessionPush(t.Context(), receipt, write)
			require.NoError(t, err)
			engine := NewEngine(t.Context(), d, EngineConfig{Machine: "receiver"})
			t.Cleanup(engine.Close)
			jobs := make(chan syncJob, 1)
			job := syncJob{agent: tc.agent, path: "fixture-source"}
			if tc.excluded {
				job.excludedSessionIDs = []string{"reserved"}
			} else {
				job.results = append(job.results, parser.ParseResult{Session: parser.ParsedSession{ID: "reserved", Agent: tc.agent, Machine: "receiver"}, Messages: []parser.ParsedMessage{{Role: parser.RoleUser, Content: "replacement"}}})
			}
			job.results = append(job.results, parser.ParseResult{Session: parser.ParsedSession{ID: tc.member, Agent: tc.agent, Machine: "receiver"}, Messages: []parser.ParsedMessage{{Role: parser.RoleUser, Content: tc.content}}})
			jobs <- job
			close(jobs)
			stats := engine.collectAndBatch(t.Context(), jobs, 1, 1, nil, syncWriteDefault)
			assert.Zero(t, stats.Failed)
			reserved, err := d.GetAllMessages(t.Context(), "reserved")
			require.NoError(t, err)
			require.Len(t, reserved, 1)
			assert.Equal(t, "accepted snapshot", reserved[0].Content)
			member, err := d.GetAllMessages(t.Context(), tc.member)
			require.NoError(t, err)
			require.Len(t, member, 1)
			assert.Equal(t, tc.content, member[0].Content)
		})
	}
}

func TestSessionPushContributorMergePreservesRebuildDecision(t *testing.T) {
	for name, merge := range map[string]func(*SyncStats, SyncStats){"contributors": mergeSyncStats, "reconciliation": mergeReconciliationSyncStats} {
		t.Run(name, func(t *testing.T) {
			for _, mixed := range []bool{false, true} {
				var aggregate SyncStats
				if mixed {
					aggregate = SyncStats{TotalSessions: 1, Skipped: 1, filesOK: 1, nonContainerDiscovered: 1, deferredPreservedIDs: []string{"deferred"}}
				}
				merge(&aggregate, SyncStats{TotalSessions: 1, Skipped: 1, filesOK: 1, nonContainerDiscovered: 1, sessionPushReservedFiles: 1})
				assert.Empty(t, resyncSwapAbortReason(aggregate, 1, 0), "known reserved/deferred outcomes must survive aggregation")
				failed := aggregate
				failed.Failed++
				assert.NotEmpty(t, resyncSwapAbortReason(failed, 1, 0), "real failures still abort")
				unexplained := aggregate
				unexplained.TotalSessions++
				unexplained.filesOK++
				unexplained.Skipped++
				assert.NotEmpty(t, resyncSwapAbortReason(unexplained, 1, 0), "unexplained empty outcomes still abort")
			}
		})
	}
}
