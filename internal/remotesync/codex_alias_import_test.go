package remotesync

import (
	"bytes"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestRemoteCodexAliasTitleSurvivesArchiveImport(t *testing.T) {
	base := t.TempDir()
	primary := filepath.Join(base, "primary", "sessions")
	alias := filepath.Join(base, "alternate", "sessions")
	require.NoError(t, os.MkdirAll(primary, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(alias), 0o755))
	require.NoError(t, os.Symlink(primary, alias))
	const id = "019f0000-0000-7000-8000-000000000009"
	transcript := filepath.Join(primary, "rollout-2026-09-03T10-00-00-"+id+".jsonl")
	require.NoError(t, os.WriteFile(transcript, []byte(
		`{"timestamp":"2026-09-03T10:00:00Z","type":"session_meta","payload":{"id":"`+id+`","cwd":"/work"}}`+"\n"+
			`{"timestamp":"2026-09-03T10:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Original prompt"}]}}`+"\n",
	), 0o600))
	index := filepath.Join(filepath.Dir(alias), parser.CodexSessionIndexFilename)
	require.NoError(t, os.WriteFile(index, []byte(
		`{"id":"`+id+`","thread_name":"Renamed in alternate home"}`+"\n",
	), 0o600))
	targets, err := ResolveTargets(config.Config{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {primary}},
		RootAliases: map[parser.AgentType]map[string][]string{
			parser.AgentCodex: {primary: {alias}},
		},
	})
	require.NoError(t, err)
	// Exercise the same request and target split used by remote HTTP sync.
	requestJSON, err := json.Marshal(ArchiveRequest{TargetSet: targets})
	require.NoError(t, err)
	var request ArchiveRequest
	require.NoError(t, json.Unmarshal(requestJSON, &request))
	targets = request.TargetSet
	manifest, err := BuildManifest(targets)
	require.NoError(t, err)
	var paths []string
	for _, entry := range manifest.Files {
		paths = append(paths, entry.Path)
	}
	assert.ElementsMatch(t, []string{transcript, index}, paths)

	dirScoped, _ := targets.SplitFileScoped()
	selected, ok := SelectAllowedTargets(targets, dirScoped)
	require.True(t, ok)
	var archive bytes.Buffer
	require.NoError(t, WriteArchive(&archive, selected))
	extracted := t.TempDir()
	_, err = ExtractTarStream(t.Context(), &archive, extracted)
	require.NoError(t, err)
	database, err := db.Open(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	t.Cleanup(func() { parser.SetCodexRootAliases(nil) })
	localRoot := filepath.Join(base, "local", "sessions")
	parser.SetCodexRootAliases(map[string][]string{localRoot: {alias}})
	stats, err := (Importer{Host: "remote", DB: database}).ImportExtracted(t.Context(), selected, extracted)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.SessionsSynced)
	session, err := database.GetSessionFull(t.Context(), "remote~codex:"+id)
	require.NoError(t, err)
	require.NotNil(t, session)
	require.NotNil(t, session.SessionName)
	assert.Equal(t, "Renamed in alternate home", *session.SessionName)
	assert.Equal(t, "Renamed in alternate home", parser.LookupCodexThreadName(
		filepath.Join(localRoot, filepath.Base(transcript)), id,
	), "remote import must preserve the daemon's local title sources")
	assert.Empty(t, parser.LookupCodexThreadName(
		remappedRemotePath(extracted, transcript), id,
	), "temporary title associations must be released when import closes")
}
