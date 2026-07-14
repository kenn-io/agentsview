package vector

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitvec "go.kenn.io/kit/vector"
)

func TestBuildRepairInvalidRegeneratesOnlyAffectedDocuments(t *testing.T) {
	ix := openTestIndex(t)
	ix.split = kitvec.SplitOptions{MaxRunes: 5}
	ctx := context.Background()
	src := &fakeUnitSource{rows: []fakeUnit{
		{unit: userDoc("s1", "bad", 0, "abcdefghij"), endedAt: "2024-01-01T00:00:00Z"},
		{unit: userDoc("s1", "good", 1, "short"), endedAt: "2024-01-01T00:00:01Z"},
	}}
	oldGen := fakeGeneration("old-model")
	activeGen := fakeGeneration("active-model")
	require.NoError(t, buildWithoutResult(ix, ctx, src, oldGen))
	require.NoError(t, buildWithoutResult(ix, ctx, src, activeGen))

	oldOrdinal, err := ix.ordinalForFingerprint(ctx, oldGen.Fingerprint())
	require.NoError(t, err)
	activeOrdinal, err := ix.ordinalForFingerprint(ctx, activeGen.Fingerprint())
	require.NoError(t, err)

	var oldBadRowID, activeBadRowID, activeGoodRowID int64
	var oldBadBlob, activeGoodBlob []byte
	require.NoError(t, ix.db.QueryRowContext(ctx, `
SELECT c.vec_rowid, v.embedding
  FROM message_vectors_chunks c
  JOIN message_vectors_v`+fmtInt64(oldOrdinal)+` v ON v.rowid = c.vec_rowid
 WHERE c.ordinal = ? AND c.doc_key = 'u:s1:bad' AND c.chunk_index = 0`, oldOrdinal,
	).Scan(&oldBadRowID, &oldBadBlob))
	require.NoError(t, ix.db.QueryRowContext(ctx, `
SELECT c.vec_rowid
  FROM message_vectors_chunks c
 WHERE c.ordinal = ? AND c.doc_key = 'u:s1:bad' AND c.chunk_index = 0`, activeOrdinal,
	).Scan(&activeBadRowID))
	require.NoError(t, ix.db.QueryRowContext(ctx, `
SELECT c.vec_rowid, v.embedding
  FROM message_vectors_chunks c
  JOIN message_vectors_v`+fmtInt64(activeOrdinal)+` v ON v.rowid = c.vec_rowid
 WHERE c.ordinal = ? AND c.doc_key = 'u:s1:good' AND c.chunk_index = 0`, activeOrdinal,
	).Scan(&activeGoodRowID, &activeGoodBlob))

	_, err = ix.db.ExecContext(ctx,
		`UPDATE message_vectors_v`+fmtInt64(activeOrdinal)+` SET embedding = ? WHERE rowid = ?`,
		make([]byte, 3*4), activeBadRowID)
	require.NoError(t, err)
	// This source document did not exist when the mirror was built. Repair mode
	// must not refresh the mirror, discover it, or turn it into missing work.
	src.rows = append(src.rows,
		fakeUnit{unit: userDoc("s1", "pending", 2, "pending"), endedAt: "2024-01-01T00:00:02Z"})

	var encodedChunks int
	repairEncoder := func(_ context.Context, texts []string) ([][]float32, error) {
		encodedChunks += len(texts)
		out := make([][]float32, len(texts))
		for i := range out {
			out[i] = []float32{0, 1, 0}
		}
		return out, nil
	}
	result, err := ix.Build(ctx, src, repairEncoder, activeGen, BuildOptions{RepairInvalid: true})
	require.NoError(t, err)

	assert.Equal(t, RepairStats{
		Scanned: true, ScanComplete: true, Documents: 1, Chunks: 2, RemainingKnown: true,
	}, result.Repair)
	assert.Equal(t, 1, result.Fill.Documents)
	assert.Equal(t, 2, encodedChunks, "all chunks of the affected document are regenerated")

	var newActiveBadRowID, newActiveGoodRowID, newOldBadRowID int64
	var newActiveBadBlob, newActiveGoodBlob, newOldBadBlob []byte
	require.NoError(t, ix.db.QueryRowContext(ctx, `
SELECT c.vec_rowid, v.embedding
  FROM message_vectors_chunks c
  JOIN message_vectors_v`+fmtInt64(activeOrdinal)+` v ON v.rowid = c.vec_rowid
 WHERE c.ordinal = ? AND c.doc_key = 'u:s1:bad' AND c.chunk_index = 0`, activeOrdinal,
	).Scan(&newActiveBadRowID, &newActiveBadBlob))
	require.NoError(t, ix.db.QueryRowContext(ctx, `
SELECT c.vec_rowid, v.embedding
  FROM message_vectors_chunks c
  JOIN message_vectors_v`+fmtInt64(activeOrdinal)+` v ON v.rowid = c.vec_rowid
 WHERE c.ordinal = ? AND c.doc_key = 'u:s1:good' AND c.chunk_index = 0`, activeOrdinal,
	).Scan(&newActiveGoodRowID, &newActiveGoodBlob))
	require.NoError(t, ix.db.QueryRowContext(ctx, `
SELECT c.vec_rowid, v.embedding
  FROM message_vectors_chunks c
  JOIN message_vectors_v`+fmtInt64(oldOrdinal)+` v ON v.rowid = c.vec_rowid
 WHERE c.ordinal = ? AND c.doc_key = 'u:s1:bad' AND c.chunk_index = 0`, oldOrdinal,
	).Scan(&newOldBadRowID, &newOldBadBlob))

	assert.NotEqual(t, activeBadRowID, newActiveBadRowID)
	newActiveBadVector, err := decodeFloat32Blob(newActiveBadBlob)
	require.NoError(t, err)
	assert.NoError(t, validateEmbedding(newActiveBadVector, 0))
	assert.Equal(t, activeGoodRowID, newActiveGoodRowID)
	assert.Equal(t, activeGoodBlob, newActiveGoodBlob)
	assert.Equal(t, oldBadRowID, newOldBadRowID)
	assert.Equal(t, oldBadBlob, newOldBadBlob)

	var mirrorDocuments int64
	require.NoError(t, ix.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM `+ix.spec.DocsTable).Scan(&mirrorDocuments))
	assert.Equal(t, int64(2), mirrorDocuments, "repair must not expand the mirror")

	activeInfo, err := ix.GenerationByID(ctx, activeOrdinal)
	require.NoError(t, err)
	assert.Zero(t, activeInfo.Missing, "repair must preserve complete generation coverage")

	second, err := ix.Build(ctx, src, func(context.Context, []string) ([][]float32, error) {
		t.Fatal("a clean repair scan must not encode unrelated pending documents")
		return nil, nil
	}, activeGen, BuildOptions{RepairInvalid: true})
	require.NoError(t, err)
	assert.Equal(t, RepairStats{
		Scanned: true, ScanComplete: true, RemainingKnown: true,
	}, second.Repair)
	assert.Zero(t, second.Fill.Documents)
}

func TestValidateStoredEmbeddingBlobRejectsEveryCorruptionClass(t *testing.T) {
	tests := []struct {
		name      string
		blob      []byte
		dimension int
		reason    string
	}{
		{name: "malformed bytes", blob: []byte{1, 2, 3}, dimension: 3, reason: "byte length"},
		{name: "wrong dimension", blob: float32Blob(1, 2), dimension: 3, reason: "dimension"},
		{name: "nan", blob: float32Blob(1, float32(math.NaN()), 2), dimension: 3, reason: "non-finite"},
		{name: "positive infinity", blob: float32Blob(1, float32(math.Inf(1)), 2), dimension: 3, reason: "non-finite"},
		{name: "negative infinity", blob: float32Blob(1, float32(math.Inf(-1)), 2), dimension: 3, reason: "non-finite"},
		{name: "zero norm", blob: float32Blob(0, 0, 0), dimension: 3, reason: "zero norm"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateStoredEmbeddingBlob(tt.blob, tt.dimension)
			require.Error(t, err)
			assert.ErrorContains(t, err, tt.reason)
		})
	}
}

func TestRepairInvalidVectorsQueuesOnlyAffectedDocument(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := &fakeUnitSource{rows: []fakeUnit{
		{unit: userDoc("s1", "bad", 0, "bad"), endedAt: "2024-01-01T00:00:00Z"},
		{unit: userDoc("s1", "good", 1, "good"), endedAt: "2024-01-01T00:00:01Z"},
	}}
	gen := fakeGeneration("active-model")
	require.NoError(t, buildWithoutResult(ix, ctx, src, gen))
	ordinal, err := ix.ordinalForFingerprint(ctx, gen.Fingerprint())
	require.NoError(t, err)

	var rowID int64
	require.NoError(t, ix.db.QueryRowContext(ctx, `
SELECT vec_rowid FROM message_vectors_chunks
 WHERE ordinal = ? AND doc_key = 'u:s1:bad'`, ordinal).Scan(&rowID))
	_, err = ix.db.ExecContext(ctx,
		`UPDATE message_vectors_v`+fmtInt64(ordinal)+` SET embedding = ? WHERE rowid = ?`,
		make([]byte, 3*4), rowID)
	require.NoError(t, err)

	result, err := ix.repairInvalidVectors(ctx, gen.Fingerprint())
	require.NoError(t, err)
	assert.Equal(t, RepairStats{
		Scanned: true, ScanComplete: true, Documents: 1, Chunks: 1,
	}, result.Stats)

	var queued, badChunks, badStamps, goodChunks int
	require.NoError(t, ix.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM message_vectors_repair_queue
 WHERE ordinal = ? AND doc_key = 'u:s1:bad'`, ordinal).Scan(&queued))
	require.NoError(t, ix.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM message_vectors_chunks
 WHERE ordinal = ? AND doc_key = 'u:s1:bad'`, ordinal).Scan(&badChunks))
	require.NoError(t, ix.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM message_vectors_stamps
 WHERE ordinal = ? AND doc_key = 'u:s1:bad'`, ordinal).Scan(&badStamps))
	require.NoError(t, ix.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM message_vectors_chunks
 WHERE ordinal = ? AND doc_key = 'u:s1:good'`, ordinal).Scan(&goodChunks))
	assert.Equal(t, 1, queued)
	assert.Zero(t, badChunks)
	assert.Zero(t, badStamps)
	assert.Equal(t, 1, goodChunks)
}

func TestBuildRepairInvalidRejectsBackstop(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	gen := fakeGeneration("active-model")
	require.NoError(t, buildWithoutResult(ix, ctx, twoDocSource(), gen))

	_, err := ix.Build(ctx, twoDocSource(), fakeBuildEncoder(), gen, BuildOptions{
		Backstop:      true,
		RepairInvalid: true,
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "repair-invalid and backstop are mutually exclusive")
}

func TestBuildRepairInvalidRegeneratesMissingVectorRow(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := &fakeUnitSource{rows: []fakeUnit{
		{unit: userDoc("s1", "bad", 0, "bad"), endedAt: "2024-01-01T00:00:00Z"},
	}}
	gen := fakeGeneration("active-model")
	require.NoError(t, buildWithoutResult(ix, ctx, src, gen))
	ordinal, err := ix.ordinalForFingerprint(ctx, gen.Fingerprint())
	require.NoError(t, err)

	var rowID int64
	require.NoError(t, ix.db.QueryRowContext(ctx, `
SELECT vec_rowid FROM message_vectors_chunks
 WHERE ordinal = ? AND doc_key = 'u:s1:bad'`, ordinal).Scan(&rowID))
	_, err = ix.db.ExecContext(ctx,
		`DELETE FROM message_vectors_v`+fmtInt64(ordinal)+` WHERE rowid = ?`, rowID)
	require.NoError(t, err)

	result, err := ix.Build(ctx, src, fakeBuildEncoder(), gen,
		BuildOptions{RepairInvalid: true})
	require.NoError(t, err)
	assert.Equal(t, 1, result.Repair.Documents)
	assert.Equal(t, 1, result.Fill.Documents)

	var replacementRows int
	require.NoError(t, ix.db.QueryRowContext(ctx, `
SELECT COUNT(*)
  FROM message_vectors_chunks c
  JOIN message_vectors_v`+fmtInt64(ordinal)+` v ON v.rowid = c.vec_rowid
 WHERE c.ordinal = ? AND c.doc_key = 'u:s1:bad'`, ordinal).Scan(&replacementRows))
	assert.Equal(t, 1, replacementRows)
}

func TestBuildRepairInvalidResumesAffectedKeysAfterEncodeFailure(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := &fakeUnitSource{rows: []fakeUnit{
		{unit: userDoc("s1", "bad", 0, "bad"), endedAt: "2024-01-01T00:00:00Z"},
		{unit: userDoc("s1", "good", 1, "good"), endedAt: "2024-01-01T00:00:01Z"},
	}}
	gen := fakeGeneration("active-model")
	require.NoError(t, buildWithoutResult(ix, ctx, src, gen))
	ordinal, err := ix.ordinalForFingerprint(ctx, gen.Fingerprint())
	require.NoError(t, err)

	var badRowID int64
	require.NoError(t, ix.db.QueryRowContext(ctx, `
SELECT vec_rowid FROM message_vectors_chunks
 WHERE ordinal = ? AND doc_key = 'u:s1:bad'`, ordinal).Scan(&badRowID))
	_, err = ix.db.ExecContext(ctx,
		`UPDATE message_vectors_v`+fmtInt64(ordinal)+` SET embedding = ? WHERE rowid = ?`,
		make([]byte, 3*4), badRowID)
	require.NoError(t, err)
	src.rows = append(src.rows,
		fakeUnit{unit: userDoc("s1", "pending", 2, "pending"), endedAt: "2024-01-01T00:00:02Z"})

	_, err = ix.Build(ctx, src, func(context.Context, []string) ([][]float32, error) {
		return nil, errors.New("endpoint failed")
	}, gen, BuildOptions{RepairInvalid: true})
	require.ErrorContains(t, err, "endpoint failed")

	var encoded []string
	second, err := ix.Build(ctx, src, func(_ context.Context, texts []string) ([][]float32, error) {
		encoded = append(encoded, texts...)
		return fakeBuildEncoder()(ctx, texts)
	}, gen, BuildOptions{RepairInvalid: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"bad"}, encoded)
	assert.Equal(t, 1, second.Repair.Documents)
	assert.Equal(t, 1, second.Fill.Documents)

	third, err := ix.Build(ctx, src, func(context.Context, []string) ([][]float32, error) {
		t.Fatal("a completed repair must leave no durable targets")
		return nil, nil
	}, gen, BuildOptions{RepairInvalid: true})
	require.NoError(t, err)
	assert.Equal(t, RepairStats{
		Scanned: true, ScanComplete: true, RemainingKnown: true,
	}, third.Repair)
}

func TestBuildRepairInvalidQueueOwnsTargetAcrossRevisionDrift(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := &fakeUnitSource{rows: []fakeUnit{
		{unit: userDoc("s1", "bad", 0, "old content"), endedAt: "2024-01-01T00:00:00Z"},
	}}
	gen := fakeGeneration("active-model")
	require.NoError(t, buildWithoutResult(ix, ctx, src, gen))
	ordinal, err := ix.ordinalForFingerprint(ctx, gen.Fingerprint())
	require.NoError(t, err)

	var rowID int64
	require.NoError(t, ix.db.QueryRowContext(ctx, `
SELECT vec_rowid FROM message_vectors_chunks
 WHERE ordinal = ? AND doc_key = 'u:s1:bad'`, ordinal).Scan(&rowID))
	_, err = ix.db.ExecContext(ctx,
		`UPDATE message_vectors_v`+fmtInt64(ordinal)+` SET embedding = ? WHERE rowid = ?`,
		make([]byte, 3*4), rowID)
	require.NoError(t, err)

	first, err := ix.Build(ctx, src, func(_ context.Context, texts []string) ([][]float32, error) {
		require.Equal(t, []string{"old content"}, texts)
		_, updateErr := ix.db.ExecContext(ctx, `
UPDATE vector_messages
   SET content = ?, content_hash = ?
 WHERE doc_key = 'u:s1:bad'`, "new content", contentHash("new content"))
		require.NoError(t, updateErr)
		return fakeBuildEncoder()(ctx, texts)
	}, gen, BuildOptions{RepairInvalid: true})
	require.ErrorContains(t, err, "targets remain queued")
	assert.Equal(t, 1, first.Fill.Stale)
	assert.Equal(t, 1, first.Repair.Remaining)

	var encoded []string
	second, err := ix.Build(ctx, src, func(_ context.Context, texts []string) ([][]float32, error) {
		encoded = append(encoded, texts...)
		return fakeBuildEncoder()(ctx, texts)
	}, gen, BuildOptions{RepairInvalid: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"new content"}, encoded,
		"the durable queue owns the document and repairs its latest revision")
	assert.Equal(t, 1, second.Fill.Documents)
	assert.Zero(t, second.Repair.Remaining)
}

func TestBuildRepairInvalidCompletesQueuedRevisionThatBecomesEmpty(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := &fakeUnitSource{rows: []fakeUnit{
		{unit: userDoc("s1", "bad", 0, "old content"), endedAt: "2024-01-01T00:00:00Z"},
	}}
	gen := fakeGeneration("active-model")
	require.NoError(t, buildWithoutResult(ix, ctx, src, gen))
	ordinal, err := ix.ordinalForFingerprint(ctx, gen.Fingerprint())
	require.NoError(t, err)

	var rowID int64
	require.NoError(t, ix.db.QueryRowContext(ctx, `
SELECT vec_rowid FROM message_vectors_chunks
 WHERE ordinal = ? AND doc_key = 'u:s1:bad'`, ordinal).Scan(&rowID))
	_, err = ix.db.ExecContext(ctx,
		`UPDATE message_vectors_v`+fmtInt64(ordinal)+` SET embedding = ? WHERE rowid = ?`,
		make([]byte, 3*4), rowID)
	require.NoError(t, err)

	first, err := ix.Build(ctx, src, func(_ context.Context, texts []string) ([][]float32, error) {
		require.Equal(t, []string{"old content"}, texts)
		_, updateErr := ix.db.ExecContext(ctx, `
UPDATE vector_messages
   SET content = '', content_hash = ?
 WHERE doc_key = 'u:s1:bad'`, contentHash(""))
		require.NoError(t, updateErr)
		return fakeBuildEncoder()(ctx, texts)
	}, gen, BuildOptions{RepairInvalid: true})
	require.ErrorContains(t, err, "targets remain queued")
	assert.Equal(t, 1, first.Fill.Stale)

	second, err := ix.Build(ctx, src, func(context.Context, []string) ([][]float32, error) {
		t.Fatal("zero-chunk content must complete without calling the encoder")
		return nil, nil
	}, gen, BuildOptions{RepairInvalid: true})
	require.NoError(t, err)
	assert.Equal(t, 1, second.Fill.Documents)
	assert.Zero(t, second.Fill.Chunks)
	assert.Zero(t, second.Repair.Remaining)

	var queued, stamped int
	require.NoError(t, ix.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM message_vectors_repair_queue
 WHERE ordinal = ? AND doc_key = 'u:s1:bad'`, ordinal).Scan(&queued))
	require.NoError(t, ix.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM message_vectors_stamps
 WHERE ordinal = ? AND doc_key = 'u:s1:bad' AND revision = ?`,
		ordinal, contentHash("")).Scan(&stamped))
	assert.Zero(t, queued)
	assert.Equal(t, 1, stamped)
}

func TestOrdinaryBuildClearsCompletedRepairTarget(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := &fakeUnitSource{rows: []fakeUnit{
		{unit: userDoc("s1", "bad", 0, "bad"), endedAt: "2024-01-01T00:00:00Z"},
	}}
	gen := fakeGeneration("active-model")
	require.NoError(t, buildWithoutResult(ix, ctx, src, gen))
	ordinal, err := ix.ordinalForFingerprint(ctx, gen.Fingerprint())
	require.NoError(t, err)

	var rowID int64
	require.NoError(t, ix.db.QueryRowContext(ctx, `
SELECT vec_rowid FROM message_vectors_chunks
 WHERE ordinal = ? AND doc_key = 'u:s1:bad'`, ordinal).Scan(&rowID))
	_, err = ix.db.ExecContext(ctx,
		`UPDATE message_vectors_v`+fmtInt64(ordinal)+` SET embedding = ? WHERE rowid = ?`,
		make([]byte, 3*4), rowID)
	require.NoError(t, err)

	_, err = ix.Build(ctx, src, func(context.Context, []string) ([][]float32, error) {
		return nil, errors.New("endpoint failed")
	}, gen, BuildOptions{RepairInvalid: true})
	require.ErrorContains(t, err, "endpoint failed")

	result, err := ix.Build(ctx, src, fakeBuildEncoder(), gen, BuildOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, result.Fill.Documents)

	var queued int
	require.NoError(t, ix.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM message_vectors_repair_queue
 WHERE ordinal = ? AND doc_key = 'u:s1:bad'`, ordinal).Scan(&queued))
	assert.Zero(t, queued,
		"an ordinary successful save for the queued generation must complete the repair target")

	repair, err := ix.Build(ctx, src, func(context.Context, []string) ([][]float32, error) {
		t.Fatal("repair must not re-embed work completed by an ordinary build")
		return nil, nil
	}, gen, BuildOptions{RepairInvalid: true})
	require.NoError(t, err)
	assert.Equal(t, RepairStats{
		Scanned: true, ScanComplete: true, RemainingKnown: true,
	}, repair.Repair)
}

