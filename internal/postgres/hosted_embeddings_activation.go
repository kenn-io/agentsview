package postgres

import (
	"context"
	"database/sql"
)

func embeddingActivationReady(ctx context.Context, q hostedQuerier, id int64) (bool, error) {
	var ready bool
	e := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM hosted_embedding_state s JOIN hosted_embedding_generations g ON g.tenant_id=s.tenant_id AND g.id=s.desired_generation_id WHERE s.singleton=1 AND g.id=$1 AND g.backfill_finished)
 AND NOT EXISTS(SELECT 1 FROM hosted_embedding_sources WHERE tenant_id=current_setting('agentsview.tenant_id') AND dirty)
 AND NOT EXISTS(SELECT 1 FROM raw_embedding_outbox WHERE tenant_id=current_setting('agentsview.tenant_id') AND NOT embedding_consumed)
 AND NOT EXISTS(SELECT 1 FROM hosted_embedding_requirements WHERE tenant_id=current_setting('agentsview.tenant_id') AND generation_id=$1 AND (state<>'complete' OR completed_revision IS DISTINCT FROM required_revision))`, id).Scan(&ready)
	return ready, e
}
func (s *HostedEmbeddingStore) Activate(ctx context.Context, id int64) (bool, error) {
	tx, e := s.fence(ctx)
	if e != nil {
		return false, e
	}
	defer func() { _ = tx.Rollback() }()
	ready, e := embeddingActivationReady(ctx, tx, id)
	if e != nil || !ready {
		return false, e
	}
	if _, e = tx.ExecContext(ctx, `UPDATE hosted_embedding_state SET active_generation_id=$1,active_valid=true WHERE singleton=1 AND desired_generation_id=$1`, id); e != nil {
		return false, e
	}
	return true, tx.Commit()
}
func (s *HostedEmbeddingStore) Active(ctx context.Context) (*HostedEmbeddingGeneration, error) {
	var id sql.NullInt64
	if e := s.pg.QueryRowContext(ctx, `SELECT CASE WHEN active_valid THEN active_generation_id END FROM hosted_embedding_state WHERE singleton=1`).Scan(&id); e != nil {
		return nil, e
	}
	if !id.Valid {
		return nil, nil
	}
	g, e := loadEmbeddingGeneration(ctx, s.pg, id.Int64)
	return &g, e
}
func (s *HostedEmbeddingStore) Status(ctx context.Context) (HostedEmbeddingStatus, error) {
	var out HostedEmbeddingStatus
	tx, e := s.pg.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if e != nil {
		return out, e
	}
	defer func() { _ = tx.Rollback() }()
	var active, desired sql.NullInt64
	if e = tx.QueryRowContext(ctx, `SELECT active_generation_id,desired_generation_id,active_valid FROM hosted_embedding_state WHERE singleton=1`).Scan(&active, &desired, &out.ActiveAvailable); e != nil {
		return out, e
	}
	for _, v := range []struct {
		id     sql.NullInt64
		target **HostedEmbeddingGenerationStatus
	}{{active, &out.Active}, {desired, &out.Desired}} {
		if !v.id.Valid {
			continue
		}
		g, e := loadEmbeddingGeneration(ctx, tx, v.id.Int64)
		if e != nil {
			return out, e
		}
		status := &HostedEmbeddingGenerationStatus{Generation: g, Errors: map[string]int64{}}
		if e = tx.QueryRowContext(ctx, `SELECT backfill_finished,backfill_after_session_id FROM hosted_embedding_generations WHERE id=$1`, g.ID).Scan(&status.BackfillFinished, &status.BackfillAfterSessionID); e != nil {
			return out, e
		}
		rows, e := tx.QueryContext(ctx, `SELECT state,error_code,count(*) FROM hosted_embedding_requirements WHERE tenant_id=$1 AND generation_id=$2 AND (eligible OR state<>'complete') GROUP BY state,error_code`, s.tenant, g.ID)
		if e != nil {
			return out, e
		}
		for rows.Next() {
			var state, code string
			var n int64
			if e = rows.Scan(&state, &code, &n); e != nil {
				rows.Close()
				return out, e
			}
			switch state {
			case "ready":
				status.Ready += n
			case "leased":
				status.Leased += n
			case "retry":
				status.Retry += n
			case "failed":
				status.Failed += n
			case "complete":
				status.Complete += n
			}
			if code != "" {
				status.Errors[code] += n
			}
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return out, e
		}
		*v.target = status
	}
	if desired.Valid {
		out.ActivationReady, e = embeddingActivationReady(ctx, tx, desired.Int64)
		if e != nil {
			return out, e
		}
	}
	return out, tx.Commit()
}

// ClearContent implements an explicit hosted usage-only policy transition.
// Call before starting workers. This deliberate corpus cleanup includes retired
// vector caches; it is never part of idle polling or ordinary source deletion.
// Recipes and pointers survive, while all coverage must be established again.
func (s *HostedEmbeddingStore) ClearContent(ctx context.Context) error {
	tx, e := s.fence(ctx)
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback() }()
	rows, e := tx.QueryContext(ctx, `SELECT id FROM hosted_embedding_generations ORDER BY id`)
	if e != nil {
		return e
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return e
		}
		ids = append(ids, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, id := range ids {
		if _, e = tx.ExecContext(ctx, `DELETE FROM hosted_embedding_documents WHERE tenant_id=$1 AND generation_id=$2`, s.tenant, id); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `DELETE FROM `+embeddingValueTable(id)+` WHERE tenant_id=$1`, s.tenant); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `DELETE FROM hosted_embedding_requirements WHERE tenant_id=$1 AND generation_id=$2`, s.tenant, id); e != nil {
			return e
		}
	}
	if _, e = tx.ExecContext(ctx, `UPDATE hosted_embedding_state SET active_valid=false`); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, `UPDATE hosted_embedding_generations SET backfill_after_session_id='',backfill_finished=false`); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, `UPDATE hosted_embedding_sources SET dirty=true WHERE tenant_id=$1`, s.tenant); e != nil {
		return e
	}
	return tx.Commit()
}
