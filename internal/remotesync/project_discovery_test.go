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
		{
			agent: parser.AgentCommandCode, filename: "project/demo.jsonl", id: "commandcode:demo",
			body: `{"id":"m1","timestamp":"2026-09-01T10:00:00Z","sessionId":"demo","role":"user","content":[{"type":"text","text":"Remote session content"}],"metadata":{"cwd":%s}}
`,
		},
		{
			agent:    parser.AgentOpenHands,
			filename: "086c7ecf6cb746b69fbcb900358d1247/events/event-00000.json",
			id:       "openhands:086c7ecf-6cb7-46b6-9fbc-b900358d1247",
			body: `{"id":"e0","timestamp":"2026-09-01T10:00:00Z","source":"environment",
"observation":{"content":[{"type":"text","text":"Remote session content"}],
"metadata":{"working_dir":%s},"kind":"TerminalObservation"},"kind":"ObservationEvent"}`,
		},
	} {
		t.Run(string(tc.agent), func(t *testing.T) {
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
			require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(sessions, tc.filename)), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(sessions, tc.filename),
				[]byte(fmt.Sprintf(tc.body, cwdJSON)), 0o600))

			for _, mode := range []string{"full", "delta"} {
				t.Run(mode, func(t *testing.T) {
					database := dbtest.OpenTestDB(t)
					importer := Importer{
						Host: "source-host", DB: database, Root: extracted,
						Targets: TargetSet{Dirs: map[parser.AgentType][]string{tc.agent: {remoteDir}}},
					}
					var stats SyncStats
					var err error
					if mode == "full" {
						stats, err = importer.ImportExtracted(t.Context(), importer.Targets, extracted)
					} else {
						journalPath, pathErr := mirrorRelativeLocalChangePath(extracted, filepath.Join(sessions, tc.filename))
						require.NoError(t, pathErr)
						pending, prepareErr := importer.PreparePending(t.Context(), DeltaImportRequest{
							Journal: MirrorChangeJournal{
								Version: mirrorJournalVersion,
								Entries: []MirrorChangeEntry{{Path: journalPath}},
							},
						})
						require.NoError(t, prepareErr)
						stats, err = pending.Execute(t.Context())
					}

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
		})
	}
}
