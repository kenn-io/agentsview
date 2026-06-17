package sessionwatch

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

// testWatcher creates a Watcher backed by a fresh SQLite database
// and a minimal sync engine for tests that need checkDBForChanges
// access.
func testWatcher(t *testing.T) *Watcher {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	database, err := db.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {dir},
		},
		Machine: "test",
	})
	return New(database, engine)
}

func TestStatMtime_NonexistentFile(t *testing.T) {
	t.Parallel()
	got := StatMtime(
		filepath.Join(t.TempDir(), "no-such-file"),
	)
	assert.Equal(t, int64(0), got)
}

func TestStatMtime_ExistingFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "file.txt")
	require.NoError(t, os.WriteFile(path, []byte("data"), 0o644))
	got := StatMtime(path)
	assert.NotZero(t, got)
}

func TestCheckDBForChanges_FileDisappears(t *testing.T) {
	t.Parallel()
	w := testWatcher(t)

	path := filepath.Join(t.TempDir(), "gone.jsonl")
	var lastMtime int64 = 12345
	var mchanged time.Time
	var lastCount int
	var lastDBMtime int64

	changed := w.checkDBForChanges(
		"test-session",
		&lastCount,
		&lastDBMtime,
		&path,
		&lastMtime,
		&mchanged,
	)
	assert.False(t, changed, "expected no change signal")
	assert.Empty(t, path)
	assert.Equal(t, int64(0), lastMtime)
}

func TestCheckDBForChanges_FileHashChange(t *testing.T) {
	t.Parallel()
	w := testWatcher(t)
	database, ok := w.db.(*db.DB)
	require.True(t, ok, "test watcher should use SQLite DB")

	const sessionID = "hash-change"
	var mtime int64 = 12345
	hash1 := "shelley-fingerprint-1"
	dbtest.SeedSession(t, database, sessionID, "proj", func(s *db.Session) {
		s.MessageCount = 2
		s.FileMtime = &mtime
		s.FileHash = &hash1
	})

	lastCount, lastDBMtime, ok := w.db.GetSessionVersion(sessionID)
	require.True(t, ok, "initial session version")

	hash2 := "shelley-fingerprint-2"
	dbtest.SeedSession(t, database, sessionID, "proj", func(s *db.Session) {
		s.MessageCount = 2
		s.FileMtime = &mtime
		s.FileHash = &hash2
	})

	sourcePath := ""
	var lastFileMtime int64
	var mchanged time.Time
	changed := w.checkDBForChanges(
		sessionID,
		&lastCount,
		&lastDBMtime,
		&sourcePath,
		&lastFileMtime,
		&mchanged,
	)

	assert.True(t, changed,
		"file_hash-only rewrites must refresh session watchers")
}
