package rawarchive

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestReparseAppliesReceivingResultRetention(t *testing.T) {
	const resultBody = "contents of a file the archive must not keep"
	for _, tt := range []struct {
		name    string
		blocked []string
		want    string
	}{
		{name: "blocked", blocked: []string{"Read"}, want: ""},
		{name: "retained", want: resultBody},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			const owner = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			const foreign = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			const id = "019eb791-cf7d-75c1-8439-9ed74c122e02"
			data := t.TempDir()
			dbtest.WriteTestFile(t, filepath.Join(data, "telemetry-install-id"), []byte(owner))
			database := dbtest.OpenTestDB(t)
			require.NoError(t, database.EnableArchiveOnly(ctx))
			root := t.TempDir()
			dbtest.WriteTestFile(t, filepath.Join(root, "project", id+".jsonl"), []byte(testjsonl.NewSessionBuilder().
				AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "read the file", id).
				AddRaw(testjsonl.ClaudeAssistantJSON([]map[string]any{{
					"type": "tool_use", "id": "toolu_read", "name": "Read",
					"input": map[string]any{"file_path": "/workspace/project-a/notes.txt"},
				}}, "2026-01-01T00:00:01Z")).
				AddRaw(testjsonl.ClaudeToolResultUserJSON("toolu_read", resultBody, "2026-01-01T00:00:02Z")).
				String()))
			archive, err := Open(ctx, database, data, nil)
			require.NoError(t, err)
			defer archive.Close()
			capture := newImportCapture(t, foreign, "foreign", RootSpec{Provider: "claude", Path: root})
			report, err := archive.Import(ctx, loadTestCapture(t, &capture))
			require.NoError(t, err)
			require.Empty(t, report.Gaps)
			require.NoError(t, os.RemoveAll(root))

			_, err = archive.Reparse(ctx, ReparseOptions{All: true, ScratchBytes: 1 << 20, BlockedResultCategories: tt.blocked})
			require.NoError(t, err)
			messages, err := database.GetAllMessages(ctx, foreign+"~"+id)
			require.NoError(t, err)
			var results []string
			for _, message := range messages {
				for _, call := range message.ToolCalls {
					if call.ToolUseID == "toolu_read" {
						results = append(results, call.ResultContent)
					}
				}
			}
			require.Len(t, results, 1, "the reparsed session keeps its Read call")
			assert.Equal(t, tt.want, results[0])
		})
	}
}
