package sync_test

import (
	"database/sql"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

func TestAntigravityCLIPromptEnvelopeUpgradeReparsesUnchangedSource(t *testing.T) {
	const (
		uuid            = "12121212-3434-5656-7878-909090909090"
		sessionID       = "antigravity-cli:" + uuid
		prompt          = "What is the capital of Türkiye? Answer in one word, do not use tools."
		envelope        = "<USER_REQUEST>\n" + prompt + "\n</USER_REQUEST><ADDITIONAL_METADATA>injected context</ADDITIONAL_METADATA>"
		previousVersion = 158
	)
	root := t.TempDir()
	sourcePath := filepath.Join(root, "conversations", uuid+".db")
	require.NoError(t, os.MkdirAll(filepath.Dir(sourcePath), 0o755))
	createAntigravityCLIDisplayStepDB(t, sourcePath, "fallback prompt from SQLite")
	brainPath := filepath.Join(root, "brain", uuid, ".system_generated", "logs", "transcript.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(brainPath), 0o755))
	line, err := json.Marshal(map[string]any{
		"step_index": 0, "source": "USER_EXPLICIT", "type": "USER_INPUT",
		"created_at": "2026-10-30T10:53:46Z", "content": envelope,
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(brainPath, append(line, '\n'), 0o600))
	sourceInfo, err := os.Stat(sourcePath)
	require.NoError(t, err)
	brainInfo, err := os.Stat(brainPath)
	require.NoError(t, err)

	archivePath := filepath.Join(t.TempDir(), "archive.db")
	database, err := db.Open(t.Context(), archivePath)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, database.Close()) })
	config := sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentAntigravityCLI: {root}},
		Machine:   "local",
	}
	engine := sync.NewEngine(t.Context(), database, config)
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	before, err := database.GetSessionFull(t.Context(), sessionID)
	require.NoError(t, err)
	hashBefore, ok := database.GetSessionFileHash(t.Context(), sessionID)
	require.True(t, ok)
	require.NotEmpty(t, hashBefore)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Skipped)
	skipCache, err := database.LoadSkippedFiles(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, skipCache, "the restart must load a persisted freshness cache")
	engine.Close()
	require.NoError(t, database.Close())

	// Model the wrapped text and title saved by version 158, retaining the
	// source fingerprint and persisted freshness cache from the first sync.
	archive, err := sql.Open("sqlite3", archivePath)
	require.NoError(t, err)
	_, err = archive.ExecContext(t.Context(), "UPDATE messages SET content = ?, content_length = ? WHERE session_id = ?", envelope, len(envelope), sessionID)
	require.NoError(t, err)
	oldTitle := strings.ReplaceAll(envelope, "\n", " ")
	_, err = archive.ExecContext(t.Context(), "UPDATE sessions SET data_version = ?, first_message = ? WHERE id = ?", previousVersion, oldTitle, sessionID)
	require.NoError(t, err)
	_, err = archive.ExecContext(t.Context(), "PRAGMA user_version = 158")
	require.NoError(t, err)
	require.NoError(t, archive.Close())

	reopened, err := db.Open(t.Context(), archivePath)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, reopened.Close()) })
	require.False(t, reopened.NeedsResync(), "the repair must use the scoped upgrade")
	require.Equal(t, []string{envelope}, antigravityCLIMessageContents(fetchMessages(t, reopened, sessionID)))
	stored, err := reopened.GetSessionFull(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, stored.FirstMessage)
	require.Equal(t, oldTitle, *stored.FirstMessage)
	upgraded := sync.NewEngine(t.Context(), reopened, config)
	t.Cleanup(upgraded.Close)
	require.Equal(t, 1, upgraded.SyncAll(t.Context(), nil).Synced,
		"ordinary sync must reparse the unchanged source after reopening")
	assert.Equal(t, []string{prompt}, antigravityCLIMessageContents(fetchMessages(t, reopened, sessionID)))
	after, err := reopened.GetSessionFull(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, after.FirstMessage)
	assert.Equal(t, prompt, *after.FirstMessage)
	assert.Equal(t, before.ID, after.ID)
	assert.Equal(t, before.FilePath, after.FilePath)
	hashAfter, ok := reopened.GetSessionFileHash(t.Context(), sessionID)
	require.True(t, ok)
	assert.Equal(t, hashBefore, hashAfter)
	assert.Equal(t, db.CurrentDataVersion(), reopened.GetSessionDataVersion(t.Context(), sessionID))
	page, err := reopened.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
	require.NoError(t, err)
	assert.Len(t, page.Sessions, 1)
	for path, infoBefore := range map[string]os.FileInfo{sourcePath: sourceInfo, brainPath: brainInfo} {
		infoAfter, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, infoBefore.ModTime(), infoAfter.ModTime(), path)
	}
}

