package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/vector"
)

// Desired returns the owner-selected generation with one indexed catalog
// lookup. Workers use it to attempt activation after reconciliation without
// running the aggregate Status query in their idle loop.
func (s *HostedEmbeddingStore) Desired(ctx context.Context) (*HostedEmbeddingGeneration, error) {
	var id sql.NullInt64
	if err := s.pg.QueryRowContext(ctx, `SELECT desired_generation_id FROM hosted_embedding_state WHERE tenant_id=$1 AND singleton=1`, s.tenant).Scan(&id); err != nil {
		return nil, err
	}
	if !id.Valid {
		return nil, nil
	}
	generation, err := loadEmbeddingGeneration(ctx, s.pg, id.Int64)
	return &generation, err
}

// SearchGeneration searches and hydrates one already-pinned hosted generation.
// Current source revision and eligibility are checked in the same snapshot as
// KNN so stale text can never become a result while replacement work is pending.
func (s *HostedEmbeddingStore) SearchGeneration(
	ctx context.Context,
	generation HostedEmbeddingGeneration,
	query []float32,
	limit int,
) ([]db.VectorHit, error) {
	if generation.ID <= 0 || len(query) != generation.Recipe.Dimensions {
		return nil, ErrHostedEmbeddingInvalidResults
	}
	canonical, err := CanonicalHostedEmbeddingRecipe(generation.Recipe)
	if err != nil || canonical != generation.Recipe {
		return nil, ErrHostedEmbeddingInvalidResults
	}
	if limit <= 0 {
		return nil, nil
	}
	extSchema, err := vectorExtensionSchema(ctx, s.pg)
	if err != nil {
		return nil, fmt.Errorf("resolving pgvector schema: %w", err)
	}
	normal, err := normalizedEmbedding(query, generation.Recipe.Dimensions)
	if err != nil {
		return nil, fmt.Errorf("query embedding: %w", err)
	}
	literal, err := halfvecLiteral(normal)
	if err != nil {
		return nil, fmt.Errorf("query embedding: %w", err)
	}
	distance := fmt.Sprintf("c.embedding OPERATOR(%s.<=>) $1::%s.halfvec", extSchema, extSchema)
	querySQL := fmt.Sprintf(`
SELECT c.doc_key,c.chunk_index,1-(%s) AS score,
       d.session_id,d.ordinal,d.ordinal_end,d.subordinate,d.offsets,d.content
  FROM %s c
  JOIN hosted_embedding_documents d
    ON d.tenant_id=c.tenant_id AND d.generation_id=c.generation_id AND d.doc_key=c.doc_key
  JOIN hosted_embedding_sources j
    ON j.tenant_id=d.tenant_id AND j.session_id=d.session_id
   AND j.revision=d.source_revision AND NOT j.dirty
  JOIN sessions source ON source.id=d.session_id
 WHERE c.tenant_id=$2 AND c.generation_id=$3
   AND source.deleted_at IS NULL AND NOT source.prompt_evidence_discarded
   AND ($4 OR NOT source.is_automated)
 ORDER BY %s
 LIMIT $5`, distance, embeddingChunkTable(generation.ID), distance)

	tx, err := s.pg.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	stored, err := loadEmbeddingGeneration(ctx, tx, generation.ID)
	if err != nil {
		return nil, err
	}
	if stored != generation {
		return nil, ErrHostedEmbeddingStale
	}
	if err = tuneHNSWRecall(ctx, tx, limit); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, querySQL, literal, s.tenant, generation.ID, generation.Recipe.IncludeAutomated, limit)
	if err != nil {
		return nil, fmt.Errorf("hosted embedding search: %w", err)
	}
	defer func() { _ = rows.Close() }()
	seen := make(map[string]struct{})
	hits := make([]db.VectorHit, 0, limit)
	for rows.Next() {
		var key, sessionID, offsetsJSON, content string
		var chunkIndex, ordinal, ordinalEnd int
		var subordinate bool
		var score float64
		if err = rows.Scan(&key, &chunkIndex, &score, &sessionID, &ordinal, &ordinalEnd, &subordinate, &offsetsJSON, &content); err != nil {
			return nil, err
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		var offsets []db.UnitOffset
		if err = json.Unmarshal([]byte(offsetsJSON), &offsets); err != nil {
			return nil, fmt.Errorf("parsing hosted embedding offsets: %w", err)
		}
		anchor, snippet := vector.DocAnchor(content, offsets, ordinal, chunkIndex, generation.Recipe.MaxInputChars)
		hits = append(hits, db.VectorHit{SessionID: sessionID, Ordinal: anchor, OrdinalStart: ordinal, OrdinalEnd: ordinalEnd, Subordinate: subordinate, Score: float32(score), Snippet: snippet})
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	return hits, tx.Commit()
}

// ResolveGenerationUnits maps lexical hits against the same current,
// generation-owned document set used by SearchGeneration.
func (s *HostedEmbeddingStore) ResolveGenerationUnits(
	ctx context.Context,
	generation HostedEmbeddingGeneration,
	refs []db.MessageRef,
) ([]db.UnitRef, error) {
	out := make([]db.UnitRef, len(refs))
	if len(refs) == 0 {
		return out, nil
	}
	stored, err := loadEmbeddingGeneration(ctx, s.pg, generation.ID)
	if err != nil {
		return nil, err
	}
	if stored != generation {
		return nil, ErrHostedEmbeddingStale
	}
	stmt, err := s.pg.PrepareContext(ctx, `
SELECT d.doc_key,d.ordinal,d.ordinal_end,d.subordinate
  FROM hosted_embedding_documents d
  JOIN hosted_embedding_sources j
    ON j.tenant_id=d.tenant_id AND j.session_id=d.session_id
   AND j.revision=d.source_revision AND NOT j.dirty
  JOIN sessions source ON source.id=d.session_id
 WHERE d.tenant_id=$1 AND d.generation_id=$2 AND d.session_id=$3
   AND d.ordinal<=$4 AND source.deleted_at IS NULL
   AND NOT source.prompt_evidence_discarded AND ($5 OR NOT source.is_automated)
 ORDER BY d.ordinal DESC LIMIT 1`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = stmt.Close() }()
	for i, ref := range refs {
		var unit db.UnitRef
		err = stmt.QueryRowContext(ctx, s.tenant, generation.ID, ref.SessionID, ref.Ordinal, generation.Recipe.IncludeAutomated).Scan(&unit.DocKey, &unit.OrdinalStart, &unit.OrdinalEnd, &unit.Subordinate)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if ref.Ordinal > unit.OrdinalEnd {
			continue
		}
		unit.SessionID = ref.SessionID
		out[i] = unit
	}
	return out, nil
}