func TestOrdinaryBuildStampOnlySkipKeepsRepairTarget(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := &fakeUnitSource{rows: []fakeUnit{
		{unit: userDoc("s1", "bad", 0, "bad"), endedAt: "2024-01-01T00:00:00Z"},
	}}
	gen := fakeGeneration("active-model")
	require.NoError(t, buildWithoutResult(ix, ctx, src, gen))
	ordinal, err := ix.ordinalForFingerprint(ctx, gen.Fingerprint())
	require.NoError(t, err)

	var rowID int64
	require.NoError(t, ix.db.QueryRowContext(ctx, `
SELECT vec_rowid FROM message_vectors_chunks
 WHERE ordinal = ? AND doc_key = 'u:s1:bad'`, ordinal).Scan(&rowID))
	_, err = ix.db.ExecContext(ctx,
		`UPDATE message_vectors_v`+fmtInt64(ordinal)+` SET embedding = ? WHERE rowid = ?`,
		make([]byte, 3*4), rowID)
	require.NoError(t, err)

	_, err = ix.Build(ctx, src, func(context.Context, []string) ([][]float32, error) {
		return nil, errors.New("endpoint failed")
	}, gen, BuildOptions{RepairInvalid: true})
	require.ErrorContains(t, err, "endpoint failed")

	ordinary, err := ix.Build(ctx, src, func(context.Context, []string) ([][]float32, error) {
		return nil, &HTTPStatusError{Status: http.StatusBadRequest, Body: "input exceeds token limit"}
	}, gen, BuildOptions{FullRebuild: true})
	require.NoError(t, err)
	assert.Equal(t, 1, ordinary.Fill.Skipped)

	var queued int
	require.NoError(t, ix.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM message_vectors_repair_queue
 WHERE ordinal = ? AND doc_key = 'u:s1:bad'`, ordinal).Scan(&queued))
	assert.Equal(t, 1, queued, "a stamp without vectors must not complete repair")

	repair, err := ix.Build(ctx, src, fakeBuildEncoder(), gen,
		BuildOptions{RepairInvalid: true})
	require.NoError(t, err)
	assert.Equal(t, 1, repair.Fill.Documents)
	assert.Zero(t, repair.Repair.Remaining)
}

func TestBuildRepairInvalidKeepsTargetAfterPermanentEncodeFailure(t *testing.T) {
	assertFailedRepairRemainsQueued(t, &HTTPStatusError{
		Status: http.StatusBadRequest,
		Body:   "input exceeds token limit",
	})
}

func TestBuildRepairInvalidKeepsTargetAfterContextDeadline(t *testing.T) {
	assertFailedRepairRemainsQueued(t, context.DeadlineExceeded)
}

func assertFailedRepairRemainsQueued(t *testing.T, encodeErr error) {
	t.Helper()
	ix := openTestIndex(t)
	ctx := context.Background()
	src := &fakeUnitSource{rows: []fakeUnit{
		{unit: userDoc("s1", "bad", 0, "bad"), endedAt: "2024-01-01T00:00:00Z"},
	}}
	gen := fakeGeneration("active-model")
	require.NoError(t, buildWithoutResult(ix, ctx, src, gen))
	ordinal, err := ix.ordinalForFingerprint(ctx, gen.Fingerprint())
	require.NoError(t, err)

	var rowID int64
	require.NoError(t, ix.db.QueryRowContext(ctx, `
SELECT vec_rowid FROM message_vectors_chunks
 WHERE ordinal = ? AND doc_key = 'u:s1:bad'`, ordinal).Scan(&rowID))
	_, err = ix.db.ExecContext(ctx,
		`UPDATE message_vectors_v`+fmtInt64(ordinal)+` SET embedding = ? WHERE rowid = ?`,
		make([]byte, 3*4), rowID)
	require.NoError(t, err)

	failed, err := ix.Build(ctx, src, func(context.Context, []string) ([][]float32, error) {
		return nil, encodeErr
	}, gen, BuildOptions{RepairInvalid: true})
	require.Error(t, err)
	assert.Equal(t, 1, failed.Repair.Failed)
	assert.Equal(t, 1, failed.Repair.Remaining)

	var queued, stamped int
	require.NoError(t, ix.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM message_vectors_repair_queue
 WHERE ordinal = ? AND doc_key = 'u:s1:bad'`, ordinal).Scan(&queued))
	assert.Equal(t, 1, queued, "an unsuccessful replacement must remain queued")
	require.NoError(t, ix.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM message_vectors_stamps
 WHERE ordinal = ? AND doc_key = 'u:s1:bad'`, ordinal).Scan(&stamped))
	assert.Zero(t, stamped, "an unsuccessful replacement must remain unstamped")

	result, err := ix.Build(ctx, src, fakeBuildEncoder(), gen,
		BuildOptions{RepairInvalid: true})
	require.NoError(t, err)
	assert.Equal(t, 1, result.Fill.Documents)
	require.NoError(t, ix.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM message_vectors_repair_queue
 WHERE ordinal = ? AND doc_key = 'u:s1:bad'`, ordinal).Scan(&queued))
	assert.Zero(t, queued)
}