// Parser version 142 kept one turn per distinct history prompt, so a prompt
// the user repeated collapsed into one turn when the SQLite decode missed the
// repeat. The upgrade must restore the repeated turn without the source
// changing, because an unchanged source is otherwise skipped.
func TestAntigravityCLIRepeatedHistoryPromptUpgradeReparsesUnchangedSource(t *testing.T) {
	const (
		uuid            = "12121212-3434-5656-7878-909090909090"
		sessionID       = "antigravity-cli:" + uuid
		prompt          = "Run the deployment check again"
		previousVersion = 142
	)
	root := t.TempDir()
	convDir := filepath.Join(root, "conversations")
	require.NoError(t, os.MkdirAll(convDir, 0o755))
	dbPath := filepath.Join(convDir, uuid+".db")
	createAntigravityCLIDisplayStepDB(t, dbPath, prompt)
	conn := openAntigravityCLITestDB(t, dbPath)
	insertAntigravityCLIStep(t, conn, 1, 17, "The deployment check passed")
	require.NoError(t, conn.Close())
	history := `{"conversationId": "` + uuid + `", "timestamp": 1779000000000, "display": "` + prompt + `"}` + "\n" +
		`{"conversationId": "` + uuid + `", "timestamp": 1779000600000, "display": "` + prompt + `"}` + "\n"
	historyPath := filepath.Join(root, "history.jsonl")
	require.NoError(t, os.WriteFile(historyPath, []byte(history), 0o600))
	infoBefore, err := os.Stat(historyPath)
	require.NoError(t, err)

	archivePath := filepath.Join(t.TempDir(), "archive.db")
	database, err := db.Open(t.Context(), archivePath)
	require.NoError(t, err)
	config := sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentAntigravityCLI: {root}},
		Machine:   "local",
	}
	engine := sync.NewEngine(t.Context(), database, config)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	before, err := database.GetSessionFull(t.Context(), sessionID)
	require.NoError(t, err)
	hashBefore, ok := database.GetSessionFileHash(t.Context(), sessionID)
	require.True(t, ok)
	want := antigravityCLIMessageContents(fetchMessages(t, database, sessionID))
	require.Equal(t, []string{prompt, "The deployment check passed", prompt}, want)
	engine.Close()
	require.NoError(t, database.Close())

	// Model an archive version 142 wrote: the repeated prompt collapsed into
	// the first turn, and the source files are untouched.
	archive, err := sql.Open("sqlite3", archivePath)
	require.NoError(t, err)
	_, err = archive.ExecContext(t.Context(), "DELETE FROM messages WHERE session_id = ? AND ordinal = 2", sessionID)
	require.NoError(t, err)
	_, err = archive.ExecContext(t.Context(), "UPDATE sessions SET data_version = ?, message_count = 2, user_message_count = 1 WHERE id = ?", previousVersion, sessionID)
	require.NoError(t, err)
	_, err = archive.ExecContext(t.Context(), "PRAGMA user_version = 142")
	require.NoError(t, err)
	require.NoError(t, archive.Close())

	assert.Equal(t, []parser.AgentType{parser.AgentAntigravityCLI}, db.ScopedDataVersionProviders(previousVersion, previousVersion+1),
		"the history repair must reparse only antigravity-cli sources in place")
	reopened, err := db.Open(t.Context(), archivePath)
	require.NoError(t, err)
	defer func() { require.NoError(t, reopened.Close()) }()
	upgraded := sync.NewEngine(t.Context(), reopened, config)
	defer upgraded.Close()
	require.Equal(t, []string{prompt, "The deployment check passed"}, antigravityCLIMessageContents(fetchMessages(t, reopened, sessionID)))
	require.Equal(t, 1, upgraded.SyncAll(t.Context(), nil).Synced,
		"the version bump must bypass freshness checks on unchanged sources")

	assert.Equal(t, want, antigravityCLIMessageContents(fetchMessages(t, reopened, sessionID)))
	after, err := reopened.GetSessionFull(t.Context(), sessionID)
	require.NoError(t, err)
	assert.Equal(t, before.ID, after.ID)
	assert.Equal(t, before.FilePath, after.FilePath)
	hashAfter, ok := reopened.GetSessionFileHash(t.Context(), sessionID)
	require.True(t, ok)
	assert.Equal(t, hashBefore, hashAfter)
	assert.Equal(t, db.CurrentDataVersion(), reopened.GetSessionDataVersion(t.Context(), sessionID))
	page, err := reopened.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
	require.NoError(t, err)
	assert.Len(t, page.Sessions, 1, "the upgrade must not create a duplicate session")
	infoAfter, err := os.Stat(historyPath)
	require.NoError(t, err)
	assert.Equal(t, infoBefore.ModTime(), infoAfter.ModTime())
}
