package vector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"

	kitvec "go.kenn.io/kit/vector"
)

// RepairStats reports the target set prepared for a repair-only fill.
type RepairStats struct {
	Scanned bool `json:"scanned"`
	// ScanComplete distinguishes a fully traversed integrity scan from one
	// that stopped after committing one or more bounded invalidation batches.
	ScanComplete bool `json:"scan_complete"`
	// Documents includes unfinished targets resumed from the durable queue and
	// newly invalidated documents discovered by this scan.
	Documents int `json:"documents"`
	// Chunks counts mappings invalidated by this scan. Resumed targets were
	// invalidated by an earlier run and therefore do not add to this count.
	Chunks int `json:"chunks"`
	// Failed counts non-stale target encode/save failures that determine or
	// accompany this invocation's failure. Stale saves are reported by
	// Fill.Stale, and sibling work canceled after another failure is not counted.
	Failed int `json:"failed"`
	// Remaining is the durable target count after this run's fill attempt.
	Remaining int `json:"remaining"`
	// RemainingKnown is false only when the bounded queue recount failed and
	// Remaining is therefore the accumulated committed-count fallback.
	RemainingKnown bool `json:"remaining_known"`
}

type repairResult struct {
	Stats   RepairStats
	Ordinal int64
}

const repairScanDocumentBatch = 128

