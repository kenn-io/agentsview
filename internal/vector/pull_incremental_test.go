package vector

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

// Count the units actually delivered by the production archive and reservation.
// In the old pull path every source unit also became a retained hub identity.
type (
	pullVisitCounts   struct{ full, incremental, identities, sourceUnits int }
	journalPullSource struct {
		*db.DB
		source interface {
			UnitSource
			snapshotSource
		}
		counts *pullVisitCounts
	}
)

func (s *journalPullSource) EmbeddingCorpusFingerprint(ctx context.Context) (string, error) {
	return s.source.(interface {
		EmbeddingCorpusFingerprint(context.Context) (string, error)
	}).EmbeddingCorpusFingerprint(ctx)
}

func (s *journalPullSource) ScanEmbeddableUnits(ctx context.Context, since string, scope bool, limit int, fn func(db.EmbeddableUnit) error) (string, error) {
	return countPullUnits(ctx, s.source, s.counts, since, scope, limit, func(u db.EmbeddableUnit) error { s.counts.sourceUnits++; return fn(u) })
}

func countPullUnits(ctx context.Context, src UnitSource, counts *pullVisitCounts, since string, scope bool, limit int, fn func(db.EmbeddableUnit) error) (string, error) {
	return src.ScanEmbeddableUnits(ctx, since, scope, limit, func(u db.EmbeddableUnit) error {
		if since == "" {
			counts.full++
		} else {
			counts.incremental++
		}
		return fn(u)
	})
}

type countedPullSnapshot struct {
	db.EmbeddableSnapshot
	counts *pullVisitCounts
}

func (s countedPullSnapshot) ScanEmbeddableUnits(ctx context.Context, since string, scope bool, limit int, fn func(db.EmbeddableUnit) error) (string, error) {
	return countPullUnits(ctx, s.EmbeddableSnapshot, s.counts, since, scope, limit, fn)
}

func (s countedPullSnapshot) ScanEmbeddableUnitsForSession(ctx context.Context, id string, scope bool, limit int, fn func(db.EmbeddableUnit) error) error {
	return s.EmbeddableSnapshot.ScanEmbeddableUnitsForSession(ctx, id, scope, limit, func(u db.EmbeddableUnit) error { s.counts.identities++; return fn(u) })
}

type journalPullSnapshot struct {
	countedPullSnapshot
	sessionJournalSource
}

// Resolve the overlapping method explicitly so this remains an EmbeddableSnapshot.
func (s journalPullSnapshot) ScanEmbeddableUnitsForSession(ctx context.Context, id string, scope bool, limit int, fn func(db.EmbeddableUnit) error) error {
	return s.countedPullSnapshot.ScanEmbeddableUnitsForSession(ctx, id, scope, limit, fn)
}

func (s *journalPullSource) WithEmbeddableSnapshot(ctx context.Context, fn func(db.EmbeddableSnapshot) error) error {
	return s.source.WithEmbeddableSnapshot(ctx, func(snapshot db.EmbeddableSnapshot) error {
		counted := countedPullSnapshot{snapshot, s.counts}
		if journal, ok := snapshot.(sessionJournalSource); ok {
			return fn(journalPullSnapshot{counted, journal})
		}
		return fn(counted)
	})
}

// Fail after real archive units have mutated the mirror, before reconciliation
// can certify the new view. The retry must also be safe when switching back.
type interruptedPullSource struct{ *journalPullSource }

func (s interruptedPullSource) ScanEmbeddableUnits(ctx context.Context, since string, scope bool, limit int, fn func(db.EmbeddableUnit) error) (string, error) {
	_, err := s.journalPullSource.ScanEmbeddableUnits(ctx, since, scope, limit, fn)
	if err != nil {
		return "", err
	}
	return "", errors.New("archive scan interrupted")
}