func TestBuildRepairInvalidContinuesAfterPermanentTargetFailure(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := &fakeUnitSource{rows: []fakeUnit{
		{unit: userDoc("s1", "a-bad", 0, "bad"), endedAt: "2024-01-01T00:00:00Z"},
		{unit: userDoc("s1", "z-good", 1, "good"), endedAt: "2024-01-01T00:00:01Z"},
	}}
	gen := fakeGeneration("active-model")
	require.NoError(t, buildWithoutResult(ix, ctx, src, gen))
	ordinal, err := ix.ordinalForFingerprint(ctx, gen.Fingerprint())
	require.NoError(t, err)
	_, err = ix.db.ExecContext(ctx,
		`UPDATE message_vectors_v`+fmtInt64(ordinal)+` SET embedding = ?`, make([]byte, 3*4))
	require.NoError(t, err)

	result, err := ix.Build(ctx, src, func(_ context.Context, texts []string) ([][]float32, error) {
		if texts[0] == "bad" {
			return nil, &HTTPStatusError{
				Status: http.StatusBadRequest,
				Body:   "input exceeds token limit",
			}
		}
		return fakeBuildEncoder()(ctx, texts)
	}, gen, BuildOptions{RepairInvalid: true})
	require.ErrorContains(t, err, "1 permanently rejected")
	assert.Equal(t, 1, result.Fill.Documents, "later repair targets must still be attempted")
	assert.Equal(t, 1, result.Repair.Failed)
	assert.Equal(t, 1, result.Repair.Remaining)

	var queuedBad, queuedGood, stampedBad, stampedGood int
	require.NoError(t, ix.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM message_vectors_repair_queue
 WHERE ordinal = ? AND doc_key = 'u:s1:a-bad'`, ordinal).Scan(&queuedBad))
	require.NoError(t, ix.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM message_vectors_repair_queue
 WHERE ordinal = ? AND doc_key = 'u:s1:z-good'`, ordinal).Scan(&queuedGood))
	require.NoError(t, ix.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM message_vectors_stamps
 WHERE ordinal = ? AND doc_key = 'u:s1:a-bad'`, ordinal).Scan(&stampedBad))
	require.NoError(t, ix.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM message_vectors_stamps
 WHERE ordinal = ? AND doc_key = 'u:s1:z-good'`, ordinal).Scan(&stampedGood))
	assert.Equal(t, 1, queuedBad)
	assert.Zero(t, queuedGood)
	assert.Zero(t, stampedBad)
	assert.Equal(t, 1, stampedGood)
}

