package db_test

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

const hookUpgradeText = "  中文 e\u0301 <script>literal</script>\n  preserved whitespace  \n"

func TestClaudeHookOutputUpgradePreservesArchive(t *testing.T) {
	for _, tc := range []struct {
		provider    parser.AgentType
		archiveOnly bool
	}{
		{provider: parser.AgentClaude},
		{provider: parser.AgentCowork},
		{provider: parser.AgentQoder},
		{provider: parser.AgentIcodemate},
		{provider: parser.AgentClaude, archiveOnly: true},
	} {
		name := string(tc.provider)
		if tc.archiveOnly {
			name += " archive-only"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			database := dbtest.OpenTestDB(t)
			roots := []string{t.TempDir(), t.TempDir()}
			readable, _ := writeHookUpgradeSource(t, roots[0], tc.provider, "c0000000-0000-4000-8000-000000000101")
			gone, goneMeta := writeHookUpgradeSource(t, roots[1], tc.provider, "c0000000-0000-4000-8000-000000000102")
			original, err := os.ReadFile(readable)
			require.NoError(t, err)
			info, err := os.Stat(readable)
			require.NoError(t, err)
			cfg := sync.EngineConfig{Machine: "local", AgentDirs: map[parser.AgentType][]string{tc.provider: roots}}
			engine := sync.NewEngine(ctx, database, cfg)
			first := engine.SyncAll(ctx, nil)
			engine.Close()
			require.Zero(t, first.Failed)
			require.Equal(t, 2, first.Synced)
			readableID := hookUpgradeSessionID(t, database, readable)
			goneID := hookUpgradeSessionID(t, database, gone)
			legacy := seedLegacyHookOutput(t, database, readableID)
			archived := seedLegacyHookOutput(t, database, goneID)
			previous := db.CurrentDataVersion() - 1
			controls := map[string][]db.Message{}
			for id, version := range map[string]int{"unaffected": previous, "retry": previous - 1} {
				require.NoError(t, database.UpsertSession(ctx, db.Session{
					ID: id, Agent: string(parser.AgentHermes), Project: "controls", Machine: "local",
					FilePath: new(filepath.Join(t.TempDir(), id+".jsonl")), MessageCount: 1, UserMessageCount: 1,
				}))
				require.NoError(t, database.ReplaceSessionMessages(ctx, id, []db.Message{{
					SessionID: id, Ordinal: 0, Role: "user", Content: "retained control", DialogueText: new("retained control"),
					ContentLength: len("retained control"), SourceUUID: "control", Timestamp: "2024-01-01T00:00:00Z",
				}}))
				require.NoError(t, database.SetSessionDataVersion(ctx, id, version))
				controls[id], err = database.GetAllMessages(ctx, id)
				require.NoError(t, err)
			}
			published, err := database.ExportConversationChanges(ctx, db.ConversationExportOptions{})
			require.NoError(t, err)
			userIdentity := hookUpgradeChange(t, published, readableID, 0).MessageID
			answerIdentity := hookUpgradeChange(t, published, readableID, 1).MessageID
			require.NoError(t, os.Remove(gone))
			if goneMeta != "" {
				require.NoError(t, os.Remove(goneMeta))
			}
			require.NoError(t, database.Update(ctx, func(tx *sql.Tx) error {
				if _, err := tx.ExecContext(ctx, "UPDATE sessions SET data_version = ? WHERE id IN (?, ?)", previous, readableID, goneID); err != nil {
					return err
				}
				_, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", previous))
				return err
			}))
			if tc.archiveOnly {
				require.NoError(t, database.EnableArchiveOnly(ctx))
			}
			path := database.Path()
			require.NoError(t, database.Close())
			reopened, err := db.OpenIsolated(ctx, path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, reopened.Close()) })
			if tc.archiveOnly {
				require.ErrorIs(t, reopened.RequireSourceSync(ctx), db.ErrArchiveOnly)
				assert.True(t, reopened.NeedsResync(), "retained archive-only content must not be marked reparsed")
				var encoded int
				require.NoError(t, reopened.Reader().QueryRow(ctx, "PRAGMA user_version").Scan(&encoded))
				assert.Equal(t, (1<<20)+previous, encoded)
				assert.Equal(t, previous, reopened.GetSessionDataVersion(ctx, readableID))
				after, err := reopened.GetAllMessages(ctx, readableID)
				require.NoError(t, err)
				assert.Equal(t, legacy, after)
				assertHookUpgradeCuration(t, reopened, readableID, legacy[1].ID, 1)
				return
			}
			require.False(t, reopened.NeedsResync(), "the scoped upgrade must not require a replacement archive")
			assert.Equal(t, previous, reopened.GetSessionDataVersion(ctx, readableID))
			assert.Equal(t, previous, reopened.GetSessionDataVersion(ctx, goneID))
			assert.Equal(t, db.CurrentDataVersion(), reopened.GetSessionDataVersion(ctx, "unaffected"))
			assert.Equal(t, previous-1, reopened.GetSessionDataVersion(ctx, "retry"))
			pending, _, err := db.ArchiveResyncState(ctx, path)
			require.NoError(t, err)
			assert.True(t, pending)
			upgraded := sync.NewEngine(ctx, reopened, cfg)
			t.Cleanup(upgraded.Close)
			stats := upgraded.SyncAll(ctx, nil)
			require.Zero(t, stats.Failed)
			require.Equal(t, 1, stats.Synced, "an unchanged readable source still needs the new parser")
			after, err := reopened.GetAllMessages(ctx, readableID)
			require.NoError(t, err)
			require.Len(t, after, 3)
			// Row IDs may change during replacement; public archive identities
			// and the pin's logical target must survive the ordinal shift.
			republished, err := reopened.ExportConversationChanges(ctx, db.ConversationExportOptions{})
			require.NoError(t, err)
			assert.Equal(t, published.ArchiveID, republished.ArchiveID)
			assert.Equal(t, published.DatabaseID, republished.DatabaseID)
			assert.Equal(t, userIdentity, hookUpgradeChange(t, republished, readableID, 0).MessageID)
			assert.Equal(t, answerIdentity, hookUpgradeChange(t, republished, readableID, 2).MessageID)
			assert.Equal(t, []string{"u1", "h1", "a1"}, []string{after[0].SourceUUID, after[1].SourceUUID, after[2].SourceUUID})
			assert.True(t, after[1].IsSystem)
			assert.Equal(t, "attachment", after[1].SourceType)
			assert.Equal(t, "hook_success", after[1].SourceSubtype)
			assert.Equal(t, "u1", after[1].SourceParentUUID)
			assert.Equal(t, "Content:\n"+hookUpgradeText, after[1].Content)
			require.NotNil(t, after[1].DialogueText)
			assert.Empty(t, *after[1].DialogueText)
			assertHookUpgradeCuration(t, reopened, readableID, after[2].ID, 2)
			session, err := reopened.GetSessionFull(ctx, readableID)
			require.NoError(t, err)
			require.NotNil(t, session)
			assert.Equal(t, readable, reopened.GetSessionFilePath(ctx, readableID))
			assert.Equal(t, 3, session.MessageCount)
			assert.Equal(t, 1, session.UserMessageCount)
			assert.Equal(t, db.CurrentDataVersion(), session.DataVersion)
			retained, err := reopened.GetAllMessages(ctx, goneID)
			require.NoError(t, err)
			assert.Equal(t, archived, retained, "losing the original source must not lose archived rows")
			assert.Equal(t, previous, reopened.GetSessionDataVersion(ctx, goneID))
			assertHookUpgradeCuration(t, reopened, goneID, archived[1].ID, 1)
			for id, before := range controls {
				control, err := reopened.GetAllMessages(ctx, id)
				require.NoError(t, err)
				assert.Equal(t, before, control)
			}
			assert.Equal(t, previous-1, reopened.GetSessionDataVersion(ctx, "retry"))
			pending, _, err = db.ArchiveResyncState(ctx, path)
			require.NoError(t, err)
			assert.False(t, pending, "the authoritative full pass completed")
			second := upgraded.SyncAll(ctx, nil)
			assert.Zero(t, second.Failed)
			assert.Zero(t, second.Synced)
			again, err := reopened.GetAllMessages(ctx, readableID)
			require.NoError(t, err)
			assert.Equal(t, after, again)
			var sessions int
			require.NoError(t, reopened.Reader().QueryRow(ctx, "SELECT count(*) FROM sessions").Scan(&sessions))
			assert.Equal(t, 4, sessions)
			bytes, err := os.ReadFile(readable)
			require.NoError(t, err)
			assert.Equal(t, original, bytes)
			afterInfo, err := os.Stat(readable)
			require.NoError(t, err)
			assert.Equal(t, info.ModTime(), afterInfo.ModTime())
		})
	}
}

