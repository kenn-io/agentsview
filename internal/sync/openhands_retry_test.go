package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/wesm/agentsview/internal/dbtest"
	"github.com/wesm/agentsview/internal/parser"
)

func TestProcessFileOpenHandsUsesSnapshotMtimeForRetryCache(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(
		root, "086c7ecf6cb746b69fbcb900358d1247",
	)
	eventsDir := filepath.Join(sessionDir, "events")
	require.NoError(t, os.MkdirAll(eventsDir, 0o755))

	baseStatePath := filepath.Join(sessionDir, "base_state.json")
	eventPath := filepath.Join(eventsDir, "event-00000-user.json")
	dbtest.WriteTestFile(t, baseStatePath, []byte(`{
		"id":"086c7ecf-6cb7-46b6-9fbc-b900358d1247",
		"agent":{"llm":{"model":"openhands-test-model"}}
	}`))
	dbtest.WriteTestFile(t, eventPath, []byte(`{
		"id":"e0",
		"timestamp":"2026-04-02T15:25:40.706887",
		"source":"user",
		"llm_message":{"role":"user","content":[{"type":"text","text":"First version"}]},
		"kind":"MessageEvent"
	}`))

	dirInfo, err := os.Stat(sessionDir)
	require.NoError(t, err)
	oldDirMtime := dirInfo.ModTime()

	engine := &Engine{
		db:        dbtest.OpenTestDB(t),
		machine:   "local",
		skipCache: map[string]int64{sessionDir: oldDirMtime.UnixNano()},
	}

	time.Sleep(10 * time.Millisecond)
	dbtest.WriteTestFile(t, eventPath, []byte(`{
		"id":"e0",
		"timestamp":"2026-04-02T15:25:41.706887",
		"source":"user",
		"llm_message":{"role":"user","content":[{"type":"text","text":"Updated version"}]},
		"kind":"MessageEvent"
	}`))
	require.NoError(t, os.Chtimes(sessionDir, oldDirMtime, oldDirMtime))

	snapshot, err := parser.OpenHandsSnapshot(sessionDir)
	require.NoError(t, err)
	require.NotEqual(t, oldDirMtime.UnixNano(), snapshot.Mtime)

	res := engine.processFile(parser.DiscoveredFile{
		Path:  sessionDir,
		Agent: parser.AgentOpenHands,
	})
	require.False(t, res.skip)
	require.NoError(t, res.err)
	require.Len(t, res.results, 1)
	require.Equal(t, snapshot.Mtime, res.mtime)
}
