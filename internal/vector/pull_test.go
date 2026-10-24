package vector

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	kitvec "go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type lookupFunc func(context.Context, HubLookupRequest) (HubLookupReply, error)

func (f lookupFunc) LookupHubVectors(ctx context.Context, r HubLookupRequest) (HubLookupReply, error) {
	return f(ctx, r)
}

func seedPullDataVersion(t *testing.T, archive *db.DB, version int, sessions ...string) {
	t.Helper()
	for _, id := range sessions {
		require.NoError(t, archive.SetSessionDataVersion(t.Context(), id, version))
	}
	require.NoError(t, archive.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), "PRAGMA user_version="+strconv.Itoa(version))
		return err
	}))
}

func TestPullVectorsRoundTrip(t *testing.T) {
	for _, mode := range []string{"unknown source", "persisted older source", "excluded stale automation", "included stale automation", "future source"} {
		t.Run(mode, func(t *testing.T) {
			ix := openTestIndex(t)
			var src UnitSource
			wantVersion := 0
			if mode == "unknown source" {
				src = &fakeUnitSource{rows: []fakeUnit{{unit: userDoc("local", "duplicate:#%", 0, "content"), endedAt: "2025-01-01"}}}
			} else {
				archive := dbtest.OpenTestDB(t)
				dbtest.SeedSessionWithMessages(t, archive, "local", "project", []db.Message{{SessionID: "local", Ordinal: 0, Role: "user", SourceUUID: "duplicate:#%", Content: "content"}})
				wantVersion = db.CurrentDataVersion() - 1
				if mode == "future source" {
					wantVersion = db.CurrentDataVersion() + 1
				}
				seedPullDataVersion(t, archive, wantVersion, "local")
				if strings.Contains(mode, "stale automation") {
					require.NoError(t, archive.UpsertSession(t.Context(), db.Session{ID: "worker", Agent: "claude", IsAutomated: true}))
					require.NoError(t, archive.SetSessionDataVersion(t.Context(), "worker", wantVersion-1))
					if mode == "included stale automation" {
						wantVersion = 0
					}
				}
				src = archive
			}
			gen := fakeGeneration("hub")
			text := "content"
			expected := []float32{1, 1, -1}
			calls := 0
			client := lookupFunc(func(_ context.Context, r HubLookupRequest) (HubLookupReply, error) {
				calls++
				require.Equal(t, gen.Fingerprint(), r.Fingerprint)
				require.Equal(t, []HubIdentity{{SessionID: "air~local", UnitKind: "user", SourceUUID: "duplicate:#%", Occurrence: 1, ContentHash: contentHash(text)}}, r.Identities)
				encoded := "AACAPwAAgD8AAIC/"
				return HubLookupReply{Version: HubVectorVersion, Fingerprint: r.Fingerprint, Chunks: []HubChunk{{Index: 0, ChunkCount: 1, Vector: encoded}}}, nil
			})
			result, err := ix.Pull(t.Context(), src, client, gen, PullOptions{IncludeAutomated: mode == "included stale automation", MapSession: func(id string) string { return "air~" + id }})
			if mode == "future source" {
				var tooNew *db.DataVersionTooNewError
				require.ErrorAs(t, err, &tooNew)
				assert.Zero(t, calls, "source proof errors stop before contacting the hub")
				return
			}
			require.NoError(t, err)
			require.Equal(t, gen.Fingerprint(), result.Fingerprint)
			missing, err := ix.MissingEmbeddedDocs(t.Context(), gen.Fingerprint(), nil)
			require.NoError(t, err)
			require.Zero(t, missing)
			exp, ok, err := ix.BeginExport(t.Context(), nil)
			require.NoError(t, err)
			require.True(t, ok)
			assert.Equal(t, wantVersion, exp.Generation().DataVersion)
			docs, _, err := exp.SessionDocs(t.Context(), "local")
			require.NoError(t, err)
			require.Len(t, docs, 1)
			require.Len(t, docs[0].Chunks, 1)
			vectors := docs[0].Chunks[0].Embedding
			require.NoError(t, exp.Close())
			require.Equal(t, expected, vectors)
		})
	}
}

