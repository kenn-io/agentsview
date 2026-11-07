//go:build chtest

package clickhouse

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
	"go.kenn.io/agentsview/internal/vector"
)

const (
	vectorFixtureFP  = "fp-test-gen"
	vectorFixtureDim = 4
)

var (
	vecAlpha0a = []float32{1, 0, 0, 0}
	vecAlpha0b = []float32{0.9, 0.1, 0, 0}
	vecAlpha1  = []float32{0, 1, 0, 0}
	vecBeta0   = []float32{0, 0, 1, 0}
	vecChild0  = []float32{0, 0, 0, 1}
)

// fakeVectorSource is an in-memory storage.VectorPushSource whose hashes and
// docs are mutated between pushes to drive the delta and eviction cases.
type fakeVectorSource struct {
	gen        storage.VectorGenerationInfo
	hasGen     bool
	hashes     map[string]string
	docs       map[string][]storage.VectorPushDoc
	genScopes  [][]string
	hashScopes [][]string
	// notReadyUnscoped makes a generation-wide export report the local index
	// as not ready, the state a promoted scoped push can run into.
	notReadyUnscoped bool
	// onSessionDocs, when set, runs before a session's docs are exported and
	// can fail the export.
	onSessionDocs func(sessionID string) error
}

func (f *fakeVectorSource) BeginExport(
	_ context.Context, sessionIDs []string,
) (storage.VectorExport, bool, error) {
	f.genScopes = append(f.genScopes, append([]string(nil), sessionIDs...))
	if sessionIDs == nil && f.notReadyUnscoped {
		return nil, false, storage.ErrVectorSourceNotReady
	}
	if !f.hasGen {
		return nil, false, nil
	}
	return &fakeVectorExport{source: f}, true, nil
}

type fakeVectorExport struct{ source *fakeVectorSource }

func (e *fakeVectorExport) Generation() storage.VectorGenerationInfo { return e.source.gen }

func (e *fakeVectorExport) SessionDocHashes(
	_ context.Context, sessionIDs []string,
) (map[string]string, error) {
	e.source.hashScopes = append(e.source.hashScopes, append([]string(nil), sessionIDs...))
	if sessionIDs == nil {
		return e.source.hashes, nil
	}
	out := make(map[string]string, len(sessionIDs))
	for _, id := range sessionIDs {
		if h, ok := e.source.hashes[id]; ok {
			out[id] = h
		}
	}
	return out, nil
}

func (e *fakeVectorExport) SessionDocs(
	_ context.Context, id string,
) ([]storage.VectorPushDoc, string, error) {
	if e.source.onSessionDocs != nil {
		if err := e.source.onSessionDocs(id); err != nil {
			return nil, "", err
		}
	}
	return e.source.docs[id], e.source.hashes[id], nil
}

func (e *fakeVectorExport) Close() error { return nil }

// fixtureDocKey is the key the local mirror gives session's document with
// source UUID uuid. The push narrows its vector-table statements to the
// doc_key prefixes of their sessions, so fixture keys need the shape
// vector.DocKey builds.
func fixtureDocKey(sessionID, uuid string) string {
	return vector.DocKey("user", sessionID, uuid, 0, 1)
}

func vdoc(sessionID, docKey string, ordinal int, content, hash string, chunks ...[]float32) storage.VectorPushDoc {
	d := storage.VectorPushDoc{
		DocKey: docKey, SessionID: sessionID, Ordinal: ordinal, OrdinalEnd: ordinal,
		OffsetsJSON: "[]", Content: content, ContentHash: hash,
	}
	for i, emb := range chunks {
		d.Chunks = append(d.Chunks, storage.VectorPushChunk{ChunkIndex: i, Embedding: emb})
	}
	return d
}

// newFixtureVectorSource embeds the fixture's alpha (two docs, three chunks)
// and beta (one doc, one chunk) sessions.
func newFixtureVectorSource() *fakeVectorSource {
	return &fakeVectorSource{
		gen:    storage.VectorGenerationInfo{Fingerprint: vectorFixtureFP, Model: "test-model", Dimension: vectorFixtureDim},
		hasGen: true,
		hashes: map[string]string{fixtureAlphaID: "alpha-v1", fixtureBetaID: "beta-v1"},
		docs: map[string][]storage.VectorPushDoc{
			fixtureAlphaID: {
				vdoc(fixtureAlphaID, fixtureDocKey(fixtureAlphaID, "d0"), 0, "alpha first", "h-a0", vecAlpha0a, vecAlpha0b),
				vdoc(fixtureAlphaID, fixtureDocKey(fixtureAlphaID, "d1"), 1, fixtureSecret, "h-a1", vecAlpha1),
			},
			fixtureBetaID: {
				vdoc(fixtureBetaID, fixtureDocKey(fixtureBetaID, "d0"), 0, "beta first", "h-b0", vecBeta0),
			},
		},
	}
}

