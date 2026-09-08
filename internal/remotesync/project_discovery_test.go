package remotesync

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
)

func TestImporterUsesRecordedProjectWithoutLocalGitDiscovery(t *testing.T) {
	for _, tc := range []struct {
		agent    parser.AgentType
		filename string
		id       string
		body     string
	}{
		{
			agent: parser.AgentEvener, filename: "demo.transcript.jsonl", id: "evener:demo",
			body: `{"kind":"header","format_version":2,"session_id":"demo","created_at":"2026-09-01T10:00:00Z","working_dir":%s}
{"kind":"entry","seq":1,"turn":{"kind":"USER_INPUT","timestamp":"2026-09-01T10:01:00Z","message":{"content":[{"kind":"text","text":"Remote session content"}]}}}
`,
		},
		{
			agent: parser.AgentCodex, filename: "rollout-2026-09-01T10-00-00-11111111-2222-4333-8444-555555555555.jsonl", id: "codex:11111111-2222-4333-8444-555555555555",
			body: `{"type":"session_meta","payload":{"id":"11111111-2222-4333-8444-555555555555","cwd":%s,"timestamp":"2026-09-01T10:00:00Z"}}
{"type":"response_item","timestamp":"2026-09-01T10:01:00Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Remote session content"}]}}
`,
		},
	} {
		t.Run(string(tc.agent), func(t *testing.T) {
			database := dbtest.OpenTestDB(t)
			repo := filepath.Join(t.TempDir(), "local-repository")
			cwd := filepath.Join(repo, "recorded-project")
			// Plain directories exercise project discovery without invoking Git.
			require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o755))
			require.NoError(t, os.MkdirAll(cwd, 0o755))
			cwdJSON, err := json.Marshal(cwd)
			require.NoError(t, err)
			extracted := t.TempDir()
			const remoteDir = "/remote/sessions"
			sessions := remappedRemotePath(extracted, remoteDir)
			require.NoError(t, os.MkdirAll(sessions, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(sessions, tc.filename),
				[]byte(fmt.Sprintf(tc.body, cwdJSON)), 0o600))

			stats, err := (Importer{Host: "source-host", DB: database}).ImportExtracted(
				t.Context(), TargetSet{Dirs: map[parser.AgentType][]string{tc.agent: {remoteDir}}}, extracted,
			)
			require.NoError(t, err)
			require.Zero(t, stats.Failed)
			require.Equal(t, 1, stats.SessionsSynced)
			session, err := database.GetSessionFull(t.Context(), "source-host~"+tc.id)
			require.NoError(t, err)
			require.NotNil(t, session)
			assert.Equal(t, "recorded_project", session.Project)
			messages, err := database.GetMessages(t.Context(), session.ID, 0, 100, true)
			require.NoError(t, err)
			require.Len(t, messages, 1)
			assert.Equal(t, "Remote session content", messages[0].Content)
		})
	}
}
