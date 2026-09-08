package remotesync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
)

func TestEvenerRemoteFilesAndImport(t *testing.T) {
	root := t.TempDir()
	sessions := filepath.Join(root, "projects", "demo", "sessions")
	require.NoError(t, os.MkdirAll(sessions, 0o755))
	for _, name := range []string{"demo.transcript.jsonl", "demo.meta.json"} {
		data, err := os.ReadFile(filepath.Join("..", "sync", "testdata", "evener", name))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(sessions, name), data, 0o600))
	}
	secret := filepath.Join(root, "auth-token")
	log := filepath.Join(sessions, "demo.api.jsonl")
	require.NoError(t, os.WriteFile(secret, []byte("synthetic-secret"), 0o600))
	require.NoError(t, os.WriteFile(log, []byte("synthetic-operation-log"), 0o600))
	targets, err := ResolveTargets(config.Config{AgentDirs: map[parser.AgentType][]string{parser.AgentType("evener"): {root}}})
	require.NoError(t, err)
	manifest, err := BuildManifest(targets)
	require.NoError(t, err)
	var files []string
	for _, f := range manifest.Files {
		files = append(files, f.Path)
	}
	assert.ElementsMatch(t, []string{filepath.Join(sessions, "demo.transcript.jsonl"), filepath.Join(sessions, "demo.meta.json")}, files)
	_, allowed := SelectAllowedFiles(targets, []string{secret, log})
	assert.False(t, allowed)

	database := dbtest.OpenTestDB(t)
	// Repeat extraction into different directories to exercise canonical identity.
	for _, title := range []string{"Orchard investigation", "Remote diagnosis"} {
		extracted := t.TempDir()
		local := filepath.Join(extracted, "remote", "evener", "projects", "demo", "sessions")
		require.NoError(t, os.MkdirAll(local, 0o755))
		data, err := os.ReadFile(filepath.Join(sessions, "demo.transcript.jsonl"))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(local, "demo.transcript.jsonl"), data, 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(local, "demo.meta.json"), []byte(`{"id":"demo","name":"`+title+`"}`), 0o600))
		stats, err := (Importer{Host: "test-host", DB: database}).ImportExtracted(t.Context(), TargetSet{Dirs: map[parser.AgentType][]string{parser.AgentType("evener"): {"/remote/evener"}}}, extracted)
		require.NoError(t, err)
		require.Zero(t, stats.Failed)
		page, err := database.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
		require.NoError(t, err)
		require.Len(t, page.Sessions, 1)
		session, err := database.GetSessionFull(t.Context(), page.Sessions[0].ID)
		require.NoError(t, err)
		require.NotNil(t, session)
		require.NotNil(t, session.FilePath)
		assert.Equal(t, "test-host:/remote/evener/projects/demo/sessions/demo.transcript.jsonl", *session.FilePath)
		require.NotNil(t, session.SessionName)
		assert.Equal(t, title, *session.SessionName)
		messages, err := database.GetMessages(t.Context(), session.ID, 0, 100, true)
		require.NoError(t, err)
		require.Len(t, messages, 3)
		assert.Equal(t, 20, messages[1].OutputTokens)
	}
}

func TestEvenerEmptyRemoteRootDoesNotExportUnrelatedState(t *testing.T) {
	root := t.TempDir()
	secret := filepath.Join(root, "auth-token")
	require.NoError(t, os.WriteFile(secret, []byte("synthetic-secret"), 0o600))
	targets, err := ResolveTargets(config.Config{AgentDirs: map[parser.AgentType][]string{parser.AgentType("evener"): {root}}})
	require.NoError(t, err)
	manifest, err := BuildManifest(targets)
	require.NoError(t, err)
	assert.Empty(t, manifest.Files)
	_, allowed := SelectAllowedFiles(targets, []string{secret})
	assert.False(t, allowed)
}

func TestEvenerRemoteDeltaRefreshesForkWhenParentArrives(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	root := t.TempDir()
	remoteDir := "/remote/evener"
	local := filepath.Join(remappedRemotePath(root, remoteDir), "sessions")
	require.NoError(t, os.MkdirAll(local, 0700))
	parent, err := os.ReadFile(filepath.Join("..", "sync", "testdata", "evener", "demo.transcript.jsonl"))
	require.NoError(t, err)
	child := strings.Replace(string(parent), `"session_id":"demo"`, `"session_id":"fork","parent_session_id":"demo"`, 1)
	child += "{\"kind\":\"entry\",\"seq\":4,\"turn\":{\"kind\":\"USER_INPUT\",\"message\":{\"content\":[{\"kind\":\"text\",\"text\":\"child-only\"}]}}}\n"
	require.NoError(t, os.WriteFile(filepath.Join(local, "fork.transcript.jsonl"), []byte(child), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(local, "fork.meta.json"), []byte(`{"id":"fork","parent_session_id":"demo","divergence_turn":4}`), 0600))
	targets := TargetSet{Dirs: map[parser.AgentType][]string{parser.AgentEvener: {remoteDir}}}
	importer := Importer{Host: "remote", DB: database, Root: root, Targets: targets}
	stats, err := importer.ImportExtracted(t.Context(), targets, root)
	require.NoError(t, err)
	require.Zero(t, stats.Failed)
	messages, err := database.GetMessages(t.Context(), "remote~evener:fork", 0, 100, true)
	require.NoError(t, err)
	require.Len(t, messages, 4)
	parentPath := filepath.Join(local, "demo.transcript.jsonl")
	for _, tc := range []struct {
		name, content, metadata string
		wantCount, wantOutput   int
		wantFailure             bool
	}{
		{"arrival", string(parent), "", 1, 0, false},
		{"invalid parent metadata", "", `{"id":"wrong"}`, 4, 20, true},
		{"repaired parent metadata", "", `{"id":"demo"}`, 1, 0, false},
		{"rewritten prefix", strings.Replace(string(parent), "Investigate the orchard synchronization bug", "Inspect another workspace.", 1), "", 4, 20, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changedPath, content := parentPath, tc.content
			if tc.metadata != "" {
				changedPath, content = filepath.Join(local, "demo.meta.json"), tc.metadata
			}
			require.NoError(t, os.WriteFile(changedPath, []byte(content), 0600))
			relative, err := mirrorRelativeLocalChangePath(root, changedPath)
			require.NoError(t, err)
			pending, err := importer.PreparePending(t.Context(), DeltaImportRequest{Journal: MirrorChangeJournal{Version: mirrorJournalVersion, Entries: []MirrorChangeEntry{{Path: relative}}}})
			require.NoError(t, err)
			stats, err := pending.Execute(t.Context())
			if tc.wantFailure {
				require.Error(t, err)
				assert.Positive(t, stats.Failed)
			} else {
				require.NoError(t, err)
				require.Zero(t, stats.Failed)
			}
			messages, err := database.GetMessages(t.Context(), "remote~evener:fork", 0, 100, true)
			require.NoError(t, err)
			require.Len(t, messages, tc.wantCount)
			output := 0
			for _, message := range messages {
				output += message.OutputTokens
			}
			assert.Equal(t, tc.wantOutput, output)
		})
	}
}
