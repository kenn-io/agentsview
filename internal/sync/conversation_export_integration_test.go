package sync_test

import (
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

func TestConversationExportUsesParserProseThroughNormalSync(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "project-a", "conversation-a.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	// Literal source blocks make this fail if the archive converter drops
	// provenance or the exporter falls back to the flattened display content.
	source := fmt.Sprintf(`{"type":"user","uuid":"user-a","sessionId":"conversation-a","cwd":%q,"timestamp":"2026-08-01T10:00:00Z","message":{"role":"user","content":[{"type":"text","text":"Please check α"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},{"type":"tool_result","tool_use_id":"call-a","content":"Excluded result"}]}}
{"type":"assistant","uuid":"entry-a","sessionId":"conversation-a","parentUuid":"user-a","timestamp":"2026-08-01T10:00:01Z","message":{"id":"response-a","role":"assistant","model":"model-a","content":[{"type":"thinking","thinking":"Excluded reasoning"},{"type":"text","text":"Working on it."},{"type":"tool_use","id":"call-b","name":"Read","input":{"path":"excluded-argument"}}]}}
{"type":"assistant","uuid":"entry-b","sessionId":"conversation-a","parentUuid":"entry-a","timestamp":"2026-08-01T10:00:02Z","message":{"id":"response-b","role":"assistant","content":[{"type":"future_private_block","text":"Unknown content"},{"type":"text","text":"Unproven text"}]}}
{"type":"assistant","uuid":"entry-c","sessionId":"conversation-a","parentUuid":"entry-b","isApiErrorMessage":true,"timestamp":"2026-08-01T10:00:03Z","message":{"id":"response-c","role":"assistant","content":[{"type":"text","text":"API Error: request failed"}]}}
`, root)
	require.NoError(t, os.WriteFile(path, []byte(source), 0o600))
	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	initial, err := database.ExportConversationChanges(t.Context(), db.ConversationExportOptions{})
	require.NoError(t, err)
	texts := make(map[string]string)
	gaps := 0
	for _, change := range initial.Changes {
		if change.Type == "session" {
			continue
		}
		body, err := database.GetConversationMessage(t.Context(), db.ConversationMessageOptions{
			SessionID: change.SessionID, MessageID: change.MessageID, Revision: change.Revision,
			DatabaseID: initial.DatabaseID,
		})
		require.NoError(t, err)
		if change.Gap != "" {
			assert.Equal(t, "visible_text_unavailable", change.Gap)
			assert.Equal(t, "visible_text_unavailable", body.Gap)
			assert.Nil(t, body.Text, "parser uncertainty must survive the archive-to-export boundary")
			gaps++
			continue
		}
		require.NotNil(t, body.Text)
		texts[change.Role] = *body.Text
	}
	assert.Equal(t, map[string]string{"user": "Please check α", "assistant": "Working on it."}, texts)
	assert.Equal(t, 1, gaps)
	engine.SyncAll(t.Context(), nil)
	unchanged, err := database.ExportConversationChanges(t.Context(), db.ConversationExportOptions{Checkpoint: initial.Checkpoint})
	require.NoError(t, err)
	assert.Empty(t, unchanged.Changes)
	assert.Equal(t, initial.Checkpoint, unchanged.Checkpoint)
	resync := engine.ResyncAll(t.Context(), nil)
	require.False(t, resync.Aborted, "warnings: %v", resync.Warnings)
	require.Zero(t, resync.Failed)
	rebuilt, err := database.ExportConversationChanges(t.Context(), db.ConversationExportOptions{})
	require.NoError(t, err)
	assert.Equal(t, initial.ArchiveID, rebuilt.ArchiveID)
	assert.NotEqual(t, initial.DatabaseID, rebuilt.DatabaseID)
	beforeIDs, afterIDs := map[string]string{}, map[string]string{}
	for _, change := range initial.Changes {
		if change.Type == "message" {
			beforeIDs[change.MessageID] = change.Digest
		}
	}
	for _, change := range rebuilt.Changes {
		if change.Type == "message" {
			afterIDs[change.MessageID] = change.Digest
		}
	}
	assert.Equal(t, beforeIDs, afterIDs, "a full source rebuild preserves message identities and equal prose")
	_, err = database.ExportConversationChanges(t.Context(), db.ConversationExportOptions{Checkpoint: initial.Checkpoint})
	require.ErrorIs(t, err, db.ErrConversationReconciliationRequired)
}

func TestConversationExportCodexAppendAfterRestart(t *testing.T) {
	for _, staged := range []bool{false, true} {
		t.Run(fmt.Sprintf("staged=%t", staged), func(t *testing.T) {
			root := t.TempDir()
			const sessionID = "019eb791-cf7d-75c1-8439-9ed74c122c80"
			path := filepath.Join(root, "2026", "08", "01", "rollout-2026-08-01T10-00-00-"+sessionID+".jsonl")
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			source := fmt.Sprintf(`{"type":"session_meta","timestamp":"2026-08-01T10:00:00Z","payload":{"id":%q,"cwd":%q,"originator":"codex_cli_rs"}}
{"type":"response_item","timestamp":"2026-08-01T10:00:01Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Check this code"}]}}
{"type":"response_item","timestamp":"2026-08-01T10:00:02Z","payload":{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"Checking now"}]}}
{"type":"response_item","timestamp":"2026-08-01T10:00:03Z","payload":{"type":"function_call","name":"shell_command","call_id":"call-a","arguments":"excluded arguments"}}
`, sessionID, root)
			require.NoError(t, os.WriteFile(path, []byte(source), 0o600))
			database := dbtest.OpenTestDB(t)
			cfg := sync.EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Machine: "local"}
			if staged {
				cfg.StagedCodexParseMinBytes = 1
				cfg.CodexStagingDir = t.TempDir()
			}
			engine := sync.NewEngine(database, cfg)
			t.Cleanup(engine.Close)
			require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
			initial, err := database.ExportConversationChanges(t.Context(), db.ConversationExportOptions{})
			require.NoError(t, err)
			var texts []string
			for _, change := range initial.Changes {
				if change.Type != "message" {
					continue
				}
				body, err := database.GetConversationMessage(t.Context(), db.ConversationMessageOptions{
					DatabaseID: initial.DatabaseID, SessionID: change.SessionID, MessageID: change.MessageID, Revision: change.Revision,
				})
				require.NoError(t, err)
				require.NotNil(t, body.Text)
				texts = append(texts, *body.Text)
			}
			assert.ElementsMatch(t, []string{"Check this code", "Checking now"}, texts)
			engine.Close()
			engine = sync.NewEngine(database, cfg)
			t.Cleanup(engine.Close)
			engine.SyncAll(t.Context(), nil)
			quiet, err := database.ExportConversationChanges(t.Context(), db.ConversationExportOptions{Checkpoint: initial.Checkpoint})
			require.NoError(t, err)
			require.Empty(t, quiet.Changes)
			source += `{"type":"response_item","timestamp":"2026-08-01T10:00:04Z","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"The check is complete"}]}}` + "\n"
			require.NoError(t, os.WriteFile(path, []byte(source), 0o600))
			require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
			delta, err := database.ExportConversationChanges(t.Context(), db.ConversationExportOptions{Checkpoint: quiet.Checkpoint})
			require.NoError(t, err)
			var messages []db.ConversationChange
			for _, change := range delta.Changes {
				if change.Type == "message" {
					messages = append(messages, change)
				}
			}
			require.Len(t, messages, 1, "a verified source append must not replay earlier prose")
			assert.False(t, messages[0].Deleted)
			body, err := database.GetConversationMessage(t.Context(), db.ConversationMessageOptions{
				DatabaseID: delta.DatabaseID, SessionID: messages[0].SessionID, MessageID: messages[0].MessageID, Revision: messages[0].Revision,
			})
			require.NoError(t, err)
			require.NotNil(t, body.Text)
			assert.Equal(t, "The check is complete", *body.Text)
		})
	}
}