func TestVectorPushRoundTripDeltaAndEviction(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	local, target := seedFixture(t)
	source := newFixtureVectorSource()
	s := newTestSync(t, local, target, storage.PusherOptions{VectorSource: source})
	conn := chtest.Open(t, target.URL, target.Database)

	first, err := s.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Zero(t, first.Errors)
	assert.False(t, first.Vectors.Skipped)
	assert.Equal(t, 2, first.Vectors.SessionsPushed)
	assert.Equal(t, 3, first.Vectors.DocsPushed)
	assert.Equal(t, 4, first.Vectors.ChunksPushed)
	assert.Equal(t, 1, chtest.Count(t, conn, "vector_generations", "fingerprint = ?", vectorFixtureFP))
	assert.Equal(t, 3, chtest.Count(t, conn, "vector_documents", ""))
	assert.Equal(t, 4, chtest.Count(t, conn, "vector_chunks", "generation_fingerprint = ?", vectorFixtureFP))
	assert.Equal(t, 2, chtest.Count(t, conn, "vector_push_state",
		"source_archive_id = ? AND session_id <> ''", s.archiveID))
	assert.Equal(t, 1, chtest.Count(t, conn, "vector_push_state",
		"source_archive_id = ? AND session_id = ''", s.archiveID),
		"a clean generation-wide pass records its completion marker")
	assert.NotZero(t, first.Vectors.GenerationID)

	second, err := s.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, first.Vectors.GenerationID, second.Vectors.GenerationID,
		"the generation id is stable for one fingerprint")
	assert.Zero(t, second.Vectors.SessionsPushed)
	assert.Equal(t, 2, second.Vectors.SessionsUnchanged, "unchanged hashes are not re-pushed")

	// beta's content changes: one doc replaced by two, and the old chunk row
	// must not survive alongside the new ones.
	source.hashes[fixtureBetaID] = "beta-v2"
	source.docs[fixtureBetaID] = []storage.VectorPushDoc{
		vdoc(fixtureBetaID, fixtureDocKey(fixtureBetaID, "d0"), 0, "beta first", "h-b0b", vecBeta0),
		vdoc(fixtureBetaID, fixtureDocKey(fixtureBetaID, "d1"), 1, "beta second", "h-b1", vecAlpha1),
	}
	third, err := s.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, third.Vectors.SessionsPushed)
	assert.Equal(t, 1, third.Vectors.SessionsUnchanged)
	assert.Equal(t, 2, chtest.Count(t, conn, "vector_chunks", "session_id = ?", fixtureBetaID))
	assert.Equal(t, 2, chtest.Count(t, conn, "vector_documents", "session_id = ?", fixtureBetaID))
	var hash string
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT doc_agg_hash FROM vector_push_state WHERE session_id = ?`, fixtureBetaID).Scan(&hash))
	assert.Equal(t, "beta-v2", hash)

	// A doc vanishes from beta: its chunks and row go, the sibling stays.
	source.hashes[fixtureBetaID] = "beta-v3"
	source.docs[fixtureBetaID] = source.docs[fixtureBetaID][:1]
	fourth, err := s.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, fourth.Vectors.SessionsPushed)
	assert.Equal(t, 1, chtest.Count(t, conn, "vector_documents", "session_id = ?", fixtureBetaID))
	assert.Equal(t, 0, chtest.Count(t, conn, "vector_chunks", "doc_key = ?", fixtureDocKey(fixtureBetaID, "d1")))

	// beta leaves the local index entirely: its state, chunks, and docs are
	// evicted while alpha is untouched.
	delete(source.hashes, fixtureBetaID)
	delete(source.docs, fixtureBetaID)
	fifth, err := s.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, fifth.Vectors.SessionsEvicted)
	assert.Equal(t, 0, chtest.Count(t, conn, "vector_push_state", "session_id = ?", fixtureBetaID))
	assert.Equal(t, 0, chtest.Count(t, conn, "vector_chunks", "session_id = ?", fixtureBetaID))
	assert.Equal(t, 0, chtest.Count(t, conn, "vector_documents", "session_id = ?", fixtureBetaID))
	assert.Equal(t, 3, chtest.Count(t, conn, "vector_chunks", "session_id = ?", fixtureAlphaID))

	// --full re-sends every session regardless of the recorded hash.
	full, err := s.Push(ctx, true, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, full.Vectors.SessionsPushed)
	assert.Zero(t, full.Vectors.SessionsUnchanged)
	assert.Equal(t, 3, chtest.Count(t, conn, "vector_chunks", "session_id = ?", fixtureAlphaID),
		"a full re-push replaces rows instead of duplicating them")
}

// TestVectorPushBatchReplacesEachSessionsRows pins that one flush covering
// several sessions removes each session's stale rows without touching what
// the other sessions still hold, whether the sessions share a flush or not.
func TestVectorPushBatchReplacesEachSessionsRows(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		batchSessions int
	}{
		{name: "one flush", batchSessions: 0},
		{name: "one flush per session", batchSessions: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			local, target := seedFixture(t)
			source := newFixtureVectorSource()
			s := newTestSync(t, local, target, storage.PusherOptions{VectorSource: source})
			s.vectorBatchSessions = tc.batchSessions
			conn := chtest.Open(t, target.URL, target.Database)

			_, err := s.Push(ctx, false, nil)
			require.NoError(t, err)
			// Another generation embedded both alpha docs. Its chunks for a
			// doc alpha still holds survive; those for a vanished doc go.
			require.NoError(t, insertRows(ctx, conn, "vector_chunks", [][]any{
				{"fp-other-gen", fixtureDocKey(fixtureAlphaID, "d0"), int64(0), fixtureAlphaID, vecAlpha0a, uint64(1)},
				{"fp-other-gen", fixtureDocKey(fixtureAlphaID, "d1"), int64(0), fixtureAlphaID, vecAlpha1, uint64(1)},
			}))

			// alpha drops a doc and re-embeds the survivor with one chunk;
			// beta gains a doc.
			source.hashes[fixtureAlphaID] = "alpha-v2"
			source.docs[fixtureAlphaID] = []storage.VectorPushDoc{
				vdoc(fixtureAlphaID, fixtureDocKey(fixtureAlphaID, "d0"), 0, "alpha first", "h-a0b", vecAlpha0a),
			}
			source.hashes[fixtureBetaID] = "beta-v2"
			source.docs[fixtureBetaID] = []storage.VectorPushDoc{
				vdoc(fixtureBetaID, fixtureDocKey(fixtureBetaID, "d0"), 0, "beta first", "h-b0", vecBeta0),
				vdoc(fixtureBetaID, fixtureDocKey(fixtureBetaID, "d1"), 1, "beta second", "h-b1", vecChild0),
			}
			res, err := s.Push(ctx, false, nil)
			require.NoError(t, err)
			assert.Equal(t, 2, res.Vectors.SessionsPushed)
			assert.Equal(t, 3, res.Vectors.DocsPushed)
			assert.Equal(t, 3, res.Vectors.ChunksPushed)

			assert.Equal(t, 1, chtest.Count(t, conn, "vector_documents", "session_id = ?", fixtureAlphaID))
			assert.Equal(t, 0, chtest.Count(t, conn, "vector_documents", "doc_key = ?", fixtureDocKey(fixtureAlphaID, "d1")))
			assert.Equal(t, 1, chtest.Count(t, conn, "vector_chunks",
				"session_id = ? AND generation_fingerprint = ?", fixtureAlphaID, vectorFixtureFP))
			assert.Equal(t, 0, chtest.Count(t, conn, "vector_chunks", "doc_key = ?", fixtureDocKey(fixtureAlphaID, "d1")))
			assert.Equal(t, 1, chtest.Count(t, conn, "vector_chunks",
				"doc_key = ? AND generation_fingerprint = ?", fixtureDocKey(fixtureAlphaID, "d0"), "fp-other-gen"))
			assert.Equal(t, 2, chtest.Count(t, conn, "vector_documents", "session_id = ?", fixtureBetaID))
			assert.Equal(t, 2, chtest.Count(t, conn, "vector_chunks", "session_id = ?", fixtureBetaID))
			assert.Equal(t, 1, chtest.Count(t, conn, "vector_push_state",
				"session_id = ? AND doc_agg_hash = ?", fixtureAlphaID, "alpha-v2"))
			assert.Equal(t, 1, chtest.Count(t, conn, "vector_push_state",
				"session_id = ? AND doc_agg_hash = ?", fixtureBetaID, "beta-v2"))
		})
	}
}

// TestVectorStatementsFindSessionsWithUnusualIDs pins that the flush and
// eviction statements still reach every row of a session whose id holds
// characters DocKey escapes or a query parameter value would decode
// (backslash, tab, quote, ':', '%'), and leave a session whose id is a raw
// prefix of it alone.
func TestVectorStatementsFindSessionsWithUnusualIDs(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	local, target := seedFixture(t)
	s := newTestSync(t, local, target, storage.PusherOptions{})
	conn := chtest.Open(t, target.URL, target.Database)
	odd := `a\b` + "\t" + `'c:%3A#`
	neighbor := `a\b`
	flush := func(sessionID string, docs ...storage.VectorPushDoc) {
		t.Helper()
		b := s.newVectorBatch(vectorFixtureFP)
		b.add(sessionID, "hash", docs)
		require.NoError(t, s.flushVectorBatch(ctx, b))
	}
	flush(odd,
		vdoc(odd, fixtureDocKey(odd, "d0"), 0, "odd first", "h-o0", vecAlpha0a),
		vdoc(odd, fixtureDocKey(odd, "d1"), 1, "odd second", "h-o1", vecAlpha1))
	flush(neighbor, vdoc(neighbor, fixtureDocKey(neighbor, "d0"), 0, "neighbor", "h-n0", vecBeta0))

	flush(odd, vdoc(odd, fixtureDocKey(odd, "d0"), 0, "odd first", "h-o0", vecAlpha0a))
	assert.Equal(t, 1, chtest.Count(t, conn, "vector_documents", "session_id = ?", odd),
		"a re-flush removes the document the session no longer holds")
	assert.Equal(t, 1, chtest.Count(t, conn, "vector_chunks", "session_id = ?", odd))

	require.NoError(t, s.evictVectorSessions(ctx, vectorFixtureFP, []string{odd}))
	assert.Equal(t, 0, chtest.Count(t, conn, "vector_documents", "session_id = ?", odd))
	assert.Equal(t, 0, chtest.Count(t, conn, "vector_chunks", "session_id = ?", odd))
	assert.Equal(t, 1, chtest.Count(t, conn, "vector_documents", "session_id = ?", neighbor))
	assert.Equal(t, 1, chtest.Count(t, conn, "vector_chunks", "session_id = ?", neighbor))
}