func TestBuildRepairInvalidUsesConfiguredDocumentConcurrency(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := &fakeUnitSource{rows: []fakeUnit{
		{unit: userDoc("s1", "one", 0, "one"), endedAt: "2024-01-01T00:00:00Z"},
		{unit: userDoc("s1", "two", 1, "two"), endedAt: "2024-01-01T00:00:01Z"},
	}}
	gen := fakeGeneration("active-model")
	require.NoError(t, buildWithoutResult(ix, ctx, src, gen))
	ordinal, err := ix.ordinalForFingerprint(ctx, gen.Fingerprint())
	require.NoError(t, err)
	_, err = ix.db.ExecContext(ctx,
		`UPDATE message_vectors_v`+fmtInt64(ordinal)+` SET embedding = ?`, make([]byte, 3*4))
	require.NoError(t, err)

	started := make(chan struct{}, 2)
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	var active, peak atomic.Int32
	encoder := func(_ context.Context, texts []string) ([][]float32, error) {
		current := active.Add(1)
		for {
			previous := peak.Load()
			if current <= previous || peak.CompareAndSwap(previous, current) {
				break
			}
		}
		started <- struct{}{}
		<-release
		active.Add(-1)
		return fakeBuildEncoder()(ctx, texts)
	}
	type buildOutcome struct {
		result BuildResult
		err    error
	}
	done := make(chan buildOutcome, 1)
	go func() {
		result, err := ix.Build(ctx, src, encoder, gen,
			BuildOptions{RepairInvalid: true, Concurrency: 2})
		done <- buildOutcome{result: result, err: err}
	}()
	for range 2 {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			close(release)
			released = true
			require.FailNow(t, "repair did not start two document encodes concurrently")
		}
	}
	close(release)
	released = true
	outcome := <-done
	require.NoError(t, outcome.err)
	assert.Equal(t, int32(2), peak.Load())
	assert.Equal(t, 2, outcome.result.Fill.Documents)
}