func TestPullDiscardsChangedOrDeletedDocuments(t *testing.T) {
	for _, change := range []string{"rewrite", "delete"} {
		t.Run(change, func(t *testing.T) {
			ix := openTestIndex(t)
			src := twoDocSource()
			gen := fakeGeneration("hub")
			client := lookupFunc(func(_ context.Context, r HubLookupRequest) (HubLookupReply, error) {
				if change == "delete" {
					src.rows = nil
				} else {
					for i := range src.rows {
						src.rows[i].unit.Content = "rewritten"
					}
				}
				return HubLookupReply{Version: HubVectorVersion, Fingerprint: r.Fingerprint, Chunks: []HubChunk{{Index: 0, ChunkCount: 1, Vector: "AACAPwAAgD8AAIA/"}, {Index: 1, ChunkCount: 1, Vector: "AACAPwAAgD8AAIA/"}}}, nil
			})
			_, err := ix.Pull(t.Context(), src, client, gen, PullOptions{})
			require.NoError(t, err)
			infos, err := ix.Generations(t.Context())
			require.NoError(t, err)
			require.Len(t, infos, 1)
			assert.Zero(t, infos[0].Embedded)
			if change == "rewrite" {
				assert.EqualValues(t, 2, infos[0].Missing)
				assert.Equal(t, "building", infos[0].State)
			}
		})
	}
}

func TestPullKeepsOldGenerationAndResumesAfterFailure(t *testing.T) {
	for _, mode := range []string{"hub retry", "local completion", "local full rebuild"} {
		t.Run(mode, func(t *testing.T) { testPullResumesAfterFailure(t, mode) })
	}
}

func testPullResumesAfterFailure(t *testing.T, mode string) {
	t.Helper()
	ix := openTestIndex(t)
	src := twoDocSource()
	for i := 2; i < 257; i++ {
		src.rows = append(src.rows, fakeUnit{unit: userDoc("s1", strconv.Itoa(i), i, "content"), endedAt: "2025-01-01"})
	}
	ctx := t.Context()
	old := fakeGeneration("old")
	_, err := ix.Build(ctx, src, fakeBuildEncoder(), old, BuildOptions{})
	require.NoError(t, err)
	next := fakeGeneration("new")
	interrupted := errors.New("interrupted")
	calls := 0
	client := lookupFunc(func(_ context.Context, r HubLookupRequest) (HubLookupReply, error) {
		calls++
		if calls == 2 {
			return HubLookupReply{}, interrupted
		}
		chunks := make([]HubChunk, len(r.Identities))
		for i := range chunks {
			chunks[i] = HubChunk{Index: i, ChunkCount: 1, Vector: "AACAPwAAgD8AAIA/"}
		}
		return HubLookupReply{Version: HubVectorVersion, Fingerprint: r.Fingerprint, Chunks: chunks}, nil
	})
	_, err = ix.Pull(ctx, src, client, next, PullOptions{})
	require.ErrorIs(t, err, interrupted)
	require.Equal(t, 2, calls)
	missing, err := ix.MissingEmbeddedDocs(ctx, next.Fingerprint(), nil)
	require.NoError(t, err)
	require.EqualValues(t, 1, missing)
	active, ok, err := ix.ActiveFingerprint(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, old.Fingerprint(), active)
	client = func(_ context.Context, r HubLookupRequest) (HubLookupReply, error) {
		require.Len(t, r.Identities, 1, "retry should request only the uncommitted batch")
		chunks := make([]HubChunk, len(r.Identities))
		for i := range chunks {
			chunks[i] = HubChunk{Index: i, ChunkCount: 1, Vector: "AACAPwAAgD8AAIA/"}
		}
		return HubLookupReply{Version: HubVectorVersion, Fingerprint: r.Fingerprint, Chunks: chunks}, nil
	}
	var result BuildResult
	if mode == "hub retry" {
		result, err = ix.Pull(ctx, src, client, next, PullOptions{})
	} else {
		var encoded []string
		result, err = ix.Build(ctx, src, recordingEncoder(&encoded), next, BuildOptions{FullRebuild: mode == "local full rebuild"})
		if mode == "local completion" {
			assert.Equal(t, []string{"world"}, encoded, "local completion reuses the successfully imported page")
		} else {
			assert.Len(t, encoded, 257, "full rebuild explicitly replaces every imported vector")
		}
	}
	require.NoError(t, err)
	assert.True(t, result.Activated)
	active, ok, err = ix.ActiveFingerprint(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, next.Fingerprint(), active)
}