// TestVectorStatementsHandleBatchSizes pins that the flush and eviction
// statements run within the server's limits and remove exactly the batch's rows
// at the largest batch with a doc_key condition, the smallest without one, and
// the largest batch a push sends.
func TestVectorStatementsHandleBatchSizes(t *testing.T) {
	t.Parallel()
	for _, size := range []int{vectorKeyConditionSessions, vectorKeyConditionSessions + 1, idBatchSize} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			local, target := seedFixture(t)
			s := newTestSync(t, local, target, storage.PusherOptions{})
			conn := chtest.Open(t, target.URL, target.Database)
			ids := make([]string, size)
			batch := s.newVectorBatch(vectorFixtureFP)
			for i := range ids {
				ids[i] = fmt.Sprintf("batch-%03d", i)
				batch.add(ids[i], "hash", []storage.VectorPushDoc{
					vdoc(ids[i], fixtureDocKey(ids[i], "kept"), 0, "kept", "h0", vecAlpha0a),
				})
			}
			survivor := "batch-survivor"
			batch.add(survivor, "hash", []storage.VectorPushDoc{
				vdoc(survivor, fixtureDocKey(survivor, "kept"), 0, "kept", "h0", vecAlpha0a),
			})
			require.NoError(t, s.flushVectorBatch(ctx, batch))
			require.Equal(t, size+1, chtest.Count(t, conn, "vector_chunks", ""))

			require.NoError(t, s.evictVectorSessions(ctx, vectorFixtureFP, ids))

			assert.Equal(t, 1, chtest.Count(t, conn, "vector_documents", ""))
			assert.Equal(t, 1, chtest.Count(t, conn, "vector_chunks", ""))
			assert.Equal(t, 1, chtest.Count(t, conn, "vector_chunks", "session_id = ?", survivor))
		})
	}
}

