package remotesync

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestRemoteCodexAliasTitleSurvivesArchiveImport(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	base := t.TempDir()
	primary := filepath.Join(base, "primary", "sessions")
	alias := filepath.Join(base, "alternate", "sessions")
	require.NoError(os.MkdirAll(primary, 0o755))
	require.NoError(os.MkdirAll(filepath.Dir(alias), 0o755))
	require.NoError(os.Symlink(primary, alias))
	const id = "019f0000-0000-7000-8000-000000000009"
	transcript := filepath.Join(primary, "rollout-2026-09-03T10-00-00-"+id+".jsonl")
	require.NoError(os.WriteFile(transcript, []byte(
		`{"timestamp":"2026-09-03T10:00:00Z","type":"session_meta","payload":{"id":"`+id+`","cwd":"/work"}}`+"\n"+
			`{"timestamp":"2026-09-03T10:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Original prompt"}]}}`+"\n",
	), 0o600))
	index := filepath.Join(filepath.Dir(alias), parser.CodexSessionIndexFilename)
	require.NoError(os.WriteFile(index, []byte(
		`{"id":"`+id+`","thread_name":"Renamed in alternate home"}`+"\n",
	), 0o600))
	// The physical parent's index belongs only to a separately configured
	// archive root. It must not override the sessions root's explicit home.
	archiveRoot := filepath.Join(filepath.Dir(primary), "archived_sessions")
	require.NoError(os.MkdirAll(archiveRoot, 0o755))
	otherIndex := filepath.Join(filepath.Dir(primary), parser.CodexSessionIndexFilename)
	require.NoError(os.WriteFile(otherIndex, []byte(
		`{"id":"`+id+`","thread_name":"Unassociated title"}`+"\n",
	), 0o600))
	require.NoError(os.Chtimes(otherIndex, time.Now().Add(time.Hour), time.Now().Add(time.Hour)))

	targets, err := ResolveTargets(config.Config{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {primary, archiveRoot}},
		ProviderMetadata: map[parser.AgentType]map[string][]string{
			parser.AgentCodex: {primary: {filepath.Dir(alias)}, archiveRoot: {filepath.Dir(primary)}},
		},
	})
	require.NoError(err)
	// Exercise the same request and target split used by remote HTTP sync.
	requestJSON, err := json.Marshal(ArchiveRequest{TargetSet: targets})
	require.NoError(err)
	var request ArchiveRequest
	require.NoError(json.Unmarshal(requestJSON, &request))
	targets = request.TargetSet
	manifest, err := BuildManifest(targets)
	require.NoError(err)
	var paths []string
	for _, entry := range manifest.Files {
		paths = append(paths, entry.Path)
	}
	assert.ElementsMatch([]string{transcript, index, otherIndex}, paths)

	dirScoped, _ := targets.SplitFileScoped()
	selected, ok := SelectAllowedTargets(targets, dirScoped)
	require.True(ok)
	var archive bytes.Buffer
	require.NoError(WriteArchive(t.Context(), &archive, selected))
	extracted := t.TempDir()
	_, err = ExtractTarStream(t.Context(), &archive, extracted)
	require.NoError(err)
	database, err := db.Open(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(database.Close()) })
	localProvider, ok := parser.NewProvider(parser.AgentCodex, parser.ProviderConfig{
		Roots: []string{primary}, MetadataDirs: map[string][]string{primary: {filepath.Dir(alias)}},
	})
	require.True(ok)
	localMetadata := localProvider.(interface{ Metadata() parser.CodexMetadata }).Metadata()
	stats, err := (Importer{Host: "remote", DB: database}).ImportExtracted(t.Context(), selected, extracted)
	require.NoError(err)
	assert.Equal(1, stats.SessionsSynced)
	session, err := database.GetSessionFull(t.Context(), "remote~codex:"+id)
	require.NoError(err)
	require.NotNil(session)
	require.NotNil(session.SessionName)
	assert.Equal("Renamed in alternate home", *session.SessionName)
	name, present, err := localMetadata.ReadThreadName(transcript, id)
	require.NoError(err)
	assert.True(present)
	assert.Equal("Renamed in alternate home", name, "import cannot replace another provider's configuration")
	// Even an empty association must not revive the physical parent's index.
	require.NoError(os.Remove(index))
	emptyTargets, err := ResolveTargets(config.Config{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {primary, archiveRoot}},
		ProviderMetadata: map[parser.AgentType]map[string][]string{
			parser.AgentCodex: {primary: {filepath.Dir(alias)}, archiveRoot: {filepath.Dir(primary)}},
		},
	})
	require.NoError(err)
	_, err = (Importer{Host: "empty", DB: database}).ImportExtracted(t.Context(), emptyTargets, extracted)
	require.NoError(err)
	session, err = database.GetSessionFull(t.Context(), "empty~codex:"+id)
	require.NoError(err)
	require.NotNil(session)
	assert.Nil(session.SessionName)

}

