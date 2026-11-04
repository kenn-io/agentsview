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
		provider parser.AgentType
		subtype  string
	}{
		{provider: parser.AgentClaude, subtype: "hook_success"},
		{provider: parser.AgentCowork, subtype: "hook_success"},
		{provider: parser.AgentQoder, subtype: "hook_success"},
		{provider: parser.AgentIcodemate, subtype: "hook_success"},
		{provider: parser.AgentClaude, subtype: "stop_hook_summary"},
	} {
		name := tc.subtype + "/" + string(tc.provider)
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			database := dbtest.OpenTestDB(t)
			roots := []string{t.TempDir(), t.TempDir()}
			readable, _ := writeHookUpgradeSource(t, roots[0], tc.provider, tc.subtype, "c0000000-0000-4000-8000-000000000101")
			gone, goneMeta := writeHookUpgradeSource(t, roots[1], tc.provider, tc.subtype, "c0000000-0000-4000-8000-000000000102")
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
			seedLegacyHookOutput(t, database, readableID)
			archived := seedLegacyHookOutput(t, database, goneID)
			previous := db.CurrentDataVersion() - 1
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

			path := database.Path()
			require.NoError(t, database.Close())
			reopened, err := db.OpenIsolated(ctx, path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, reopened.Close()) })
			require.False(t, reopened.NeedsResync(), "the scoped upgrade must not require a replacement archive")
			assert.Equal(t, previous, reopened.GetSessionDataVersion(ctx, readableID))
			assert.Equal(t, previous, reopened.GetSessionDataVersion(ctx, goneID))
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
			sourceType := "attachment"
			if tc.subtype == "stop_hook_summary" {
				sourceType = "system"
			}
			assert.Equal(t, sourceType, after[1].SourceType)
			assert.Equal(t, tc.subtype, after[1].SourceSubtype)
			assert.Equal(t, "u1", after[1].SourceParentUUID)
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
			assert.Equal(t, 2, sessions)
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

func writeHookUpgradeSource(t *testing.T, root string, provider parser.AgentType, subtype, id string) (string, string) {
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
	content := fmt.Sprintf("%q", hookUpgradeText)
	hook := fmt.Sprintf(`{"type":"attachment","uuid":"h1","parentUuid":"u1","timestamp":"2024-01-01T00:00:01Z","attachment":{"type":%q,"content":%s}}`, subtype, content)
	if subtype == "stop_hook_summary" {
		hook = `{"type":"system","subtype":"stop_hook_summary","uuid":"h1","parentUuid":"u1","timestamp":"2024-01-01T00:00:01Z","hookCount":1,"hookInfos":[{"command":"echo hook"}],"hookErrors":["hook failed"],"preventedContinuation":false}`
	}
	body := fmt.Sprintf(`{"type":"user","uuid":"u1","sessionId":%q,"timestamp":"2024-01-01T00:00:00Z","message":{"role":"user","content":"request"}}
%s
{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2024-01-01T00:00:02Z","message":{"id":"answer-1","role":"assistant","content":[{"type":"text","text":"answer"}],"stop_reason":"end_turn"}}
`, id, hook)
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
		if !parser.IsClaudeHookSubtype(message.SourceSubtype) {
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
