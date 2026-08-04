package sync

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// TestRooCodeFreshBeforeFingerprintUsesCompositeStat pins the
// stat-only pre-fingerprint skip for RooCode. Without it, every sync
// cycle content-hashes both session files for every task, scaling
// polling work with transcript size and archive cardinality instead
// of the changed batch.
func TestRooCodeFreshBeforeFingerprintUsesCompositeStat(t *testing.T) {
	taskDir := t.TempDir()
	historyPath := filepath.Join(taskDir, "history_item.json")
	messagesPath := filepath.Join(taskDir, "ui_messages.json")
	require.NoError(t, os.WriteFile(historyPath,
		[]byte(`{"id":"task-1","ts":1,"task":"t"}`), 0o644))
	require.NoError(t, os.WriteFile(messagesPath, []byte(`[]`), 0o644))

	historyInfo, err := os.Stat(historyPath)
	require.NoError(t, err)
	messagesInfo, err := os.Stat(messagesPath)
	require.NoError(t, err)
	compositeSize := historyInfo.Size() + messagesInfo.Size()
	compositeMtime := historyInfo.ModTime().UnixNano()
	if m := messagesInfo.ModTime().UnixNano(); m > compositeMtime {
		compositeMtime = m
	}

	database := openTestDB(t)
	sess := db.Session{
		ID:        "roocode:task-1",
		Project:   "test",
		Machine:   "local",
		Agent:     "roocode",
		FilePath:  strPtr(historyPath),
		FileSize:  int64Ptr(compositeSize),
		FileMtime: int64Ptr(compositeMtime),
	}
	require.NoError(t, database.UpsertSession(sess))
	require.NoError(t, database.SetSessionDataVersion(
		sess.ID, db.CurrentDataVersion(),
	))

	engine := &Engine{db: database, machine: "local"}
	source := parser.SourceRef{DisplayPath: historyPath}
	file := parser.DiscoveredFile{
		Agent: parser.AgentRooCode,
		Path:  historyPath,
	}

	if runtime.GOOS != "windows" {
		// The gate must be stat-only: make the contents unreadable so
		// any attempt to hash them would fail loudly.
		require.NoError(t, os.Chmod(historyPath, 0o000))
		require.NoError(t, os.Chmod(messagesPath, 0o000))
		t.Cleanup(func() {
			_ = os.Chmod(historyPath, 0o644)
			_ = os.Chmod(messagesPath, 0o644)
		})
	}

	mtime, fresh := engine.providerSourceFreshBeforeFingerprint(
		t.Context(), source, file, nil,
	)
	assert.True(t, fresh, "unchanged composite stat must skip the fingerprint")
	assert.Equal(t, compositeMtime, mtime)

	if runtime.GOOS != "windows" {
		require.NoError(t, os.Chmod(historyPath, 0o644))
		require.NoError(t, os.Chmod(messagesPath, 0o644))
	}

	// A transcript append that only touches ui_messages.json must
	// defeat the skip: the composite folds in the sibling, so the
	// pre-fingerprint gate cannot hide sibling-only changes.
	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.WriteFile(messagesPath,
		[]byte(`[{"ts":2,"type":"say","say":"text","text":"hi"}]`), 0o644))
	require.NoError(t, os.Chtimes(messagesPath, future, future))

	_, fresh = engine.providerSourceFreshBeforeFingerprint(
		t.Context(), source, file, nil,
	)
	assert.False(t, fresh,
		"a sibling-only transcript change must fall through to the fingerprint")
}
