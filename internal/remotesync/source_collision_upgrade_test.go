package remotesync

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestHTTPSyncUpgradeRestoresUnchangedSourceCollisions(t *testing.T) {
	for _, agent := range []parser.AgentType{parser.AgentGemini, parser.AgentCursor} {
		t.Run(string(agent), func(t *testing.T) {
			remote := newMirrorTestRemote(t)
			remote.targets = TargetSet{Dirs: map[parser.AgentType][]string{agent: {remote.dir}}}
			ownerPath := writeRemoteCollisionSession(t, remote.dir, agent, "owner", 3)
			database, hs := newMirrorSync(t, remote, t.TempDir())
			_, err := hs.Run(t.Context())
			require.NoError(t, err)
			ownerID := "devbox~" + string(agent) + ":shared"
			_, err = database.StarSession(t.Context(), ownerID)
			require.NoError(t, err)
			require.NoError(t, database.RenameSession(t.Context(), ownerID, new("Saved conversation")))

			// Before collision preservation, both sources could be cached while
			// only the longer owner remained archived. Seed that completed
			// version-124 import without changing either file during the upgrade.
			otherPath := writeRemoteCollisionSession(t, remote.dir, agent, "other", 2)
			var archive bytes.Buffer
			require.NoError(t, WriteArchive(t.Context(), &archive, remote.targets))
			mirrorRoot := MirrorDir(hs.DataDir, hs.Host)
			_, err = ExtractTarStream(t.Context(), &archive, mirrorRoot)
			require.NoError(t, err)
			require.NoError(t, database.SetRemoteImportDataVersion(t.Context(), hs.Host, 124))
			before, err := database.ListSessionPathRecords(t.Context(), ownerID)
			require.NoError(t, err)
			require.Len(t, before, 1)
			manifest, err := BuildManifest(t.Context(), remote.targets)
			require.NoError(t, err)
			delta, err := MirrorDiff(mirrorRoot, manifest)
			require.NoError(t, err)
			require.Empty(t, delta.Fetch)
			require.Empty(t, delta.Deletions)
			remote.archiveRequests = nil

			stats, err := hs.Run(t.Context())
			require.NoError(t, err)
			assert.Zero(t, stats.Failed)
			assert.Empty(t, remote.archiveRequests, "unchanged sources must be reparsed from the mirror")
			records, err := database.ListSessionPathRecords(t.Context(), ownerID)
			require.NoError(t, err)
			require.Len(t, records, 2, "an ordinary sync must recover the previously collapsed source")
			paths := make([]string, 0, len(records))
			for _, record := range records {
				paths = append(paths, record.FilePath)
				messages, err := database.GetAllMessages(t.Context(), record.ID)
				require.NoError(t, err)
				if record.ID == ownerID {
					require.Len(t, messages, 3)
					assert.Equal(t, "continued", messages[2].Content)
				} else {
					require.Len(t, messages, 2)
					assert.Equal(t, "answer", messages[1].Content)
				}
			}
			assert.ElementsMatch(t, []string{"devbox:" + ownerPath, "devbox:" + otherPath}, paths)
			owner, err := database.GetSessionFull(t.Context(), ownerID)
			require.NoError(t, err)
			require.NotNil(t, owner)
			assert.Equal(t, new("devbox:"+ownerPath), owner.FilePath)
			assert.Equal(t, new("Saved conversation"), owner.DisplayName)
			starred, err := database.ListStarredSessionIDs(t.Context())
			require.NoError(t, err)
			assert.Contains(t, starred, ownerID)
		})
	}
}