func hookUpgradeChange(t *testing.T, result db.ConversationExportResult, sessionID string, ordinal int) db.ConversationChange {
	t.Helper()
	for _, change := range result.Changes {
		if change.Type == "message" && !change.Deleted && change.SessionID == sessionID && change.Ordinal == ordinal {
			return change
		}
	}
	require.FailNow(t, "missing exported message", "%s ordinal %d", sessionID, ordinal)
	return db.ConversationChange{}
}

func writeHookUpgradeSource(t *testing.T, root string, provider parser.AgentType, id string) (string, string) {
	t.Helper()
	path := filepath.Join(root, "project", id+".jsonl")
	meta := ""
	if provider == parser.AgentCowork {
		workspace := filepath.Join(root, "org", "workspace")
		localID := "local_" + id
		meta = filepath.Join(workspace, localID+".json")
		dbtest.WriteTestFile(t, meta, []byte(fmt.Sprintf(`{"sessionId":%q,"cliSessionId":%q}`, localID, id)))
		path = filepath.Join(workspace, localID, ".claude", "projects", "project", id+".jsonl")
	}
	body := fmt.Sprintf(`{"type":"user","uuid":"u1","sessionId":%q,"timestamp":"2024-01-01T00:00:00Z","message":{"role":"user","content":"request"}}
{"type":"attachment","uuid":"h1","parentUuid":"u1","timestamp":"2024-01-01T00:00:01Z","attachment":{"type":"hook_success","content":%q}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2024-01-01T00:00:02Z","message":{"id":"answer-1","role":"assistant","content":[{"type":"text","text":"answer"}],"stop_reason":"end_turn"}}
`, id, hookUpgradeText)
	dbtest.WriteTestFile(t, path, []byte(body))
	return path, meta
}

