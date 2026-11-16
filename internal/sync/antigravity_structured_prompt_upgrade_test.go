package sync_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

func TestAntigravityStructuredPromptUpgradeReparsesUnchangedSource(t *testing.T) {
	const (
		id       = "bbbbbbbb-cccc-dddd-eeee-ffffffffffff"
		prompt   = "比较这两个游戏 /grill-me"
		oldText  = "<GRILL_ME> The user has requested an extensive structured interrogation of the subject. Ask questions and explore every assumption carefully. </GRILL_ME>"
		previous = 182
	)
	fixture, err := os.ReadFile(filepath.Join("..", "parser", "testdata", "data-version-corpus", "antigravity-cli", "conversations", id+".db"))
	require.NoError(t, err)
	for _, agent := range []parser.AgentType{parser.AgentAntigravity, parser.AgentAntigravityCLI} {
		t.Run(string(agent), func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			source := filepath.Join(root, "conversations", id+".db")
			require.NoError(t, os.MkdirAll(filepath.Dir(source), 0o755))
			require.NoError(t, os.WriteFile(source, fixture, 0o600))
			sessionID := string(agent) + ":" + id
			archivePath := filepath.Join(t.TempDir(), "archive.db")
			database, err := db.OpenIsolated(t.Context(), archivePath)
			require.NoError(t, err)
			config := sync.EngineConfig{AgentDirs: map[parser.AgentType][]string{agent: {root}}, Machine: "local"}
			engine := sync.NewEngine(t.Context(), database, config)
			require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
			before, err := database.GetSessionFull(t.Context(), sessionID)
			require.NoError(t, err)
			require.Equal(t, []string{prompt}, antigravityCLIMessageContents(fetchMessages(t, database, sessionID)))
			hashBefore, ok := database.GetSessionFileHash(t.Context(), sessionID)
			require.True(t, ok)
			require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Skipped)
			engine.Close()
			require.NoError(t, database.Close())

			// Restore version 182's selected expansion without changing the source or its fingerprint.
			archive, err := sql.Open("sqlite3", archivePath)
			require.NoError(t, err)
			_, err = archive.ExecContext(t.Context(), "UPDATE messages SET content = ?, content_length = ? WHERE session_id = ?", oldText, len(oldText), sessionID)
			require.NoError(t, err)
			_, err = archive.ExecContext(t.Context(), "UPDATE sessions SET first_message = ?, data_version = ? WHERE id = ?", oldText, previous, sessionID)
			require.NoError(t, err)
			_, err = archive.ExecContext(t.Context(), "PRAGMA user_version = 182")
			require.NoError(t, err)
			require.NoError(t, archive.Close())

			reopened, err := db.OpenIsolated(t.Context(), archivePath)
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, reopened.Close()) })
			require.Equal(t, []string{oldText}, antigravityCLIMessageContents(fetchMessages(t, reopened, sessionID)))
			upgraded := sync.NewEngine(t.Context(), reopened, config)
			t.Cleanup(upgraded.Close)
			require.Equal(t, 1, upgraded.SyncAll(t.Context(), nil).Synced)
			assert.Equal(t, []string{prompt}, antigravityCLIMessageContents(fetchMessages(t, reopened, sessionID)))
			after, err := reopened.GetSessionFull(t.Context(), sessionID)
			require.NoError(t, err)
			require.NotNil(t, after.FirstMessage)
			assert.Equal(t, prompt, *after.FirstMessage)
			assert.Equal(t, before.ID, after.ID)
			assert.Equal(t, before.FilePath, after.FilePath)
			require.NotNil(t, after.FilePath)
			assert.Equal(t, source, *after.FilePath)
			hashAfter, ok := reopened.GetSessionFileHash(t.Context(), sessionID)
			require.True(t, ok)
			assert.Equal(t, hashBefore, hashAfter)
			assert.Equal(t, db.CurrentDataVersion(), reopened.GetSessionDataVersion(t.Context(), sessionID))
			page, err := reopened.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
			require.NoError(t, err)
			assert.Len(t, page.Sessions, 1)
		})
	}
}
