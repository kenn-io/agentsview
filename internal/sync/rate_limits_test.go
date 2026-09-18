package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestCodexRateLimitsSyncLifecycle(t *testing.T) {
	for _, staged := range []bool{false, true} {
		t.Run(map[bool]string{false: "collecting", true: "staged"}[staged], func(t *testing.T) {
			const id = "019eb791-cf7d-75c1-8439-9ed74c122e03"
			const first = `{"timestamp":"2024-01-01T10:00:01Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":"codex","primary":{"used_percent":25,"window_minutes":300,"resets_at":1704110400},"credits":{"balance":"12.50"}}}}`
			const next = `{"timestamp":"2024-01-01T10:00:01Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":"codex","secondary":{"used_percent":45}}}}`
			fixture := testjsonl.NewSessionBuilder().AddCodexMeta("2024-01-01T10:00:00Z", id, "/workspace/project-a", "codex_cli_rs").AddCodexMessage("2024-01-01T10:00:00Z", "user", "hello").AddRaw(first)
			root := writeCodexTranscriptRoot(t, id, fixture.String())
			paths, err := filepath.Glob(filepath.Join(root, "2024", "01", "01", "*.jsonl"))
			require.NoError(t, err)
			require.Len(t, paths, 1)
			other := testjsonl.NewSessionBuilder().AddCodexMeta("2024-01-01T10:00:00Z", "019eb791-cf7d-75c1-8439-9ed74c122e04", "/workspace/project-b", "codex_cli_rs").AddCodexMessage("2024-01-01T10:00:00Z", "user", "hello")
			require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(paths[0]), "rollout-other.jsonl"), []byte(other.String()), 0o600))
			database := openTestDB(t)
			config := EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Machine: "local", Ephemeral: true, DisableFilesystemProjectDiscovery: true}
			if staged {
				config.StagedCodexParseMinBytes = 1
			}
			engine := NewEngine(database, config)
			t.Cleanup(engine.Close)
			stats := engine.SyncAll(t.Context(), nil)
			require.Zero(t, stats.Failed)
			series, err := database.RateLimits(t.Context(), "local", 0, 0)
			require.NoError(t, err)
			require.Len(t, series, 1)
			require.Len(t, series[0].Points, 1)
			point := series[0].Points[0]
			require.NotNil(t, point.Primary)
			assert.Equal(t, new(25.0), point.Primary.UsedPercent)
			assert.Equal(t, new(int64(300)), point.Primary.WindowMinutes)
			assert.Equal(t, new(int64(1704110400)), point.Primary.ResetsAt)
			require.NotNil(t, point.Credits)
			assert.Equal(t, new("12.50"), point.Credits.Balance)
			file, err := os.OpenFile(paths[0], os.O_WRONLY|os.O_APPEND, 0o600)
			require.NoError(t, err)
			_, err = file.WriteString(next + "\n")
			require.NoError(t, err)
			require.NoError(t, file.Close())
			result := engine.processFile(t.Context(), parser.DiscoveredFile{Agent: parser.AgentCodex, Path: paths[0]})
			defer result.releaseStaged()
			defer result.retentionLease.Release()
			require.NoError(t, result.err)
			require.NotNil(t, result.incremental)
			require.NoError(t, engine.writeIncremental(result.incremental))
			require.NoError(t, engine.writeIncremental(result.incremental))
			series, err = database.RateLimits(t.Context(), "local", 0, 0)
			require.NoError(t, err)
			require.Len(t, series[0].Points, 1)
			assert.Nil(t, series[0].Current.Primary)
			assert.Equal(t, new(45.0), series[0].Current.Secondary.UsedPercent)
			var count int
			require.NoError(t, database.Reader().QueryRow("SELECT count(*) FROM rate_limit_snapshots").Scan(&count))
			assert.Equal(t, 2, count)
			require.NoError(t, os.Remove(paths[0]))
			stats = engine.ResyncAll(t.Context(), nil)
			require.False(t, stats.Aborted)
			series, err = database.RateLimits(t.Context(), "local", 0, 0)
			require.NoError(t, err)
			require.Len(t, series, 1)
			assert.Len(t, series[0].Points, 1)
			require.NoError(t, database.Reader().QueryRow("SELECT count(*) FROM rate_limit_snapshots").Scan(&count))
			assert.Equal(t, 2, count)
		})
	}
}
