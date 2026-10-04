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
)

// Cursor recognizes a format replacement in the same project as the same
// source, including when the corrected transcript contains fewer messages.
func TestHTTPCursorFormatMovePreservesOwnership(t *testing.T) {
	for _, tc := range []struct {
		name      string
		archive   bool
		derived   bool
		forbidden bool
	}{
		{name: "mirror original"},
		{name: "mirror derived", derived: true},
		{name: "archive original", archive: true},
		{name: "archive derived", archive: true, derived: true},
		{name: "unresolved forbidden owner", forbidden: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remote := newMirrorTestRemote(t)
			remote.targets = TargetSet{Dirs: map[parser.AgentType][]string{parser.AgentCursor: {remote.dir}}}
			if tc.archive {
				remote.manifestStatus = http.StatusNotImplemented
			}
			oldPath := writeRemoteCollisionSession(t, remote.dir, parser.AgentCursor, "owner", 2)
			database, hs := newMirrorSync(t, remote, t.TempDir())
			_, err := hs.Run(t.Context())
			require.NoError(t, err)
			if tc.derived {
				oldPath = writeRemoteCollisionSession(t, remote.dir, parser.AgentCursor, "copy", 2)
				_, err = hs.Run(t.Context())
				require.NoError(t, err)
			}
			const baseID = "devbox~cursor:shared"
			records, err := database.ListSessionPathRecords(t.Context(), baseID)
			require.NoError(t, err)
			var movingID string
			for _, record := range records {
				if record.FilePath == "devbox:"+oldPath {
					movingID = record.ID
				}
			}
			require.NotEmpty(t, movingID)
			if tc.derived {
				require.NotEqual(t, baseID, movingID)
			}
			require.NoError(t, database.RenameSession(t.Context(), movingID, new("Saved conversation")))
			_, err = database.StarSession(t.Context(), movingID)
			require.NoError(t, err)

			newPath := filepath.Join(filepath.Dir(oldPath), "shared.jsonl")
			require.NoError(t, os.WriteFile(newPath, []byte("{\"role\":\"user\",\"message\":{\"content\":\"corrected question\"}}\n"), 0o600))
			require.NoError(t, os.Remove(oldPath))
			if tc.forbidden {
				remote.targets.ForbiddenRoots = []string{oldPath}
			}
			stats, err := hs.Run(t.Context())
			require.NoError(t, err)
			require.Zero(t, stats.Failed)

			after, err := database.ListSessionPathRecords(t.Context(), baseID)
			require.NoError(t, err)
			stored, err := database.GetSessionFull(t.Context(), movingID)
			require.NoError(t, err)
			require.NotNil(t, stored)
			assert.Equal(t, new("Saved conversation"), stored.DisplayName)
			starred, err := database.ListStarredSessionIDs(t.Context())
			require.NoError(t, err)
			assert.Contains(t, starred, movingID)
			if tc.forbidden {
				assert.Len(t, after, len(records)+1)
				assert.Equal(t, new("devbox:"+oldPath), stored.FilePath)
				assert.Equal(t, 2, stored.MessageCount)
			} else {
				assert.Len(t, after, len(records), "the format move must not create a duplicate")
				assert.Equal(t, new("devbox:"+newPath), stored.FilePath)
				assert.Equal(t, 1, stored.MessageCount)
				messages, err := database.GetAllMessages(t.Context(), movingID)
				require.NoError(t, err)
				require.Len(t, messages, 1)
				assert.Equal(t, "corrected question", messages[0].Content)
			}
		})
	}
}

func TestPartialCursorFormatMovePreservesOwner(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	const remoteRoot = "/remote/cursor"
	targets := TargetSet{Dirs: map[parser.AgentType][]string{parser.AgentCursor: {remoteRoot}}}
	initial := t.TempDir()
	writeRemoteCollisionSession(t, remappedRemotePath(initial, remoteRoot), parser.AgentCursor, "project", 2)
	importer := Importer{Host: "devbox", DB: database, Full: true, RequireComplete: true}
	_, err := importer.ImportExtracted(t.Context(), targets, initial)
	require.NoError(t, err)
	const id = "devbox~cursor:shared"
	original, err := database.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, original)
	require.NoError(t, database.RenameSession(t.Context(), id, new("Saved conversation")))

	partial := t.TempDir()
	path := filepath.Join(remappedRemotePath(partial, remoteRoot), "project", "agent-transcripts", "shared.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("{\"role\":\"user\",\"message\":{\"content\":\"partial question\"}}\n"), 0o600))
	stats, err := importer.ImportExtracted(t.Context(), targets, partial)
	require.NoError(t, err)
	require.Zero(t, stats.Failed)
	records, err := database.ListSessionPathRecords(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, records, 2)
	stored, err := database.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, new("Saved conversation"), stored.DisplayName)
	assert.Equal(t, original.FilePath, stored.FilePath)
	assert.Equal(t, 2, stored.MessageCount)
}

// The first imported source owns the base ID even if a later rebuild discovers
// the colliding source first. Remote contributors need that ownership snapshot
// when the local engine has no source roots.
func TestHTTPSourceCollisionRebuildWithoutLocalRoots(t *testing.T) {
	for _, agent := range []parser.AgentType{parser.AgentGemini, parser.AgentCursor} {
		t.Run(string(agent), func(t *testing.T) {
			remote := newMirrorTestRemote(t)
			remote.targets = TargetSet{Dirs: map[parser.AgentType][]string{agent: {remote.dir}}}
			writeRemoteCollisionSession(t, remote.dir, agent, "z-owner", 2)
			database, hs := newMirrorSync(t, remote, t.TempDir())
			_, err := hs.Run(t.Context())
			require.NoError(t, err)
			writeRemoteCollisionSession(t, remote.dir, agent, "a-copy", 2)
			_, err = hs.Run(t.Context())
			require.NoError(t, err)
			baseID := "devbox~" + string(agent) + ":shared"
			records, err := database.ListSessionPathRecords(t.Context(), baseID)
			require.NoError(t, err)
			require.Len(t, records, 2)
			for _, record := range records {
				name := "Saved copy"
				if record.ID == baseID {
					name = "Saved owner"
				}
				require.NoError(t, database.RenameSession(t.Context(), record.ID, &name))
			}

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

			after, err := database.ListSessionPathRecords(t.Context(), baseID)
			require.NoError(t, err)
			assert.ElementsMatch(t, records, after)
			for _, record := range records {
				stored, err := database.GetSessionFull(t.Context(), record.ID)
				require.NoError(t, err)
				require.NotNil(t, stored)
				name := "Saved copy"
				if record.ID == baseID {
					name = "Saved owner"
				}
				assert.Equal(t, &name, stored.DisplayName)
			}
		})
	}
}