func TestPullRejectsIncompleteOrChangedGeneration(t *testing.T) {
	for _, mode := range []string{"partial", "409", "cancel", "wrong fingerprint"} {
		t.Run(mode, func(t *testing.T) {
			ix := openTestIndex(t)
			src := twoDocSource()
			gen := fakeGeneration("hub")
			gen.Params = map[string]string{"max_input_chars": "4"}
			for i := range src.rows {
				src.rows[i].unit.Content = strings.Repeat("a", 8)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			pageFailed := errors.New("page failed")
			client := lookupFunc(func(_ context.Context, r HubLookupRequest) (HubLookupReply, error) {
				calls++
				if mode == "409" {
					return HubLookupReply{}, ErrHubGenerationChanged
				}
				if mode == "cancel" {
					cancel()
					return HubLookupReply{}, ctx.Err()
				}
				if calls > 1 {
					return HubLookupReply{}, pageFailed
				}
				reply := HubLookupReply{Version: HubVectorVersion, Fingerprint: r.Fingerprint, Chunks: []HubChunk{{Index: 0, ChunkCount: 2, Vector: "AACAPwAAgD8AAIA/"}}, NextCursor: "next"}
				if mode == "wrong fingerprint" {
					reply.Fingerprint = "other"
				}
				return reply, nil
			})
			_, err := ix.Pull(ctx, src, client, gen, PullOptions{})
			switch mode {
			case "409", "wrong fingerprint":
				require.ErrorIs(t, err, ErrHubGenerationChanged)
			case "cancel":
				require.ErrorIs(t, err, context.Canceled)
			case "partial":
				require.ErrorIs(t, err, pageFailed)
			}
			if mode == "partial" {
				require.Equal(t, 2, calls)
			}
			infos, err := ix.Generations(t.Context())
			require.NoError(t, err)
			require.Len(t, infos, 1)
			assert.Zero(t, infos[0].Embedded)
			assert.Equal(t, "building", infos[0].State)
		})
	}
}

// The in-memory source mutates only between calls; its callback is a reservation.
func (f *fakeUnitSource) WithEmbeddableSnapshot(_ context.Context, fn func(db.EmbeddableSnapshot) error) error {
	return fn(f)
}

func (f *fakeUnitSource) ScanEmbeddableUnitsForSession(ctx context.Context, id string, scope bool, limit int, fn func(db.EmbeddableUnit) error) error {
	_, err := f.ScanEmbeddableUnits(ctx, "", scope, limit, func(u db.EmbeddableUnit) error {
		if u.SessionID == id {
			return fn(u)
		}
		return nil
	})
	return err
}

type countedPullSource struct {
	*fakeUnitSource
	scans int
}

func (s *countedPullSource) ScanEmbeddableUnits(ctx context.Context, since string, scope bool, limit int, fn func(db.EmbeddableUnit) error) (string, error) {
	s.scans++
	return s.fakeUnitSource.ScanEmbeddableUnits(ctx, since, scope, limit, fn)
}

func (s *countedPullSource) WithEmbeddableSnapshot(_ context.Context, fn func(db.EmbeddableSnapshot) error) error {
	return fn(s)
}

func TestPullReconcilesEarlierBatchesOnce(t *testing.T) {
	ix := openTestIndex(t)
	src := &countedPullSource{fakeUnitSource: &fakeUnitSource{}}
	for i := range 257 {
		src.rows = append(src.rows, fakeUnit{unit: userDoc("s", strconv.Itoa(i), i, "original"), endedAt: "2025-01-01"})
	}
	calls := 0
	var first string
	hub := lookupFunc(func(_ context.Context, r HubLookupRequest) (HubLookupReply, error) {
		calls++
		if calls == 1 {
			first = r.Identities[0].SourceUUID
		} else {
			for i := range src.rows {
				if src.rows[i].unit.SourceUUID == first {
					src.rows[i].unit.Content = "changed"
				}
			}
		}
		chunks := make([]HubChunk, len(r.Identities))
		for i := range chunks {
			chunks[i] = HubChunk{Index: i, ChunkCount: 1, Vector: "AACAPwAAgD8AAIA/"}
		}
		return HubLookupReply{Version: HubVectorVersion, Fingerprint: r.Fingerprint, Chunks: chunks}, nil
	})
	result, err := ix.Pull(t.Context(), src, hub, fakeGeneration("hub"), PullOptions{})
	require.NoError(t, err)
	require.False(t, result.Activated)
	require.Equal(t, 2, src.scans)
	infos, err := ix.Generations(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 1, infos[0].Missing)
}

func TestPullContinuationContracts(t *testing.T) {
	for _, mode := range []string{"valid", "empty", "late absent", "partial", "nonfinite", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			ix := openTestIndex(t)
			ix.split = kitvec.SplitOptions{MaxRunes: 4}
			docs := []kitvec.Pending[string]{{Content: strings.Repeat("a", 8)}}
			ids := []HubIdentity{{SessionID: "s", ContentHash: "hash"}}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			hub := lookupFunc(func(_ context.Context, r HubLookupRequest) (HubLookupReply, error) {
				calls++
				reply := HubLookupReply{Version: HubVectorVersion, Fingerprint: "fp", Chunks: []HubChunk{{ChunkIndex: calls - 1, ChunkCount: 2, Vector: "AACAPwAAgD8AAIA/"}}}
				if calls == 2 {
					reply.Chunks[0].Vector = "AAAAQAAAAEAAAABA"
				}
				if calls == 1 {
					reply.NextCursor = "next"
				}
				switch mode {
				case "empty":
					reply.Chunks = nil
					reply.NextCursor = strconv.Itoa(calls)
				case "late absent":
					if calls == 2 {
						reply.Chunks = nil
						reply.Absent = []int{0}
					}
				case "partial":
					reply.NextCursor = ""
				case "nonfinite":
					reply.Chunks[0].Vector = "AACAfwAAgD8AAIA/"
				case "cancel":
					cancel()
				}
				if calls > 3 {
					return HubLookupReply{}, errors.New("unbounded pagination")
				}
				return reply, nil
			})
			vectors, _, err := ix.pullBatch(ctx, hub, "fp", 3, docs, ids)
			if mode == "valid" {
				require.NoError(t, err)
				require.Equal(t, []kitvec.ChunkVector{{ChunkIndex: 0, Vector: []float32{1, 1, 1}}, {ChunkIndex: 1, Vector: []float32{2, 2, 2}}}, vectors[0])
			} else {
				require.Error(t, err)
			}
			if mode == "empty" || mode == "cancel" {
				require.Equal(t, 1, calls)
			}
		})
	}
}

