package sync_test

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/parsertest/hermesfixture"
	"go.kenn.io/agentsview/internal/sync"
)

// Opt in remotely with AGENTSVIEW_HERMES_PERF=1. Uses synthetic sessions only.
// Run the same test on base and branch on one host. It exercises the actual
// engine's startup/full sync, heartbeat SyncPaths and selected-member writes.
func TestHermesWatcherPerformance(t *testing.T) {
	if os.Getenv("AGENTSVIEW_HERMES_PERF") != "1" {
		t.Skip("opt-in realistic synthetic workload")
	}
	const members, messages, bodyBytes = 19200, 60, 1536
	root := t.TempDir()
	conn, path := hermesFreshnessWriter(t, root)
	hermesfixture.CreateLedger(t, conn)
	_, err := conn.ExecContext(t.Context(), `CREATE INDEX idx_synthetic_ledger_session ON session_model_usage(session_id)`)
	require.NoError(t, err)
	tx, err := conn.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `DELETE FROM messages;DELETE FROM sessions`)
	require.NoError(t, err)
	session, err := tx.PrepareContext(t.Context(), `INSERT INTO sessions(id,source,started_at,title,message_count) VALUES (?,'cli',1778767200,'Synthetic session',?)`)
	require.NoError(t, err)
	message, err := tx.PrepareContext(t.Context(), `INSERT INTO messages(session_id,role,content,timestamp) VALUES (?,'user',?,?)`)
	require.NoError(t, err)
	ledger, err := tx.PrepareContext(t.Context(), `INSERT INTO session_model_usage(session_id,model,input_tokens) VALUES (?,'synthetic-model',123)`)
	require.NoError(t, err)
	body := strings.Repeat("synthetic conversation text with enough words for search. ", 30)[:bodyBytes]
	for i := range members {
		id := fmt.Sprintf("member-%05d", i)
		_, err = session.ExecContext(t.Context(), id, messages)
		require.NoError(t, err)
		_, err = ledger.ExecContext(t.Context(), id)
		require.NoError(t, err)
		for j := range messages {
			_, err = message.ExecContext(t.Context(), id, body, 1778767200+j)
			require.NoError(t, err)
		}
	}
	require.NoError(t, session.Close())
	require.NoError(t, message.Close())
	require.NoError(t, ledger.Close())
	require.NoError(t, tx.Commit())
	// A small transcript subset represents the native dual-source layout.
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sessions"), 0700))
	for i := range 100 {
		require.NoError(t, os.WriteFile(filepath.Join(root, "sessions", fmt.Sprintf("member-%05d.jsonl", i)), []byte("{\"role\":\"user\",\"content\":\"transcript fallback\",\"timestamp\":1778767200}\n"), 0600))
	}
	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentHermes: {root}}, Machine: "synthetic"})
	t.Cleanup(engine.Close)
	start := time.Now()
	stats := engine.SyncAll(t.Context(), nil)
	require.Equal(t, members, stats.Synced)
	require.Zero(t, stats.Failed)
	t.Logf("PERF fixture members=%d messages=%d body_bytes=%d transcript_files=100 startup_ms=%d", members, members*messages, bodyBytes, time.Since(start).Milliseconds())
	ticksRaw, err := exec.CommandContext(t.Context(), "getconf", "CLK_TCK").Output()
	require.NoError(t, err)
	ticks, err := strconv.ParseFloat(strings.TrimSpace(string(ticksRaw)), 64)
	require.NoError(t, err)
	cpu := func() float64 {
		data, err := os.ReadFile("/proc/self/stat")
		require.NoError(t, err)
		fields := strings.Fields(string(data)[strings.LastIndex(string(data), ")")+1:])
		user, err := strconv.ParseFloat(fields[11], 64)
		require.NoError(t, err)
		system, err := strconv.ParseFloat(fields[12], 64)
		require.NoError(t, err)
		return (user + system) / ticks
	}
	// Warm SQLite and the engine caches before measuring heartbeat work.
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))
	start = time.Now()
	cpuStart := cpu()
	for i := range 5 {
		tick := time.Now()
		_, err = conn.ExecContext(t.Context(), `INSERT OR REPLACE INTO gateway_heartbeats VALUES ('gateway',?)`, 1778768000+i)
		require.NoError(t, err)
		var scans atomic.Int64
		ctx := parser.WithSharedContainerScanObserver(t.Context(), func() { scans.Add(1) })
		pass := time.Now()
		selected, updated := hermesPassStats(t, func() error { return engine.SyncPathsContext(ctx, []string{path + "-wal"}) })
		t.Logf("PERF heartbeat sample=%d wall_ms=%.3f selected=%d synced=%d scans=%d", i, float64(time.Since(pass).Microseconds())/1000, selected, updated, scans.Load())
		// Same three-second writer cadence for both builds; slow base passes
		// naturally coalesce writes just as the watcher does.
		if remaining := 3*time.Second - time.Since(tick); remaining > 0 {
			time.Sleep(remaining)
		}
	}
	t.Logf("PERF steady_cpu_percent=%.3f heartbeat_wall_seconds=%.3f", 100*(cpu()-cpuStart)/time.Since(start).Seconds(), time.Since(start).Seconds())
	for i := range 5 {
		_, err = conn.ExecContext(t.Context(), `INSERT INTO messages(session_id,role,content,timestamp) VALUES ('member-00000','assistant','changed reply',?)`, 1778769000+i)
		require.NoError(t, err)
		var scans atomic.Int64
		ctx := parser.WithSharedContainerScanObserver(t.Context(), func() { scans.Add(1) })
		pass := time.Now()
		selected, updated := hermesPassStats(t, func() error { return engine.SyncPathsContext(ctx, []string{path + "-wal"}) })
		t.Logf("PERF append sample=%d wall_ms=%.3f selected=%d synced=%d scans=%d", i, float64(time.Since(pass).Microseconds())/1000, selected, updated, scans.Load())
		require.Equal(t, 1, engine.LastSyncStats().Synced)
	}
	rows, err := database.GetAllMessages(t.Context(), "hermes:member-00000")
	require.NoError(t, err)
	require.Len(t, rows, messages+5)
	for i := range 5 {
		pass := time.Now()
		stats = engine.SyncAll(t.Context(), nil)
		require.Zero(t, stats.Failed)
		t.Logf("PERF full sample=%d wall_ms=%.3f synced=%d", i, float64(time.Since(pass).Microseconds())/1000, stats.Synced)
	}
	if db.CurrentDataVersion() == 170 {
		require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(t.Context(), `UPDATE sessions SET data_version=169`)
			return err
		}))
		engine = sync.NewEngine(t.Context(), database, sync.EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentHermes: {root}}, Machine: "synthetic"})
		t.Cleanup(engine.Close)
		pass := time.Now()
		stats = engine.SyncAll(t.Context(), nil)
		require.Zero(t, stats.Failed)
		require.Equal(t, members, stats.Synced)
		t.Logf("PERF scoped_upgrade_ms=%.3f synced=%d", float64(time.Since(pass).Microseconds())/1000, stats.Synced)
	}

}
