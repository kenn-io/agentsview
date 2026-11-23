package vector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

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
	var blobs [][]byte
	for rows.Next() {
		var index int
		var blob []byte
		if err := rows.Scan(&index, &blob); err != nil {
			rows.Close()
			return err
		}
		if len(blobs) >= len(chunks) || chunks[len(blobs)].Index != index {
			rows.Close()
			return nil
		}
		blobs = append(blobs, blob)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(blobs) != len(chunks) {
		return nil
	}
	for i, chunk := range chunks {
		h := contentHash(chunk.Text)
		if !wanted[h] || validateStoredEmbeddingBlob(blobs[i], gen.dimension) != nil {
			continue
		}
		if _, err := ix.db.ExecContext(ctx, `INSERT OR IGNORE INTO `+ix.spec.reuseTable()+` (ordinal, text_hash, embedding) VALUES (?, ?, ?)`, gen.ordinal, h, blobs[i]); err != nil {
			return fmt.Errorf("save chunk for reuse: %w", err)
		}
	}
	return nil
}

func (ix *Index) reuseEncoder(enc kitvec.EncodeFunc, gen reuseGeneration) kitvec.EncodeFunc {
	return func(ctx context.Context, texts []string) ([][]float32, error) {
		hashes := make([]string, len(texts))
		for i, text := range texts {
			hashes[i] = contentHash(text)
		}
		hits := make(map[string][]float32)
		for start := 0; start < len(hashes); start += maxSQLVars - 1 {
			end := min(start+maxSQLVars-1, len(hashes))
			args := []any{gen.ordinal}
			for _, hash := range hashes[start:end] {
				args = append(args, hash)
			}
			placeholders := strings.TrimSuffix(strings.Repeat("?,", end-start), ",")
			rows, err := ix.db.QueryContext(ctx, `SELECT text_hash, embedding FROM `+ix.spec.reuseTable()+` WHERE ordinal = ? AND text_hash IN (`+placeholders+`)`, args...)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var hash string
				var blob []byte
				if err := rows.Scan(&hash, &blob); err != nil {
					rows.Close()
					return nil, err
				}
				if validateStoredEmbeddingBlob(blob, gen.dimension) == nil {
					hits[hash], _ = decodeFloat32Blob(blob)
				}
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return nil, err
			}
			if err := rows.Close(); err != nil {
				return nil, err
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
			var invalid *kitvec.InvalidVectorError
			if errors.As(err, &invalid) && invalid.Chunk >= 0 && invalid.Chunk < len(positions) {
				mapped := *invalid
				mapped.Chunk = positions[invalid.Chunk]
				return nil, &mapped
			}
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