// Returning to the original build recipe must restore its document boundaries,
// even when the intervening import is interrupted during its initial reshape.
func TestPullRunCapBuildPullBuild(t *testing.T) {
	for _, hubCap := range []int{1000, 2000, 4000, 5000} {
		for _, interrupted := range []bool{false, true} {
			t.Run(fmt.Sprintf("cap-%d/interrupted-%t", hubCap, interrupted), func(t *testing.T) {
				testPullRunCapRestore(t, hubCap, interrupted)
			})
		}
	}
}

func testPullRunCapRestore(t *testing.T, hubCap int, interrupted bool) {
	t.Helper()
	archive := seedRunCapArchive(t, false)
	dbtest.SeedSessionWithMessages(t, archive, "newer", "project", []db.Message{dbtest.UserMsg("newer", 0, "new work")}, func(s *db.Session) { s.EndedAt = new("2024-02-01T00:00:00Z") })
	ix, err := Open(t.Context(), filepath.Join(t.TempDir(), "vectors.db"), false, 4000)
	require.NoError(t, err)
	defer ix.Close()
	gen := fakeGeneration("local")
	gen.Params = map[string]string{"max_input_chars": "4000"}
	_, err = ix.Build(t.Context(), archive, fakeBuildEncoder(), gen, BuildOptions{})
	require.NoError(t, err)
	hubGen := fakeGeneration("hub")
	hubGen.Params = map[string]string{"max_input_chars": strconv.Itoa(hubCap)}
	var source UnitSource = archive
	if interrupted {
		source = interruptedSource{archive}
	}
	hub := lookupFunc(func(_ context.Context, r HubLookupRequest) (HubLookupReply, error) {
		absent := make([]int, len(r.Identities))
		for i := range absent {
			absent[i] = i
		}
		return HubLookupReply{Version: HubVectorVersion, Fingerprint: r.Fingerprint, Absent: absent}, nil
	})
	_, err = ix.Pull(t.Context(), source, hub, hubGen, PullOptions{})
	if interrupted {
		require.Error(t, err)
	} else {
		require.NoError(t, err)
	}
	_, err = ix.Build(t.Context(), archive, fakeBuildEncoder(), gen, BuildOptions{})
	require.NoError(t, err)
	var documents int
	require.NoError(t, ix.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM vector_messages WHERE session_id='session'").Scan(&documents))
	require.Equal(t, 6, documents)
}

func TestPullClearsCompletedActiveRebuild(t *testing.T) {
	ix := openTestIndex(t)
	src, gen := twoDocSource(), fakeGeneration("hub")
	_, err := ix.Build(t.Context(), src, fakeBuildEncoder(), gen, BuildOptions{})
	require.NoError(t, err)
	stopped := errors.New("stopped rebuild")
	_, err = ix.Build(t.Context(), src, func(context.Context, []string) ([][]float32, error) { return nil, stopped }, gen, BuildOptions{FullRebuild: true})
	require.ErrorIs(t, err, stopped)
	_, err = ix.Pull(t.Context(), src, lookupFunc(func(_ context.Context, r HubLookupRequest) (HubLookupReply, error) {
		return HubLookupReply{Version: HubVectorVersion, Fingerprint: r.Fingerprint, Absent: []int{0, 1}}, nil
	}), gen, PullOptions{})
	require.NoError(t, err)
	pending, err := ix.ActiveFullRebuildPending(t.Context(), gen.Fingerprint())
	require.NoError(t, err)
	require.True(t, pending, "an incomplete pull must preserve the rebuild marker")
	hub := lookupFunc(func(_ context.Context, r HubLookupRequest) (HubLookupReply, error) {
		chunks := make([]HubChunk, len(r.Identities))
		for i := range chunks {
			chunks[i] = HubChunk{Index: i, ChunkCount: 1, Vector: "AACAPwAAgD8AAIA/"}
		}
		return HubLookupReply{Version: HubVectorVersion, Fingerprint: r.Fingerprint, Chunks: chunks}, nil
	})
	_, err = ix.Pull(t.Context(), src, hub, gen, PullOptions{})
	require.NoError(t, err)
	exp, ok, err := ix.BeginExport(t.Context(), nil)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, exp.Close())
}

