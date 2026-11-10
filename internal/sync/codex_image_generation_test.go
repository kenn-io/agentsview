package sync

import (
	"database/sql"
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestCodexGeneratedImageStoredAsImageResult(t *testing.T) {
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122b07"
	png := testjsonl.SolidPNGBase64(4, 3, color.RGBA{R: 30, G: 120, B: 200, A: 255})
	head := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(uuid, "/workspace/project", "user", "2024-01-01T10:00:00Z"),
		testjsonl.CodexMsgJSON("user", "draw a blue square", "2024-01-01T10:00:01Z"),
	)
	image := testjsonl.JoinJSONL(
		testjsonl.CodexImageGenerationCallJSON("ig_1", "A blue square", png, "2024-01-01T10:00:02Z"),
		testjsonl.CodexImageGenerationEndJSON("ig_1", "A blue square", png, "/generated/ig_1.png", "2024-01-01T10:00:02Z"),
	)
	for _, appended := range []bool{false, true} {
		for _, threshold := range []int64{1, 1 << 30} {
			t.Run("appended="+strconv.FormatBool(appended)+"/threshold="+strconv.FormatInt(threshold, 10), func(t *testing.T) {
				root := t.TempDir()
				day := filepath.Join(root, "2024", "01", "01")
				require.NoError(t, os.MkdirAll(day, 0o755))
				path := filepath.Join(day, "rollout-2024-01-01T10-00-00-"+uuid+".jsonl")
				initial := head + image
				if appended {
					initial = head
				}
				require.NoError(t, os.WriteFile(path, []byte(initial), 0o600))
				database := openTestDB(t)
				engine := NewEngine(t.Context(), database, EngineConfig{
					Machine: "local", Ephemeral: true,
					AgentDirs:                         map[parser.AgentType][]string{parser.AgentCodex: {root}},
					ToolResultImages:                  config.ToolResultImagesKeep,
					StagedCodexParseMinBytes:          threshold,
					DisableFilesystemProjectDiscovery: true,
				})
				t.Cleanup(engine.Close)
				require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
				if appended {
					file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
					require.NoError(t, err)
					_, err = file.WriteString(image)
					require.NoError(t, err)
					require.NoError(t, file.Close())
					require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))
				}

				messages, err := database.GetAllMessages(t.Context(), "codex:"+uuid)
				require.NoError(t, err)
				var calls []db.ToolCall
				for _, message := range messages {
					calls = append(calls, message.ToolCalls...)
				}
				require.Len(t, calls, 1, "the image_generation_end event adds no second call")
				call := calls[0]
				assert.Equal(t, "image_generation", call.ToolName)
				assert.Equal(t,
					`{"id":"ig_1","revised_prompt":"A blue square","status":"completed","type":"image_generation_call"}`,
					call.InputJSON)
				want := `[{"image_url":"data:image/png;base64,` + png + `","type":"input_image"}]`
				assert.Equal(t, want, call.ResultContent)
				require.Len(t, call.ResultEvents, 1)
				assert.Equal(t, "completed", call.ResultEvents[0].Status)
			})
		}
	}
}

func TestCodexGeneratedImageUpgradeReparsesUnchangedSource(t *testing.T) {
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122b08"
	png := testjsonl.SolidPNGBase64(2, 2, color.RGBA{G: 180, A: 255})
	root := t.TempDir()
	writeCodexRollout(t, root, "01", "rollout-2024-01-01T10-00-00-"+uuid+".jsonl", testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(uuid, "/workspace/project", "user", "2024-01-01T10:00:00Z"),
		testjsonl.CodexMsgJSON("user", "draw a green square", "2024-01-01T10:00:01Z"),
		testjsonl.CodexImageGenerationCallJSON("ig_1", "A green square", png, "2024-01-01T10:00:02Z"),
	))
	sessionID := "codex:" + uuid
	resultContent := func(t *testing.T, database *db.DB) string {
		t.Helper()
		messages, err := database.GetAllMessages(t.Context(), sessionID)
		require.NoError(t, err)
		for _, message := range messages {
			for _, call := range message.ToolCalls {
				return call.ResultContent
			}
		}
		return ""
	}
	want := `[{"image_url":"data:image/png;base64,` + png + `","type":"input_image"}]`

	database := dbtest.OpenTestDB(t)
	agentDirs := map[parser.AgentType][]string{parser.AgentCodex: {root}}
	engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: agentDirs, Machine: "local"})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	require.Equal(t, want, resultContent(t, database))

	// Model an archive written before data version 153, which kept no image:
	// no result event and no result summary that could restore it.
	const previousVersion = 152
	require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
		for _, statement := range []string{
			`DELETE FROM tool_result_events WHERE session_id = ?`,
			`UPDATE tool_calls SET result_content = NULL, result_content_length = NULL WHERE session_id = ?`,
		} {
			if _, err := tx.ExecContext(t.Context(), statement, sessionID); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(t.Context(), fmt.Sprintf("PRAGMA user_version = %d", previousVersion))
		return err
	}))
	require.NoError(t, database.SetSessionDataVersion(t.Context(), sessionID, previousVersion))
	require.Empty(t, resultContent(t, database), "the modeled archive holds no image")
	engine.Close()
	dbPath := database.Path()
	require.NoError(t, database.Close())

	database, err := db.OpenIsolated(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	assert.False(t, database.NeedsResync(), "the archive upgrades in place")
	engine = NewEngine(t.Context(), database, EngineConfig{AgentDirs: agentDirs, Machine: "local"})
	t.Cleanup(engine.Close)
	require.NoError(t, engine.ReconcileWatchRoots(t.Context(), []string{root}, false))
	assert.Equal(t, db.CurrentDataVersion(), database.GetSessionDataVersion(t.Context(), sessionID))
	assert.Equal(t, want, resultContent(t, database))
}
