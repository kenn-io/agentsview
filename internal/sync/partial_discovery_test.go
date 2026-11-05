package sync

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestPartialDiscoveryImportsHealthyLocalHistory(t *testing.T) {
	const id = "22222222-2222-4222-8222-222222222222"
	const stamp = "2026-01-02T00:00:00Z"
	for _, agent := range []parser.AgentType{parser.AgentClaude, parser.AgentCodex, parser.AgentCursor, parser.AgentIcodemate} {
		t.Run(string(agent), func(t *testing.T) {
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, "<Error><Code>AccessDenied</Code></Error>")
			}))
			t.Cleanup(remote.Close)
			t.Setenv("AWS_S3_ENDPOINT", remote.URL)
			t.Setenv("AWS_ACCESS_KEY_ID", "test-key")
			t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
			t.Setenv("AWS_REGION", "us-east-1")
			local := t.TempDir()
			relative := "project/" + id + ".jsonl"
			content := testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID(stamp, "healthy local history", id).String()
			storedID := id
			if agent == parser.AgentIcodemate {
				storedID = "icodemate:" + id
			}
			if agent == parser.AgentCodex {
				storedID = "codex:" + id
				relative = "rollout-2026-01-02T00-00-00-" + id + ".jsonl"
				content = testjsonl.CodexSessionMetaJSON(id, "/demo/project", "codex_cli_rs", stamp) + "\n" + testjsonl.CodexMsgJSON("user", "healthy local history", stamp) + "\n"
			}
			if agent == parser.AgentCursor {
				relative = "project/agent-transcripts/" + id + ".jsonl"
				content = "{\"role\":\"user\",\"message\":{\"content\":\"healthy local history\"}}\n"
				storedID = "cursor:" + id
			}
			dbtest.WriteTestFile(t, filepath.Join(local, filepath.FromSlash(relative)), []byte(content))
			database := dbtest.OpenTestDB(t)
			engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{agent: {"s3://bucket/sessions", local}}, Machine: "test-machine"})
			t.Cleanup(engine.Close)
			stats := engine.SyncAll(t.Context(), nil)
			assert.False(t, stats.ProcessingComplete())
			messages, err := database.GetAllMessages(t.Context(), storedID)
			require.NoError(t, err)
			require.Len(t, messages, 1)
			assert.Equal(t, "healthy local history", messages[0].Content)
		})
	}
}
