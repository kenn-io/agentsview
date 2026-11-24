package vector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"sync"

	kitvec "go.kenn.io/kit/vector"
)

type reuseGeneration struct {
	ordinal   int64
	dimension int
}

const chunkTextMarker = "chunk_text_backfill:"

type reuseCompletingStore struct {
	*repairQueueCompletingStore
	ix      *Index
	ordinal int64
	mu      sync.Mutex
	saved   []string
}

func (s *reuseCompletingStore) SaveVectors(ctx context.Context, gen, doc string, revision any, vectors []kitvec.ChunkVector) error {
	if s.ix.spec.Name == "messages" && vectors != nil {
		var content string
		if err := s.db.QueryRowContext(ctx, `SELECT content FROM `+s.spec.DocsTable+` WHERE doc_key = ? AND content_hash = ?`, doc, revision).Scan(&content); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return s.repairQueueCompletingStore.SaveVectors(ctx, gen, doc, revision, vectors)
			}
			return err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, `DELETE FROM `+s.spec.chunkTextTable()+` WHERE ordinal = ? AND doc_key = ?`, s.ordinal, doc); err != nil {
			return err
		}
		for _, c := range kitvec.Split(content, s.ix.split) {
			if _, err = tx.ExecContext(ctx, `INSERT INTO `+s.spec.chunkTextTable()+` (ordinal, doc_key, chunk_index, revision, text_hash) VALUES (?, ?, ?, ?, ?)`, s.ordinal, doc, c.Index, revision, contentHash(c.Text)); err != nil {
				return err
			}
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	if err := s.repairQueueCompletingStore.SaveVectors(ctx, gen, doc, revision, vectors); err != nil {
		return err
	}
	if s.ix.spec.Name == "messages" && vectors != nil {
		s.mu.Lock()
		s.saved = append(s.saved, doc)
		s.mu.Unlock()
	}
	return nil
}

