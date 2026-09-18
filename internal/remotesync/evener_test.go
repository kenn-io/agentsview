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
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	sessions := filepath.Join(root, "projects", "demo", "sessions")
	require.NoError(os.MkdirAll(sessions, 0o755))
	for _, name := range []string{"demo.transcript.jsonl", "demo.meta.json"} {
		data, err := os.ReadFile(filepath.Join("..", "sync", "testdata", "evener", name))
		require.NoError(err)
		require.NoError(os.WriteFile(filepath.Join(sessions, name), data, 0o600))
	}
	secret := filepath.Join(root, "auth-token")
	log := filepath.Join(sessions, "demo.api.jsonl")
	require.NoError(os.WriteFile(secret, []byte("synthetic-secret"), 0o600))
	require.NoError(os.WriteFile(log, []byte("synthetic-operation-log"), 0o600))
	targets, err := ResolveTargets(config.Config{AgentDirs: map[parser.AgentType][]string{parser.AgentType("evener"): {root}}})
	require.NoError(err)
	manifest, err := BuildManifest(targets)
	require.NoError(err)
	var files []string
	for _, f := range manifest.Files {
		files = append(files, f.Path)
	}
	assert.ElementsMatch([]string{filepath.Join(sessions, "demo.transcript.jsonl"), filepath.Join(sessions, "demo.meta.json")}, files)
	_, allowed := SelectAllowedFiles(targets, []string{secret, log})
	assert.False(allowed)

	database := dbtest.OpenTestDB(t)
	// Repeat extraction into different directories to exercise canonical identity.
	for _, title := range []string{"Orchard investigation", "Remote diagnosis"} {
		extracted := t.TempDir()
		local := filepath.Join(extracted, "remote", "evener", "projects", "demo", "sessions")
		require.NoError(os.MkdirAll(local, 0o755))
		data, err := os.ReadFile(filepath.Join(sessions, "demo.transcript.jsonl"))
		require.NoError(err)
		require.NoError(os.WriteFile(filepath.Join(local, "demo.transcript.jsonl"), data, 0o600))
		require.NoError(os.WriteFile(filepath.Join(local, "demo.meta.json"), []byte(`{"id":"demo","name":"`+title+`"}`), 0o600))
		stats, err := (Importer{Host: "test-host", DB: database}).ImportExtracted(t.Context(), TargetSet{Dirs: map[parser.AgentType][]string{parser.AgentType("evener"): {"/remote/evener"}}}, extracted)
		require.NoError(err)
		require.Zero(stats.Failed)
		page, err := database.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
		require.NoError(err)
		require.Len(page.Sessions, 1)
		session, err := database.GetSessionFull(t.Context(), page.Sessions[0].ID)
		require.NoError(err)
		require.NotNil(session)
		require.NotNil(session.FilePath)
		assert.Equal("test-host:/remote/evener/projects/demo/sessions/demo.transcript.jsonl", *session.FilePath)
		require.NotNil(session.SessionName)
		assert.Equal(title, *session.SessionName)
		messages, err := database.GetMessages(t.Context(), session.ID, 0, 100, true)
		require.NoError(err)
		require.Len(messages, 3)
		assert.Equal(20, messages[1].OutputTokens)
	}
}

func TestEvenerRemoteSyncToleratesDeletedCompanions(t *testing.T) {
	for _, layout := range []string{"state", "project", "sessions"} {
		for _, deleted := range []string{"demo.meta.json", "demo.transcript.jsonl"} {
			t.Run(layout+"/"+deleted, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)

				root := t.TempDir()
				sessions := filepath.Join(root, "projects", "demo", "sessions")
				require.NoError(os.MkdirAll(sessions, 0o755))
				switch layout {
				case "project":
					root = filepath.Dir(sessions)
				case "sessions":
					root = sessions
				}
				transcript := filepath.Join(sessions, "demo.transcript.jsonl")
				metadata := filepath.Join(sessions, "demo.meta.json")
				for _, file := range []string{transcript, metadata} {
					require.NoError(os.WriteFile(file, []byte("{}\n"), 0o600))
				}
				cfg := config.Config{AgentDirs: map[parser.AgentType][]string{parser.AgentEvener: {root}}}
				requested, err := ResolveTargets(cfg)
				require.NoError(err)
				require.ElementsMatch([]string{transcript, metadata}, requested.Files[parser.AgentEvener])
				require.NoError(os.Remove(filepath.Join(sessions, deleted)))
				fresh, err := ResolveTargets(cfg)
				require.NoError(err)
				selected, ok := SelectAllowedTargets(fresh, requested)
				require.True(ok, "deleting a companion must not reject the sync request")
				manifest, err := BuildManifest(selected)
				require.NoError(err)
				var paths []string
				for _, file := range manifest.Files {
					paths = append(paths, file.Path)
				}
				var expected []string
				if deleted == "demo.meta.json" {
					expected = []string{transcript}
				}
				assert.ElementsMatch(expected, paths, "the manifest must omit deleted sources and orphan metadata")
				delta, ok := SelectAllowedFiles(fresh, requested.Files[parser.AgentEvener])
				require.True(ok)
				assert.ElementsMatch(expected, delta)
				_, ok = SelectAllowedFiles(fresh, []string{filepath.Join(sessions, "demo.api.jsonl")})
				assert.False(ok, "unrelated file shapes must remain rejected")
			})
		}
	}
}

