package sync

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
)

// TestSourceMtimeRooCodeUsesCompositeStat confirms that the
// session-watch fallback observes ui_messages.json updates without
// reading either file. RooCode freshness spans both history_item.json
// and ui_messages.json, so a plain stat of the stored
// history_item.json path would miss transcript updates — but the
// watcher polls SourceMtime continuously, so routing through the
// content-hashing provider fingerprint would re-read whole transcripts
// every poll. SourceMtime must return the newer ui_messages.json mtime
// from stat information alone.
func TestSourceMtimeRooCodeUsesCompositeStat(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentRooCode: {root},
		},
		Machine: "local",
	})

	rawID := "mtime-task"
	taskDir := filepath.Join(root, "tasks", rawID)
	require.NoError(t, os.MkdirAll(taskDir, 0o755))

	historyPath := filepath.Join(taskDir, "history_item.json")
	messagesPath := filepath.Join(taskDir, "ui_messages.json")
	require.NoError(t, os.WriteFile(historyPath, []byte(
		`{"id":"mtime-task","number":1,"ts":1688836851000,`+
			`"task":"Test task","tokensIn":10,"tokensOut":20,`+
			`"workspace":"/tmp/roocode","mode":"code","status":"completed"}`,
	), 0o644))
	require.NoError(t, os.WriteFile(messagesPath, []byte(
		`[{"ts":1688836851000,"type":"say","say":"text","text":"Test task"}]`,
	), 0o644))

	historyTime := time.Date(2026, time.June, 4, 10, 0, 0, 0, time.UTC)
	messagesTime := historyTime.Add(5 * time.Minute)
	require.NoError(t, os.Chtimes(historyPath, historyTime, historyTime))
	require.NoError(t, os.Chtimes(messagesPath, messagesTime, messagesTime))

	if runtime.GOOS != "windows" {
		// The lookup must be stat-only: make the contents unreadable so
		// any attempt to hash them fails loudly (SourceMtime returns 0
		// when the fingerprint errors).
		require.NoError(t, os.Chmod(historyPath, 0o000))
		require.NoError(t, os.Chmod(messagesPath, 0o000))
		t.Cleanup(func() {
			_ = os.Chmod(historyPath, 0o644)
			_ = os.Chmod(messagesPath, 0o644)
		})
	}

	assert.Equal(
		t,
		messagesTime.UnixNano(),
		engine.SourceMtime(t.Context(), "roocode:"+rawID),
		"SourceMtime must return the newer ui_messages.json mtime",
	)
}
