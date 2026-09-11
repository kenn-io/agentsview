package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

type embeddingVectorKey struct {
	doc   string
	index int
}

func normalizedEmbedding(v []float32, dimensions int) ([]float32, error) {
	if len(v) != dimensions {
		return nil, ErrHostedEmbeddingInvalidResults
	}
	var norm float64
	for _, x := range v {
		f := float64(x)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, ErrHostedEmbeddingInvalidResults
		}
		norm += f * f
	}
	if norm == 0 {
		return nil, ErrHostedEmbeddingInvalidResults
	}
	norm = math.Sqrt(norm)
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(float64(x) / norm)
	}
	return out, nil
}
func embeddingVectorLiteral(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(x), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}
func (s *HostedEmbeddingStore) validatePublication(snap *HostedEmbeddingSnapshot, vectors []HostedEmbeddingVector) (map[embeddingVectorKey][]float32, error) {
	if snap == nil || len(snap.Documents) > s.limits.MaxDocuments || len(vectors) > s.limits.MaxChunks || snap.Generation.ID != snap.Lease.GenerationID {
		return nil, ErrHostedEmbeddingInvalidResults
	}
	if snap.ManifestHash == "" || embeddingManifest(snap) != snap.ManifestHash {
		return nil, ErrHostedEmbeddingInvalidResults
	}
	r, e := CanonicalHostedEmbeddingRecipe(snap.Generation.Recipe)
	if e != nil {
		return nil, ErrHostedEmbeddingInvalidResults
	}
	expected := map[embeddingVectorKey]bool{}
	docs := map[string]bool{}
	for _, d := range snap.Documents {
		if docs[d.Key] || d.Unit.SessionID != snap.Lease.SessionID {
			return nil, ErrHostedEmbeddingInvalidResults
		}
		docs[d.Key] = true
		for _, c := range d.Chunks {
			k := embeddingVectorKey{d.Key, c.Index}
			if expected[k] || c.Index < 0 {
				return nil, ErrHostedEmbeddingInvalidResults
			}
			expected[k] = true
		}
	}
	if len(expected) != len(vectors) {
		return nil, ErrHostedEmbeddingInvalidResults
	}
	out := make(map[embeddingVectorKey][]float32, len(vectors))
	var bytes int64
	for _, v := range vectors {
		k := embeddingVectorKey{v.DocumentKey, v.ChunkIndex}
		if !expected[k] || out[k] != nil {
			return nil, ErrHostedEmbeddingInvalidResults
		}
		bytes += int64(len(v.Values)) * 4
		if bytes > s.limits.MaxVectorBytes {
			return nil, ErrHostedEmbeddingWorkLimit
		}
		normal, e := normalizedEmbedding(v.Values, r.Dimensions)
		if e != nil {
			return nil, e
		}
		out[k] = normal
	}
	return out, nil
}
func (s *HostedEmbeddingStore) Publish(ctx context.Context, snap *HostedEmbeddingSnapshot, vectors []HostedEmbeddingVector) error {
	values, e := s.validatePublication(snap, vectors)
	if e != nil {
		return e
	}
	tx, e := s.fence(ctx)
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback() }()
	hash, e := s.lockLease(ctx, tx, snap.Lease, true)
	if e != nil {
		return e
	}
	if hash != snap.ManifestHash {
		return ErrHostedEmbeddingInvalidResults
	}
	g, e := loadEmbeddingGeneration(ctx, tx, snap.Generation.ID)
	if e != nil {
		return e
	}
	if g.Recipe != snap.Generation.Recipe {
		return ErrHostedEmbeddingStale
	}
	eligible, e := embeddingEligible(ctx, tx, snap.Lease.SessionID, g.Recipe.IncludeAutomated)
	if e != nil {
		return e
	}
	if eligible != snap.Eligible {
		return ErrHostedEmbeddingStale
	}
	if _, e = tx.ExecContext(ctx, `DELETE FROM hosted_embedding_documents WHERE tenant_id=$1 AND generation_id=$2 AND session_id=$3`, s.tenant, g.ID, snap.Lease.SessionID); e != nil {
		return e
	}
	for _, doc := range snap.Documents {
		offsets, _ := json.Marshal(doc.Unit.Offsets)
		chunks := make([]struct {
			Index     int
			InputHash string
		}, len(doc.Chunks))
		for i, c := range doc.Chunks {
			chunks[i].Index = c.Index
			chunks[i].InputHash = c.InputHash
		}
		manifest, _ := json.Marshal(chunks)
		_, e = tx.ExecContext(ctx, `INSERT INTO hosted_embedding_documents(tenant_id,generation_id,doc_key,session_id,source_revision,kind,source_uuid,ordinal,ordinal_end,subordinate,offsets,content,content_hash,chunk_manifest) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, s.tenant, g.ID, doc.Key, snap.Lease.SessionID, snap.Lease.Revision, doc.Unit.Kind, doc.Unit.SourceUUID, doc.Unit.Ordinal, doc.Unit.OrdinalEnd, doc.Unit.Subordinate, offsets, doc.Unit.Content, doc.ContentHash, manifest)
		if e != nil {
			return e
		}
		for _, c := range doc.Chunks {
			_, e = tx.ExecContext(ctx, `INSERT INTO `+embeddingChunkTable(g.ID)+`(tenant_id,generation_id,doc_key,chunk_index,input_hash,embedding) VALUES($1,$2,$3,$4,$5,$6)`, s.tenant, g.ID, doc.Key, c.Index, c.InputHash, embeddingVectorLiteral(values[embeddingVectorKey{doc.Key, c.Index}]))
			if e != nil {
				return e
			}
			_, e = tx.ExecContext(ctx, `INSERT INTO `+embeddingValueTable(g.ID)+`(tenant_id,generation_id,input_hash,embedding) VALUES($1,$2,$3,$4) ON CONFLICT(tenant_id,input_hash) DO NOTHING`, s.tenant, g.ID, c.InputHash, embeddingVectorLiteral(values[embeddingVectorKey{doc.Key, c.Index}]))
			if e != nil {
				return e
			}
		}
	}
	// Recheck server time after every insertion. An expiry rolls back replacement.
	result, e := tx.ExecContext(ctx, `UPDATE hosted_embedding_requirements SET completed_revision=required_revision,state='complete',lease_owner=NULL,lease_token=NULL,lease_expires_at=NULL,error_code='' WHERE tenant_id=$1 AND generation_id=$2 AND session_id=$3 AND lease_expires_at>clock_timestamp()`, s.tenant, g.ID, snap.Lease.SessionID)
	if e != nil {
		return e
	}
	n, e := result.RowsAffected()
	if e != nil {
		return e
	}
	if n != 1 {
		return ErrHostedEmbeddingLeaseLost
	}
	return tx.Commit()
}
func (s *HostedEmbeddingStore) ReusableVectors(ctx context.Context, snap *HostedEmbeddingSnapshot) ([]HostedEmbeddingVector, error) {
	if snap == nil || embeddingManifest(snap) != snap.ManifestHash {
		return nil, ErrHostedEmbeddingInvalidResults
	}
	gens, e := embeddingServiced(ctx, s.pg)
	if e != nil {
		return nil, e
	}
	var out []HostedEmbeddingVector
	var bytes int64
	for _, doc := range snap.Documents {
		for _, chunk := range doc.Chunks {
			for _, g := range gens {
				if g.Recipe.Fingerprint != snap.Generation.Recipe.Fingerprint {
					continue
				}
				var encoded string
				e = s.pg.QueryRowContext(ctx, `SELECT embedding::text FROM `+embeddingValueTable(g.ID)+` WHERE tenant_id=$1 AND input_hash=$2 LIMIT 1`, s.tenant, chunk.InputHash).Scan(&encoded)
				if e != nil {
					if e == sql.ErrNoRows {
						continue
					}
					return nil, e
				}
				var values []float32
				if e = json.Unmarshal([]byte(encoded), &values); e != nil {
					return nil, ErrHostedEmbeddingInvalidResults
				}
				values, e = normalizedEmbedding(values, snap.Generation.Recipe.Dimensions)
				if e != nil {
					return nil, e
				}
				bytes += int64(len(values)) * 4
				if bytes > s.limits.MaxVectorBytes || len(out) >= s.limits.MaxChunks {
					return nil, ErrHostedEmbeddingWorkLimit
				}
				out = append(out, HostedEmbeddingVector{doc.Key, chunk.Index, values})
				break
			}
		}
	}
	return out, nil
}