// TestVectorFlushHandlesKeysOverOneStatementsLimit pins that a flush whose
// rewritten keys exceed what one statement may carry over HTTP still removes
// exactly the rows the batch no longer holds. One session holds 4,000
// documents, and one of them holds 6,000 chunks, so the keys of the session,
// and of that one document, each span several statements. The "HTTP bound"
// case splits over the native protocol too; the other uses the bound the
// target's transport picks.
func TestVectorFlushHandlesKeysOverOneStatementsLimit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		limit int
	}{
		{name: "transport bound", limit: 0},
		{name: "HTTP bound", limit: vectorFlushKeysBytes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			testVectorFlushOverOneStatementsLimit(t, tc.limit)
		})
	}
}

func testVectorFlushOverOneStatementsLimit(t *testing.T, limit int) {
	ctx := t.Context()
	local, target := seedFixture(t)
	s := newTestSync(t, local, target, storage.PusherOptions{})
	s.vectorFlushKeysBytes = limit
	conn := chtest.Open(t, target.URL, target.Database)
	const (
		session  = "long-session"
		docCount = 4000
		bigDoc   = 2000
	)
	docKey := func(i int) string { return fixtureDocKey(session, fmt.Sprintf("d%04d", i)) }
	docs := func(dropped func(int) bool, bigChunks int) []storage.VectorPushDoc {
		var out []storage.VectorPushDoc
		for i := range docCount {
			if dropped(i) {
				continue
			}
			chunks := [][]float32{vecAlpha0a}
			if i == bigDoc {
				chunks = make([][]float32, bigChunks)
				for c := range chunks {
					chunks[c] = vecAlpha1
				}
			}
			out = append(out, vdoc(session, docKey(i), i, "content", "h", chunks...))
		}
		return out
	}
	flush := func(docs []storage.VectorPushDoc) {
		t.Helper()
		b := s.newVectorBatch(vectorFixtureFP)
		b.add(session, "hash", docs)
		require.NoError(t, s.flushVectorBatch(ctx, b))
	}

	flush(docs(func(int) bool { return false }, 6000))
	require.Equal(t, docCount, chtest.Count(t, conn, "vector_documents", ""))
	// Another generation embedded the big document, a document that stays,
	// and one that vanishes. Its chunks go only with a vanished document.
	require.NoError(t, insertRows(ctx, conn, "vector_chunks", [][]any{
		{"fp-other-gen", docKey(bigDoc), int64(0), session, vecAlpha0a, uint64(1)},
		{"fp-other-gen", docKey(bigDoc), int64(3000), session, vecAlpha0a, uint64(1)},
		{"fp-other-gen", docKey(bigDoc), int64(5999), session, vecAlpha0a, uint64(1)},
		{"fp-other-gen", docKey(3999), int64(0), session, vecAlpha0a, uint64(1)},
		{"fp-other-gen", docKey(1), int64(0), session, vecAlpha0a, uint64(1)},
	}))

	// Every document ending in 1 vanishes, and the big document keeps its
	// first 4,500 chunks.
	flush(docs(func(i int) bool { return i%10 == 1 }, 4500))

	assert.Equal(t, 3600, chtest.Count(t, conn, "vector_documents", ""))
	assert.Equal(t, 0, chtest.Count(t, conn, "vector_documents", "doc_key = ?", docKey(1)))
	assert.Equal(t, 3599+4500, chtest.Count(t, conn, "vector_chunks",
		"generation_fingerprint = ?", vectorFixtureFP))
	assert.Equal(t, 4500, chtest.Count(t, conn, "vector_chunks",
		"generation_fingerprint = ? AND doc_key = ? AND chunk_index < 4500", vectorFixtureFP, docKey(bigDoc)))
	assert.Equal(t, 4, chtest.Count(t, conn, "vector_chunks", "generation_fingerprint = ?", "fp-other-gen"))
	assert.Equal(t, 0, chtest.Count(t, conn, "vector_chunks", "doc_key = ?", docKey(1)))
}

// TestVectorEvictionReadsOnlyTheSessionsKeyRanges pins that reading a session's
// chunks skips the key ranges other sessions hold, the reason the vector
// statements carry a doc_key condition on top of their session_id filter.
func TestVectorEvictionReadsOnlyTheSessionsKeyRanges(t *testing.T) {
	t.Parallel()
	const rows = 200_000
	local, target := seedFixture(t)
	s := newTestSync(t, local, target, storage.PusherOptions{})
	conn := chtest.Open(t, target.URL, target.Database)
	_, err := conn.ExecContext(t.Context(), `
		INSERT INTO vector_chunks (generation_fingerprint, doc_key, chunk_index, session_id, embedding, push_version)
		SELECT ?, concat('u:', sid, ':d'), 0, sid, [0.5], 1
		FROM (SELECT concat('session-', leftPad(toString(number), 8, '0')) AS sid FROM numbers(?))`,
		vectorFixtureFP, rows)
	require.NoError(t, err)
	comment := "vector-eviction-pruning-" + target.Database
	ctx := chdriver.Context(t.Context(), chdriver.WithSettings(chdriver.Settings{"log_comment": comment}))
	evicted := fmt.Sprintf("session-%08d", rows/2)

	bounds, err := s.vectorEvictionBounds(ctx, vectorFixtureFP, []string{evicted})
	require.NoError(t, err)
	require.Equal(t, map[string]uint64{evicted: 1}, bounds)

	_, err = conn.ExecContext(t.Context(), `SYSTEM FLUSH LOGS`)
	require.NoError(t, err)
	var read uint64
	require.NoError(t, conn.QueryRowContext(t.Context(), `
		SELECT read_rows FROM system.query_log
		WHERE type = 'QueryFinish' AND log_comment = ?`, comment).Scan(&read))
	assert.Less(t, read, uint64(rows/2))
}

