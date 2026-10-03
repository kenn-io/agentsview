package remotesync

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	syncpkg "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func writeRemoteCollisionSession(t *testing.T, root string, agent parser.AgentType, location string, count int) string {
	t.Helper()
	var relative, body string
	if agent == parser.AgentGemini {
		relative = filepath.Join("tmp", location, "chats", "session-2026-01-01T10-00-shared.json")
		messages := []map[string]any{
			testjsonl.GeminiUserMsg("m1", "2026-01-01T10:00:00Z", "question"),
			testjsonl.GeminiAssistantMsg("m2", "2026-01-01T10:00:01Z", "answer", nil),
			testjsonl.GeminiUserMsg("m3", "2026-01-01T10:00:02Z", "continued"),
		}
		body = testjsonl.GeminiSessionJSON("shared", "fixture", "2026-01-01T10:00:00Z", "2026-01-01T10:00:02Z", messages[:count])
	} else {
		relative = filepath.Join(location, "agent-transcripts", "shared.txt")
		body = []string{"", "user:\nquestion\n", "user:\nquestion\nassistant:\nanswer\n", "user:\nquestion\nassistant:\nanswer\nuser:\ncontinued\n"}[count]
	}
	path := filepath.Join(root, relative)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

// A complete remote inventory can prove that a source moved. Losing that
// proof, or accepting a shorter replacement, must preserve both transcripts.
func TestHTTPSourceCollisionOwnership(t *testing.T) {
	for _, tc := range []struct {
		name       string
		agent      parser.AgentType
		legacy     bool
		rebuild    bool
		keepOld    bool
		forbidden  bool
		narrowRoot bool
		count      int
		wantMove   bool
	}{
		{name: "gemini delta equal", agent: parser.AgentGemini, count: 2, wantMove: true},
		{name: "cursor delta longer", agent: parser.AgentCursor, count: 3, wantMove: true},
		{name: "gemini archive longer", agent: parser.AgentGemini, legacy: true, count: 3, wantMove: true},
		{name: "cursor archive equal", agent: parser.AgentCursor, legacy: true, count: 2, wantMove: true},
		{name: "shorter replacement", agent: parser.AgentGemini, count: 1},
		{name: "existing owner", agent: parser.AgentCursor, legacy: true, keepOld: true, count: 3},
		{name: "forbidden owner", agent: parser.AgentGemini, keepOld: true, forbidden: true, count: 3},
		{name: "owner outside configured root", agent: parser.AgentGemini, narrowRoot: true, count: 3},
		{name: "rebuild moved owner", agent: parser.AgentGemini, rebuild: true, count: 3, wantMove: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remote := newMirrorTestRemote(t)
			remote.targets = TargetSet{Dirs: map[parser.AgentType][]string{tc.agent: {remote.dir}}}
			if tc.legacy {
				remote.manifestStatus = http.StatusNotImplemented
			}
			oldPath := writeRemoteCollisionSession(t, remote.dir, tc.agent, "old", 2)
			database, hs := newMirrorSync(t, remote, t.TempDir())
			_, err := hs.Run(t.Context())
			require.NoError(t, err)
			id := "devbox~" + string(tc.agent) + ":shared"
			original, err := database.GetSessionFull(t.Context(), id)
			require.NoError(t, err)
			require.NotNil(t, original)
			require.Equal(t, 2, original.MessageCount)
			_, err = database.StarSession(t.Context(), id)
			require.NoError(t, err)
			require.NoError(t, database.RenameSession(t.Context(), id, new("Saved conversation")))

			if !tc.keepOld {
				require.NoError(t, os.Remove(oldPath))
			}
			if tc.forbidden {
				remote.targets.ForbiddenRoots = []string{filepath.Dir(oldPath)}
			}
			newRoot := remote.dir
			if tc.narrowRoot {
				newRoot = filepath.Join(remote.dir, "narrowed")
				remote.targets.Dirs[tc.agent] = []string{newRoot}
			}
			newPath := writeRemoteCollisionSession(t, newRoot, tc.agent, "new", tc.count)
			if tc.rebuild {
				hs.Full = true
				hs.FullReason = FullImportDataRebuild
				prepared, err := hs.Prepare(t.Context())
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, prepared.Close()) })
				contributor, err := prepared.RebuildContributor(t.Context())
				require.NoError(t, err)
				engine := syncpkg.NewEngine(t.Context(), database, syncpkg.EngineConfig{})
				t.Cleanup(engine.Close)
				stats, err := engine.ResyncAllWithOptions(t.Context(), nil, syncpkg.RebuildOptions{
					Contributors: []syncpkg.RebuildContributor{contributor},
				})
				require.NoError(t, err)
				require.False(t, stats.Aborted, "%v", stats.Warnings)
				require.Zero(t, stats.Failed)
				require.NoError(t, prepared.Commit())
			} else {
				stats, err := hs.Run(t.Context())
				require.NoError(t, err)
				require.Zero(t, stats.Failed)
			}

			stored, err := database.GetSessionFull(t.Context(), id)
			require.NoError(t, err)
			require.NotNil(t, stored)
			require.NotNil(t, stored.FilePath)
			assert.Equal(t, new("Saved conversation"), stored.DisplayName)
			starred, err := database.ListStarredSessionIDs(t.Context())
			require.NoError(t, err)
			assert.Contains(t, starred, id)
			records, err := database.ListSessionPathRecords(t.Context(), id)
			require.NoError(t, err)
			if tc.wantMove {
				assert.Len(t, records, 1, "moving a complete remote source must retain its session identity")
				assert.Equal(t, "devbox:"+newPath, *stored.FilePath)
				assert.Equal(t, tc.count, stored.MessageCount)
				assert.Nil(t, stored.SourceMissingAt)
				messages, err := database.GetAllMessages(t.Context(), id)
				require.NoError(t, err)
				require.Len(t, messages, tc.count)
				assert.Equal(t, []string{"question", "answer", "continued"}[tc.count-1], messages[tc.count-1].Content)
			} else {
				require.Len(t, records, 2)
				assert.Equal(t, original.FilePath, stored.FilePath)
				assert.Equal(t, 2, stored.MessageCount)
				for _, record := range records {
					if record.ID != id {
						assert.Equal(t, "devbox:"+newPath, record.FilePath)
						assert.Equal(t, tc.count, record.MessageCount)
					}
				}
			}
		})
	}
}

