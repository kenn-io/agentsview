package sync_test

import (
	"database/sql"
	"encoding/binary"
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
	t.Parallel()
	const (
		id       = "bbbbbbbb-cccc-dddd-eeee-ffffffffffff"
		prompt   = "比较这两个游戏 /grill-me"
		oldText  = "<GRILL_ME> The user has requested an extensive structured interrogation of the subject. Ask questions and explore every assumption carefully. </GRILL_ME>"
		previous = 182
	)
	for _, agent := range []parser.AgentType{parser.AgentAntigravity, parser.AgentAntigravityCLI} {
		t.Run(string(agent), func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			source := filepath.Join(root, "conversations", id+".db")
			require.NoError(t, os.MkdirAll(filepath.Dir(source), 0o755))
			copyAntigravityCLITestSchemaTemplate(t, source)
			conn := openAntigravityCLITestDB(t, source)
			field := func(num uint64, value []byte) []byte {
				b := binary.AppendUvarint(nil, num<<3|2)
				b = binary.AppendUvarint(b, uint64(len(value)))
				return append(b, value...)
			}
			item := append(field(1, []byte(prompt)), field(2, field(27, field(1, field(2, []byte(oldText)))))...)
			input := field(3, item)
			_, err := conn.ExecContext(t.Context(), `INSERT INTO steps (idx, step_type, step_payload) VALUES (?, ?, ?)`, 0, 14, field(19, input))
			require.NoError(t, err)
			require.NoError(t, conn.Close())
			sessionID := string(agent) + ":" + id
			archivePath := filepath.Join(t.TempDir(), "archive.db")
			database, err := db.OpenIsolated(t.Context(), archivePath)
			require.NoError(t, err)
			config := sync.EngineConfig{AgentDirs: map[parser.AgentType][]string{agent: {root}}, Machine: "local"}
			engine := sync.NewEngine(t.Context(), database, config)
			require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
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
			upgraded := sync.NewEngine(t.Context(), reopened, config)
			t.Cleanup(upgraded.Close)
			require.Equal(t, 1, upgraded.SyncAll(t.Context(), nil).Synced)
			assert.Equal(t, []string{prompt}, antigravityCLIMessageContents(fetchMessages(t, reopened, sessionID)))
			after, err := reopened.GetSessionFull(t.Context(), sessionID)
			require.NoError(t, err)
			require.NotNil(t, after.FirstMessage)
			assert.Equal(t, prompt, *after.FirstMessage)
		})
	}
}