func TestPullStampsWhitespaceDocument(t *testing.T) {
	ix := openTestIndex(t)
	src := &fakeUnitSource{rows: []fakeUnit{{unit: userDoc("s", "blank", 0, " \t\n "), endedAt: "2024-01-01"}}}
	gen := fakeGeneration("hub")
	hub := lookupFunc(func(_ context.Context, r HubLookupRequest) (HubLookupReply, error) {
		return HubLookupReply{Version: HubVectorVersion, Fingerprint: r.Fingerprint, Absent: []int{0}}, nil
	})
	result, err := ix.Pull(t.Context(), src, hub, gen, PullOptions{})
	require.NoError(t, err)
	require.True(t, result.Activated)
	missing, err := ix.MissingEmbeddedDocs(t.Context(), gen.Fingerprint(), nil)
	require.NoError(t, err)
	require.Zero(t, missing)
}

func TestPullPreservesSparseChunkIndices(t *testing.T) {
	for _, gap := range []int{1, 2, 10, 20} {
		t.Run(strconv.Itoa(gap), func(t *testing.T) { testPullSparseChunks(t, gap) })
	}
}

func TestRepairAfterFailedPullUsesLocalRecipe(t *testing.T) {
	ix := openTestIndex(t)
	src := &fakeUnitSource{rows: []fakeUnit{{unit: userDoc("s", "u", 0, "abcdefgh"), endedAt: "2024-01-01"}}}
	gen := fakeGeneration("local")
	gen.Params = map[string]string{"max_input_chars": "4"}
	_, err := ix.Build(t.Context(), src, fakeBuildEncoder(), gen, BuildOptions{})
	require.NoError(t, err)
	hubGen := fakeGeneration("hub")
	hubGen.Params = map[string]string{"max_input_chars": "2"}
	stopped := errors.New("pull failed")
	_, err = ix.Pull(t.Context(), src, lookupFunc(func(context.Context, HubLookupRequest) (HubLookupReply, error) { return HubLookupReply{}, stopped }), hubGen, PullOptions{})
	require.ErrorIs(t, err, stopped)
	var seen []string
	_, err = ix.Build(t.Context(), src, recordingEncoder(&seen), gen, BuildOptions{RepairInvalid: true})
	require.NoError(t, err)
	require.Empty(t, seen, "healthy local vectors must not be rewritten using the failed hub recipe")
}

