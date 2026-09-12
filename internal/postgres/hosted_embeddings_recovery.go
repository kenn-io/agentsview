package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
)

var (
	ErrHostedEmbeddingGenerationNotServiced    = errors.New("hosted embedding generation is not active or desired")
	ErrHostedEmbeddingRecoveryIndexUnavailable = errors.New("hosted embedding recovery index unavailable; reprovision embeddings with the owner role")
)

type HostedEmbeddingRetryResult struct {
	GenerationID int64 `json:"generation_id"`
	Examined     int   `json:"examined"`
	Retried      int   `json:"retried"`
	Skipped      int   `json:"skipped"`
}

const hostedEmbeddingFailedSQL = `SELECT CASE WHEN octet_length(session_id)<=65536 THEN session_id ELSE repeat('!',65537) END,required_revision FROM hosted_embedding_requirements WHERE tenant_id=$1 AND generation_id=$2 AND state='failed' ORDER BY session_id LIMIT $3 FOR UPDATE`

// RetryFailed resets one bounded batch of current terminal failures. Source
// reconciliation remains the worker's responsibility; stale candidates are
// counted and left unchanged. Counts are returned only after the batch commits.
func (s *HostedEmbeddingStore) RetryFailed(ctx context.Context, generationID int64, limit int) (HostedEmbeddingRetryResult, error) {
	empty := HostedEmbeddingRetryResult{GenerationID: generationID}
	if generationID <= 0 || limit < 1 || limit > 256 {
		return empty, ErrHostedEmbeddingWorkLimit
	}
	tx, err := s.fence(ctx)
	if err != nil {
		return empty, err
	}
	defer func() { _ = tx.Rollback() }()
	if err = checkEmbeddingRecoveryIndex(ctx, tx, s.schema); err != nil {
		return empty, err
	}
	var serviced bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM hosted_embedding_state WHERE tenant_id=$1 AND $2 IN (active_generation_id,desired_generation_id))`, s.tenant, generationID).Scan(&serviced)
	if err != nil {
		return empty, err
	}
	if !serviced {
		return empty, ErrHostedEmbeddingGenerationNotServiced
	}
	generation, err := loadEmbeddingGeneration(ctx, tx, generationID)
	if err != nil {
		return empty, err
	}
	rows, err := tx.QueryContext(ctx, hostedEmbeddingFailedSQL, s.tenant, generationID, limit)
	if err != nil {
		return empty, err
	}
	type candidate struct {
		id       string
		revision int64
	}
	candidates := make([]candidate, 0, limit)
	for rows.Next() {
		var v candidate
		if err = rows.Scan(&v.id, &v.revision); err != nil {
			rows.Close()
			return empty, err
		}
		if len(v.id) > 65536 {
			rows.Close()
			return empty, ErrHostedEmbeddingWorkLimit
		}
		candidates = append(candidates, v)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return empty, err
	}
	if closeErr != nil {
		return empty, closeErr
	}
	result := HostedEmbeddingRetryResult{GenerationID: generationID, Examined: len(candidates)}
	for _, v := range candidates {
		var current bool
		var fence int64
		err = tx.QueryRowContext(ctx, `SELECT r.eligible AND NOT s.dirty AND s.revision=r.required_revision,r.attempt_fence FROM hosted_embedding_requirements r JOIN hosted_embedding_sources s ON s.tenant_id=r.tenant_id AND s.session_id=r.session_id WHERE r.tenant_id=$1 AND r.generation_id=$2 AND r.session_id=$3`, s.tenant, generationID, v.id).Scan(&current, &fence)
		if err != nil {
			return empty, err
		}
		if current {
			current, err = embeddingEligible(ctx, tx, v.id, generation.Recipe.IncludeAutomated)
			if err != nil {
				return empty, err
			}
		}
		if !current {
			result.Skipped++
			continue
		}
		// Leave space for both this invalidation and the next Claim's fresh fence.
		if fence < 0 || fence >= math.MaxInt64-1 {
			return empty, ErrHostedEmbeddingWorkLimit
		}
		updated, err := tx.ExecContext(ctx, `UPDATE hosted_embedding_requirements SET state='ready',attempts=0,attempt_fence=attempt_fence+1,available_at=clock_timestamp(),error_code='',lease_owner=NULL,lease_token=NULL,lease_expires_at=NULL,snapshot_manifest_hash=NULL,expected_documents=NULL,expected_chunks=NULL WHERE tenant_id=$1 AND generation_id=$2 AND session_id=$3 AND state='failed' AND required_revision=$4 AND attempt_fence=$5`, s.tenant, generationID, v.id, v.revision, fence)
		if err != nil {
			return empty, err
		}
		changed, err := updated.RowsAffected()
		if err != nil {
			return empty, err
		}
		if changed != 1 {
			return empty, ErrHostedEmbeddingStale
		}
		result.Retried++
	}
	if err = tx.Commit(); err != nil {
		return empty, err
	}
	return result, nil
}

func checkEmbeddingRecoveryIndex(ctx context.Context, q hostedQuerier, schema string) error {
	if err := checkEmbeddingIndex(ctx, q, schema, "hosted_embedding_failed", "hosted_embedding_requirements", "tenant_id,generation_id,session_id", "state='failed'::text", "btree"); err != nil {
		return fmt.Errorf("%w: %w", ErrHostedEmbeddingRecoveryIndexUnavailable, err)
	}
	// Column names alone do not guarantee this index supports the candidate
	// query's equality bounds and ordinary ORDER BY. Require each key's default
	// built-in B-tree operator class and the underlying column's collation.
	var ordered bool
	err := q.QueryRowContext(ctx, `SELECT count(*)=3 AND COALESCE(bool_and(
 o.opcdefault AND o.opcintype=a.atttypid AND o.opcnamespace='pg_catalog'::regnamespace
 AND o.opcmethod=am.oid AND i.indcollation[k.position::integer-1]=a.attcollation),false)
 FROM pg_index i CROSS JOIN LATERAL unnest(i.indkey) WITH ORDINALITY k(attnum,position)
 JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.attnum
 JOIN pg_opclass o ON o.oid=i.indclass[k.position::integer-1]
 JOIN pg_am am ON am.amname='btree'
 WHERE i.indexrelid=to_regclass(format('%I.hosted_embedding_failed',$1::text))`, schema).Scan(&ordered)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrHostedEmbeddingRecoveryIndexUnavailable, err)
	}
	if !ordered {
		return fmt.Errorf("%w: embedding failed index ordering altered", ErrHostedEmbeddingRecoveryIndexUnavailable)
	}
	return nil
}
