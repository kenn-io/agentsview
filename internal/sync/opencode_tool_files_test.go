package sync_test

import (
	"database/sql"
	"encoding/base64"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	syncengine "go.kenn.io/agentsview/internal/sync"
)

func TestOpenCodeV2ToolFilesSurviveSync(t *testing.T) {
	schema, err := os.ReadFile("../parser/testdata/opencode_v2/beta.sql")
	require.NoError(t, err)
	raw, err := os.ReadFile("../parser/testdata/opencode_v2/tool_files.json")
	require.NoError(t, err)
	var source struct {
		Content []struct {
			State struct {
				Content []map[string]string `json:"content"`
			} `json:"state"`
		} `json:"content"`
	}
	require.NoError(t, json.Unmarshal(raw, &source))
	imageURI := source.Content[0].State.Content[1]["uri"]

	for _, policy := range []config.ToolResultImages{config.ToolResultImagesKeep, config.ToolResultImagesDrop} {
		t.Run(string(policy), func(t *testing.T) {
			root := t.TempDir()
			writer, err := sql.Open("sqlite3", filepath.Join(root, "opencode.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, writer.Close()) })
			_, err = writer.Exec(string(schema))
			require.NoError(t, err)
			_, err = writer.Exec(`INSERT INTO session_v2
 (id, project_id, slug, directory, version, time_created, time_updated, time_idle)
 SELECT 'ses_files', id, 'tool-files', '/workspace/project-a', '0.0.0-beta-19381',
 1700000000000, 1700000002000, 1700000002000 FROM project LIMIT 1`)
			require.NoError(t, err)
			_, err = writer.Exec(`INSERT INTO session_message VALUES
 ('msg_files', 'ses_files', 'assistant', 1, 1700000000000, 1700000002000, ?)`, string(raw))
			require.NoError(t, err)

			database := dbtest.OpenTestDB(t)
			database.SetToolResultImages(policy)
			engine := syncengine.NewEngine(database, syncengine.EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentOpenCode: {root}},
				Machine:   "local", Ephemeral: true, ToolResultImages: policy,
				DisableFilesystemProjectDiscovery: true,
			})
			t.Cleanup(engine.Close)
			stats := engine.SyncAll(t.Context(), nil)
			require.False(t, stats.Aborted)
			require.Equal(t, 4, stats.Synced)
			messages, err := database.GetAllMessages(t.Context(), "opencode:ses_files")
			require.NoError(t, err)
			require.Len(t, messages, 1)
			require.Len(t, messages[0].ToolCalls, 2)
			for i, call := range messages[0].ToolCalls {
				require.Len(t, call.ResultEvents, 1)
				var stored string
				require.NoError(t, database.Reader().QueryRowContext(t.Context(),
					`SELECT content FROM tool_result_events WHERE session_id = ? AND tool_use_id = ?`,
					"opencode:ses_files", call.ToolUseID).Scan(&stored))
				assert.Equal(t, stored, call.ResultEvents[0].Content)
				assert.Equal(t, stored, call.ResultContent, "summary reads the same retained result")
				var blocks []map[string]any
				require.NoError(t, json.Unmarshal([]byte(stored), &blocks))
				if i == 0 {
					require.Len(t, blocks, 3)
					assert.Equal(t, "Image read successfully", blocks[0]["text"])
					assert.Equal(t, "After the image", blocks[2]["text"])
					if policy == config.ToolResultImagesKeep {
						assert.Equal(t, "input_image", blocks[1]["type"])
						assert.Equal(t, imageURI, blocks[1]["image_url"])
					} else {
						assert.Equal(t, "agentsview_image", blocks[1]["type"])
						assert.Equal(t, float64(68), blocks[1]["byte_size"])
						assert.Equal(t, "plot.png", blocks[1]["name"])
						assert.NotContains(t, stored, imageURI)
					}
				} else {
					var files []map[string]string
					require.NoError(t, json.Unmarshal([]byte(stored), &files))
					assert.Equal(t, source.Content[1].State.Content, files, "image policy must retain PDFs and other file records")
					pdf, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(files[1]["uri"], "data:application/pdf;base64,"))
					require.NoError(t, err)
					assert.Len(t, pdf, 327)
					assert.True(t, strings.HasPrefix(string(pdf), "%PDF-1.4\n"))
					text, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(files[2]["uri"], "data:text/plain;base64,"))
					require.NoError(t, err)
					assert.Equal(t, "alpha\n", string(text))
				}
			}
			assert.Zero(t, engine.SyncAll(t.Context(), nil).Synced, "unchanged payloads do not cause resync churn")
		})
	}
}
