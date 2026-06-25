package sync_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

func TestSyncAllSinceCortexHistoryUpdateTriggersResync(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	cortexDir := t.TempDir()
	testDB := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(testDB, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCortex: {cortexDir},
		},
		Machine: "local",
	})

	uuid := "11111111-2222-3333-4444-555555555555"
	metaPath := filepath.Join(cortexDir, uuid+".json")
	historyPath := filepath.Join(cortexDir, uuid+".history.jsonl")
	require.NoError(t, os.WriteFile(metaPath, []byte(cortexSyncMeta(uuid)), 0o644))
	require.NoError(t, os.WriteFile(historyPath, []byte(cortexSyncHistory("Before cutoff")), 0o644))

	baseTime := time.Unix(1_781_475_200, 0)
	require.NoError(t, os.Chtimes(metaPath, baseTime, baseTime))
	require.NoError(t, os.Chtimes(historyPath, baseTime, baseTime))

	engine.SyncPaths([]string{metaPath})
	assertMessageContent(t, testDB, "cortex:"+uuid, "Before cutoff", "ack")

	cutoff := baseTime.Add(500 * time.Millisecond)
	historyTime := baseTime.Add(time.Second)
	require.NoError(t, os.WriteFile(historyPath, []byte(cortexSyncHistory("After cutoff")), 0o644))
	require.NoError(t, os.Chtimes(historyPath, historyTime, historyTime))

	stats := engine.SyncAllSince(context.Background(), cutoff, nil)
	require.Equal(t, 1, stats.Synced, "synced = %d, want 1", stats.Synced)
	assertMessageContent(t, testDB, "cortex:"+uuid, "After cutoff", "ack")
}

func cortexSyncMeta(uuid string) string {
	return `{
	"session_id": "` + uuid + `",
	"title": "Cortex split history",
	"working_directory": "/home/user/cortex-project",
	"created_at": "2024-06-01T10:00:00Z",
	"last_updated": "2024-06-01T10:05:00Z"
}`
}

func cortexSyncHistory(prompt string) string {
	return strings.Join([]string{
		`{"role":"user","id":"m1","content":[{"type":"text","text":"` + prompt + `"}]}`,
		`{"role":"assistant","id":"m2","content":[{"type":"text","text":"ack"}]}`,
	}, "\n") + "\n"
}