// TestVectorPushFailedFlushKeepsSessionsStale pins that a flush failing
// after its rows land records no push state for any of its sessions, so the
// next push repeats them instead of trusting the half-finished write.
func TestVectorPushFailedFlushKeepsSessionsStale(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	local, target := seedFixture(t)
	source := newFixtureVectorSource()
	s := newTestSync(t, local, target, storage.PusherOptions{VectorSource: source})
	conn := chtest.Open(t, target.URL, target.Database)

	_, err := s.Push(ctx, false, nil)
	require.NoError(t, err)

	source.hashes[fixtureAlphaID] = "alpha-v2"
	source.hashes[fixtureBetaID] = "beta-v2"
	var flushed []string
	boom := errors.New("injected vector state failure")
	s.hooks = &pushHooks{beforeVectorState: func(ids []string) error {
		flushed = append(flushed, ids...)
		return boom
	}}
	_, err = s.Push(ctx, false, nil)
	require.ErrorIs(t, err, boom)
	assert.ElementsMatch(t, []string{fixtureAlphaID, fixtureBetaID}, flushed,
		"both changed sessions share one flush")
	assert.Equal(t, 1, chtest.Count(t, conn, "vector_push_state",
		"session_id = ? AND doc_agg_hash = ?", fixtureAlphaID, "alpha-v1"))
	assert.Equal(t, 1, chtest.Count(t, conn, "vector_push_state",
		"session_id = ? AND doc_agg_hash = ?", fixtureBetaID, "beta-v1"))

	s.hooks = nil
	res, err := s.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, res.Vectors.SessionsPushed, "the failed flush's sessions are repeated")
	assert.Equal(t, 1, chtest.Count(t, conn, "vector_push_state",
		"session_id = ? AND doc_agg_hash = ?", fixtureAlphaID, "alpha-v2"))
	assert.Equal(t, 1, chtest.Count(t, conn, "vector_push_state",
		"session_id = ? AND doc_agg_hash = ?", fixtureBetaID, "beta-v2"))
}

func TestVectorPushSkipsWithoutSourceOrGeneration(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	local, target := seedFixture(t)

	plain := newTestSync(t, local, target, storage.PusherOptions{})
	res, err := plain.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.True(t, res.Vectors.Skipped)
	assert.Equal(t, "no vector source configured", res.Vectors.SkippedReason)

	noGen := newTestSync(t, local, target, storage.PusherOptions{
		VectorSource: &fakeVectorSource{hasGen: false},
	})
	res, err = noGen.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.True(t, res.Vectors.Skipped)
	assert.Equal(t, "no active local generation", res.Vectors.SkippedReason)
	conn := chtest.Open(t, target.URL, target.Database)
	assert.Equal(t, 0, chtest.Count(t, conn, "vector_generations", ""))
}

// TestVectorPushFollowsMirrorResidency pins ownership by archive: a session
// the local index embeds but the session phase did not mirror (out of the
// project scope) gets no vectors, and a mirrored session that later leaves
// the scope has its vectors evicted with it.
func TestVectorPushFollowsMirrorResidency(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	local, target := seedFixture(t)
	source := newFixtureVectorSource()
	source.hashes["ghost"] = "g-v1"
	source.docs["ghost"] = []storage.VectorPushDoc{vdoc("ghost", fixtureDocKey("ghost", "d0"), 0, "ghost", "h-g", vecBeta0)}
	conn := chtest.Open(t, target.URL, target.Database)

	all := newTestSync(t, local, target, storage.PusherOptions{VectorSource: source})
	res, err := all.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, res.Vectors.SessionsPushed, "the ghost session has no mirror row and is skipped")
	assert.Equal(t, 0, chtest.Count(t, conn, "vector_documents", "session_id = ?", "ghost"))

	scoped := newTestSync(t, local, target, storage.PusherOptions{
		Projects: []string{"alpha"}, VectorSource: source,
	})
	res, err = scoped.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, chtest.Count(t, conn, "sessions", "id = ?", fixtureBetaID),
		"the session phase removed beta from the mirror")
	assert.Equal(t, 1, res.Vectors.SessionsEvicted)
	assert.Equal(t, 0, chtest.Count(t, conn, "vector_chunks", "session_id = ?", fixtureBetaID))
	assert.Equal(t, 0, chtest.Count(t, conn, "vector_push_state", "session_id = ?", fixtureBetaID))
	assert.Equal(t, 3, chtest.Count(t, conn, "vector_chunks", "session_id = ?", fixtureAlphaID))
}

// TestVectorPushScopedPromotesUntilArchiveCompletes pins the watch-mode
// contract: a change-scoped push reconciles generation-wide until this
// archive has recorded a clean generation-wide pass, then reads only its
// changed sessions. The first push is full, so it ignores the scope. Losing
// the marker (an interrupted pass) promotes a scoped push again even though
// the generation row itself is registered.
func TestVectorPushScopedPromotesUntilArchiveCompletes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	local, target := seedFixture(t)
	source := newFixtureVectorSource()
	s := newTestSync(t, local, target, storage.PusherOptions{VectorSource: source})
	conn := chtest.Open(t, target.URL, target.Database)

	res, err := s.PushWithOptions(ctx, storage.PushOptions{ScopeVectorsToChangedSessions: true}, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, res.Vectors.SessionsPushed)
	require.Len(t, source.genScopes, 1, "a full push exports generation-wide")
	assert.Nil(t, source.genScopes[0])
	assert.Nil(t, source.hashScopes[len(source.hashScopes)-1])

	appendMessage(t, local, fixtureBetaID, "beta again", "2026-01-11T00:10:00.000Z")
	source.hashes[fixtureBetaID] = "beta-v2"
	res, err = s.PushWithOptions(ctx, storage.PushOptions{ScopeVectorsToChangedSessions: true}, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, res.SessionsPushed, "only beta changed relationally")
	assert.Equal(t, 1, res.Vectors.SessionsPushed)
	assert.Equal(t, []string{fixtureBetaID}, source.hashScopes[len(source.hashScopes)-1],
		"a scoped push reads local hashes only for the changed sessions")

	_, err = conn.ExecContext(ctx,
		`DELETE FROM vector_push_state WHERE source_archive_id = ? AND session_id = ''`, s.archiveID)
	require.NoError(t, err)
	assert.Equal(t, 1, chtest.Count(t, conn, "vector_generations", "fingerprint = ?", vectorFixtureFP))
	appendMessage(t, local, fixtureBetaID, "beta third", "2026-01-11T00:20:00.000Z")
	source.hashes[fixtureBetaID] = "beta-v3"
	res, err = s.PushWithOptions(ctx, storage.PushOptions{ScopeVectorsToChangedSessions: true}, nil)
	require.NoError(t, err)
	assert.Nil(t, source.hashScopes[len(source.hashScopes)-1],
		"a registered generation without this archive's completion marker still promotes")
	assert.Equal(t, 1, chtest.Count(t, conn, "vector_push_state",
		"source_archive_id = ? AND session_id = ''", s.archiveID))
}

