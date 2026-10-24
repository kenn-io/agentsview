package vector

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"go.kenn.io/agentsview/internal/db"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A recipe change reshapes real assistant runs. An incomplete replacement must
// preserve the old documents, anchors and snippets, rather than only its flag.
func TestPullRecipeChangePreservesActiveProjection(t *testing.T) {
	for _, mode := range []string{"hub failure", "canceled", "absent", "scope change", "local completion", "local full rebuild", "unknown replacement"} {
		t.Run(mode, func(t *testing.T) {
			archive := seedRunCapArchive(t, false)
			version := db.CurrentDataVersion() - 2
			seedPullDataVersion(t, archive, version, "session")
			path := filepath.Join(t.TempDir(), "vectors.db")
			ix, err := Open(t.Context(), path, false, 4000)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, ix.Close()) })
			old, next := fakeGeneration("local"), fakeGeneration("hub")
			old.Params = map[string]string{"max_input_chars": "4000"}
			next.Params = map[string]string{"max_input_chars": "2000"}
			enc := fakeBuildEncoder()
			_, err = ix.Build(t.Context(), archive, enc, old, BuildOptions{})
			require.NoError(t, err)
			before, err := ix.Search(t.Context(), enc, "work", 256)
			require.NoError(t, err)
			require.Len(t, before, 6)
			preparedCtx, release := context.WithCancel(t.Context())
			defer release()
			prepared, err := ix.PrepareSearch(preparedCtx, enc, "work")
			require.NoError(t, err)
			oldExport, ok, err := ix.BeginExport(t.Context(), nil)
			require.NoError(t, err)
			require.True(t, ok)
			defer oldExport.Close()
			assert.Equal(t, version, oldExport.Generation().DataVersion)
			if mode == "unknown replacement" {
				require.NoError(t, archive.SetSessionDataVersion(t.Context(), "session", version-1))
			} else {
				seedPullDataVersion(t, archive, version+1, "session")
			}
			includeAutomated := mode == "scope change"
			stopped := errors.New("hub unavailable")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result, err := ix.Pull(ctx, archive, lookupFunc(func(_ context.Context, r HubLookupRequest) (HubLookupReply, error) {
				during, err := ix.Search(t.Context(), enc, "work", 256)
				require.NoError(t, err)
				assert.Equal(t, before, during, "the hub request must not reshape active search")
				switch mode {
				case "hub failure":
					return HubLookupReply{}, stopped
				case "canceled":
					cancel()
					return HubLookupReply{}, context.Canceled
				}
				absent := make([]int, len(r.Identities))
				for i := range absent {
					absent[i] = i
				}
				return HubLookupReply{Version: HubVectorVersion, Fingerprint: r.Fingerprint, Absent: absent}, nil
			}), next, PullOptions{IncludeAutomated: includeAutomated})
			switch mode {
			case "hub failure":
				require.ErrorIs(t, err, stopped)
			case "canceled":
				require.ErrorIs(t, err, context.Canceled)
			default:
				require.NoError(t, err)
			}
			assert.False(t, result.Activated)
			after, err := ix.Search(t.Context(), enc, "work", 256)
			require.NoError(t, err)
			assert.Equal(t, before, after)
			missing, err := ix.MissingEmbeddedDocs(t.Context(), next.Fingerprint(), nil)
			require.NoError(t, err)
			assert.EqualValues(t, 11, missing, "target diagnostics use the staged recipe")
			infos, err := ix.Generations(t.Context())
			require.NoError(t, err)
			require.Len(t, infos, 2)
			assert.EqualValues(t, 6, infos[0].Embedded)
			assert.Zero(t, infos[0].Missing)
			assert.EqualValues(t, 11, infos[1].Missing)
			manager := NewManager(ix, archive, soloEncoders(enc), next)
			require.ErrorContains(t, manager.Activate(t.Context(), infos[1].ID, true), "staged hub documents")
			if mode == "absent" {
				read, err := Open(t.Context(), path, true, 99)
				require.NoError(t, err)
				defer read.Close()
				missing, err := read.MissingEmbeddedDocs(t.Context(), next.Fingerprint(), nil)
				require.NoError(t, err)
				assert.EqualValues(t, 11, missing, "read-only diagnostics also resolve the persisted stage")
				readHits, err := read.Search(t.Context(), enc, "work", 256)
				require.NoError(t, err)
				assert.Equal(t, before, readHits)
			}
			export, ok, err := ix.BeginExport(t.Context(), nil)
			require.NoError(t, err)
			require.True(t, ok)
			docs, _, err := export.SessionDocs(t.Context(), "session")
			require.NoError(t, err)
			assert.Len(t, docs, 6)
			assert.Equal(t, version, export.Generation().DataVersion, "incomplete replacement preserves active proof")
			require.NotNil(t, export.Generation().IncludeAutomated)
			assert.False(t, *export.Generation().IncludeAutomated, "an incomplete target must not certify its scope on the old generation")
			require.NoError(t, export.Close())

			if mode == "local completion" || mode == "local full rebuild" {
				result, err = ix.Build(t.Context(), archive, enc, next, BuildOptions{FullRebuild: mode == "local full rebuild"})
			} else {
				// Use the normal builder and export to provide the hub's complete
				// chunk set, rather than guessing counts for the archive reducer.
				hub, openErr := Open(t.Context(), filepath.Join(t.TempDir(), "hub.db"), false, 2000)
				require.NoError(t, openErr)
				defer hub.Close()
				_, buildErr := hub.Build(t.Context(), archive, enc, next, BuildOptions{IncludeAutomated: includeAutomated})
				require.NoError(t, buildErr)
				hubExport, ok, exportErr := hub.BeginExport(t.Context(), nil)
				require.NoError(t, exportErr)
				require.True(t, ok)
				defer hubExport.Close()
				docs, _, exportErr := hubExport.SessionDocs(t.Context(), "session")
				require.NoError(t, exportErr)
				byOrdinal := map[int]ExportDoc{}
				for _, doc := range docs {
					byOrdinal[doc.Ordinal] = doc
				}
				result, err = ix.Pull(t.Context(), archive, lookupFunc(func(_ context.Context, r HubLookupRequest) (HubLookupReply, error) {
					reply := HubLookupReply{Version: HubVectorVersion, Fingerprint: r.Fingerprint}
					for i, id := range r.Identities {
						doc := byOrdinal[id.Ordinal]
						require.Equal(t, doc.ContentHash, id.ContentHash)
						for _, chunk := range doc.Chunks {
							encoded, err := EncodeHubVector(chunk.Embedding)
							require.NoError(t, err)
							reply.Chunks = append(reply.Chunks, HubChunk{Index: i, ChunkIndex: chunk.ChunkIndex, ChunkCount: len(doc.Chunks), Vector: encoded})
						}
					}
					return reply, nil
				}), next, PullOptions{IncludeAutomated: includeAutomated})
			}
			require.NoError(t, err)
			assert.True(t, result.Activated)
			after, err = ix.Search(t.Context(), enc, "work", 256)
			require.NoError(t, err)
			assert.Len(t, after, 11)
			export, ok, err = ix.BeginExport(t.Context(), nil)
			require.NoError(t, err)
			require.True(t, ok)
			require.NotNil(t, export.Generation().IncludeAutomated)
			assert.Equal(t, includeAutomated, *export.Generation().IncludeAutomated)
			wantVersion := version + 1
			if mode == "unknown replacement" {
				wantVersion = 0
			}
			assert.Equal(t, wantVersion, export.Generation().DataVersion)
			require.NoError(t, export.Close())
			pinned, _, err := prepared(t.Context(), 256)
			require.NoError(t, err)
			assert.Equal(t, before, pinned, "prepared hydration stays in the old archive projection")
			assert.Equal(t, version, oldExport.Generation().DataVersion, "pinned export keeps its original proof")
			docs, _, err = oldExport.SessionDocs(t.Context(), "session")
			require.NoError(t, err)
			assert.Len(t, docs, 6, "an existing export also retains its SQLite snapshot")
			stage, err := ix.pendingPullStage(t.Context(), next.Fingerprint())
			require.NoError(t, err)
			assert.Same(t, ix, stage, "successful activation consumes the retry stage")
			var tables int
			require.NoError(t, ix.db.QueryRowContext(t.Context(), `SELECT count(*) FROM sqlite_master WHERE name LIKE 'message_vectors_pull_%'`).Scan(&tables))
			assert.Zero(t, tables, "successful switches retain no staging tables")
		})
	}
}
