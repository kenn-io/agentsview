package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"
)

func embeddingEligible(ctx context.Context, q hostedQuerier, id string, automated bool) (bool, error) {
	var v bool
	e := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE id=$1 AND deleted_at IS NULL AND NOT prompt_evidence_discarded AND ($2 OR NOT is_automated))`, id, automated).Scan(&v)
	return v, e
}
func embeddingServiced(ctx context.Context, q hostedQuerier) ([]HostedEmbeddingGeneration, error) {
	rows, e := q.QueryContext(ctx, `SELECT id FROM hosted_embedding_generations WHERE id IN (SELECT active_generation_id FROM hosted_embedding_state UNION SELECT desired_generation_id FROM hosted_embedding_state) ORDER BY id`)
	if e != nil {
		return nil, e
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return nil, e
		}
		ids = append(ids, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	var out []HostedEmbeddingGeneration
	for _, id := range ids {
		g, e := loadEmbeddingGeneration(ctx, q, id)
		if e != nil {
			return nil, e
		}
		out = append(out, g)
	}
	return out, nil
}
func (s *HostedEmbeddingStore) SelectDesired(ctx context.Context, id int64) error {
	tx, e := s.fence(ctx)
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback() }()
	if _, e = loadEmbeddingGeneration(ctx, tx, id); e != nil {
		return e
	}
	e = selectEmbeddingDesired(ctx, tx, s.tenant, id)
	if e != nil {
		return e
	}
	return tx.Commit()
}
func (s *HostedEmbeddingStore) reconcileSource(ctx context.Context, tx *sql.Tx, g HostedEmbeddingGeneration, id string, revision int64) error {
	eligible, e := embeddingEligible(ctx, tx, id, g.Recipe.IncludeAutomated)
	if e != nil {
		return e
	}
	state := "ready"
	var complete any
	if !eligible {
		state = "complete"
		complete = revision
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO hosted_embedding_requirements(tenant_id,generation_id,session_id,required_revision,completed_revision,eligible,state) VALUES($1,$2,$3,$4,$5,$6,$7)
 ON CONFLICT(tenant_id,generation_id,session_id) DO UPDATE SET required_revision=EXCLUDED.required_revision,completed_revision=EXCLUDED.completed_revision,eligible=EXCLUDED.eligible,state=EXCLUDED.state,attempts=0,lease_owner=NULL,lease_token=NULL,lease_expires_at=NULL,available_at=clock_timestamp(),error_code='',snapshot_manifest_hash=NULL,expected_documents=NULL,expected_chunks=NULL
 WHERE hosted_embedding_requirements.required_revision<>EXCLUDED.required_revision OR hosted_embedding_requirements.eligible<>EXCLUDED.eligible`, s.tenant, g.ID, id, revision, complete, eligible, state)
	if e != nil {
		return e
	}
	if !eligible {
		_, e = tx.ExecContext(ctx, `DELETE FROM hosted_embedding_documents WHERE tenant_id=$1 AND generation_id=$2 AND session_id=$3`, s.tenant, g.ID, id)
	}
	return e
}
func (s *HostedEmbeddingStore) Reconcile(ctx context.Context, limit int) (HostedEmbeddingReconcileResult, error) {
	var result HostedEmbeddingReconcileResult
	if limit < 1 || limit > 512 {
		return result, ErrHostedEmbeddingWorkLimit
	}
	tx, e := s.fence(ctx)
	if e != nil {
		return result, e
	}
	defer func() { _ = tx.Rollback() }()
	gens, e := embeddingServiced(ctx, tx)
	if e != nil {
		return result, e
	}
	rows, e := tx.QueryContext(ctx, `SELECT CASE WHEN octet_length(session_id)<=65536 THEN session_id ELSE repeat('!',65537) END,selection_revision,corpus_revision FROM raw_embedding_outbox WHERE tenant_id=$1 AND NOT embedding_consumed ORDER BY session_id,selection_revision,corpus_revision LIMIT $2`, s.tenant, limit)
	if e != nil {
		return result, e
	}
	type event struct {
		id                string
		selection, corpus int64
	}
	var events []event
	for rows.Next() {
		var v event
		if e = rows.Scan(&v.id, &v.selection, &v.corpus); e != nil {
			rows.Close()
			return result, e
		}
		if len(v.id) > 65536 {
			rows.Close()
			return result, ErrHostedEmbeddingWorkLimit
		}
		events = append(events, v)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return result, e
	}
	for _, v := range events {
		if _, e = tx.ExecContext(ctx, `INSERT INTO hosted_embedding_sources(tenant_id,session_id,revision,dirty) VALUES($1,$2,1,true) ON CONFLICT(tenant_id,session_id) DO UPDATE SET dirty=true`, s.tenant, v.id); e != nil {
			return result, e
		}
		if _, e = tx.ExecContext(ctx, `UPDATE raw_embedding_outbox SET embedding_consumed=true WHERE tenant_id=$1 AND session_id=$2 AND selection_revision=$3 AND corpus_revision=$4`, s.tenant, v.id, v.selection, v.corpus); e != nil {
			return result, e
		}
		result.Outbox++
	}
	rows, e = tx.QueryContext(ctx, hostedEmbeddingDirtySQL, s.tenant, limit)
	if e != nil {
		return result, e
	}
	type source struct {
		id       string
		revision int64
	}
	var sources []source
	for rows.Next() {
		var v source
		if e = rows.Scan(&v.id, &v.revision); e != nil {
			rows.Close()
			return result, e
		}
		if len(v.id) > 65536 {
			rows.Close()
			return result, ErrHostedEmbeddingWorkLimit
		}
		sources = append(sources, v)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return result, e
	}
	for _, v := range sources {
		for _, g := range gens {
			if e = s.reconcileSource(ctx, tx, g, v.id, v.revision); e != nil {
				return result, e
			}
		}
		if _, e = tx.ExecContext(ctx, `UPDATE hosted_embedding_sources SET dirty=false WHERE tenant_id=$1 AND session_id=$2`, s.tenant, v.id); e != nil {
			return result, e
		}
		result.Dirty++
	}
	var desired sql.NullInt64
	if e = tx.QueryRowContext(ctx, `SELECT desired_generation_id FROM hosted_embedding_state WHERE singleton=1`).Scan(&desired); e != nil {
		return result, e
	}
	if desired.Valid {
		var after string
		var finished bool
		if e = tx.QueryRowContext(ctx, `SELECT backfill_after_session_id,backfill_finished FROM hosted_embedding_generations WHERE id=$1`, desired.Int64).Scan(&after, &finished); e != nil {
			return result, e
		}
		result.BackfillFinished = finished
		if !finished {
			g, e := loadEmbeddingGeneration(ctx, tx, desired.Int64)
			if e != nil {
				return result, e
			}
			rows, e = tx.QueryContext(ctx, hostedEmbeddingBackfillSQL, s.tenant, after, limit)
			if e != nil {
				return result, e
			}
			var ids []string
			for rows.Next() {
				var id string
				if e = rows.Scan(&id); e != nil {
					rows.Close()
					return result, e
				}
				if len(id) > 65536 {
					rows.Close()
					return result, ErrHostedEmbeddingWorkLimit
				}
				ids = append(ids, id)
			}
			e = rows.Err()
			rows.Close()
			if e != nil {
				return result, e
			}
			for _, id := range ids {
				var rev int64
				if _, e = tx.ExecContext(ctx, `INSERT INTO hosted_embedding_sources(tenant_id,session_id,revision,dirty) VALUES($1,$2,1,true) ON CONFLICT DO NOTHING`, s.tenant, id); e != nil {
					return result, e
				}
				if e = tx.QueryRowContext(ctx, `SELECT revision FROM hosted_embedding_sources WHERE tenant_id=$1 AND session_id=$2`, s.tenant, id).Scan(&rev); e != nil {
					return result, e
				}
				if e = s.reconcileSource(ctx, tx, g, id, rev); e != nil {
					return result, e
				}
				after = id
				result.Backfill++
			}
			result.BackfillFinished = len(ids) == 0
			if _, e = tx.ExecContext(ctx, `UPDATE hosted_embedding_generations SET backfill_after_session_id=$2,backfill_finished=$3 WHERE id=$1`, g.ID, after, result.BackfillFinished); e != nil {
				return result, e
			}
		}
	}
	return result, tx.Commit()
}
func embeddingTTL(ttl time.Duration) (time.Duration, error) {
	if ttl == 0 {
		ttl = time.Minute
	}
	if ttl < time.Millisecond || ttl > time.Minute {
		return 0, ErrHostedEmbeddingWorkLimit
	}
	return ttl, nil
}
func (s *HostedEmbeddingStore) Claim(ctx context.Context, owner string, limit int, ttl time.Duration) ([]HostedEmbeddingLease, error) {
	if owner == "" || len(owner) > 128 || limit < 1 || limit > 64 {
		return nil, ErrHostedEmbeddingWorkLimit
	}
	ttl, e := embeddingTTL(ttl)
	if e != nil {
		return nil, e
	}
	tx, e := s.fence(ctx)
	if e != nil {
		return nil, e
	}
	defer func() { _ = tx.Rollback() }()
	// The corpus fence serializes the cursor across workers. Capture server time
	// once so future timestamps form index range bounds, rather than volatile
	// per-row filters. The first generation rotates even when a queue is idle.
	var cutoff time.Time
	var active, desired sql.NullInt64
	var after int64
	e = tx.QueryRowContext(ctx, `SELECT clock_timestamp(),active_generation_id,desired_generation_id,claim_after_generation_id FROM hosted_embedding_state WHERE tenant_id=$1 AND singleton=1`, s.tenant).Scan(&cutoff, &active, &desired, &after)
	if e != nil {
		return nil, e
	}
	var ids []int64
	if active.Valid {
		ids = append(ids, active.Int64)
	}
	if desired.Valid && (!active.Valid || desired.Int64 != active.Int64) {
		ids = append(ids, desired.Int64)
	}
	if len(ids) == 2 && ids[0] > ids[1] {
		ids[0], ids[1] = ids[1], ids[0]
	}
	if len(ids) == 2 && after == ids[0] {
		ids[0], ids[1] = ids[1], ids[0]
	}
	var leases []HostedEmbeddingLease
	for i, id := range ids {
		// Each generation gets a reserved share; an idle predecessor's unused
		// share is available to the next. Each phase inspects at most 2*limit
		// candidates and returns at most limit leases, regardless of queue size.
		budget := (limit - len(leases) + len(ids) - i - 1) / (len(ids) - i)
		if budget == 0 {
			break
		}
		if _, e = tx.ExecContext(ctx, hostedEmbeddingExpiredSQL, s.tenant, budget, s.maxAttempts, id, cutoff); e != nil {
			return nil, e
		}
		rows, err := tx.QueryContext(ctx, hostedEmbeddingDueSQL, s.tenant, budget, s.maxAttempts, id, cutoff)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var l HostedEmbeddingLease
			if e = rows.Scan(&l.GenerationID, &l.SessionID, &l.Revision, &l.AttemptFence); e != nil {
				rows.Close()
				return nil, e
			}
			if len(l.SessionID) > 65536 {
				rows.Close()
				return nil, ErrHostedEmbeddingWorkLimit
			}
			leases = append(leases, l)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return nil, e
		}
	}
	if len(ids) > 0 {
		if _, e = tx.ExecContext(ctx, `UPDATE hosted_embedding_state SET claim_after_generation_id=$2 WHERE tenant_id=$1 AND singleton=1`, s.tenant, ids[0]); e != nil {
			return nil, e
		}
	}
	for i := range leases {
		l := &leases[i]
		var token [32]byte
		if _, e = rand.Read(token[:]); e != nil {
			return nil, e
		}
		l.Token = hex.EncodeToString(token[:])
		l.Owner = owner
		l.AttemptFence++
		e = tx.QueryRowContext(ctx, `UPDATE hosted_embedding_requirements SET state='leased',attempts=attempts+1,attempt_fence=$4,lease_owner=$5,lease_token=$6,lease_expires_at=clock_timestamp()+$7::bigint*interval '1 microsecond',snapshot_manifest_hash=NULL,expected_documents=NULL,expected_chunks=NULL WHERE tenant_id=$1 AND generation_id=$2 AND session_id=$3 RETURNING lease_expires_at`, s.tenant, l.GenerationID, l.SessionID, l.AttemptFence, l.Owner, l.Token, ttl.Microseconds()).Scan(&l.ExpiresAt)
		if e != nil {
			return nil, e
		}
	}
	return leases, tx.Commit()
}
func (s *HostedEmbeddingStore) lockLease(ctx context.Context, tx *sql.Tx, l HostedEmbeddingLease, source bool) (string, error) {
	var rev, fence int64
	var owner, token, hash sql.NullString
	var expires sql.NullTime
	var state string
	e := tx.QueryRowContext(ctx, `SELECT required_revision,attempt_fence,lease_owner,lease_token,lease_expires_at,state,snapshot_manifest_hash FROM hosted_embedding_requirements WHERE tenant_id=$1 AND generation_id=$2 AND session_id=$3 FOR UPDATE`, s.tenant, l.GenerationID, l.SessionID).Scan(&rev, &fence, &owner, &token, &expires, &state, &hash)
	if e == sql.ErrNoRows {
		return "", ErrHostedEmbeddingLeaseLost
	}
	if e != nil {
		return "", e
	}
	if source {
		var current int64
		var serviced bool
		e = tx.QueryRowContext(ctx, `SELECT revision,EXISTS(SELECT 1 FROM hosted_embedding_state WHERE $3 IN (active_generation_id,desired_generation_id)) FROM hosted_embedding_sources WHERE tenant_id=$1 AND session_id=$2`, s.tenant, l.SessionID, l.GenerationID).Scan(&current, &serviced)
		if e != nil {
			return "", e
		}
		if current != l.Revision || rev != l.Revision || !serviced {
			return "", ErrHostedEmbeddingStale
		}
	}
	var now time.Time
	if e = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); e != nil {
		return "", e
	}
	if rev != l.Revision || fence != l.AttemptFence || owner.String != l.Owner || token.String != l.Token || state != "leased" || !expires.Valid || !expires.Time.After(now) {
		return "", ErrHostedEmbeddingLeaseLost
	}
	return hash.String, nil
}
func (s *HostedEmbeddingStore) Heartbeat(ctx context.Context, l HostedEmbeddingLease, ttl time.Duration) (HostedEmbeddingLease, error) {
	ttl, e := embeddingTTL(ttl)
	if e != nil {
		return l, e
	}
	tx, e := s.pg.BeginTx(ctx, nil)
	if e != nil {
		return l, e
	}
	defer func() { _ = tx.Rollback() }()
	if _, e = s.lockLease(ctx, tx, l, false); e != nil {
		return l, e
	}
	e = tx.QueryRowContext(ctx, `UPDATE hosted_embedding_requirements SET lease_expires_at=clock_timestamp()+$4::bigint*interval '1 microsecond' WHERE tenant_id=$1 AND generation_id=$2 AND session_id=$3 RETURNING lease_expires_at`, s.tenant, l.GenerationID, l.SessionID, ttl.Microseconds()).Scan(&l.ExpiresAt)
	if e != nil {
		return l, e
	}
	return l, tx.Commit()
}
func (s *HostedEmbeddingStore) Fail(ctx context.Context, l HostedEmbeddingLease, code string, retryable bool) error {
	switch code {
	case "encoder_unavailable", "encoder_timeout", "encoder_rate_limit", "invalid_results", "work_limit", "source_read", "lease_expired":
	default:
		code = "encoder_unavailable"
	}
	tx, e := s.pg.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback() }()
	if _, e = s.lockLease(ctx, tx, l, false); e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, `UPDATE hosted_embedding_requirements SET state=CASE WHEN $4 AND attempts<$5 THEN 'retry' ELSE 'failed' END,available_at=clock_timestamp()+LEAST(60000000::bigint,$6::bigint*(1::bigint<<LEAST(attempts-1,6)))*interval '1 microsecond',error_code=$7,lease_owner=NULL,lease_token=NULL,lease_expires_at=NULL,snapshot_manifest_hash=NULL WHERE tenant_id=$1 AND generation_id=$2 AND session_id=$3`, s.tenant, l.GenerationID, l.SessionID, retryable, s.maxAttempts, s.retryDelay.Microseconds(), code)
	if e != nil {
		return fmt.Errorf("record embedding failure: %w", e)
	}
	return tx.Commit()
}