func TestBuildRepairInvalidRegeneratesPartiallyMissingChunkMap(t *testing.T) {
	ix := openTestIndex(t)
	ix.split = kitvec.SplitOptions{MaxRunes: 5}
	ctx := context.Background()
	src := &fakeUnitSource{rows: []fakeUnit{
		{unit: userDoc("s1", "bad", 0, "abcdefghij"), endedAt: "2024-01-01T00:00:00Z"},
	}}
	gen := fakeGeneration("active-model")
	require.NoError(t, buildWithoutResult(ix, ctx, src, gen))
	ordinal, err := ix.ordinalForFingerprint(ctx, gen.Fingerprint())
	require.NoError(t, err)

	var rowID int64
	require.NoError(t, ix.db.QueryRowContext(ctx, `
SELECT vec_rowid FROM message_vectors_chunks
 WHERE ordinal = ? AND doc_key = 'u:s1:bad' AND chunk_index = 1`, ordinal).Scan(&rowID))
	_, err = ix.db.ExecContext(ctx, `
DELETE FROM message_vectors_chunks
 WHERE ordinal = ? AND doc_key = 'u:s1:bad' AND chunk_index = 1`, ordinal)
	require.NoError(t, err)
	_, err = ix.db.ExecContext(ctx,
		`DELETE FROM message_vectors_v`+fmtInt64(ordinal)+` WHERE rowid = ?`, rowID)
	require.NoError(t, err)

	result, err := ix.Build(ctx, src, fakeBuildEncoder(), gen,
		BuildOptions{RepairInvalid: true})
	require.NoError(t, err)
	assert.Equal(t, 1, result.Repair.Documents)
	assert.Equal(t, 1, result.Fill.Documents)
	assert.Equal(t, 2, result.Fill.Chunks)
}