func TestEvenerEmptyRemoteRootDoesNotExportUnrelatedState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	secret := filepath.Join(root, "auth-token")
	require.NoError(os.WriteFile(secret, []byte("synthetic-secret"), 0o600))
	targets, err := ResolveTargets(config.Config{AgentDirs: map[parser.AgentType][]string{parser.AgentType("evener"): {root}}})
	require.NoError(err)
	manifest, err := BuildManifest(targets)
	require.NoError(err)
	assert.Empty(manifest.Files)
	_, allowed := SelectAllowedFiles(targets, []string{secret})
	assert.False(allowed)
}

func TestEvenerRemoteDeltaRefreshesForkWhenParentArrives(t *testing.T) {
	parentRequire := require.New(t)

	database := dbtest.OpenTestDB(t)
	root := t.TempDir()
	remoteDir := "/remote/evener"
	local := filepath.Join(remappedRemotePath(root, remoteDir), "sessions")
	parentRequire.NoError(os.MkdirAll(local, 0700))
	parent, err := os.ReadFile(filepath.Join("..", "sync", "testdata", "evener", "demo.transcript.jsonl"))
	parentRequire.NoError(err)
	child := strings.Replace(string(parent), `"session_id":"demo"`, `"session_id":"fork","parent_session_id":"demo"`, 1)
	child += "{\"kind\":\"entry\",\"seq\":4,\"turn\":{\"kind\":\"USER_INPUT\",\"message\":{\"content\":[{\"kind\":\"text\",\"text\":\"child-only\"}]}}}\n"
	parentRequire.NoError(os.WriteFile(filepath.Join(local, "fork.transcript.jsonl"), []byte(child), 0600))
	parentRequire.NoError(os.WriteFile(filepath.Join(local, "fork.meta.json"), []byte(`{"id":"fork","parent_session_id":"demo","divergence_turn":4}`), 0600))
	targets := TargetSet{Dirs: map[parser.AgentType][]string{parser.AgentEvener: {remoteDir}}}
	importer := Importer{Host: "remote", DB: database, Root: root, Targets: targets}
	stats, err := importer.ImportExtracted(t.Context(), targets, root)
	parentRequire.NoError(err)
	parentRequire.Zero(stats.Failed)
	messages, err := database.GetMessages(t.Context(), "remote~evener:fork", 0, 100, true)
	parentRequire.NoError(err)
	parentRequire.Len(messages, 4)
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
			assert := assert.New(t)
			require := require.New(t)

			changedPath, content := parentPath, tc.content
			if tc.metadata != "" {
				changedPath, content = filepath.Join(local, "demo.meta.json"), tc.metadata
			}
			require.NoError(os.WriteFile(changedPath, []byte(content), 0600))
			relative, err := mirrorRelativeLocalChangePath(root, changedPath)
			require.NoError(err)
			pending, err := importer.PreparePending(t.Context(), DeltaImportRequest{Journal: MirrorChangeJournal{Version: mirrorJournalVersion, Entries: []MirrorChangeEntry{{Path: relative}}}})
			require.NoError(err)
			stats, err := pending.Execute(t.Context())
			if tc.wantFailure {
				require.Error(err)
				assert.Positive(stats.Failed)
			} else {
				require.NoError(err)
				require.Zero(stats.Failed)
			}
			messages, err := database.GetMessages(t.Context(), "remote~evener:fork", 0, 100, true)
			require.NoError(err)
			require.Len(messages, tc.wantCount)
			output := 0
			for _, message := range messages {
				output += message.OutputTokens
			}
			assert.Equal(tc.wantOutput, output)
		})
	}
}