// TestVectorPushPromotedExportNotReady pins that a scoped push whose
// promotion to generation-wide finds the local index not ready reports the
// phase skipped instead of failing or panicking on the unopened export.
func TestVectorPushPromotedExportNotReady(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	local, target := seedFixture(t)
	source := newFixtureVectorSource()
	s := newTestSync(t, local, target, storage.PusherOptions{VectorSource: source})
	_, err := s.Push(ctx, false, nil)
	require.NoError(t, err)
	// Dropping the completion marker makes the next scoped push promote.
	conn := chtest.Open(t, target.URL, target.Database)
	_, err = conn.ExecContext(ctx,
		`DELETE FROM vector_push_state WHERE source_archive_id = ? AND session_id = ''`, s.archiveID)
	require.NoError(t, err)
	appendMessage(t, local, fixtureBetaID, "beta again", "2026-01-11T00:10:00.000Z")
	source.hashes[fixtureBetaID] = "beta-v2"
	source.notReadyUnscoped = true
	source.genScopes = nil

	res, err := s.PushWithOptions(ctx, storage.PushOptions{ScopeVectorsToChangedSessions: true}, nil)
	require.NoError(t, err)
	assert.True(t, res.Vectors.Skipped)
	assert.Equal(t, storage.ErrVectorSourceNotReady.Error(), res.Vectors.SkippedReason)
	require.Len(t, source.genScopes, 2, "scoped export, then the promoted export that was not ready")
}

// TestVectorPushEvictionKeepsOtherArchiveVectors pins the handoff: once
// another archive owns a session, the former owner's eviction drops only
// its own state row. Ownership is a state row, or the session mirrored
// under the other archive with its chunks in place and no state row yet (a
// push mirrors sessions before chunks, and chunks before its state row).
func TestVectorPushEvictionKeepsOtherArchiveVectors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		claim func(t *testing.T, conn *sql.DB, version uint64)
	}{
		{"state row", func(t *testing.T, conn *sql.DB, version uint64) {
			require.NoError(t, insertRows(t.Context(), conn, "vector_push_state", [][]any{{
				"other-archive", vectorFixtureFP, fixtureBetaID, "beta-other-v1", version,
			}}))
		}},
		{"mirrored residency", func(t *testing.T, conn *sql.DB, version uint64) {
			_, err := conn.ExecContext(t.Context(),
				`INSERT INTO sessions (id, project, source_archive_id, push_version) VALUES (?, ?, ?, ?)`,
				fixtureBetaID, "beta", "other-archive", version)
			require.NoError(t, err)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			local, target := seedFixture(t)
			source := newFixtureVectorSource()
			s := newTestSync(t, local, target, storage.PusherOptions{VectorSource: source})
			conn := chtest.Open(t, target.URL, target.Database)
			_, err := s.Push(ctx, false, nil)
			require.NoError(t, err)

			version := newPushVersion()
			require.NoError(t, insertRows(ctx, conn, "vector_chunks", [][]any{{
				vectorFixtureFP, fixtureDocKey(fixtureBetaID, "other"), int64(0), fixtureBetaID, vecBeta0, version,
			}}))
			tc.claim(t, conn, version)

			delete(source.hashes, fixtureBetaID)
			delete(source.docs, fixtureBetaID)
			res, err := s.Push(ctx, false, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, res.Vectors.SessionsEvicted)
			assert.Equal(t, 0, chtest.Count(t, conn, "vector_push_state",
				"source_archive_id = ? AND session_id = ?", s.archiveID, fixtureBetaID))
			assert.Equal(t, 1, chtest.Count(t, conn, "vector_chunks", "doc_key = ?", fixtureDocKey(fixtureBetaID, "other")),
				"the other archive's chunk survives the former owner's eviction")
			assert.Equal(t, 1, chtest.Count(t, conn, "vector_documents", "session_id = ?", fixtureBetaID),
				"documents stay while any generation still holds chunks for the session")

			// An eviction deletes only through the chunk version it observed
			// with the owner check, so chunks inserted between that read and
			// the delete survive.
			bounds, err := s.vectorEvictionBounds(ctx, vectorFixtureFP, []string{fixtureAlphaID, fixtureBetaID})
			require.NoError(t, err)
			require.NotContains(t, bounds, fixtureBetaID, "beta has another owner")
			newest := bounds[fixtureAlphaID]
			require.NotZero(t, newest)
			require.NoError(t, insertRows(ctx, conn, "vector_chunks", [][]any{{
				vectorFixtureFP, fixtureDocKey(fixtureAlphaID, "concurrent"), int64(0), fixtureAlphaID, vecAlpha0a, newest + 1,
			}}))
			require.NoError(t, s.deleteVectorChunksThrough(ctx, vectorFixtureFP, bounds))
			assert.Equal(t, 0, chtest.Count(t, conn, "vector_chunks", "session_id = ? AND push_version <= ?", fixtureAlphaID, newest))
			assert.Equal(t, 1, chtest.Count(t, conn, "vector_chunks", "doc_key = ?", fixtureDocKey(fixtureAlphaID, "concurrent")),
				"chunks newer than the observed version survive the bounded delete")
		})
	}
}