func TestPullSameGenerationWorkTracksChangedSessions(t *testing.T) {
	for _, size := range []int{8, 256} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			ctx := t.Context()
			archivePath := filepath.Join(t.TempDir(), "archive.db")
			archive := dbtest.OpenTestDBAt(t, archivePath)
			seed := func(id, content, ended string) {
				dbtest.SeedSession(t, archive, id, "project", func(s *db.Session) {
					s.EndedAt = new(ended)
					if strings.HasPrefix(id, "sforeign") {
						s.Machine = "mirror"
					}
				})
				require.NoError(t, archive.ReplaceSessionMessages(ctx, id, []db.Message{
					{SessionID: id, Ordinal: 0, Role: "user", SourceUUID: "duplicate", Content: content},
					{SessionID: id, Ordinal: 1, Role: "user", SourceUUID: "duplicate", Content: "second"},
				}))
			}
			for i := range size {
				seed(fmt.Sprintf("s%04d", i), "original", "2024-01-01T00:00:00Z")
			}
			seed("s0000", "original", "2024-01-02T00:00:00Z")
			counts := &pullVisitCounts{}
			source := &journalPullSource{archive, archive.LocalEmbeddableSource(), counts}
			indexPath := filepath.Join(t.TempDir(), "vectors.db")
			ix, err := Open(ctx, indexPath, false, 4000)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, ix.Close()) })
			gen := fakeGeneration("hub")
			// Ordinary local builds cover the complete archive, including mirror rows.
			seed("sforeign", "foreign archived", "2023-01-01T00:00:00Z")
			_, err = ix.Build(ctx, archive, fakeBuildEncoder(), gen, BuildOptions{})
			require.NoError(t, err)
			require.Contains(t, mirrorSessionIDs(t, ix), "sforeign")
			absent := false
			var requested []HubIdentity
			var duringLookup func()
			hub := lookupFunc(func(_ context.Context, r HubLookupRequest) (HubLookupReply, error) {
				requested = append(requested, r.Identities...)
				if duringLookup != nil {
					change := duringLookup
					duringLookup = nil
					change()
				}
				reply := HubLookupReply{Version: HubVectorVersion, Fingerprint: r.Fingerprint}
				for i, id := range r.Identities {
					require.True(t, strings.HasPrefix(id.SessionID, "node~s"))
					require.Equal(t, "duplicate", id.SourceUUID)
					switch id.Ordinal {
					case 0:
						require.Equal(t, 1, id.Occurrence)
					case 1:
						require.Equal(t, 2, id.Occurrence)
					default:
						require.Fail(t, "unexpected unit ordinal")
					}
					if absent && id.SessionID == "node~s0001" {
						reply.Absent = append(reply.Absent, i)
					} else {
						reply.Chunks = append(reply.Chunks, HubChunk{Index: i, ChunkCount: 1, Vector: "AACAPwAAgD8AAIA/"})
					}
				}
				return reply, nil
			})
			options := PullOptions{MapSession: func(id string) string { return "node~" + id }}
			result, err := ix.Pull(ctx, source, hub, gen, options)
			require.NoError(t, err)
			assert.Equal(t, gen.Fingerprint(), result.Fingerprint)
			checkForeign := func(want bool) {
				t.Helper()
				exported, ok, err := ix.BeginExport(ctx, nil)
				require.NoError(t, err)
				require.True(t, ok)
				docs, _, err := exported.SessionDocs(ctx, "sforeign")
				require.NoError(t, err)
				require.NoError(t, exported.Close())
				hits, err := ix.Search(ctx, fakeBuildEncoder(), "foreign", 2*size+8)
				require.NoError(t, err)
				found := false
				for _, hit := range hits {
					if hit.SessionID == "sforeign" {
						found = true
					}
				}
				assert.Equal(t, want, found, "search ownership")
				assert.Equal(t, want, len(docs) > 0, "export ownership")
			}
			checkForeign(false)
			assert.NotContains(t, mirrorSessionIDs(t, ix), "sforeign")
			// The inverse switch must recover old foreign rows below the watermark.
			fullSource := &journalPullSource{archive, archive, counts}
			_, err = ix.Build(ctx, fullSource, fakeBuildEncoder(), gen, BuildOptions{})
			require.NoError(t, err)
			checkForeign(true)
			assert.Contains(t, mirrorSessionIDs(t, ix), "sforeign")
			// Stable complete-archive builds remain bounded too.
			*counts = pullVisitCounts{}
			_, err = ix.Build(ctx, fullSource, fakeBuildEncoder(), gen, BuildOptions{})
			require.NoError(t, err)
			assert.Zero(t, counts.full)
			assert.LessOrEqual(t, counts.sourceUnits, 4)
			// A failed widening can mutate the mirror before its certificate is
			// stored. Returning to the narrow view must reconcile that mutation.
			_, err = ix.Pull(ctx, source, hub, gen, options)
			require.NoError(t, err)
			_, err = ix.Build(ctx, interruptedPullSource{fullSource}, fakeBuildEncoder(), gen, BuildOptions{})
			require.ErrorContains(t, err, "archive scan interrupted")
			assert.Contains(t, mirrorSessionIDs(t, ix), "sforeign")
			_, err = ix.Pull(ctx, source, hub, gen, options)
			require.NoError(t, err)
			checkForeign(false)
			assert.NotContains(t, mirrorSessionIDs(t, ix), "sforeign")
			// An interrupted ordinary build can also leave foreign work pending.
			seed("sforeignpending", "pending foreign", "2024-01-03T00:00:00Z")
			stopped := errors.New("encoder stopped")
			_, err = ix.Build(ctx, fullSource, func(context.Context, []string) ([][]float32, error) { return nil, stopped }, gen, BuildOptions{})
			require.ErrorIs(t, err, stopped)
			_, err = ix.Pull(ctx, source, hub, gen, options)
			require.NoError(t, err)
			checkForeign(false)
			assert.NotContains(t, mirrorSessionIDs(t, ix), "sforeignpending")
			missing, err := ix.MissingEmbeddedDocs(ctx, gen.Fingerprint(), nil)
			require.NoError(t, err)
			assert.Zero(t, missing)
			// Upgrading a mirror with no source-view certificate reconciles once.
			require.NoError(t, ix.metaDelete(ctx, corpusFingerprintMetaKey))
			*counts = pullVisitCounts{}
			_, err = ix.Pull(ctx, source, hub, gen, options)
			require.NoError(t, err)
			assert.GreaterOrEqual(t, counts.full, 2*size)
			*counts = pullVisitCounts{}
			_, err = ix.Pull(ctx, source, hub, gen, options)
			require.NoError(t, err)
			assert.Zero(t, counts.full)
			check := func(phase string, wantRequests int) {
				t.Helper()
				t.Logf("%s archive=%d full_visits=%d incremental_visits=%d identity_visits=%d source_units=%d requests=%d", phase, size, counts.full, counts.incremental, counts.identities, counts.sourceUnits, len(requested))
				assert.Zero(t, counts.full, "ordinary top-up must not visit the unchanged corpus")
				assert.LessOrEqual(t, counts.incremental, 4)
				assert.LessOrEqual(t, counts.sourceUnits, 2)
				assert.LessOrEqual(t, counts.identities, 8)
				assert.Len(t, requested, wantRequests)
				*counts = pullVisitCounts{}
				requested = nil
			}
			*counts = pullVisitCounts{}
			requested = nil
			seed("s0000", "edited", "2024-01-03T00:00:00Z")
			_, err = ix.Pull(ctx, source, hub, gen, options)
			require.NoError(t, err)
			check("edit", 1)
			// Both deletion states are emitted by the real archive, including tombstones.
			n, err := archive.SoftDeleteSessions(ctx, []string{"s0001"})
			require.NoError(t, err)
			require.Equal(t, 1, n)
			_, err = ix.Pull(ctx, source, hub, gen, options)
			require.NoError(t, err)
			check("trash", 0)
			deleted, err := archive.DeleteParserExcludedSessions(ctx, []string{"s0001"})
			require.NoError(t, err)
			require.Equal(t, 1, deleted)
			_, err = ix.Pull(ctx, source, hub, gen, options)
			require.NoError(t, err)
			check("hard delete", 0)
			assert.NotContains(t, mirrorSessionIDs(t, ix), "s0001")
			absent = true
			seed("s0001", "restored", "2024-01-01T00:00:00Z")
			_, err = ix.Pull(ctx, source, hub, gen, options)
			require.NoError(t, err)
			check("restore behind watermark", 2)
			// Absent hub coverage stays pending across both archive and index reopen.
			seed("s0000", "pending", "2024-01-04T00:00:00Z")
			absent = true
			_, err = ix.Pull(ctx, source, hub, gen, options)
			require.NoError(t, err)
			check("absent", 3)
			require.NoError(t, ix.Close())
			ix, err = Open(ctx, indexPath, false, 4000)
			require.NoError(t, err)
			require.NoError(t, archive.Close())
			archive = dbtest.OpenTestDBAt(t, archivePath)
			source = &journalPullSource{archive, archive.LocalEmbeddableSource(), counts}
			absent = false
			_, err = ix.Pull(ctx, source, hub, gen, options)
			require.NoError(t, err)
			check("reopened pending retry", 2)
			missing, err = ix.MissingEmbeddedDocs(ctx, gen.Fingerprint(), nil)
			require.NoError(t, err)
			assert.Zero(t, missing)

			// The hub returns vectors for the pre-edit revision. Revalidation under
			// the writer reservation must discard them and leave the edit pending.
			seed("s0000", "before lookup", "2024-01-05T00:00:00Z")
			duringLookup = func() { seed("s0000", "during lookup", "2024-01-06T00:00:00Z") }
			_, err = ix.Pull(ctx, source, hub, gen, options)
			require.NoError(t, err)
			check("concurrent edit", 1)
			missing, err = ix.MissingEmbeddedDocs(ctx, gen.Fingerprint(), nil)
			require.NoError(t, err)
			assert.EqualValues(t, 1, missing)
			_, err = ix.Pull(ctx, source, hub, gen, options)
			require.NoError(t, err)
			check("concurrent edit retry", 1)
			missing, err = ix.MissingEmbeddedDocs(ctx, gen.Fingerprint(), nil)
			require.NoError(t, err)
			assert.Zero(t, missing)

			// Scheduled backstops still reconcile older source content in full.
			seed("s0002", "backstop edit", "2024-01-01T00:00:00Z")
			options.Backstop = true
			_, err = ix.Pull(ctx, source, hub, gen, options)
			require.NoError(t, err)
			assert.GreaterOrEqual(t, counts.full, 2*size)
			assert.Len(t, requested, 1)
		})
	}
}