func TestPullAndBuildKeepSearchRecipeWithActiveGeneration(t *testing.T) {
	for _, mode := range []string{"pull success", "pull failed", "pull incomplete", "pull publication failed", "build success", "build failed", "build cleanup failed"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "vectors.db")
			ix, err := Open(t.Context(), path, false, 4000)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, ix.Close()) })
			src := &fakeUnitSource{rows: []fakeUnit{{unit: userDoc("s", "u", 0, "abcdwxyz"), endedAt: "2024-01-01"}}}
			old, next := fakeGeneration("old"), fakeGeneration("next")
			old.Params = map[string]string{"max_input_chars": "4"}
			next.Params = map[string]string{"max_input_chars": "2"}
			enc := func(_ context.Context, texts []string) ([][]float32, error) {
				vectors := make([][]float32, len(texts))
				for i, text := range texts {
					vectors[i] = []float32{0, 1, 0}
					if text == "wxyz" || text == "wx" {
						vectors[i] = []float32{1, 0, 0}
					}
				}
				return vectors, nil
			}
			var initialSource UnitSource = src
			oldVersion := db.CurrentDataVersion() - 1
			if mode == "pull publication failed" {
				archive := dbtest.OpenTestDB(t)
				dbtest.SeedSessionWithMessages(t, archive, "s", "project", []db.Message{{SessionID: "s", Ordinal: 0, Role: "user", SourceUUID: "u", Content: "abcdwxyz"}})
				seedPullDataVersion(t, archive, oldVersion, "s")
				initialSource = archive
			}
			_, err = ix.Build(t.Context(), initialSource, enc, old, BuildOptions{})
			require.NoError(t, err)
			prepared, err := ix.PrepareSearch(t.Context(), enc, "wxyz")
			require.NoError(t, err)
			checkDuring := func(ctx context.Context) {
				hits, err := ix.Search(ctx, enc, "wxyz", 1)
				require.NoError(t, err)
				require.Len(t, hits, 1)
				assert.Equal(t, "wxyz", hits[0].Snippet)
			}
			stopped := errors.New("stopped operation")
			publicationFailed := mode == "pull publication failed"
			cleanupFailed := mode == "build cleanup failed"
			var observer *pullContentConnector
			var deletedProof, copiedMeta bool
			if publicationFailed || cleanupFailed {
				oldDB := ix.db
				t.Cleanup(func() { require.NoError(t, oldDB.Close()) })
				observer = &pullContentConnector{drv: oldDB.Driver(), dsn: vectorDSN(path, false), failExec: func(query string, args []driver.NamedValue) error {
					if publicationFailed {
						if query == "DELETE FROM vector_meta WHERE key=?" && len(args) == 1 && args[0].Value == sourceDataVersionKey {
							deletedProof = true
						}
						if strings.HasPrefix(query, "INSERT OR REPLACE INTO vector_meta SELECT key,value FROM ") {
							copiedMeta = true
						}
					}
					if (publicationFailed && strings.HasPrefix(query, "UPDATE message_vectors_generations SET state=CASE")) ||
						(cleanupFailed && strings.Contains(query, "DELETE FROM vector_meta WHERE key = ? AND value != ?") && len(args) == 2 && args[0].Value == activeFullRebuildKey) {
						return stopped
					}
					return nil
				}}
				ix.db = sql.OpenDB(observer)
				ix.store, err = sqlitevec.New[string, string](t.Context(), ix.db, ix.spec.schema())
				require.NoError(t, err)
			}
			var result BuildResult
			var hub lookupFunc
			if strings.HasPrefix(mode, "build") {
				result, err = ix.Build(t.Context(), src, func(ctx context.Context, texts []string) ([][]float32, error) {
					checkDuring(ctx)
					if mode == "build failed" {
						return nil, stopped
					}
					return enc(ctx, texts)
				}, next, BuildOptions{})
			} else {
				hub = lookupFunc(func(ctx context.Context, r HubLookupRequest) (HubLookupReply, error) {
					checkDuring(ctx)
					if mode == "pull failed" {
						return HubLookupReply{}, stopped
					}
					reply := HubLookupReply{Version: HubVectorVersion, Fingerprint: r.Fingerprint}
					if mode == "pull incomplete" {
						reply.Absent = []int{0}
						return reply, nil
					}
					for i := range 4 {
						encoded := "AAAAAAAAgD8AAAAA"
						if i == 2 {
							encoded = "AACAPwAAAAAAAAAA"
						}
						reply.Chunks = append(reply.Chunks, HubChunk{ChunkIndex: i, ChunkCount: 4, Vector: encoded})
					}
					return reply, nil
				})
				result, err = ix.Pull(t.Context(), src, hub, next, PullOptions{})
			}
			if strings.HasSuffix(mode, "failed") {
				require.ErrorIs(t, err, stopped)
			} else {
				require.NoError(t, err)
			}
			if cleanupFailed {
				assert.True(t, result.Activated, "cleanup failure follows committed build activation")
			}
			if publicationFailed {
				assert.True(t, deletedProof, "failure follows proof deletion")
				assert.True(t, copiedMeta, "failure follows staged metadata copy")
				assert.False(t, result.Activated, "publication failure rolls back activation")
				export, ok, exportErr := ix.BeginExport(t.Context(), nil)
				require.NoError(t, exportErr)
				require.True(t, ok)
				assert.Equal(t, oldVersion, export.Generation().DataVersion, "failed publication restores deleted proof")
				require.NoError(t, export.Close())
			}
			hits, _, err := prepared(t.Context(), 1)
			require.NoError(t, err)
			require.Len(t, hits, 1)
			assert.Equal(t, "wxyz", hits[0].Snippet, "prepared queries retain their original snapshot")
			want := "wxyz"
			if strings.HasSuffix(mode, "success") || cleanupFailed {
				want = "wx"
			}
			hits, err = ix.Search(t.Context(), enc, "wxyz", 1)
			require.NoError(t, err)
			require.Len(t, hits, 1)
			assert.Equal(t, want, hits[0].Snippet)
			if publicationFailed {
				observer.failExec = nil
				result, err = ix.Pull(t.Context(), src, hub, next, PullOptions{})
				require.NoError(t, err)
				assert.True(t, result.Activated, "retry publishes the completed stage")
				export, ok, exportErr := ix.BeginExport(t.Context(), nil)
				require.NoError(t, exportErr)
				require.True(t, ok)
				assert.Zero(t, export.Generation().DataVersion, "unknown replacement clears old proof")
				require.NoError(t, export.Close())
				hits, err = ix.Search(t.Context(), enc, "wxyz", 1)
				require.NoError(t, err)
				require.Len(t, hits, 1)
				assert.Equal(t, "wx", hits[0].Snippet)
			}
		})
	}
}

