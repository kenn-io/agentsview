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

func TestClaudeChannelUpgradeKeepsRetryCuration(t *testing.T) {
	for _, tt := range []struct {
		name, fixture, seedSQL string
		pinIndex, messageCount int
	}{
		{
			name: "rewind", fixture: "channel_rewind.jsonl", pinIndex: 1, messageCount: 2,
			seedSQL: `DELETE FROM messages WHERE session_id = 'rewind-c0';
				DELETE FROM sessions WHERE id = 'rewind-c0'`,
		},
		{
			name: "linear", fixture: "channel_messages.jsonl", pinIndex: 3, messageCount: 4,
			seedSQL: `DELETE FROM messages WHERE session_id = 'linear' AND ordinal = 2;
				UPDATE messages SET ordinal = 2 WHERE session_id = 'linear' AND ordinal = 3;
				UPDATE pinned_messages SET ordinal = 2 WHERE session_id = 'linear';
				UPDATE sessions SET message_count = 3, user_message_count = 1 WHERE id = 'linear'`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			root := t.TempDir()
			projectDir := filepath.Join(root, "project-a")
			require.NoError(t, os.MkdirAll(projectDir, 0o700))
			path := filepath.Join(projectDir, tt.name+".jsonl")
			content, err := os.ReadFile(filepath.Join("..", "parser", "testdata", "claude", tt.fixture))
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, content, 0o600))
			archivePath := filepath.Join(t.TempDir(), "archive.db")
			database, err := db.OpenIsolated(ctx, archivePath)
			require.NoError(t, err)
			cfg := sync.EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}, Machine: "local"}
			engine := sync.NewEngine(ctx, database, cfg)
			require.Zero(t, engine.SyncAll(ctx, nil).Failed)
			before := fetchMessages(t, database, tt.name)
			require.Len(t, before, tt.messageCount)
			_, err = database.PinMessage(ctx, tt.name, before[tt.pinIndex].ID, new("saved message"))
			require.NoError(t, err)
			engine.Close()
			require.NoError(t, database.Close())

			// Model version 144's discarded branch or missing question without
			// changing the source fingerprint.
			raw, err := sql.Open("sqlite3", archivePath)
			require.NoError(t, err)
			_, err = raw.ExecContext(ctx, tt.seedSQL)
			require.NoError(t, err)
			if tt.name == "linear" {
				var pinOrdinal, messageOrdinal int
				err = raw.QueryRowContext(ctx, `SELECT p.ordinal, m.ordinal
					FROM pinned_messages p JOIN messages m ON m.id = p.message_id
					WHERE p.session_id = 'linear'`).Scan(&pinOrdinal, &messageOrdinal)
				require.NoError(t, err)
				require.Equal(t, 2, pinOrdinal)
				require.Equal(t, 2, messageOrdinal)
			}
			_, err = raw.ExecContext(ctx, "UPDATE sessions SET data_version = 144; PRAGMA user_version = 144")
			require.NoError(t, err)
			require.NoError(t, raw.Close())

			reopened, err := db.OpenIsolated(ctx, archivePath)
			require.NoError(t, err)
			defer func() { require.NoError(t, reopened.Close()) }()
			assert.False(t, reopened.NeedsResync(), "the archive upgrades in place")
			upgraded := sync.NewEngine(ctx, reopened, cfg)
			defer upgraded.Close()
			stats := upgraded.SyncAll(ctx, nil)
			require.Zero(t, stats.Failed)
			assert.Positive(t, stats.Synced)
			after := fetchMessages(t, reopened, tt.name)
			require.Len(t, after, tt.messageCount)
			if tt.name == "rewind" {
				fork, err := reopened.GetSessionFull(ctx, "rewind-c0")
				require.NoError(t, err)
				require.NotNil(t, fork)
			}
			pins, err := reopened.ListPinnedMessages(ctx, tt.name, "")
			require.NoError(t, err)
			require.Len(t, pins, 1)
			assert.Equal(t, before[tt.pinIndex].ID, pins[0].MessageID)
			assert.Equal(t, new("saved message"), pins[0].Note)
			if tt.name == "linear" {
				assert.Equal(t, 3, pins[0].Ordinal)
				assert.Equal(t, after[3].ID, pins[0].MessageID)
			}
		})
	}
}