func hookUpgradeSessionID(t *testing.T, database *db.DB, path string) string {
	t.Helper()
	var id string
	require.NoError(t, database.Reader().QueryRow(t.Context(), "SELECT id FROM sessions WHERE file_path = ?", path).Scan(&id))
	return id
}

func seedLegacyHookOutput(t *testing.T, database *db.DB, id string) []db.Message {
	t.Helper()
	ctx := t.Context()
	parsed, err := database.GetAllMessages(ctx, id)
	require.NoError(t, err)
	require.Len(t, parsed, 3)
	var legacy []db.Message
	for _, message := range parsed {
		if message.SourceSubtype != "hook_success" {
			message.Ordinal = len(legacy)
			legacy = append(legacy, message)
		}
	}
	require.Len(t, legacy, 2)
	require.NoError(t, database.ReplaceSessionMessages(ctx, id, legacy))
	require.NoError(t, database.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE sessions SET message_count=2, user_message_count=1 WHERE id=?", id)
		return err
	}))
	legacy, err = database.GetAllMessages(ctx, id)
	require.NoError(t, err)
	require.Len(t, legacy, 2)
	_, err = database.PinMessage(ctx, id, legacy[1].ID, new("saved answer"))
	require.NoError(t, err)
	require.NoError(t, database.RenameSession(ctx, id, new("Saved title")))
	_, err = database.StarSession(ctx, id)
	require.NoError(t, err)
	return legacy
}

func assertHookUpgradeCuration(t *testing.T, database *db.DB, id string, messageID int64, ordinal int) {
	t.Helper()
	pins, err := database.ListPinnedMessages(t.Context(), id, "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.Equal(t, messageID, pins[0].MessageID)
	assert.Equal(t, ordinal, pins[0].Ordinal)
	assert.Equal(t, new("saved answer"), pins[0].Note)
	session, err := database.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, new("Saved title"), session.DisplayName)
	starred, err := database.ListStarredSessionIDs(t.Context())
	require.NoError(t, err)
	assert.Contains(t, starred, id)
}