// Full parsing and successful processing do not establish that an extracted
// import includes every remote file. An omitted source still owns its ID.
func TestPartialImportKeepsSourceCollisionSeparate(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "full complete processing"}[full], func(t *testing.T) {
			database := dbtest.OpenTestDB(t)
			const remoteRoot = "/remote/gemini"
			targets := TargetSet{Dirs: map[parser.AgentType][]string{parser.AgentGemini: {remoteRoot}}}
			initial := t.TempDir()
			writeRemoteCollisionSession(t, remappedRemotePath(initial, remoteRoot), parser.AgentGemini, "old", 2)
			importer := Importer{Host: "devbox", DB: database, Full: full, RequireComplete: full}
			_, err := importer.ImportExtracted(t.Context(), targets, initial)
			require.NoError(t, err)
			const id = "devbox~gemini:shared"
			original, err := database.GetSessionFull(t.Context(), id)
			require.NoError(t, err)
			require.NotNil(t, original)

			partial := t.TempDir()
			writeRemoteCollisionSession(t, remappedRemotePath(partial, remoteRoot), parser.AgentGemini, "new", 3)
			stats, err := importer.ImportExtracted(t.Context(), targets, partial)
			require.NoError(t, err)
			require.Zero(t, stats.Failed)
			records, err := database.ListSessionPathRecords(t.Context(), id)
			require.NoError(t, err)
			require.Len(t, records, 2)
			for _, record := range records {
				if record.ID == id {
					assert.Equal(t, *original.FilePath, record.FilePath)
					assert.Equal(t, 2, record.MessageCount)
				} else {
					assert.Equal(t, "devbox:/remote/gemini/tmp/new/chats/session-2026-01-01T10-00-shared.json", record.FilePath)
					assert.Equal(t, 3, record.MessageCount)
				}
			}
		})
	}
}