func (ix *Index) seedReuse(ctx context.Context, gen reuseGeneration, key, old, hash string) error {
	var stamp sql.NullString
	err := ix.db.QueryRowContext(ctx, `SELECT revision FROM `+ix.spec.stampsTable()+` WHERE ordinal = ? AND doc_key = ?`, gen.ordinal, key).Scan(&stamp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read chunk reuse stamp: %w", err)
	}
	if !stamp.Valid || stamp.String != hash {
		return nil
	}
	chunks := kitvec.Split(old, ix.split)
	rows, err := ix.db.QueryContext(ctx, `SELECT c.chunk_index, v.embedding FROM `+ix.spec.chunksTable()+` c LEFT JOIN `+fmt.Sprintf("%s_v%d", ix.spec.VectorsPrefix, gen.ordinal)+` v ON v.rowid = c.vec_rowid WHERE c.ordinal = ? AND c.doc_key = ? ORDER BY c.chunk_index`, gen.ordinal, key)
	if err != nil {
		return fmt.Errorf("read chunks for reuse: %w", err)
	}
	defer rows.Close()
	var blobs [][]byte
	for rows.Next() {
		var index int
		var blob []byte
		if err := rows.Scan(&index, &blob); err != nil {
			return err
		}
		if len(blobs) >= len(chunks) || chunks[len(blobs)].Index != index {
			return nil
		}
		blobs = append(blobs, blob)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(blobs) != len(chunks) {
		return nil
	}
	tx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for i, chunk := range chunks {
		h := contentHash(chunk.Text)
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO `+ix.spec.reuseTable()+` (ordinal, doc_key, text_hash, embedding) VALUES (?, ?, ?, ?)`, gen.ordinal, key, h, blobs[i]); err != nil {
			return fmt.Errorf("save chunk for reuse: %w", err)
		}
	}
	return tx.Commit()
}

func (ix *Index) reuseEncoder(enc kitvec.EncodeFunc, gen reuseGeneration) kitvec.EncodeFunc {
	return func(ctx context.Context, texts []string) ([][]float32, error) {
		hashes := make([]string, len(texts))
		for i, text := range texts {
			hashes[i] = contentHash(text)
		}
		hits := make(map[string][]float32)
		seen := make(map[string]bool)
		for _, hash := range hashes {
			if seen[hash] {
				continue
			}
			seen[hash] = true
			queries := []string{
				`SELECT embedding FROM ` + ix.spec.reuseTable() + ` INDEXED BY ` + ix.spec.reuseTable() + `_hash WHERE ordinal = ? AND text_hash = ?`,
				`SELECT v.embedding FROM ` + ix.spec.chunkTextTable() + ` t INDEXED BY ` + ix.spec.chunkTextTable() + `_hash JOIN ` + ix.spec.stampsTable() + ` s ON s.ordinal = t.ordinal AND s.doc_key = t.doc_key AND s.revision = t.revision JOIN ` + ix.spec.chunksTable() + ` c ON c.ordinal = t.ordinal AND c.doc_key = t.doc_key AND c.chunk_index = t.chunk_index JOIN ` + fmt.Sprintf("%s_v%d", ix.spec.VectorsPrefix, gen.ordinal) + ` v ON v.rowid = c.vec_rowid WHERE t.ordinal = ? AND t.text_hash = ?`,
			}
			for _, query := range queries {
				rows, err := ix.db.QueryContext(ctx, query, gen.ordinal, hash)
				if err != nil {
					return nil, err
				}
				for rows.Next() {
					var blob []byte
					if err := rows.Scan(&blob); err != nil {
						rows.Close()
						return nil, err
					}
					if vector, err := validatedStoredEmbeddingBlob(blob, gen.dimension); err == nil {
						hits[hash] = vector
						break
					}
				}
				err = rows.Err()
				rows.Close()
				if err != nil {
					return nil, err
				}
				if _, ok := hits[hash]; ok {
					break
				}
			}
		}
		out := make([][]float32, len(texts))
		var misses []string
		positions := make(map[string][]int)
		var missedHashes []string
		for i, hash := range hashes {
			if v, ok := hits[hash]; ok {
				out[i] = v
				continue
			}
			if len(positions[hash]) == 0 {
				misses = append(misses, texts[i])
				missedHashes = append(missedHashes, hash)
			}
			positions[hash] = append(positions[hash], i)
		}
		if len(misses) == 0 {
			return out, nil
		}
		vectors, err := enc(ctx, misses)
		if err != nil {
			return nil, err
		}
		if len(vectors) != len(misses) {
			return nil, fmt.Errorf("encoder returned %d vectors for %d inputs", len(vectors), len(misses))
		}
		for i, hash := range missedHashes {
			for _, pos := range positions[hash] {
				out[pos] = vectors[i]
			}
		}
		return out, nil
	}
}

func (ix *Index) backfillChunkTextPageQuery() string {
	return `SELECT d.doc_key, d.content, d.content_hash FROM ` + ix.spec.DocsTable + ` d WHERE d.doc_key > ? AND EXISTS (SELECT 1 FROM ` + ix.spec.stampsTable() + ` s WHERE s.ordinal = ? AND s.doc_key = d.doc_key AND s.revision = d.content_hash) AND EXISTS (SELECT 1 FROM ` + ix.spec.chunksTable() + ` c WHERE c.ordinal = ? AND c.doc_key = d.doc_key) AND NOT EXISTS (SELECT 1 FROM ` + ix.spec.chunkTextTable() + ` t WHERE t.ordinal = ? AND t.doc_key = d.doc_key) ORDER BY d.doc_key LIMIT 100`
}

func (ix *Index) backfillChunkText(ctx context.Context, ordinal int64) error {
	marker := chunkTextMarker + strconv.FormatInt(ordinal, 10)
	_, done, err := ix.metaGet(ctx, marker)
	if err != nil || done {
		return err
	}
	after := ""
	for {
		rows, err := ix.db.QueryContext(ctx, ix.backfillChunkTextPageQuery(), after, ordinal, ordinal, ordinal)
		if err != nil {
			return err
		}
		type doc struct{ key, content, revision string }
		var docs []doc
		for rows.Next() {
			var d doc
			if err := rows.Scan(&d.key, &d.content, &d.revision); err != nil {
				rows.Close()
				return err
			}
			docs = append(docs, d)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(docs) == 0 {
			break
		}
		tx, err := ix.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO `+ix.spec.chunkTextTable()+` (ordinal, doc_key, chunk_index, revision, text_hash) VALUES (?, ?, ?, ?, ?)`)
		if err != nil {
			tx.Rollback()
			return err
		}
		for _, d := range docs {
			for _, c := range kitvec.Split(d.content, ix.split) {
				if _, err = stmt.ExecContext(ctx, ordinal, d.key, c.Index, d.revision, contentHash(c.Text)); err != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		stmt.Close()
		if err != nil {
			tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		after = docs[len(docs)-1].key
	}
	return ix.metaSet(ctx, marker, "1")
}
