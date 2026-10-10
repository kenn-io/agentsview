package sessionwatch

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

type polledVersionStore struct {
	db.Store
	reads  atomic.Int32
	polled chan struct{}
}

type messageEmissionCounter struct{ count atomic.Int32 }

func (e *messageEmissionCounter) Emit(scope string) {
	if scope == "messages" {
		e.count.Add(1)
	}
}

func (s *polledVersionStore) GetSessionVersion(ctx context.Context, id string) (int, int64, bool) {
	count, version, ok := s.Store.GetSessionVersion(ctx, id)
	if s.reads.Add(1) == 2 {
		close(s.polled)
	}
	return count, version, ok
}

func TestReconnectPreservesUnchangedCodexTranscript(t *testing.T) {
	t.Cleanup(SetTimingsForTest(25*time.Millisecond, 50*time.Millisecond))
	for _, unrelatedCount := range []int{0, 100} {
		t.Run(fmt.Sprintf("unrelated=%d", unrelatedCount), func(t *testing.T) {
			const uuid = "11111111-1111-4111-8111-111111111111"
			const sessionID = "codex:" + uuid
			root := t.TempDir()
			path := filepath.Join(root, "2026", "01", "01", "rollout-2026-01-01T00-00-00-"+uuid+".jsonl")
			initial := testjsonl.JoinJSONL(
				testjsonl.CodexSessionMetaJSON(uuid, "/tmp/project", "codex_cli_rs", "2026-01-01T00:00:00Z"),
				testjsonl.CodexMsgJSON("user", "First message", "2026-01-01T00:00:01Z"),
			)
			dbtest.WriteTestFile(t, path, []byte(initial))
			database := dbtest.OpenTestDB(t)
			emitter := &messageEmissionCounter{}
			engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}},
				Machine:   "test",
				Emitter:   emitter,
			})
			t.Cleanup(engine.Close)
			require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
			appendLine := testjsonl.JoinJSONL(testjsonl.CodexMsgJSON("assistant", "Reply", "2026-01-01T00:00:02Z"))
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
			require.NoError(t, err)
			_, err = f.WriteString(appendLine)
			require.NoError(t, err)
			require.NoError(t, f.Close())
			require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
			for i := range unrelatedCount {
				dbtest.SeedSession(t, database, fmt.Sprintf("unrelated-%d", i), "other")
			}
			before, err := database.GetSessionFull(t.Context(), sessionID)
			require.NoError(t, err)
			require.True(t, before.LastWriteIncremental)
			probe, err := sql.Open("sqlite3", database.Path())
			require.NoError(t, err)
			defer probe.Close()
			_, err = probe.Exec(`CREATE TABLE watch_writes (kind TEXT);
				CREATE TRIGGER watch_message_insert AFTER INSERT ON messages BEGIN INSERT INTO watch_writes VALUES ('insert'); END;
				CREATE TRIGGER watch_message_delete AFTER DELETE ON messages BEGIN INSERT INTO watch_writes VALUES ('delete'); END;`)
			require.NoError(t, err)
			watchOnce := func() {
				t.Helper()
				observed := &polledVersionStore{Store: database, polled: make(chan struct{})}
				ctx, cancel := context.WithCancel(t.Context())
				updates := New(observed, engine).Events(ctx, sessionID)
				select {
				case <-observed.polled:
				case <-time.After(5 * time.Second):
					cancel()
					t.Fatal("watch did not finish its initial reconciliation")
				}
				cancel()
				for range updates {
				}
			}
			emittedBefore := emitter.count.Load()
			for range 2 {
				watchOnce()
			}
			var writes int
			require.NoError(t, probe.QueryRow("SELECT count(*) FROM watch_writes").Scan(&writes))
			assert.Zero(t, writes, "unchanged reconnects must not replace stored messages")
			after, err := database.GetSessionFull(t.Context(), sessionID)
			require.NoError(t, err)
			assert.True(t, after.LastWriteIncremental, "reconnect must preserve the last incremental write")
			assert.Equal(t, before.TranscriptRevision, after.TranscriptRevision)
			assert.Equal(t, emittedBefore, emitter.count.Load(), "unchanged reconnects must not announce new messages")

			// A later connection must still import an append made while offline,
			// preserving the incremental checkpoint instead of replacing history.
			f, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
			require.NoError(t, err)
			_, err = f.WriteString(testjsonl.JoinJSONL(testjsonl.CodexMsgJSON("user", "Offline update", "2026-01-01T00:00:03Z")))
			require.NoError(t, err)
			require.NoError(t, f.Close())
			watchOnce()
			after, err = database.GetSessionFull(t.Context(), sessionID)
			require.NoError(t, err)
			assert.True(t, after.LastWriteIncremental)
			assert.Equal(t, before.MessageCount+1, after.MessageCount)
			assert.Equal(t, emittedBefore+1, emitter.count.Load())
			require.NoError(t, probe.QueryRow("SELECT count(*) FROM watch_writes WHERE kind = 'insert'").Scan(&writes))
			assert.Equal(t, 1, writes)
			require.NoError(t, probe.QueryRow("SELECT count(*) FROM watch_writes WHERE kind = 'delete'").Scan(&writes))
			assert.Zero(t, writes)
		})
	}
}
