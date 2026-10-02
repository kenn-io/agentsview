package service_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// Syncing a Codex thread/revert page by path returns the page's own session.
func TestSyncCodexRevertPagePathReturnsPage(t *testing.T) {
	const thread = "11111111-1111-4111-8111-111111111111"
	const rollout = "22222222-2222-4222-8222-222222222222"
	for _, agent := range []parser.AgentType{parser.AgentCodex, parser.AgentTraeX, parser.AgentAugureCode} {
		t.Run(string(agent), func(t *testing.T) {
			root := t.TempDir()
			var paths []string
			for _, day := range []string{"24", "25"} {
				identity := thread
				extra := map[string]any{}
				if day == "25" {
					identity += "_" + rollout
					extra["history_mode"] = "paginated"
					extra["history_base"] = map[string]any{"thread_id": thread, "end_ordinal_exclusive": 2}
				}
				timestamp := "2026-09-" + day + "T04:00:00Z"
				path := filepath.Join(root, "2026", "09", day, "rollout-2026-09-"+day+"T12-00-00-"+identity+".jsonl")
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				content := testjsonl.JoinJSONL(
					testjsonl.CodexSessionMetaWithFieldsJSON(thread, "/work/project", "codex_cli_rs", timestamp, extra),
					`{"type":"response_item","timestamp":"`+timestamp+`","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Page `+day+`"}]}}`,
				)
				require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
				paths = append(paths, path)
			}
			database := dbtest.OpenTestDB(t)
			engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{
				AgentDirs: map[parser.AgentType][]string{agent: {root}}, Machine: "local",
			})
			t.Cleanup(engine.Close)
			svc := service.NewDirectBackend(database, engine)
			for range 2 {
				detail, err := svc.Sync(t.Context(), service.SyncInput{Path: paths[1]})
				require.NoError(t, err)
				require.NotNil(t, detail)
				assert.Equal(t, string(agent)+":"+thread+"_"+rollout, detail.ID)
				require.NotNil(t, detail.ParentSessionID)
				assert.Equal(t, string(agent)+":"+thread, *detail.ParentSessionID)
				assert.Equal(t, paths[1], database.GetSessionFilePath(t.Context(), detail.ID))
				messages, err := database.GetAllMessages(t.Context(), detail.ID)
				require.NoError(t, err)
				require.Len(t, messages, 1)
				assert.Equal(t, "Page 25", messages[0].Content)
			}
		})
	}
}