func TestHTTPMirrorCodexIndexRemoval(t *testing.T) {
	for _, removal := range []string{"delete", "truncate", "remove-home"} {
		for _, unrelated := range []int{1, 40} {
			t.Run(fmt.Sprintf("%s/unrelated-%d", removal, unrelated), func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)

				remote := newMirrorTestRemote(t)
				base := t.TempDir()
				primary := filepath.Join(base, "primary")
				alternate := filepath.Join(base, "alternate")
				root := filepath.Join(primary, "sessions")
				require.NoError(os.MkdirAll(root, 0o755))
				require.NoError(os.MkdirAll(alternate, 0o755))
				const id = "019f0000-0000-7000-8000-000000000009"
				for i := 0; i <= unrelated; i++ {
					uuid := id
					if i > 0 {
						uuid = fmt.Sprintf("019f0000-0000-7000-8001-%012d", i)
					}
					transcript := filepath.Join(root, "rollout-2026-09-03T10-00-00-"+uuid+".jsonl")
					require.NoError(os.WriteFile(transcript, []byte(
						`{"timestamp":"2026-09-03T10:00:00Z","type":"session_meta","payload":{"id":"`+uuid+`","cwd":"/work"}}`+"\n"+
							`{"timestamp":"2026-09-03T10:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Original prompt"}]}}`+"\n",
					), 0o600))
				}
				primaryIndex := filepath.Join(primary, parser.CodexSessionIndexFilename)
				alternateIndex := filepath.Join(alternate, parser.CodexSessionIndexFilename)
				require.NoError(os.WriteFile(primaryIndex, []byte(`{"id":"`+id+`","thread_name":"Primary title"}`+"\n"), 0o600))
				require.NoError(os.WriteFile(alternateIndex, []byte(`{"id":"`+id+`","thread_name":"Alternate title"}`+"\n"), 0o600))
				stamp := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
				require.NoError(os.Chtimes(primaryIndex, stamp, stamp))
				require.NoError(os.Chtimes(alternateIndex, stamp.Add(time.Hour), stamp.Add(time.Hour)))
				cfg := config.Config{
					AgentDirs:        map[parser.AgentType][]string{parser.AgentCodex: {root}},
					ProviderMetadata: map[parser.AgentType]map[string][]string{parser.AgentCodex: {root: {primary, alternate}}},
				}
				var err error
				remote.targets, err = ResolveTargets(cfg)
				require.NoError(err)
				database, hs := newMirrorSync(t, remote, t.TempDir())
				_, err = hs.Run(t.Context())
				require.NoError(err)
				session, err := database.GetSessionFull(t.Context(), "devbox~codex:"+id)
				require.NoError(err)
				require.NotNil(session.SessionName)
				require.Equal("Alternate title", *session.SessionName)

				require.NoError(os.WriteFile(alternateIndex, []byte(`{"id":"`+id+`","thread_name":"Renamed alternate title"}`+"\n"), 0o600))
				stats, err := hs.Run(t.Context())
				require.NoError(err)
				assert.Equal(1, stats.SessionsSynced)
				session, err = database.GetSessionFull(t.Context(), "devbox~codex:"+id)
				require.NoError(err)
				require.NotNil(session.SessionName)
				require.Equal("Renamed alternate title", *session.SessionName)

				switch removal {
				case "delete":
					require.NoError(os.Remove(alternateIndex))
				case "truncate":
					require.NoError(os.WriteFile(alternateIndex, nil, 0o600))
				case "remove-home":
					cfg.ProviderMetadata[parser.AgentCodex][root] = []string{primary}
				}
				remote.targets, err = ResolveTargets(cfg)
				require.NoError(err)
				// Leave the mirrored change pending, then replay it after the old
				// index contents are gone from disk.
				prepared, err := hs.Prepare(t.Context())
				require.NoError(err)
				require.NoError(prepared.Close())
				stats, err = hs.Run(t.Context())
				require.NoError(err)
				assert.Equal(1, stats.ExactSources, "index changes must not schedule unrelated sessions")
				assert.Zero(stats.FallbackProviders)
				assert.Equal(1, stats.SessionsSynced)
				session, err = database.GetSessionFull(t.Context(), "devbox~codex:"+id)
				require.NoError(err)
				require.NotNil(session.SessionName)
				assert.Equal("Primary title", *session.SessionName)
				stats, err = hs.Run(t.Context())
				require.NoError(err)
				assert.Zero(stats.SessionsSynced, "the completed change must not repeat")

				require.NoError(os.Remove(primaryIndex))
				cfg.ProviderMetadata[parser.AgentCodex][root] = []string{primary}
				remote.targets, err = ResolveTargets(cfg)
				require.NoError(err)
				_, err = hs.Run(t.Context())
				require.NoError(err)
				session, err = database.GetSessionFull(t.Context(), "devbox~codex:"+id)
				require.NoError(err)
				require.NotNil(session.SessionName)
				assert.Equal("Primary title", *session.SessionName, "absence of every title is not a rename to empty")
			})
		}
	}
}
