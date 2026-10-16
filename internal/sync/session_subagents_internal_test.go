package sync

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestTrySessionRefreshRefusesContentionAtEachStage(t *testing.T) {
	for _, stage := range []string{"parent", "local child", "s3 child"} {
		t.Run(stage, func(t *testing.T) {
			database := openTestDB(t)
			root := t.TempDir()
			parentPath := filepath.Join(root, "project", "parent.jsonl")
			require.NoError(t, os.MkdirAll(filepath.Dir(parentPath), 0o755))
			require.NoError(t, os.WriteFile(parentPath, []byte(testjsonl.NewSessionBuilder().AddClaudeUser("2026-01-01T00:00:00Z", "parent transcript").String()), 0o600))
			if stage == "s3 child" {
				parentPath = "s3://bucket/host-a/raw/claude/project/parent.jsonl"
			}
			require.NoError(t, database.UpsertSession(t.Context(), db.Session{ID: "parent", Agent: "claude", Project: "project", FilePath: new(parentPath)}))
			engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}, Ephemeral: true})
			t.Cleanup(engine.Close)
			original := subagentTranscriptPaths
			t.Cleanup(func() { subagentTranscriptPaths = original })
			var locked atomic.Bool
			t.Cleanup(func() {
				if locked.Swap(false) {
					engine.syncMu.Unlock()
				}
			})
			subagentTranscriptPaths = func(string) []string {
				assert.NotEqual(t, "parent", stage, "busy parent must stop before child discovery")
				engine.syncMu.Lock()
				locked.Store(true)
				return []string{parentPath + "/subagents/agent-child.jsonl"}
			}
			if stage == "parent" {
				engine.syncMu.Lock()
				locked.Store(true)
			}
			// Avoid S3 transport: the archived parent is unavailable, while child
			// discovery still reaches the lock that protects child ingestion.
			if stage == "s3 child" {
				oldStat := statClaudeS3Session
				statClaudeS3Session = func(string) (parser.S3Object, error) { return parser.S3Object{}, missingS3ObjectError() }
				t.Cleanup(func() { statClaudeS3Session = oldStat })
			}
			done := make(chan error, 1)
			go func() {
				done <- engine.TrySyncSessionWithSubagentsContext(t.Context(), "parent")
			}()
			var err error
			select {
			case err = <-done:
			case <-time.After(time.Second):
				t.Error("try refresh waited for the sync lock")
				if locked.Swap(false) {
					engine.syncMu.Unlock()
				}
				select {
				case err = <-done:
				case <-time.After(time.Second):
					t.Error("try refresh stayed blocked after releasing the test lock")
					return
				}
			}
			assert.ErrorIs(t, err, ErrSyncInProgress)
		})
	}
}

func TestUnmarkedSessionRefreshWaitsForSyncLock(t *testing.T) {
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{Ephemeral: true})
	t.Cleanup(engine.Close)
	engine.syncMu.Lock()
	started, done := make(chan struct{}), make(chan error, 1)
	received := false
	defer func() {
		engine.syncMu.Unlock()
		if !received {
			assert.NotErrorIs(t, <-done, ErrSyncInProgress)
		}
	}()
	go func() {
		close(started)
		done <- engine.SyncSessionWithSubagentsContext(context.Background(), "unknown")
	}()
	<-started
	// Mutex acquisition does not durably block under testing/synctest.
	select {
	case err := <-done:
		received = true
		require.FailNowf(t, "manual refresh returned before the lock was released", "%v", err)
	case <-time.After(100 * time.Millisecond):
	}
}
