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

const musePersonalRemoteID = "11111111-2222-4333-8444-555555555555"

func musePersonalRemoteFixture(t *testing.T, root string) (string, string) {
	t.Helper()
	dir := filepath.Join(root, "agent-"+musePersonalRemoteID, "sessions")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	for _, name := range []string{musePersonalRemoteID + ".jsonl", "sessions.json"} {
		data, err := os.ReadFile(filepath.Join("..", "parser", "testdata", "muse_personal", "agent-"+musePersonalRemoteID, "sessions", name))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o600))
	}
	return filepath.Join(dir, musePersonalRemoteID+".jsonl"), filepath.Join(dir, "sessions.json")
}

func TestMusePersonalRemoteExactFilesAndImport(t *testing.T) {
	root := t.TempDir()
	transcript, index := musePersonalRemoteFixture(t, root)
	secret := filepath.Join(root, "auth-token")
	require.NoError(t, os.WriteFile(secret, []byte("synthetic-secret"), 0o600))
	targets, err := ResolveTargets(t.Context(), config.Config{AgentDirs: map[parser.AgentType][]string{parser.AgentMusePersonal: {root}}})
	require.NoError(t, err)
	manifest, err := BuildManifest(t.Context(), targets)
	require.NoError(t, err)
	var paths []string
	for _, file := range manifest.Files {
		paths = append(paths, file.Path)
	}
	assert.ElementsMatch(t, []string{transcript, index}, paths)
	_, ok := SelectAllowedFiles(targets, []string{secret})
	assert.False(t, ok)
	database := dbtest.OpenTestDB(t)
	extracted := t.TempDir()
	musePersonalRemoteFixture(t, filepath.Join(extracted, "remote", "agents"))
	stats, err := (Importer{Host: "test-host", DB: database}).ImportExtracted(t.Context(), TargetSet{Dirs: map[parser.AgentType][]string{parser.AgentMusePersonal: {"/remote/agents"}}}, extracted)
	require.NoError(t, err)
	assert.Zero(t, stats.Failed)
	page, err := database.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	session, err := database.GetSessionFull(t.Context(), page.Sessions[0].ID)
	require.NoError(t, err)
	require.NotNil(t, session.FilePath)
	assert.Equal(t, "test-host:/remote/agents/agent-"+musePersonalRemoteID+"/sessions/"+musePersonalRemoteID+".jsonl", *session.FilePath)
	messages, err := database.GetMessages(t.Context(), session.ID, 0, 100, true)
	require.NoError(t, err)
	require.Len(t, messages, 11)
	assert.Equal(t, "synthetic widget inventory: 3 widgets", messages[4].ToolCalls[0].ResultContent)
}

func TestMusePersonalRemoteDeletedCompanions(t *testing.T) {
	for _, deleted := range []string{"index", "transcript"} {
		t.Run(deleted, func(t *testing.T) {
			root := t.TempDir()
			transcript, index := musePersonalRemoteFixture(t, root)
			cfg := config.Config{AgentDirs: map[parser.AgentType][]string{parser.AgentMusePersonal: {root}}}
			requested, err := ResolveTargets(t.Context(), cfg)
			require.NoError(t, err)
			if deleted == "index" {
				require.NoError(t, os.Remove(index))
			} else {
				require.NoError(t, os.Remove(transcript))
			}
			fresh, err := ResolveTargets(t.Context(), cfg)
			require.NoError(t, err)
			delta, ok := SelectAllowedFiles(fresh, requested.Files[parser.AgentMusePersonal])
			require.True(t, ok)
			if deleted == "index" {
				assert.Equal(t, []string{transcript}, delta)
			} else {
				assert.Empty(t, delta)
			}
			_, ok = SelectAllowedFiles(fresh, []string{strings.TrimSuffix(transcript, ".jsonl") + ".api.jsonl"})
			assert.False(t, ok)
		})
	}
}