func TestBuildRepairInvalidResumesAfterLaterScanBatchFails(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	const documentCount = repairScanDocumentBatch + 1
	src := &fakeUnitSource{rows: make([]fakeUnit, 0, documentCount)}
	for i := range documentCount {
		id := fmt.Sprintf("doc-%03d", i)
		src.rows = append(src.rows, fakeUnit{
			unit:    userDoc("s1", id, i, id),
			endedAt: fmt.Sprintf("2024-01-01T00:%02d:%02dZ", i/60, i%60),
		})
	}
	gen := fakeGeneration("active-model")
	require.NoError(t, buildWithoutResult(ix, ctx, src, gen))
	ordinal, err := ix.ordinalForFingerprint(ctx, gen.Fingerprint())
	require.NoError(t, err)
	_, err = ix.db.ExecContext(ctx,
		`UPDATE message_vectors_v`+fmtInt64(ordinal)+` SET embedding = ?`, make([]byte, 3*4))
	require.NoError(t, err)
	_, err = ix.db.ExecContext(ctx, `
CREATE TRIGGER fail_second_repair_batch
BEFORE INSERT ON message_vectors_repair_queue
WHEN NEW.doc_key = 'u:s1:doc-128'
BEGIN
    SELECT RAISE(ABORT, 'injected later repair batch failure');
END`)
	require.NoError(t, err)

	partial, err := ix.Build(ctx, src, fakeBuildEncoder(), gen,
		BuildOptions{RepairInvalid: true})
	require.ErrorContains(t, err, "injected later repair batch failure")
	assert.True(t, partial.Repair.Scanned)
	assert.False(t, partial.Repair.ScanComplete)
	assert.Equal(t, repairScanDocumentBatch, partial.Repair.Documents)
	assert.Equal(t, repairScanDocumentBatch, partial.Repair.Remaining)
	assert.True(t, partial.Repair.RemainingKnown)
	var queued int
	require.NoError(t, ix.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM message_vectors_repair_queue WHERE ordinal = ?`, ordinal).Scan(&queued))
	assert.Equal(t, repairScanDocumentBatch, queued,
		"the first committed scan batch must remain durably queued")
	_, err = ix.db.ExecContext(ctx, `DROP TRIGGER fail_second_repair_batch`)
	require.NoError(t, err)

	result, err := ix.Build(ctx, src, fakeBuildEncoder(), gen,
		BuildOptions{RepairInvalid: true})
	require.NoError(t, err)
	assert.True(t, result.Repair.ScanComplete)
	assert.Equal(t, documentCount, result.Repair.Documents)
	assert.Equal(t, documentCount, result.Fill.Documents)

	require.NoError(t, ix.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM message_vectors_repair_queue WHERE ordinal = ?`, ordinal).Scan(&queued))
	assert.Zero(t, queued)
}