// repairInvalidVectors removes target-generation state only for documents
// containing at least one unusable stored vector. Stamp removal makes those
// documents pending for the restricted Fill that follows in Build.
func (ix *Index) repairInvalidVectors(
	ctx context.Context, fingerprint string,
) (repairResult, error) {
	var ordinal int64
	var dimension int
	err := ix.db.QueryRowContext(ctx,
		`SELECT ordinal, dimension FROM `+ix.spec.generationsTable()+` WHERE gen_key = ?`,
		fingerprint,
	).Scan(&ordinal, &dimension)
	if err != nil {
		return repairResult{}, fmt.Errorf(
			"lookup generation for invalid vector repair %s: %w", fingerprint, err)
	}
	result := repairResult{Stats: RepairStats{Scanned: true}, Ordinal: ordinal}
	vecTable := fmt.Sprintf("%s_v%d", ix.spec.VectorsPrefix, ordinal)
	if _, err := ix.db.ExecContext(ctx, `
DELETE FROM `+ix.spec.repairQueueTable()+` AS q
 WHERE q.ordinal = ?
   AND NOT EXISTS (
       SELECT 1 FROM `+ix.spec.DocsTable+` d WHERE d.doc_key = q.doc_key)`, ordinal); err != nil {
		return result, fmt.Errorf("prune orphaned invalid vector repair targets: %w", err)
	}
	if err := ix.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM `+ix.spec.repairQueueTable()+` WHERE ordinal = ?`, ordinal,
	).Scan(&result.Stats.Documents); err != nil {
		return result, fmt.Errorf("count queued invalid vector repairs: %w", err)
	}

	lastDocKey := ""
	for {
		documents, err := ix.repairScanBatchKeys(ctx, ordinal, lastDocKey)
		if err != nil {
			return result, err
		}
		if len(documents) == 0 {
			break
		}
		lastDocKey = documents[len(documents)-1]
		affected, err := ix.scanInvalidRepairDocuments(
			ctx, ordinal, dimension, vecTable, documents,
		)
		if err != nil {
			return result, err
		}
		batch, err := ix.invalidateRepairDocuments(ctx, ordinal, vecTable, affected)
		if err != nil {
			return result, err
		}
		result.Stats.Documents += batch.Documents
		result.Stats.Chunks += batch.Chunks
	}
	result.Stats.ScanComplete = true
	return result, nil
}

func (ix *Index) repairScanBatchKeys(
	ctx context.Context, ordinal int64, after string,
) ([]string, error) {
	rows, err := ix.db.QueryContext(ctx, `
SELECT c.doc_key
  FROM `+ix.spec.chunksTable()+` c
  JOIN `+ix.spec.DocsTable+` d ON d.doc_key = c.doc_key
  JOIN `+ix.spec.stampsTable()+` s
    ON s.ordinal = c.ordinal AND s.doc_key = c.doc_key
   AND s.revision IS d.content_hash
 WHERE c.ordinal = ? AND c.doc_key > ?
 GROUP BY c.doc_key
 ORDER BY c.doc_key
 LIMIT ?`, ordinal, after, repairScanDocumentBatch)
	if err != nil {
		return nil, fmt.Errorf("scan target generation document keys: %w", err)
	}
	defer rows.Close()

	documents := make([]string, 0, repairScanDocumentBatch)
	for rows.Next() {
		var docKey string
		if err := rows.Scan(&docKey); err != nil {
			return nil, fmt.Errorf("scan target generation document key: %w", err)
		}
		documents = append(documents, docKey)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate target generation document keys: %w", err)
	}
	return documents, nil
}

func (ix *Index) scanInvalidRepairDocuments(
	ctx context.Context, ordinal int64, dimension int, vecTable string, documents []string,
) ([]string, error) {
	if len(documents) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(documents)+1)
	args = append(args, ordinal)
	for _, docKey := range documents {
		args = append(args, docKey)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(documents)), ",")
	type documentState struct {
		expected int
		seen     int
		invalid  bool
	}
	states := make(map[string]*documentState, len(documents))
	contentRows, err := ix.db.QueryContext(ctx, `
SELECT d.doc_key, d.content
  FROM `+ix.spec.DocsTable+` d
  JOIN `+ix.spec.stampsTable()+` s
    ON s.doc_key = d.doc_key AND s.ordinal = ?
   AND s.revision IS d.content_hash
 WHERE d.doc_key IN (`+placeholders+`)
 ORDER BY d.doc_key`, args...)
	if err != nil {
		return nil, fmt.Errorf("read repair document content: %w", err)
	}
	for contentRows.Next() {
		var docKey, content string
		if err := contentRows.Scan(&docKey, &content); err != nil {
			contentRows.Close()
			return nil, fmt.Errorf("scan repair document content: %w", err)
		}
		states[docKey] = &documentState{expected: len(kitvec.Split(content, ix.split))}
	}
	if err := contentRows.Err(); err != nil {
		contentRows.Close()
		return nil, fmt.Errorf("iterate repair document content: %w", err)
	}
	if err := contentRows.Close(); err != nil {
		return nil, fmt.Errorf("close repair document content scan: %w", err)
	}

	rows, err := ix.db.QueryContext(ctx, `
SELECT c.doc_key, c.chunk_index, v.embedding
  FROM `+ix.spec.chunksTable()+` c
  JOIN `+ix.spec.DocsTable+` d ON d.doc_key = c.doc_key
  JOIN `+ix.spec.stampsTable()+` s
    ON s.ordinal = c.ordinal AND s.doc_key = c.doc_key
   AND s.revision IS d.content_hash
  LEFT JOIN `+vecTable+` v ON v.rowid = c.vec_rowid
 WHERE c.ordinal = ? AND c.doc_key IN (`+placeholders+`)
 ORDER BY c.doc_key, c.chunk_index`, args...)
	if err != nil {
		return nil, fmt.Errorf("scan target generation vectors: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var docKey string
		var chunkIndex int
		var blob []byte
		if err := rows.Scan(&docKey, &chunkIndex, &blob); err != nil {
			return nil, fmt.Errorf("scan target generation vector: %w", err)
		}
		state := states[docKey]
		if state == nil {
			continue
		}
		if state.seen >= state.expected || state.seen != chunkIndex {
			state.invalid = true
		}
		state.seen++
		if validateStoredEmbeddingBlob(blob, dimension) != nil {
			state.invalid = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate target generation vectors: %w", err)
	}
	var affected []string
	for _, docKey := range documents {
		state := states[docKey]
		if state != nil && (state.invalid || state.seen != state.expected) {
			affected = append(affected, docKey)
		}
	}
	return affected, nil
}

func (ix *Index) invalidateRepairDocuments(
	ctx context.Context, ordinal int64, vecTable string, documents []string,
) (RepairStats, error) {
	if len(documents) == 0 {
		return RepairStats{}, nil
	}
	tx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return RepairStats{}, fmt.Errorf("begin invalid vector repair batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var stats RepairStats
	for _, docKey := range documents {
		inserted, err := tx.ExecContext(ctx, `
INSERT INTO `+ix.spec.repairQueueTable()+` (ordinal, doc_key) VALUES (?, ?)
ON CONFLICT(ordinal, doc_key) DO NOTHING`, ordinal, docKey)
		if err != nil {
			return RepairStats{}, fmt.Errorf("queue invalid document %s for repair: %w", docKey, err)
		}
		count, err := inserted.RowsAffected()
		if err != nil {
			return RepairStats{}, fmt.Errorf("count queued invalid document %s: %w", docKey, err)
		}
		stats.Documents += int(count)
		rowids, err := repairDocumentRowIDs(ctx, tx, ix.spec, ordinal, docKey)
		if err != nil {
			return RepairStats{}, err
		}
		stats.Chunks += len(rowids)
		for _, rowid := range rowids {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM `+vecTable+` WHERE rowid = ?`, rowid); err != nil {
				return RepairStats{}, fmt.Errorf("delete invalid vector for %s: %w", docKey, err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM `+ix.spec.chunksTable()+` WHERE ordinal = ? AND doc_key = ?`,
			ordinal, docKey); err != nil {
			return RepairStats{}, fmt.Errorf("delete invalid chunk map for %s: %w", docKey, err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM `+ix.spec.stampsTable()+` WHERE ordinal = ? AND doc_key = ?`,
			ordinal, docKey); err != nil {
			return RepairStats{}, fmt.Errorf("delete invalid vector stamp for %s: %w", docKey, err)
		}

		// SaveVectors removes all generations when embed_gen points to an
		// unstamped generation (the normal content-invalidation case). If this
		// unchanged document has another revision-current generation, point to
		// it temporarily so the following fill replaces only target vectors.
		if _, err := tx.ExecContext(ctx, `
UPDATE `+ix.spec.DocsTable+` AS d
   SET embed_gen = (
       SELECT g.gen_key
         FROM `+ix.spec.stampsTable()+` s
         JOIN `+ix.spec.generationsTable()+` g ON g.ordinal = s.ordinal
        WHERE s.doc_key = d.doc_key
          AND s.ordinal <> ?
          AND s.revision IS d.content_hash
        ORDER BY s.ordinal DESC
        LIMIT 1)
 WHERE d.doc_key = ?
   AND EXISTS (
       SELECT 1
         FROM `+ix.spec.stampsTable()+` s
        WHERE s.doc_key = d.doc_key
          AND s.ordinal <> ?
          AND s.revision IS d.content_hash)`, ordinal, docKey, ordinal); err != nil {
			return RepairStats{}, fmt.Errorf("preserve fallback generation for %s: %w", docKey, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return RepairStats{}, fmt.Errorf("commit invalid vector repair batch: %w", err)
	}
	return stats, nil
}

func validateStoredEmbeddingBlob(blob []byte, dimension int) error {
	vector, err := decodeFloat32Blob(blob)
	if err != nil {
		return fmt.Errorf("invalid embedding byte length: %w", err)
	}
	if len(vector) != dimension {
		return fmt.Errorf("invalid embedding dimension: got %d, want %d", len(vector), dimension)
	}
	return validateEmbedding(vector, 0)
}

// repairStore restricts kit's fill loop to the documents found by the repair
// scan. Other legitimately pending documents remain untouched for a later
// ordinary incremental build.
type repairStore struct {
	base        kitvec.Store[string, string]
	db          *sql.DB
	spec        IndexSpec
	fingerprint string
	ordinal     int64
	queueTable  string
	split       kitvec.SplitOptions
}

// repairQueueCompletingStore lets an ordinary build satisfy a durable repair
// target when it saves non-empty replacement vectors for that same generation.
// Without this, a later repair invocation would re-embed work the ordinary
// build already completed. Stamp-only saves intentionally leave the target
// queued: they do not replace the invalid vectors repair was asked to restore.
type repairQueueCompletingStore struct {
	kitvec.Store[string, string]
	db   *sql.DB
	spec IndexSpec
}

func (s *repairQueueCompletingStore) SaveVectors(
	ctx context.Context, gen, doc string, revision any, vectors []kitvec.ChunkVector,
) error {
	if err := s.Store.SaveVectors(ctx, gen, doc, revision, vectors); err != nil {
		return err
	}
	if len(vectors) == 0 {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `
DELETE FROM `+s.spec.repairQueueTable()+`
 WHERE doc_key = ?
   AND ordinal = (
       SELECT ordinal FROM `+s.spec.generationsTable()+` WHERE gen_key = ?)`, doc, gen); err != nil {
		return fmt.Errorf("complete invalid vector repair during ordinary build for %s: %w", doc, err)
	}
	return nil
}

func (s *repairStore) pendingAfter(
	ctx context.Context, gen string, after string, limit int,
) ([]kitvec.Pending[string], error) {
	if gen != s.fingerprint {
		return nil, fmt.Errorf("repair store generation %s does not match target %s", gen, s.fingerprint)
	}
	if limit <= 0 {
		return nil, nil
	}

	rows, err := s.db.QueryContext(ctx, `
SELECT d.doc_key, d.content, d.content_hash
  FROM `+s.spec.DocsTable+` d
  JOIN `+s.queueTable+` q ON q.doc_key = d.doc_key AND q.ordinal = ?
 WHERE d.doc_key > ?
 ORDER BY d.doc_key
 LIMIT ?`, s.ordinal, after, limit)
	if err != nil {
		return nil, fmt.Errorf("scan repair documents: %w", err)
	}
	var pending []kitvec.Pending[string]
	for rows.Next() {
		var p kitvec.Pending[string]
		if err := rows.Scan(&p.Doc, &p.Content, &p.Revision); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan repair document: %w", err)
		}
		pending = append(pending, p)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate repair documents: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close repair document scan: %w", err)
	}
	return pending, nil
}

