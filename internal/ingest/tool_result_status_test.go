package ingest_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/signals"
)

func TestProviderToolResultFailureMetadata(t *testing.T) {
	for _, tt := range []struct {
		name, result, content, status string
		agent                         parser.AgentType
	}{
		{"claude_error", `{"type":"tool_result","tool_use_id":"call-1","content":"File does not exist.","is_error":true}`, "File does not exist.", "errored", parser.AgentClaude},
		{"claude_unknown", `{"type":"tool_result","tool_use_id":"call-1","content":"file contents"}`, "file contents", "", parser.AgentClaude},
		{"amp_error", `{"type":"tool_result","toolUseID":"call-1","run":{"status":"error","error":{"message":"File does not exist."}}}`, "File does not exist.", "errored", parser.AgentAmp},
		{"amp_unsuccessful", `{"type":"tool_result","toolUseID":"call-1","run":{"status":"done","result":{"success":false}}}`, "failed", "errored", parser.AgentAmp},
		{"amp_unknown", `{"type":"tool_result","toolUseID":"call-1","run":{"result":"file contents"}}`, "file contents", "", parser.AgentAmp},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			provider, ok := parser.NewProvider(tt.agent, parser.ProviderConfig{Roots: []string{root}, Machine: "local"})
			require.True(t, ok)
			var parsed parser.ParseResult
			if tt.agent == parser.AgentClaude {
				raw := fmt.Sprintf(`{"type":"user","uuid":"u1","message":{"content":"Read the file"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","message":{"content":[{"type":"tool_use","id":"call-1","name":"Read","input":{"file_path":"missing.go"}}]}}
{"type":"user","uuid":"r1","parentUuid":"a1","message":{"content":[%s]}}
`, tt.result)
				path := filepath.Join(root, "session.jsonl")
				require.NoError(t, os.WriteFile(path, []byte(raw), 0o600))
				uploader, ok := provider.(parser.ClaudeUploadParser)
				require.True(t, ok)
				results, err := uploader.ParseUploadedTranscript(path, "project-a", "local")
				require.NoError(t, err)
				require.Len(t, results, 1)
				parsed = results[0]
			} else {
				raw := fmt.Sprintf(`{"id":"T-019ca26f-aaaa-bbbb-cccc-dddddddddddd","created":1704067200000,"messages":[{"role":"user","content":[{"type":"text","text":"Read the file"}]},{"role":"assistant","content":[{"type":"tool_use","id":"call-1","name":"Read","input":{"path":"missing.go"}}]},{"role":"user","content":[%s]}]}`, tt.result)
				require.NoError(t, os.WriteFile(filepath.Join(root, "T-019ca26f-aaaa-bbbb-cccc-dddddddddddd.json"), []byte(raw), 0o600))
				sources, err := provider.Discover(t.Context())
				require.NoError(t, err)
				require.Len(t, sources, 1)
				outcome, err := provider.Parse(t.Context(), parser.ParseRequest{Source: sources[0]})
				require.NoError(t, err)
				require.Len(t, outcome.Results, 1)
				parsed = outcome.Results[0].Result
			}
			candidate, err := ingest.PrepareCandidate(t.Context(), parsed, ingest.ContentOptions{})
			require.NoError(t, err)
			prepared, err := ingest.Finalize(t.Context(), candidate, ingest.ContentOptions{})
			require.NoError(t, err)
			database := dbtest.OpenTestDB(t)
			written, err := database.WriteSessionBatch([]db.SessionBatchWrite{{Session: prepared.Session, Messages: prepared.Messages}})
			require.NoError(t, err)
			require.Empty(t, written.Errors)
			stored, err := database.GetMessages(t.Context(), prepared.Session.ID, 0, 100, true)
			require.NoError(t, err)
			rows := ingest.ExtractToolCallRows(stored)
			require.Len(t, rows, 1)
			assert.Equal(t, tt.content, rows[0].ResultContent)
			assert.Equal(t, tt.status, rows[0].EventStatus)
			assert.Equal(t, tt.status == "errored", signals.ComputeToolHealth(rows).FailureSignalCount == 1)
			for _, message := range stored {
				for _, call := range message.ToolCalls {
					require.Len(t, call.ResultEvents, 1)
					assert.Equal(t, tt.content, call.ResultEvents[0].Content)
				}
			}
		})
	}
}