func TestRepairRemainingAfterScanErrorIgnoresCanceledBuildContext(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	gen := fakeGeneration("active-model")
	require.NoError(t, buildWithoutResult(ix, ctx, twoDocSource(), gen))
	ordinal, err := ix.ordinalForFingerprint(ctx, gen.Fingerprint())
	require.NoError(t, err)
	_, err = ix.db.ExecContext(ctx, `
INSERT INTO message_vectors_repair_queue (ordinal, doc_key)
VALUES (?, 'u:s1:u1')`, ordinal)
	require.NoError(t, err)

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	store := &repairStore{
		base: ix.store, db: ix.db, spec: ix.spec, fingerprint: gen.Fingerprint(),
		ordinal: ordinal, queueTable: ix.spec.repairQueueTable(), split: ix.split,
	}
	remaining, known, err := repairRemainingAfterScanError(canceled, store, 7)
	require.NoError(t, err)
	assert.True(t, known)
	assert.Equal(t, 1, remaining,
		"the recount must survive cancellation instead of reporting zero or only the fallback")
}

func TestRepairRemainingAfterScanErrorReturnsFallbackWhenRecountFails(t *testing.T) {
	raw, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "closed.db"))
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	store := &repairStore{db: raw, ordinal: 1, queueTable: "repair_queue"}

	remaining, known, err := repairRemainingAfterScanError(context.Background(), store, 7)
	require.Error(t, err)
	assert.Equal(t, 7, remaining)
	assert.False(t, known)
}