// A retired generation missed notifications consumed by other generations.
// Replay its bounded live+journal keyset, including physical sources now gone.
// Selecting an already serviced generation is an idempotent resume.
func selectEmbeddingDesired(ctx context.Context, tx *sql.Tx, tenant string, id int64) error {
	var serviced bool
	if e := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM hosted_embedding_state WHERE tenant_id=$1 AND $2 IN (active_generation_id,desired_generation_id))`, tenant, id).Scan(&serviced); e != nil {
		return e
	}
	if !serviced {
		if _, e := tx.ExecContext(ctx, `UPDATE hosted_embedding_generations SET backfill_after_session_id='',backfill_finished=false WHERE tenant_id=$1 AND id=$2`, tenant, id); e != nil {
			return e
		}
	}
	_, e := tx.ExecContext(ctx, `UPDATE hosted_embedding_state SET desired_generation_id=$2 WHERE tenant_id=$1 AND singleton=1`, tenant, id)
	return e
}

// Both ordered index arms are limited before UNION deduplicates at most 2*page
// IDs. A deleted source remains in the journal and is part of recovery proof.
const hostedEmbeddingBackfillSQL = `SELECT CASE WHEN octet_length(id)<=65536 THEN id ELSE repeat('!',65537) END FROM (
 (SELECT id FROM sessions WHERE tenant_id=$1 AND id>$2 ORDER BY id LIMIT $3)
 UNION
 (SELECT session_id AS id FROM hosted_embedding_sources WHERE tenant_id=$1 AND session_id>$2 ORDER BY session_id LIMIT $3)
) candidates ORDER BY id LIMIT $3`

const hostedEmbeddingDirtySQL = `SELECT CASE WHEN octet_length(session_id)<=65536 THEN session_id ELSE repeat('!',65537) END,revision FROM hosted_embedding_sources WHERE tenant_id=$1 AND dirty ORDER BY session_id LIMIT $2`