func (s *repairStore) countTargets(ctx context.Context) (int, error) {
	var count int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM `+s.queueTable+` WHERE ordinal = ?`, s.ordinal,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("count remaining invalid vector repair targets: %w", err)
	}
	return count, nil
}

func (s *repairStore) countPendingChunks(
	ctx context.Context, split kitvec.SplitOptions,
) (int64, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT d.content
  FROM `+s.spec.DocsTable+` d
  JOIN `+s.queueTable+` q ON q.doc_key = d.doc_key AND q.ordinal = ?
 ORDER BY d.doc_key`, s.ordinal)
	if err != nil {
		return 0, fmt.Errorf("count repair documents: %w", err)
	}
	defer rows.Close()

	var total int64
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			return 0, fmt.Errorf("scan repair document content: %w", err)
		}
		total += int64(len(kitvec.Split(content, split)))
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate repair document content: %w", err)
	}
	return total, nil
}

func (s *repairStore) SaveVectors(
	ctx context.Context, gen, doc string, revision any, vectors []kitvec.ChunkVector,
) error {
	if len(vectors) == 0 {
		var content string
		err := s.db.QueryRowContext(ctx, `
SELECT content FROM `+s.spec.DocsTable+`
 WHERE doc_key = ? AND content_hash IS ?`, doc, revision).Scan(&content)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("validate empty invalid vector repair for %s: %w", doc, err)
		}
		if err == nil && len(kitvec.Split(content, s.split)) > 0 {
			return fmt.Errorf("refusing to complete invalid vector repair for %s without replacement vectors", doc)
		}
	}
	if err := s.base.SaveVectors(ctx, gen, doc, revision, vectors); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM `+s.queueTable+` WHERE ordinal = ? AND doc_key = ?`, s.ordinal, doc); err != nil {
		return fmt.Errorf("complete invalid vector repair for %s: %w", doc, err)
	}
	return nil
}

type repairFillOptions struct {
	Split       kitvec.SplitOptions
	Batch       kitvec.BatchOptions
	Concurrency int
}

type repairFillOutcome struct {
	Stats        kitvec.FillStats
	Failed       int
	FirstFailure error
}

type repairEncoded struct {
	doc     kitvec.Pending[string]
	chunks  []kitvec.Chunk
	vectors []kitvec.Vector
	err     error
}

// fillRepairQueue attempts every target that was queued when the invocation
// began. Its keyset cursor lets permanently rejected targets remain queued
// without starving later keys or accumulating an invocation-sized skip set.
func fillRepairQueue(
	ctx context.Context, store *repairStore, gen string, enc kitvec.EncodeFunc,
	o repairFillOptions,
) (repairFillOutcome, error) {
	var outcome repairFillOutcome
	after := ""
	for {
		pending, err := store.pendingAfter(ctx, gen, after, repairScanDocumentBatch)
		if err != nil {
			return outcome, err
		}
		if len(pending) == 0 {
			return outcome, nil
		}
		after = pending[len(pending)-1].Doc
		page, err := fillRepairPage(ctx, store, gen, enc, o, pending)
		outcome.Stats.Documents += page.Stats.Documents
		outcome.Stats.Chunks += page.Stats.Chunks
		outcome.Stats.Stale += page.Stats.Stale
		outcome.Failed += page.Failed
		if outcome.FirstFailure == nil {
			outcome.FirstFailure = page.FirstFailure
		}
		if err != nil {
			return outcome, err
		}
	}
}

func fillRepairPage(
	ctx context.Context, store *repairStore, gen string, enc kitvec.EncodeFunc,
	o repairFillOptions, pending []kitvec.Pending[string],
) (repairFillOutcome, error) {
	pageCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	workers := o.Concurrency
	if workers <= 0 {
		workers = 1
	}
	workers = min(workers, len(pending))
	sem := make(chan struct{}, workers)
	results := make(chan repairEncoded, len(pending))
	var wg sync.WaitGroup
	for _, document := range pending {
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-pageCtx.Done():
				results <- repairEncoded{doc: document, err: pageCtx.Err()}
				return
			}
			chunks := kitvec.Split(document.Content, o.Split)
			vectors, err := kitvec.EncodeBatched(pageCtx, enc, chunks, o.Batch)
			results <- repairEncoded{doc: document, chunks: chunks, vectors: vectors, err: err}
		})
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	var outcome repairFillOutcome
	var pageErr error
	for encoded := range results {
		if encoded.err != nil {
			if isPermanentEncodeError(encoded.err) {
				outcome.Failed++
				if outcome.FirstFailure == nil {
					outcome.FirstFailure = fmt.Errorf(
						"encode repair target %s: %w", encoded.doc.Doc, encoded.err)
				}
				continue
			}
			if pageErr == nil {
				outcome.Failed++
				pageErr = fmt.Errorf("encode repair target %s: %w", encoded.doc.Doc, encoded.err)
				cancel()
			}
			continue
		}
		if pageErr != nil {
			continue
		}
		vectors := make([]kitvec.ChunkVector, len(encoded.chunks))
		for i, chunk := range encoded.chunks {
			vectors[i] = kitvec.ChunkVector{ChunkIndex: chunk.Index, Vector: encoded.vectors[i]}
		}
		if err := store.SaveVectors(ctx, gen, encoded.doc.Doc, encoded.doc.Revision, vectors); err != nil {
			if errors.Is(err, kitvec.ErrStale) {
				outcome.Stats.Stale++
				continue
			}
			outcome.Failed++
			pageErr = fmt.Errorf("save repair target %s: %w", encoded.doc.Doc, err)
			cancel()
			continue
		}
		outcome.Stats.Documents++
		outcome.Stats.Chunks += len(vectors)
	}
	return outcome, pageErr
}

func repairDocumentRowIDs(
	ctx context.Context, tx *sql.Tx, spec IndexSpec, ordinal int64, docKey string,
) ([]int64, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT vec_rowid FROM `+spec.chunksTable()+`
          WHERE ordinal = ? AND doc_key = ? ORDER BY chunk_index`, ordinal, docKey)
	if err != nil {
		return nil, fmt.Errorf("read invalid chunk map for %s: %w", docKey, err)
	}
	defer rows.Close()

	var rowids []int64
	for rows.Next() {
		var rowid int64
		if err := rows.Scan(&rowid); err != nil {
			return nil, fmt.Errorf("scan invalid chunk map for %s: %w", docKey, err)
		}
		rowids = append(rowids, rowid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate invalid chunk map for %s: %w", docKey, err)
	}
	return rowids, nil
}