func TestBuildRepairInvalidRetriesAfterQueueCleanupFailure(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := &fakeUnitSource{rows: []fakeUnit{
		{unit: userDoc("s1", "saved", 0, "saved"), endedAt: "2024-01-01T00:00:00Z"},
	}}
	gen := fakeGeneration("active-model")
	require.NoError(t, buildWithoutResult(ix, ctx, src, gen))
	ordinal, err := ix.ordinalForFingerprint(ctx, gen.Fingerprint())
	require.NoError(t, err)
	var rowID int64
	require.NoError(t, ix.db.QueryRowContext(ctx, `
SELECT vec_rowid FROM message_vectors_chunks
 WHERE ordinal = ? AND doc_key = 'u:s1:saved'`, ordinal).Scan(&rowID))
	_, err = ix.db.ExecContext(ctx,
		`UPDATE message_vectors_v`+fmtInt64(ordinal)+` SET embedding = ? WHERE rowid = ?`,
		make([]byte, 3*4), rowID)
	require.NoError(t, err)
	repair, err := ix.repairInvalidVectors(ctx, gen.Fingerprint())
	require.NoError(t, err)
	require.Equal(t, 1, repair.Stats.Documents)
	_, err = ix.db.ExecContext(ctx, `
CREATE TRIGGER fail_repair_queue_cleanup
BEFORE DELETE ON message_vectors_repair_queue
WHEN OLD.doc_key = 'u:s1:saved'
BEGIN
    SELECT RAISE(ABORT, 'injected repair queue cleanup failure');
END`)
	require.NoError(t, err)
	store := &repairStore{
		base: ix.store, db: ix.db, spec: ix.spec, fingerprint: gen.Fingerprint(),
		ordinal: ordinal, queueTable: ix.spec.repairQueueTable(), split: ix.split,
	}
	_, err = fillRepairQueue(ctx, store, gen.Fingerprint(), fakeBuildEncoder(),
		repairFillOptions{Split: ix.split})
	require.ErrorContains(t, err, "injected repair queue cleanup failure")
	_, err = ix.db.ExecContext(ctx, `DROP TRIGGER fail_repair_queue_cleanup`)
	require.NoError(t, err)

	var encoded []string
	result, err := ix.Build(ctx, src, func(_ context.Context, texts []string) ([][]float32, error) {
		encoded = append(encoded, texts...)
		return fakeBuildEncoder()(ctx, texts)
	}, gen, BuildOptions{RepairInvalid: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"saved"}, encoded)
	assert.Equal(t, 1, result.Fill.Documents)

	var queued int
	require.NoError(t, ix.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM message_vectors_repair_queue
 WHERE ordinal = ? AND doc_key = 'u:s1:saved'`, ordinal).Scan(&queued))
	assert.Zero(t, queued)
}

func TestBuildRepairInvalidIgnoresOrdinaryPendingContentChange(t *testing.T) {
	ix := openTestIndex(t)
	ix.split = kitvec.SplitOptions{MaxRunes: 5}
	ctx := context.Background()
	src := &fakeUnitSource{rows: []fakeUnit{
		{unit: userDoc("s1", "changed", 0, "short"), endedAt: "2024-01-01T00:00:00Z"},
	}}
	gen := fakeGeneration("active-model")
	require.NoError(t, buildWithoutResult(ix, ctx, src, gen))

	src.rows[0] = fakeUnit{
		unit:    userDoc("s1", "changed", 0, "abcdefghij"),
		endedAt: "2024-01-01T00:00:01Z",
	}
	_, err := ix.Build(ctx, src, func(context.Context, []string) ([][]float32, error) {
		return nil, errors.New("leave changed document pending")
	}, gen, BuildOptions{})
	require.ErrorContains(t, err, "leave changed document pending")

	result, err := ix.Build(ctx, src, func(context.Context, []string) ([][]float32, error) {
		t.Fatal("repair must not embed an ordinary pending content change")
		return nil, nil
	}, gen, BuildOptions{RepairInvalid: true})
	require.NoError(t, err)
	assert.Equal(t, RepairStats{
		Scanned: true, ScanComplete: true, RemainingKnown: true,
	}, result.Repair)
	assert.Zero(t, result.Fill.Documents)
}

func TestBackstopRemovesRepairQueueEntryForDeletedDocument(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := &fakeUnitSource{rows: []fakeUnit{
		{unit: userDoc("s1", "bad", 0, "bad"), endedAt: "2024-01-01T00:00:00Z"},
		{unit: userDoc("s1", "good", 1, "good"), endedAt: "2024-01-01T00:00:01Z"},
	}}
	gen := fakeGeneration("active-model")
	require.NoError(t, buildWithoutResult(ix, ctx, src, gen))
	ordinal, err := ix.ordinalForFingerprint(ctx, gen.Fingerprint())
	require.NoError(t, err)

	var badRowID int64
	require.NoError(t, ix.db.QueryRowContext(ctx, `
SELECT vec_rowid FROM message_vectors_chunks
 WHERE ordinal = ? AND doc_key = 'u:s1:bad'`, ordinal).Scan(&badRowID))
	_, err = ix.db.ExecContext(ctx,
		`UPDATE message_vectors_v`+fmtInt64(ordinal)+` SET embedding = ? WHERE rowid = ?`,
		make([]byte, 3*4), badRowID)
	require.NoError(t, err)

	_, err = ix.Build(ctx, src, func(context.Context, []string) ([][]float32, error) {
		return nil, errors.New("endpoint failed")
	}, gen, BuildOptions{RepairInvalid: true})
	require.ErrorContains(t, err, "endpoint failed")

	var queued int
	require.NoError(t, ix.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM message_vectors_repair_queue
 WHERE ordinal = ? AND doc_key = 'u:s1:bad'`, ordinal).Scan(&queued))
	require.Equal(t, 1, queued)

	src.rows = src.rows[1:]
	_, err = ix.Build(ctx, src, fakeBuildEncoder(), gen, BuildOptions{Backstop: true})
	require.NoError(t, err)

	require.NoError(t, ix.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM message_vectors_repair_queue
 WHERE ordinal = ? AND doc_key = 'u:s1:bad'`, ordinal).Scan(&queued))
	assert.Zero(t, queued, "deleting a mirror document must clear its repair target")
}

func TestRepairQueueDocumentCleanupUsesDocumentKeyIndex(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	rows, err := ix.db.QueryContext(ctx, `
EXPLAIN QUERY PLAN
DELETE FROM message_vectors_repair_queue WHERE doc_key = ?`, "u:s1:deleted")
	require.NoError(t, err)
	defer rows.Close()

	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		details = append(details, detail)
	}
	require.NoError(t, rows.Err())
	assert.Contains(t, strings.Join(details, "\n"),
		"message_vectors_repair_queue_doc_key",
		"mirror cleanup must not scan the ordinal-first repair queue")
}

func TestOrdinaryRepairCompletionUsesQueuePrimaryKey(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	rows, err := ix.db.QueryContext(ctx, `
EXPLAIN QUERY PLAN
DELETE FROM message_vectors_repair_queue
 WHERE doc_key = ?
   AND ordinal = (
       SELECT ordinal FROM message_vectors_generations WHERE gen_key = ?)`,
		"u:s1:done", "fingerprint")
	require.NoError(t, err)
	defer rows.Close()

	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		details = append(details, detail)
	}
	require.NoError(t, rows.Err())
	assert.Contains(t, strings.Join(details, "\n"),
		"sqlite_autoindex_message_vectors_repair_queue_1",
		"ordinary saves must add only one indexed queue lookup")
}

func float32Blob(values ...float32) []byte {
	blob := make([]byte, 4*len(values))
	for i, value := range values {
		binary.LittleEndian.PutUint32(blob[i*4:], math.Float32bits(value))
	}
	return blob
}

func buildWithoutResult(
	ix *Index, ctx context.Context, src UnitSource, gen kitvec.Generation,
) error {
	_, err := ix.Build(ctx, src, fakeBuildEncoder(), gen, BuildOptions{})
	return err
}

func fmtInt64(value int64) string {
	return strconv.FormatInt(value, 10)
}
