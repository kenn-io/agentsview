package vector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	kitvec "go.kenn.io/kit/vector"
)

type reuseGeneration struct {
	ordinal   int64
	dimension int
}

func (ix *Index) clearReuse(ctx context.Context) error {
	_, err := ix.db.ExecContext(ctx, `DELETE FROM `+ix.spec.reuseTable())
	if err != nil {
		return fmt.Errorf("clear chunk reuse: %w", err)
	}
	return nil
}

type reuseCompletingStore struct{ *repairQueueCompletingStore }

func (s *reuseCompletingStore) SaveVectors(ctx context.Context, gen, doc string, revision any, vectors []kitvec.ChunkVector) error {
	if err := s.repairQueueCompletingStore.SaveVectors(ctx, gen, doc, revision, vectors); err != nil {
		return err
	}
	// Fill passes nil for a skipped document and a non-nil slice for a
	// successful replacement, including a document with no chunks.
	if s.spec.Name != "messages" || vectors == nil {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM `+s.spec.reuseTable()+` WHERE doc_key = ? AND ordinal = (SELECT ordinal FROM `+s.spec.generationsTable()+` WHERE gen_key = ?)`, doc, gen)
	if err != nil {
		return fmt.Errorf("clear completed chunk reuse for %s: %w", doc, err)
	}
	return nil
}

func (ix *Index) seedReuse(ctx context.Context, gen reuseGeneration, key, old, hash, current string) error {
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
	wanted := make(map[string]bool)
	for _, c := range kitvec.Split(current, ix.split) {
		wanted[contentHash(c.Text)] = true
	}
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
		if !wanted[h] {
			continue
		}
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
			query := `SELECT doc_key, embedding FROM ` + ix.spec.reuseTable() + ` INDEXED BY ` + ix.spec.reuseTable() + `_hash WHERE ordinal = ? AND text_hash = ?`
			args := []any{gen.ordinal, hash}
			for {
				var key string
				var blob []byte
				err := ix.db.QueryRowContext(ctx, query+` ORDER BY doc_key LIMIT 1`, args...).Scan(&key, &blob)
				if errors.Is(err, sql.ErrNoRows) {
					break
				}
				if err != nil {
					return nil, err
				}
				if vector, err := validatedStoredEmbeddingBlob(blob, gen.dimension); err == nil {
					hits[hash] = vector
					break
				}
				query = `SELECT doc_key, embedding FROM ` + ix.spec.reuseTable() + ` INDEXED BY ` + ix.spec.reuseTable() + `_hash WHERE ordinal = ? AND text_hash = ? AND doc_key > ?`
				args = []any{gen.ordinal, hash, key}
			}
		}
		out := make([][]float32, len(texts))
		var misses []string
		var positions []int
		for i, hash := range hashes {
			if v, ok := hits[hash]; ok {
				out[i] = v
			} else {
				misses = append(misses, texts[i])
				positions = append(positions, i)
			}
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
		for i, pos := range positions {
			out[pos] = vectors[i]
		}
		return out, nil
	}
}