// TestVectorPushUsageOnlyClearsArchiveVectors pins the archive policy: once
// the local archive keeps usage only, a push evicts every vector row this
// archive recorded, whether or not a vector source is still attached, and
// leaves another archive's rows for the same session alone.
func TestVectorPushUsageOnlyClearsArchiveVectors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		detach bool
	}{
		{"attached", false},
		{"detached", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			local, target := seedFixture(t)
			source := newFixtureVectorSource()
			s := newTestSync(t, local, target, storage.PusherOptions{VectorSource: source})
			conn := chtest.Open(t, target.URL, target.Database)
			_, err := s.Push(ctx, false, nil)
			require.NoError(t, err)
			require.Equal(t, 4, chtest.Count(t, conn, "vector_chunks", ""))

			version := newPushVersion()
			require.NoError(t, insertRows(ctx, conn, "vector_chunks", [][]any{{
				vectorFixtureFP, fixtureDocKey(fixtureBetaID, "other"), int64(0), fixtureBetaID, vecBeta0, version,
			}}))
			require.NoError(t, insertRows(ctx, conn, "vector_push_state", [][]any{{
				"other-archive", vectorFixtureFP, fixtureBetaID, "beta-other-v1", version,
			}}))

			// A push under a second generation that stopped after its chunks landed
			// but before its state row: nothing but the session's archive leads back
			// to those chunks, and alpha's ordinary eviction under the fixture
			// generation does not touch them.
			const otherGen = "fp-second-gen"
			require.NoError(t, insertRows(ctx, conn, "vector_chunks", [][]any{{
				otherGen, fixtureDocKey(fixtureAlphaID, "d0"), int64(0), fixtureAlphaID, vecAlpha0a, version,
			}}))
			require.Equal(t, 0, chtest.Count(t, conn, "vector_push_state", "generation_fingerprint = ?", otherGen))
			// A push that stopped right after inserting documents, on a session with
			// no chunks and no state row in any generation: only transcript text is
			// left, and only the archive-wide document sweep can remove it.
			require.NoError(t, insertRows(ctx, conn, "vector_documents", [][]any{{
				fixtureDocKey(fixtureChildID, "orphan"), fixtureChildID, "child-uuid", int64(0), int64(0), false,
				"[]", "child first", "h-c0", version,
			}}))
			require.Equal(t, 0, chtest.Count(t, conn, "vector_chunks", "session_id = ?", fixtureChildID))
			require.Equal(t, 0, chtest.Count(t, conn, "vector_push_state", "session_id = ?", fixtureChildID))

			local.SetArchiveContent(config.ArchiveContentUsage)
			if tc.detach {
				s.vectorSource = nil
			}
			res, err := s.Push(ctx, false, nil)
			require.NoError(t, err)
			assert.True(t, res.Vectors.Skipped)
			assert.Equal(t, 0, chtest.Count(t, conn, "vector_push_state", "source_archive_id = ?", s.archiveID))
			assert.Equal(t, 0, chtest.Count(t, conn, "vector_chunks", "session_id = ?", fixtureAlphaID))
			assert.Equal(t, 0, chtest.Count(t, conn, "vector_documents", "session_id = ?", fixtureAlphaID))
			assert.Equal(t, 0, chtest.Count(t, conn, "vector_chunks", "generation_fingerprint = ?", otherGen),
				"chunks with no state row are swept through the session's archive")
			assert.Equal(t, 0, chtest.Count(t, conn, "vector_documents", "session_id = ?", fixtureChildID),
				"a document with neither chunks nor a state row is swept by archive")
			assert.Equal(t, 1, chtest.Count(t, conn, "vector_chunks", "doc_key = ?", fixtureDocKey(fixtureBetaID, "other")),
				"the other archive's beta chunk survives")
			assert.Equal(t, 1, chtest.Count(t, conn, "vector_push_state", "source_archive_id = 'other-archive'"))
		})
	}
}

func fixedEncoder(vec []float32) storage.VectorQueryEncoder {
	return func(context.Context, string) ([]float32, error) { return vec, nil }
}

