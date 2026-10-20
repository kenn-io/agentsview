package remotesync

import (
	"database/sql"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestHTTPAntigravityCLIShmChangesDoNotReimport(t *testing.T) {
	const id = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	remote := newMirrorTestRemote(t)
	root := remote.dir
	cliRoot := filepath.Join(root, "antigravity-cli")
	conversations := filepath.Join(cliRoot, "conversations")
	require.NoError(t, os.MkdirAll(conversations, 0o700))
	path := filepath.Join(conversations, id+".db")
	writer, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, writer.Close()) })
	writer.SetMaxOpenConns(1)
	for _, statement := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA wal_autocheckpoint=0",
		"CREATE TABLE steps (idx INTEGER PRIMARY KEY, step_type INTEGER, step_payload BLOB)",
	} {
		_, err = writer.ExecContext(t.Context(), statement)
		require.NoError(t, err)
	}
	appendMessage := func(idx int, text string) {
		t.Helper()
		payload := binary.AppendUvarint(nil, 17<<3|2)
		payload = binary.AppendUvarint(payload, uint64(len(text)))
		payload = append(payload, text...)
		_, err := writer.ExecContext(t.Context(),
			"INSERT INTO steps (idx, step_type, step_payload) VALUES (?, 14, ?)", idx, payload)
		require.NoError(t, err)
	}
	appendMessage(0, "Original user message for the remote fixture")
	geminiPath := filepath.Join(root, "tmp", "project", "chats", "session-fixture.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(geminiPath), 0o700))
	require.NoError(t, os.WriteFile(geminiPath, []byte(
		`{"sessionId":"gemini-fixture","startTime":"2026-09-01T00:00:00Z","messages":[{"type":"user","content":"Unchanged Gemini message","timestamp":"2026-09-01T00:00:00Z"}]}`,
	), 0o600))
	remote.targets.Dirs = map[parser.AgentType][]string{
		parser.AgentGemini:         {root},
		parser.AgentAntigravityCLI: {cliRoot},
	}
	database, hs := newMirrorSync(t, remote, t.TempDir())
	initial, err := hs.Run(t.Context())
	require.NoError(t, err)
	require.Zero(t, initial.Failed)
	require.Equal(t, 2, initial.SessionsSynced)
	sessionID := "devbox~antigravity-cli:" + id
	messages, err := database.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "Original user message for the remote fixture", messages[0].Content)
	gemini, err := database.GetSessionFull(t.Context(), "devbox~gemini:gemini-fixture")
	require.NoError(t, err)
	require.NotNil(t, gemini)

	mirrorSHM := remappedRemotePath(MirrorDir(hs.DataDir, hs.Host), path+"-shm")
	for i := range 3 {
		// SQLite readers can rewrite only the collector's WAL index.
		mtime := time.Now().Add(time.Duration(i+1) * time.Hour)
		require.NoError(t, os.Chtimes(mirrorSHM, mtime, mtime))
		idle, err := hs.Run(t.Context())
		require.NoError(t, err)
		assert.Zero(t, idle.Failed)
		assert.Zero(t, idle.SessionsSynced)
		assert.Zero(t, idle.FilesProcessed)
		assert.Zero(t, idle.FallbackProviders)
		assert.Zero(t, idle.FallbackSources)
	}

	for _, checkpoint := range []bool{false, true} {
		idx := 1
		if checkpoint {
			idx = 2
		}
		appendMessage(idx, "Another user message for the remote fixture")
		changedPath := path + "-wal"
		if checkpoint {
			_, err = writer.ExecContext(t.Context(), "PRAGMA wal_checkpoint(TRUNCATE)")
			require.NoError(t, err)
			changedPath = path
		}
		// Windows can defer metadata updates while the SQLite writer is open.
		mtime := time.Now().Add(time.Duration(idx) * time.Hour)
		require.NoError(t, os.Chtimes(changedPath, mtime, mtime))
		updated, err := hs.Run(t.Context())
		require.NoError(t, err)
		assert.Zero(t, updated.Failed)
		assert.Equal(t, 1, updated.SessionsSynced, "checkpointed=%t", checkpoint)
		assert.Zero(t, updated.FallbackProviders)
		messages, err = database.GetAllMessages(t.Context(), sessionID)
		require.NoError(t, err)
		assert.Len(t, messages, idx+1)
	}
	unchanged, err := database.GetSessionFull(t.Context(), gemini.ID)
	require.NoError(t, err)
	require.NotNil(t, unchanged)
	assert.Equal(t, gemini.LocalModifiedAt, unchanged.LocalModifiedAt,
		"nested Antigravity changes must not rewrite unrelated Gemini sessions")
}
