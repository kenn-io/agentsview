package vector

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/kit/vector/sqlitevec"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A recipe change reshapes real assistant runs. An incomplete replacement must
// preserve the old documents, anchors and snippets, rather than only its flag.
// Publishing a smaller corpus must also keep retired-generation search usable.
func TestPullRecipeChangePreservesActiveProjection(t *testing.T) {
	for _, mode := range []string{"hub failure", "canceled", "absent", "scope change", "local completion", "local full rebuild", "unknown replacement", "retired active", "corpus shrink", "shrink publication failed"} {
		t.Run(mode, func(t *testing.T) {
			shrink := strings.Contains(mode, "shrink")
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
			if shrink {
				enc = func(_ context.Context, texts []string) ([][]float32, error) {
					vectors := make([][]float32, len(texts))
					for i, text := range texts {
						vectors[i] = []float32{1, 0, 0}
						if text == "work" {
							vectors[i] = []float32{0, 1, 0}
						}
					}
					return vectors, nil
				}
			}
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
			wantDocs := 11
			if shrink {
				require.NoError(t, archive.ReplaceSessionMessages(t.Context(), "session", []db.Message{dbtest.UserMsg("session", 0, "work")}))
				wantDocs = 1
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
			assert.EqualValues(t, wantDocs, missing, "target diagnostics use the staged recipe")
			infos, err := ix.Generations(t.Context())
			require.NoError(t, err)
			require.Len(t, infos, 2)
			assert.EqualValues(t, 6, infos[0].Embedded)
			assert.Zero(t, infos[0].Missing)
			assert.EqualValues(t, wantDocs, infos[1].Missing)
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

			if mode == "retired active" {
				require.NoError(t, manager.Retire(t.Context(), infos[0].ID, true))
				_, hasActive, activeErr := ix.ActiveFingerprint(t.Context())
				require.NoError(t, activeErr)
				require.False(t, hasActive, "forced retirement leaves the replacement stage without an active generation")
			}

			var observer *pullContentConnector
			if mode == "shrink publication failed" {
				oldDB := ix.db
				t.Cleanup(func() { require.NoError(t, oldDB.Close()) })
				observer = &pullContentConnector{drv: oldDB.Driver(), dsn: vectorDSN(path, false), failExec: func(query string, _ []driver.NamedValue) error {
					if strings.HasPrefix(query, "UPDATE message_vectors_generations SET state=CASE") {
						return stopped
					}
					return nil
				}}
				ix.db = sql.OpenDB(observer)
				ix.store, err = sqlitevec.New[string, string](t.Context(), ix.db, ix.spec.schema())
				require.NoError(t, err)
			}
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
				hubLookup := lookupFunc(func(_ context.Context, r HubLookupRequest) (HubLookupReply, error) {
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
				})
				result, err = ix.Pull(t.Context(), archive, hubLookup, next, PullOptions{IncludeAutomated: includeAutomated})
				if observer != nil {
					require.ErrorIs(t, err, stopped)
					assert.False(t, result.Activated)
					after, searchErr := ix.Search(t.Context(), enc, "work", 256)
					require.NoError(t, searchErr)
					assert.Equal(t, before, after, "failed publication rolls back cross-generation deletion")
					var chunks, stamps, vectors int
					require.NoError(t, ix.db.QueryRowContext(t.Context(), `SELECT count(*) FROM message_vectors_chunks WHERE ordinal=?`, infos[0].ID).Scan(&chunks))
					require.NoError(t, ix.db.QueryRowContext(t.Context(), `SELECT count(*) FROM message_vectors_stamps WHERE ordinal=?`, infos[0].ID).Scan(&stamps))
					require.NoError(t, ix.db.QueryRowContext(t.Context(), fmt.Sprintf(`SELECT count(*) FROM message_vectors_v%d`, infos[0].ID)).Scan(&vectors))
					assert.Equal(t, 6, stamps)
					assert.Equal(t, 26, chunks)
					assert.Equal(t, 26, vectors)
					observer.failExec = nil
					result, err = ix.Pull(t.Context(), archive, hubLookup, next, PullOptions{})
				}
			}
			require.NoError(t, err)
			assert.True(t, result.Activated)
			after, err = ix.Search(t.Context(), enc, "work", 256)
			require.NoError(t, err)
			assert.Len(t, after, wantDocs)
			missing, err = ix.MissingEmbeddedDocs(t.Context(), next.Fingerprint(), nil)
			require.NoError(t, err)
			assert.Zero(t, missing, "completed publication and coverage diagnostics use the same projection")
			if mode == "retired active" {
				completed, generationErr := manager.Generations(t.Context())
				require.NoError(t, generationErr)
				require.Len(t, completed, 2)
				assert.Equal(t, string(sqlitevec.StateActive), completed[1].State)
				assert.EqualValues(t, 11, completed[1].Embedded)
				assert.Zero(t, completed[1].Missing)
			}
			export, ok, err = ix.BeginExport(t.Context(), nil)
			require.NoError(t, err)
			require.True(t, ok)
			docs, _, err = export.SessionDocs(t.Context(), "session")
			require.NoError(t, err)
			assert.Len(t, docs, wantDocs, "search and export publish the same completed document projection")
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
			if shrink {
				require.NoError(t, manager.Activate(t.Context(), infos[0].ID, false), "retired generation can be activated without a refresh")
				hits, searchErr := ix.Search(t.Context(), enc, "a", 1)
				require.NoError(t, searchErr)
				assert.Len(t, hits, 1, "removed assistant vectors must not consume the only KNN slot")
				for _, info := range infos {
					for _, query := range []string{
						fmt.Sprintf(`SELECT count(*) FROM message_vectors_v%d`, info.ID),
						fmt.Sprintf(`SELECT count(*) FROM message_vectors_chunks WHERE ordinal=%d`, info.ID),
						fmt.Sprintf(`SELECT count(*) FROM message_vectors_stamps WHERE ordinal=%d`, info.ID),
					} {
						var count int
						require.NoError(t, ix.db.QueryRowContext(t.Context(), query).Scan(&count))
						assert.Equal(t, 1, count, query)
					}
				}
				require.Len(t, hits, 1)
				assert.Equal(t, "work", hits[0].Snippet)
			}
			var tables int
			require.NoError(t, ix.db.QueryRowContext(t.Context(), `SELECT count(*) FROM sqlite_master WHERE name LIKE 'message_vectors_pull_%'`).Scan(&tables))
			assert.Zero(t, tables, "successful switches retain no staging tables")
		})
	}
}