func TestVectorSearcherServesSemanticAndHybrid(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	local, target := seedFixture(t)
	source := newFixtureVectorSource()
	source.hashes[fixtureChildID] = "child-v1"
	source.docs[fixtureChildID] = []storage.VectorPushDoc{
		vdoc(fixtureChildID, fixtureDocKey(fixtureChildID, "d0"), 0, "child first", "h-c0", vecChild0),
	}
	s := newTestSync(t, local, target, storage.PusherOptions{VectorSource: source})
	_, err := s.Push(ctx, false, nil)
	require.NoError(t, err)

	store, err := NewStore(ctx, target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	assert.False(t, store.HasSemantic())

	dim, found, err := LookupVectorGeneration(ctx, store.DB(), vectorFixtureFP)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, vectorFixtureDim, dim)
	gens, err := ListVectorGenerationInfo(ctx, store.DB())
	require.NoError(t, err)
	assert.Equal(t, []storage.VectorGenerationInfo{
		{Fingerprint: vectorFixtureFP, Model: "test-model", Dimension: vectorFixtureDim},
	}, gens)

	searcher := NewVectorSearcher(store.DB(), vectorFixtureFP, vectorFixtureDim, 20, fixedEncoder(vecAlpha0a))
	hits, err := dbtest.SemanticSearch(ctx, searcher, "alpha", 10)
	require.NoError(t, err)
	require.Len(t, hits, 4, "one hit per document: both alpha chunks roll up to alpha-d0")
	assert.Equal(t, fixtureAlphaID, hits[0].SessionID)
	assert.Equal(t, 0, hits[0].Ordinal)
	assert.InDelta(t, 1.0, hits[0].Score, 1e-5)
	assert.Equal(t, "alpha first", hits[0].Snippet)
	assert.Greater(t, hits[0].Score, hits[1].Score)

	t.Run("prepared expansion and exhaustion", func(t *testing.T) {
		encodes := 0
		prepared := NewVectorSearcher(store.DB(), vectorFixtureFP, vectorFixtureDim, 20, func(_ context.Context, query string) ([]float32, error) {
			encodes++
			assert.Equal(t, "alpha", query)
			return vecAlpha0a, nil
		})
		search, _, err := prepared.PrepareSemanticSearch(ctx, "alpha")
		require.NoError(t, err)
		for _, tc := range []struct {
			k         int
			exhausted bool
		}{{1, false}, {200, true}} {
			_, exhausted, err := search(ctx, tc.k)
			require.NoError(t, err)
			assert.Equal(t, tc.exhausted, exhausted)
			assert.Equal(t, 1, encodes)
		}
	})

	units, err := searcher.ResolveMessageUnits(ctx, []db.MessageRef{
		{SessionID: fixtureAlphaID, Ordinal: 1},
		{SessionID: fixtureBetaID, Ordinal: 7},
	})
	require.NoError(t, err)
	assert.Equal(t, fixtureDocKey(fixtureAlphaID, "d1"), units[0].DocKey)
	assert.Equal(t, fixtureAlphaID, units[0].SessionID)
	assert.Empty(t, units[1].DocKey, "a ref outside every unit resolves to zero")

	store.SetVectorSearcher(searcher)
	assert.True(t, store.HasSemantic())
	// Every fixture root session carries one user message, so the default
	// one-shot exclusion would hide them all; the searches opt them in.
	page, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "alpha", Mode: "semantic", Limit: 10, IncludeOneShot: true,
	})
	require.NoError(t, err)
	require.NotEmpty(t, page.Matches)
	top := page.Matches[0]
	assert.Equal(t, fixtureAlphaID, top.SessionID)
	assert.Equal(t, 0, top.Ordinal)
	assert.Equal(t, [2]int{0, 0}, top.OrdinalRange)
	assert.Equal(t, "message", top.Location)
	assert.Equal(t, "alpha", top.Project)
	assert.Equal(t, fixtureMachine, top.Machine)
	require.NotNil(t, top.DisplayName)
	assert.Equal(t, "Alpha Saved Title", *top.DisplayName)
	require.NotNil(t, top.Score)
	assert.Contains(t, top.Snippet, "alpha first")

	// Child visibility belongs to Scope, not the sidebar's IncludeChildren:
	// the child session's own hit must survive with IncludeChildren off.
	childSearcher := NewVectorSearcher(store.DB(), vectorFixtureFP, vectorFixtureDim, 20, fixedEncoder(vecChild0))
	store.SetVectorSearcher(childSearcher)
	for _, mode := range []string{"semantic", "hybrid"} {
		childPage, err := store.SearchContent(ctx, db.ContentSearchFilter{
			Pattern: "child first", Mode: mode, Limit: 10, IncludeOneShot: true,
		})
		require.NoError(t, err)
		require.NotEmpty(t, childPage.Matches, mode)
		assert.Equal(t, fixtureChildID, childPage.Matches[0].SessionID, mode)
	}
	store.SetVectorSearcher(searcher)

	scoped, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "alpha", Mode: "semantic", Limit: 10, Project: "beta", IncludeOneShot: true,
	})
	require.NoError(t, err)
	for _, m := range scoped.Matches {
		assert.Equal(t, fixtureBetaID, m.SessionID, "the session scope filters vector hits")
	}

	hybrid, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "beta first", Mode: "hybrid", Limit: 10, IncludeOneShot: true,
	})
	require.NoError(t, err)
	require.NotEmpty(t, hybrid.Matches)
	var sawBeta bool
	for _, m := range hybrid.Matches {
		assert.Equal(t, fixtureMachine, m.Machine)
		if m.SessionID == fixtureAlphaID {
			require.NotNil(t, m.DisplayName)
			assert.Equal(t, "Alpha Saved Title", *m.DisplayName)
		} else {
			assert.Nil(t, m.DisplayName)
		}
		if m.SessionID == fixtureBetaID && m.Ordinal == 0 {
			sawBeta = true
			assert.Contains(t, m.Snippet, "beta first")
		}
	}
	assert.True(t, sawBeta, "the keyword leg surfaces the exact-phrase session")

	store.SetVectorSearcher(nil)
	store.SetSemanticUnavailableReason("semantic search: test reason")
	_, err = store.SearchContent(ctx, db.ContentSearchFilter{Pattern: "x", Mode: "semantic", Limit: 5})
	require.ErrorIs(t, err, db.ErrSemanticUnavailable)
	assert.Contains(t, err.Error(), "test reason")
}

func TestVectorPushRepairsNamedSessions(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	local, target := seedFixture(t)
	source := newFixtureVectorSource()
	s := newTestSync(t, local, target, storage.PusherOptions{VectorSource: source})

	_, err := s.Push(ctx, false, nil)
	require.NoError(t, err)
	repair, err := s.PushWithOptions(ctx, storage.PushOptions{
		FullVectorSessions: []string{fixtureAlphaID},
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, repair.Vectors.SessionsPushed, "the named session is re-sent")
	assert.Equal(t, 1, repair.Vectors.SessionsUnchanged, "other sessions keep the hash skip")
}