func testPullSparseChunks(t *testing.T, gap int) {
	t.Helper()
	ix := openTestIndex(t)
	src := &fakeUnitSource{rows: []fakeUnit{{unit: userDoc("s", "sparse", 0, "a"+strings.Repeat(" ", 4*gap+3)+"b"), endedAt: "2024-01-01"}}}
	gen := fakeGeneration("hub")
	gen.Params = map[string]string{"max_input_chars": "4"}
	calls := 0
	hub := lookupFunc(func(_ context.Context, r HubLookupRequest) (HubLookupReply, error) {
		calls++
		reply := HubLookupReply{Version: HubVectorVersion, Fingerprint: r.Fingerprint}
		if calls == 1 {
			reply.Chunks = []HubChunk{{ChunkIndex: 0, ChunkCount: 2, Vector: "AACAPwAAgD8AAIA/"}}
			reply.NextCursor = "next"
		} else {
			reply.Chunks = []HubChunk{{ChunkIndex: gap + 1, ChunkCount: 2, Vector: "AAAAQAAAAEAAAABA"}}
		}
		return reply, nil
	})
	_, err := ix.Pull(t.Context(), src, hub, gen, PullOptions{})
	require.NoError(t, err)
	exp, ok, err := ix.BeginExport(t.Context(), nil)
	require.NoError(t, err)
	require.True(t, ok)
	docs, _, err := exp.SessionDocs(t.Context(), "s")
	require.NoError(t, err)
	require.Len(t, docs, 1)
	require.Equal(t, []ExportChunk{{ChunkIndex: 0, Embedding: []float32{1, 1, 1}}, {ChunkIndex: gap + 1, Embedding: []float32{2, 2, 2}}}, docs[0].Chunks)
	require.NoError(t, exp.Close())
}
