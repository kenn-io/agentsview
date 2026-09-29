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

func TestProviderToolSequenceEvidence(t *testing.T) {
	for _, tt := range []struct {
		name  string
		agent parser.AgentType
		body  string
		want  signals.ToolOutcome
	}{
		{"amp_empty_string", parser.AgentAmp, `""`, signals.ToolOutcomeEmpty},
		{"amp_empty_array", parser.AgentAmp, `[]`, signals.ToolOutcomeEmpty},
		{"amp_text_control", parser.AgentAmp, `"file contents"`, signals.ToolOutcomeContent},
		{"cline_image", parser.AgentCline, `[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAEC"}}]`, signals.ToolOutcomeUnknown},
		{"cline_image_object", parser.AgentCline, `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAEC"}}`, signals.ToolOutcomeUnknown},
		{"cline_image_and_text", parser.AgentCline, `[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAEC"}},{"type":"text","text":"file contents"}]`, signals.ToolOutcomeContent},
		{"cline_empty_control", parser.AgentCline, `""`, signals.ToolOutcomeEmpty},
		{"cline_text_control", parser.AgentCline, `[{"type":"text","text":"file contents"}]`, signals.ToolOutcomeContent},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if tt.agent == parser.AgentAmp {
				raw := fmt.Sprintf(`{"id":"T-019ca26f-aaaa-bbbb-cccc-dddddddddddd","created":1704067200000,"messages":[{"role":"user","content":[{"type":"text","text":"Read the file"}]},{"role":"assistant","content":[{"type":"tool_use","id":"call-1","name":"Read","input":{"path":"file.png"}}]},{"role":"user","content":[{"type":"tool_result","toolUseID":"call-1","run":{"status":"done","result":%s}}]}]}`, tt.body)
				require.NoError(t, os.WriteFile(filepath.Join(root, "T-019ca26f-aaaa-bbbb-cccc-dddddddddddd.json"), []byte(raw), 0o600))
			} else {
				id := "1789000000088_probe"
				dir := filepath.Join(root, "data", "sessions", id)
				require.NoError(t, os.MkdirAll(dir, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(dir, id+".json"), []byte(`{"session_id":"1789000000088_probe","started_at":"2026-09-10T10:00:00Z","status":"completed"}`), 0o600))
				raw := fmt.Sprintf(`{"messages":[{"role":"user","ts":1789000000000,"content":[{"type":"text","text":"Read the file"}]},{"role":"assistant","ts":1789000001000,"content":[{"type":"tool_use","id":"call-1","name":"read_files","input":{"paths":["file.png"]}}]},{"role":"user","ts":1789000002000,"content":[{"type":"tool_result","tool_use_id":"call-1","content":%s}]}]}`, tt.body)
				require.NoError(t, os.WriteFile(filepath.Join(dir, id+".messages.json"), []byte(raw), 0o600))
			}
			provider, ok := parser.NewProvider(tt.agent, parser.ProviderConfig{Roots: []string{root}, Machine: "local"})
			require.True(t, ok)
			sources, err := provider.Discover(t.Context())
			require.NoError(t, err)
			require.Len(t, sources, 1)
			parsed, err := provider.Parse(t.Context(), parser.ParseRequest{Source: sources[0]})
			require.NoError(t, err)
			require.Len(t, parsed.Results, 1)
			candidate, err := ingest.PrepareCandidate(t.Context(), parsed.Results[0].Result, ingest.ContentOptions{})
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
			got := signals.ExtractToolSequences(rows, true)
			assert.Equal(t, "completed", rows[0].EventStatus)
			assert.Equal(t, tt.want, got.Calls[0].Outcome)
		})
	}
}
