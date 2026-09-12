//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Losing either reset or the monotonic fence breaks same-owner recovery.
func TestHostedEmbeddingRecoveryClaimsPublishesAndActivates(t *testing.T) {
	f, store, g, snap := embeddingOne(t)
	ctx := t.Context()
	lease := snap.Lease
	require.NoError(t, store.Fail(ctx, lease, "encoder_unavailable", false))
	r, err := store.RetryFailed(ctx, g.ID, 1)
	require.NoError(t, err)
	assert.Equal(t, HostedEmbeddingRetryResult{GenerationID: g.ID, Examined: 1, Retried: 1}, r)
	var state, code string
	var attempts int
	var fence, required int64
	var completed sql.NullInt64
	var cleared, due bool
	require.NoError(t, f.runtime.QueryRow(`SELECT state,attempts,attempt_fence,required_revision,completed_revision,error_code,lease_owner IS NULL AND lease_token IS NULL AND lease_expires_at IS NULL AND snapshot_manifest_hash IS NULL AND expected_documents IS NULL AND expected_chunks IS NULL,available_at<=clock_timestamp() FROM hosted_embedding_requirements WHERE session_id='s'`).Scan(&state, &attempts, &fence, &required, &completed, &code, &cleared, &due))
	assert.Equal(t, "ready", state)
	assert.Zero(t, attempts)
	assert.Equal(t, lease.AttemptFence+1, fence)
	assert.Equal(t, lease.Revision, required)
	assert.False(t, completed.Valid)
	assert.Empty(t, code)
	assert.True(t, cleared)
	assert.True(t, due)
	r, err = store.RetryFailed(ctx, g.ID, 1)
	require.NoError(t, err)
	assert.Equal(t, HostedEmbeddingRetryResult{GenerationID: g.ID}, r)
	next, err := store.Claim(ctx, lease.Owner, 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, next, 1)
	assert.Equal(t, lease.AttemptFence+2, next[0].AttemptFence)
	assert.NotEqual(t, lease.Token, next[0].Token)
	_, err = store.Heartbeat(ctx, lease, time.Minute)
	assert.ErrorIs(t, err, ErrHostedEmbeddingLeaseLost)
	assert.ErrorIs(t, store.Fail(ctx, lease, "encoder_unavailable", false), ErrHostedEmbeddingLeaseLost)
	assert.ErrorIs(t, store.Publish(ctx, snap, embeddingOneVector()), ErrHostedEmbeddingLeaseLost)
	replacement, err := store.ReadSession(ctx, next[0])
	require.NoError(t, err)
	require.NoError(t, store.Publish(ctx, replacement, embeddingOneVector()))
	ok, err := store.Activate(ctx, g.ID)
	require.NoError(t, err)
	assert.True(t, ok)
	var docs, chunks, values int
	require.NoError(t, f.runtime.QueryRow(`SELECT (SELECT count(*) FROM hosted_embedding_documents),(SELECT count(*) FROM hosted_embedding_chunks_g1),(SELECT count(*) FROM hosted_embedding_values_g1)`).Scan(&docs, &chunks, &values))
	assert.Equal(t, 1, docs)
	assert.Equal(t, 1, chunks)
	assert.Equal(t, 1, values)
}

// Source predicates must run after the failed batch is bounded. A full stale
// first page must not draw valid failures from the next page into the operation.
func TestHostedEmbeddingRecoverySkipsStaleFirstPage(t *testing.T) {
	f, store, g := embeddingFixture(t)
	_, err := f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent) SELECT id,'p','m','codex' FROM unnest(ARRAY['a-dirty','b-revision','c-deleted','c-tombstone','d-automated','e-discarded','f-ineligible','g-valid'])id`)
	require.NoError(t, err)
	reconcileEmbedding(t, store)
	leases, err := store.Claim(t.Context(), "worker", 8, time.Minute)
	require.NoError(t, err)
	require.Len(t, leases, 8)
	for _, lease := range leases {
		require.NoError(t, store.Fail(t.Context(), lease, "encoder_unavailable", false))
	}
	_, err = f.runtime.Exec(`UPDATE hosted_embedding_sources SET dirty=true WHERE session_id='a-dirty';
 UPDATE hosted_embedding_sources SET revision=revision+1 WHERE session_id='b-revision';
 DELETE FROM sessions WHERE id='c-deleted';
 UPDATE sessions SET deleted_at='2026-01-01T00:00:00Z' WHERE id='c-tombstone';
 UPDATE sessions SET is_automated=true WHERE id='d-automated';
 UPDATE sessions SET prompt_evidence_discarded=true WHERE id='e-discarded';
 UPDATE hosted_embedding_sources SET dirty=false WHERE session_id IN ('c-deleted','c-tombstone','d-automated','e-discarded');
 UPDATE hosted_embedding_requirements SET required_revision=(SELECT revision FROM hosted_embedding_sources s WHERE s.session_id=hosted_embedding_requirements.session_id) WHERE session_id IN ('c-deleted','c-tombstone','d-automated','e-discarded');
 UPDATE hosted_embedding_requirements SET eligible=false WHERE session_id='f-ineligible'`)
	require.NoError(t, err)
	var before string
	require.NoError(t, f.runtime.QueryRow(`SELECT jsonb_agg(to_jsonb(r) ORDER BY session_id)::text FROM hosted_embedding_requirements r`).Scan(&before))
	r, err := store.RetryFailed(t.Context(), g.ID, 7)
	require.NoError(t, err)
	assert.Equal(t, HostedEmbeddingRetryResult{GenerationID: g.ID, Examined: 7, Skipped: 7}, r)
	var after string
	require.NoError(t, f.runtime.QueryRow(`SELECT jsonb_agg(to_jsonb(r) ORDER BY session_id)::text FROM hosted_embedding_requirements r`).Scan(&after))
	assert.Equal(t, before, after)
	r, err = store.RetryFailed(t.Context(), g.ID, 8)
	require.NoError(t, err)
	assert.Equal(t, HostedEmbeddingRetryResult{GenerationID: g.ID, Examined: 8, Retried: 1, Skipped: 7}, r)
	var state string
	require.NoError(t, f.runtime.QueryRow(`SELECT state FROM hosted_embedding_requirements WHERE session_id='g-valid'`).Scan(&state))
	assert.Equal(t, "ready", state)
}

func TestHostedEmbeddingRecoveryRejectsInvalidLimits(t *testing.T) {
	_, store, g, snap := embeddingOne(t)
	require.NoError(t, store.Fail(t.Context(), snap.Lease, "encoder_unavailable", false))
	for _, v := range []struct {
		generation int64
		limit      int
	}{{0, 1}, {-1, 1}, {g.ID, 0}, {g.ID, -1}, {g.ID, 257}} {
		r, err := store.RetryFailed(t.Context(), v.generation, v.limit)
		assert.ErrorIs(t, err, ErrHostedEmbeddingWorkLimit)
		assert.Zero(t, r.Retried)
	}
	r, err := store.RetryFailed(t.Context(), g.ID, 256)
	require.NoError(t, err)
	assert.Equal(t, HostedEmbeddingRetryResult{GenerationID: g.ID, Examined: 1, Retried: 1}, r)
}

func TestHostedEmbeddingRecoveryServicedGenerationsOnly(t *testing.T) {
	f, store, g, snap := embeddingOne(t)
	require.NoError(t, store.Publish(t.Context(), snap, embeddingOneVector()))
	ok, err := store.Activate(t.Context(), g.ID)
	require.NoError(t, err)
	require.True(t, ok)
	// Fail newly changed work in the active generation before selecting a new
	// desired one. Recovery must still reach this active generation's failure.
	_, err = f.runtime.Exec(`UPDATE messages SET content='updated prompt' WHERE session_id='s'`)
	require.NoError(t, err)
	reconcileEmbedding(t, store)
	leases, err := store.Claim(t.Context(), "worker", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	require.NoError(t, store.Fail(t.Context(), leases[0], "encoder_unavailable", false))
	second, err := ProvisionHostedEmbeddings(t.Context(), f.admin, f.schema, f.tenant, embeddingRecipe(), "second", f.role)
	require.NoError(t, err)
	third, err := ProvisionHostedEmbeddings(t.Context(), f.admin, f.schema, f.tenant, embeddingRecipe(), "third", f.role)
	require.NoError(t, err)
	for _, v := range []struct {
		id                int64
		examined, retried int
	}{{g.ID, 1, 1}, {third.ID, 0, 0}} {
		r, err := store.RetryFailed(t.Context(), v.id, 1)
		require.NoError(t, err)
		assert.Equal(t, HostedEmbeddingRetryResult{GenerationID: v.id, Examined: v.examined, Retried: v.retried}, r)
	}
	for _, id := range []int64{second.ID, 999999} {
		r, err := store.RetryFailed(t.Context(), id, 1)
		assert.ErrorIs(t, err, ErrHostedEmbeddingGenerationNotServiced)
		assert.Zero(t, r.Retried)
	}
	status, err := store.Status(t.Context())
	require.NoError(t, err)
	require.NotNil(t, status.Active)
	require.NotNil(t, status.Desired)
	assert.Equal(t, g.ID, status.Active.Generation.ID)
	assert.Equal(t, third.ID, status.Desired.Generation.ID)
}

func TestHostedEmbeddingRecoveryLeavesOtherWorkAndVectorsUntouched(t *testing.T) {
	f, store, g, snap := embeddingOne(t)
	require.NoError(t, store.Publish(t.Context(), snap, embeddingOneVector()))
	_, err := f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent) SELECT id,'p','m','codex' FROM unnest(ARRAY['a-failed','b-retry','c-leased','d-ready'])id`)
	require.NoError(t, err)
	reconcileEmbedding(t, store)
	leases, err := store.Claim(t.Context(), "worker", 3, time.Minute)
	require.NoError(t, err)
	require.Len(t, leases, 3)
	require.NoError(t, store.Fail(t.Context(), leases[0], "encoder_unavailable", false))
	require.NoError(t, store.Fail(t.Context(), leases[1], "encoder_unavailable", true))
	var before, after string
	query := `SELECT jsonb_build_array((SELECT jsonb_agg(to_jsonb(r) ORDER BY session_id) FROM hosted_embedding_requirements r WHERE session_id<>'a-failed'),(SELECT jsonb_agg(to_jsonb(d)) FROM hosted_embedding_documents d),(SELECT jsonb_agg(to_jsonb(c)) FROM hosted_embedding_chunks_g1 c),(SELECT jsonb_agg(to_jsonb(v)) FROM hosted_embedding_values_g1 v),(SELECT to_jsonb(g) FROM hosted_embedding_generations g),(SELECT to_jsonb(s) FROM hosted_embedding_state s))::text`
	require.NoError(t, f.runtime.QueryRow(query).Scan(&before))
	r, err := store.RetryFailed(t.Context(), g.ID, 256)
	require.NoError(t, err)
	assert.Equal(t, HostedEmbeddingRetryResult{GenerationID: g.ID, Examined: 1, Retried: 1}, r)
	require.NoError(t, f.runtime.QueryRow(query).Scan(&after))
	assert.Equal(t, before, after)
}

func TestHostedEmbeddingRecoveryConcurrentCommands(t *testing.T) {
	f, store, g, snap := embeddingOne(t)
	require.NoError(t, store.Fail(t.Context(), snap.Lease, "encoder_unavailable", false))
	type outcome struct {
		r   HostedEmbeddingRetryResult
		err error
	}
	start := make(chan struct{})
	done := make(chan outcome, 2)
	for range 2 {
		go func() { <-start; r, err := store.RetryFailed(t.Context(), g.ID, 1); done <- outcome{r, err} }()
	}
	close(start)
	total := HostedEmbeddingRetryResult{GenerationID: g.ID}
	for range 2 {
		v := <-done
		require.NoError(t, v.err)
		total.Examined += v.r.Examined
		total.Retried += v.r.Retried
		total.Skipped += v.r.Skipped
	}
	assert.Equal(t, HostedEmbeddingRetryResult{GenerationID: g.ID, Examined: 1, Retried: 1}, total)
	var fence int64
	require.NoError(t, f.runtime.QueryRow(`SELECT attempt_fence FROM hosted_embedding_requirements WHERE session_id='s'`).Scan(&fence))
	assert.Equal(t, snap.Lease.AttemptFence+1, fence)
}

func TestHostedEmbeddingRecoveryIndexUpgrade(t *testing.T) {
	f, store, g, snap := embeddingOne(t)
	require.NoError(t, store.Fail(t.Context(), snap.Lease, "encoder_unavailable", false))
	_, err := f.admin.Exec(`DROP INDEX hosted_embedding_failed`)
	require.NoError(t, err)
	reopened, err := NewHostedEmbeddingStore(t.Context(), f.runtime, HostedEmbeddingOptions{Schema: f.schema, Tenant: f.tenant})
	require.NoError(t, err)
	require.NoError(t, reopened.CheckWritable(t.Context()))
	status, err := reopened.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(1), status.Desired.Failed)
	var before, after string
	query := `SELECT jsonb_build_array((SELECT to_jsonb(g) FROM hosted_embedding_generations g),(SELECT to_jsonb(r) FROM hosted_embedding_requirements r),(SELECT to_jsonb(s) FROM hosted_embedding_state s))::text`
	require.NoError(t, f.runtime.QueryRow(query).Scan(&before))
	r, err := reopened.RetryFailed(t.Context(), g.ID, 1)
	assert.ErrorIs(t, err, ErrHostedEmbeddingRecoveryIndexUnavailable)
	assert.Zero(t, r.Retried)
	same, err := ProvisionHostedEmbeddings(t.Context(), f.admin, f.schema, f.tenant, embeddingRecipe(), "initial", f.role)
	require.NoError(t, err)
	assert.Equal(t, g, same)
	require.NoError(t, f.runtime.QueryRow(query).Scan(&after))
	assert.Equal(t, before, after)
	r, err = reopened.RetryFailed(t.Context(), g.ID, 1)
	require.NoError(t, err)
	assert.Equal(t, 1, r.Retried)
}

func TestHostedEmbeddingRecoveryAlteredIndexFailsClosed(t *testing.T) {
	f, store, g, snap := embeddingOne(t)
	require.NoError(t, store.Fail(t.Context(), snap.Lease, "encoder_unavailable", false))
	_, err := f.admin.Exec(`DROP INDEX hosted_embedding_failed; CREATE INDEX hosted_embedding_failed ON hosted_embedding_requirements(tenant_id,generation_id,session_id) WHERE state='ready'`)
	require.NoError(t, err)
	r, err := store.RetryFailed(t.Context(), g.ID, 1)
	assert.ErrorIs(t, err, ErrHostedEmbeddingRecoveryIndexUnavailable)
	assert.Zero(t, r.Retried)
	_, err = ProvisionHostedEmbeddings(t.Context(), f.admin, f.schema, f.tenant, embeddingRecipe(), "initial", f.role)
	assert.ErrorIs(t, err, ErrHostedEmbeddingRecoveryIndexUnavailable)
	var pred, state string
	require.NoError(t, f.admin.QueryRow(`SELECT pg_get_expr(indpred,indrelid) FROM pg_index WHERE indexrelid='hosted_embedding_failed'::regclass`).Scan(&pred))
	assert.Equal(t, "(state = 'ready'::text)", pred)
	require.NoError(t, f.runtime.QueryRow(`SELECT state FROM hosted_embedding_requirements WHERE session_id='s'`).Scan(&state))
	assert.Equal(t, "failed", state)
}

func TestHostedEmbeddingRecoveryRejectsDeniedWritesAndTenantMismatch(t *testing.T) {
	f, store, g, snap := embeddingOne(t)
	require.NoError(t, store.Fail(t.Context(), snap.Lease, "encoder_unavailable", false))
	_, err := NewHostedEmbeddingStore(t.Context(), f.runtime, HostedEmbeddingOptions{Schema: f.schema, Tenant: "other-tenant"})
	assert.Error(t, err)
	role, err := quoteIdentifier(f.role)
	require.NoError(t, err)
	_, err = f.admin.Exec(`REVOKE UPDATE ON hosted_embedding_requirements FROM ` + role)
	require.NoError(t, err)
	r, err := store.RetryFailed(t.Context(), g.ID, 1)
	assert.Error(t, err)
	assert.Zero(t, r.Retried)
	var state string
	require.NoError(t, f.runtime.QueryRow(`SELECT state FROM hosted_embedding_requirements WHERE session_id='s'`).Scan(&state))
	assert.Equal(t, "failed", state)
}

func TestHostedEmbeddingRecoveryRollsBackInvalidFence(t *testing.T) {
	for _, invalid := range []int64{-1, math.MaxInt64 - 1, math.MaxInt64} {
		t.Run(fmt.Sprint(invalid), func(t *testing.T) {
			f, store, g := embeddingFixture(t)
			_, err := f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent) VALUES('a','p','m','codex'),('b','p','m','codex')`)
			require.NoError(t, err)
			reconcileEmbedding(t, store)
			leases, err := store.Claim(t.Context(), "worker", 2, time.Minute)
			require.NoError(t, err)
			require.Len(t, leases, 2)
			for _, l := range leases {
				require.NoError(t, store.Fail(t.Context(), l, "encoder_unavailable", false))
			}
			_, err = f.runtime.Exec(`UPDATE hosted_embedding_requirements SET attempt_fence=$1 WHERE session_id='b'`, invalid)
			require.NoError(t, err)
			r, err := store.RetryFailed(t.Context(), g.ID, 2)
			assert.ErrorIs(t, err, ErrHostedEmbeddingWorkLimit)
			assert.Zero(t, r.Retried)
			var failed int
			require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM hosted_embedding_requirements WHERE state='failed'`).Scan(&failed))
			assert.Equal(t, 2, failed)
		})
	}
}

func TestHostedEmbeddingRecoveryCancellationRollsBackBatch(t *testing.T) {
	f, store, g := embeddingFixture(t)
	_, err := f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent) VALUES('a','p','m','codex'),('b','p','m','codex')`)
	require.NoError(t, err)
	reconcileEmbedding(t, store)
	leases, err := store.Claim(t.Context(), "worker", 2, time.Minute)
	require.NoError(t, err)
	require.Len(t, leases, 2)
	for _, l := range leases {
		require.NoError(t, store.Fail(t.Context(), l, "encoder_unavailable", false))
	}
	gate, err := f.admin.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = gate.Rollback() }()
	_, err = gate.Exec(`SELECT pg_advisory_xact_lock(hashtext($1),737)`, f.schema)
	require.NoError(t, err)
	_, err = f.admin.Exec(`CREATE FUNCTION embedding_recovery_gate() RETURNS trigger LANGUAGE plpgsql AS $b$ BEGIN IF NEW.session_id='b' AND NEW.state='ready' THEN PERFORM pg_advisory_xact_lock(hashtext(TG_TABLE_SCHEMA),737); END IF; RETURN NEW; END $b$; CREATE TRIGGER embedding_recovery_gate BEFORE UPDATE ON hosted_embedding_requirements FOR EACH ROW EXECUTE FUNCTION embedding_recovery_gate()`)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type outcome struct {
		r   HostedEmbeddingRetryResult
		err error
	}
	done := make(chan outcome, 1)
	go func() { r, err := store.RetryFailed(ctx, g.ID, 2); done <- outcome{r, err} }()
	waitCtx, stop := context.WithTimeout(t.Context(), 10*time.Second)
	defer stop()
	for {
		var blocked bool
		require.NoError(t, f.admin.QueryRowContext(waitCtx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE query LIKE 'UPDATE hosted_embedding_requirements SET state=%' AND wait_event='advisory')`).Scan(&blocked))
		if blocked {
			break
		}
		runtime.Gosched()
	}
	cancel()
	v := <-done
	assert.Error(t, v.err)
	assert.Zero(t, v.r.Retried)
	require.NoError(t, gate.Rollback())
	var failed int
	require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM hosted_embedding_requirements WHERE state='failed' AND attempt_fence=1`).Scan(&failed))
	assert.Equal(t, 2, failed)
}

func TestHostedEmbeddingRecoveryFailedCandidatesStayBounded(t *testing.T) {
	type measurement struct{ work, buffers float64 }
	measured := map[int]measurement{}
	for _, count := range []int{100, 10000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			f, store, g := embeddingFixture(t)
			_, err := f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent,prompt_evidence_discarded) SELECT 'history-'||lpad(n::text,5,'0'),'p','m','codex',n%2=0 FROM generate_series(1,$1::int)n`, count)
			require.NoError(t, err)
			settleEmbeddingScaleCorpus(t, store)
			_, err = f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent) VALUES('z-stale','p','m','codex'),('zz-valid','p','m','codex')`)
			require.NoError(t, err)
			reconcileEmbedding(t, store)
			// These two rows model terminal work independent of the ready/complete
			// history; behavior tests above obtain failures through the actual API.
			_, err = f.runtime.Exec(`UPDATE hosted_embedding_requirements SET state='failed',attempts=5 WHERE session_id IN ('z-stale','zz-valid'); UPDATE hosted_embedding_sources SET dirty=true WHERE session_id='z-stale'`)
			require.NoError(t, err)
			_, err = f.admin.Exec(`VACUUM ANALYZE hosted_embedding_requirements`)
			require.NoError(t, err)
			tx, err := f.runtime.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback() }()
			// PostgreSQL prefers scanning the few heap pages in the tiny fixture.
			// Compare the available index path there; the large fixture must
			// choose this bounded path without planner overrides.
			if count == 100 {
				_, err = tx.ExecContext(t.Context(), `SET LOCAL enable_seqscan=off`)
				require.NoError(t, err)
			}
			var raw []byte
			require.NoError(t, tx.QueryRowContext(t.Context(), `EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) `+hostedEmbeddingFailedSQL, f.tenant, g.ID, 1).Scan(&raw))
			var plan []struct {
				Plan struct {
					embeddingExplainPlan
					IndexName        string  `json:"Index Name"`
					SharedHitBlocks  float64 `json:"Shared Hit Blocks"`
					SharedReadBlocks float64 `json:"Shared Read Blocks"`
				} `json:"Plan"`
			}
			require.NoError(t, json.Unmarshal(raw, &plan))
			require.Len(t, plan, 1)
			work, _ := embeddingPlanWork(plan[0].Plan.embeddingExplainPlan)
			buffers := plan[0].Plan.SharedHitBlocks + plan[0].Plan.SharedReadBlocks
			// Inspect the actual execution tree's index name, not the SQL text.
			var tree []struct {
				Plan recoveryIndexPlan `json:"Plan"`
			}
			require.NoError(t, json.Unmarshal(raw, &tree))
			require.Len(t, tree, 1)
			assert.True(t, recoveryPlanUsesIndex(tree[0].Plan, "hosted_embedding_failed"))
			require.NoError(t, tx.Rollback())
			r, err := store.RetryFailed(t.Context(), g.ID, 1)
			require.NoError(t, err)
			assert.Equal(t, HostedEmbeddingRetryResult{GenerationID: g.ID, Examined: 1, Skipped: 1}, r)
			assert.Equal(t, float64(1), work)
			assert.Less(t, buffers, float64(32))
			measured[count] = measurement{work, buffers}
			t.Logf("history=%d failed candidate leaf tuples=%.0f buffers=%.0f", count, work, buffers)
		})
	}
	assert.Equal(t, measured[100].work, measured[10000].work)
	assert.LessOrEqual(t, measured[10000].buffers, measured[100].buffers+8)
}

type recoveryIndexPlan struct {
	IndexName string              `json:"Index Name"`
	Plans     []recoveryIndexPlan `json:"Plans"`
}

func recoveryPlanUsesIndex(plan recoveryIndexPlan, name string) bool {
	if plan.IndexName == name {
		return true
	}
	for _, child := range plan.Plans {
		if recoveryPlanUsesIndex(child, name) {
			return true
		}
	}
	return false
}

// Ignoring the immutable generation policy would strand explicitly included
// automated sessions even when their failed requirement is current.
func TestHostedEmbeddingRecoveryUsesGenerationEligibilityPolicy(t *testing.T) {
	f, store, _ := embeddingFixture(t)
	recipe := embeddingRecipe()
	recipe.IncludeAutomated = true
	generation, err := ProvisionHostedEmbeddings(t.Context(), f.admin, f.schema, f.tenant, recipe, "with-automated", f.role)
	require.NoError(t, err)
	_, err = f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent,is_automated) VALUES('automated','p','m','codex',true)`)
	require.NoError(t, err)
	reconcileEmbedding(t, store)
	leases, err := store.Claim(t.Context(), "worker", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	assert.Equal(t, generation.ID, leases[0].GenerationID)
	require.NoError(t, store.Fail(t.Context(), leases[0], "encoder_unavailable", false))
	result, err := store.RetryFailed(t.Context(), generation.ID, 1)
	require.NoError(t, err)
	assert.Equal(t, HostedEmbeddingRetryResult{GenerationID: generation.ID, Examined: 1, Retried: 1}, result)
}

// An index with the right columns and predicate can still lose the query's
// ordinary text ordering. Neither recovery nor owner reprovision may accept it.
func TestHostedEmbeddingRecoveryRejectsAlteredIndexOrdering(t *testing.T) {
	for _, kind := range []string{"operator class", "collation"} {
		t.Run(kind, func(t *testing.T) {
			f, store, generation, snapshot := embeddingOne(t)
			require.NoError(t, store.Fail(t.Context(), snapshot.Lease, "encoder_unavailable", false))
			sessionKey := "session_id text_pattern_ops"
			if kind == "collation" {
				// Resolve an available deterministic collation with a different OID from
				// the column, without assuming an OS-specific locale name exists.
				var collation string
				err := f.admin.QueryRow(`SELECT format('%I.%I',n.nspname,c.collname)
 FROM pg_collation c JOIN pg_namespace n ON n.oid=c.collnamespace
 JOIN pg_attribute a ON a.attrelid='hosted_embedding_requirements'::regclass AND a.attname='session_id'
 WHERE c.oid<>a.attcollation AND c.collisdeterministic
 AND c.collencoding IN (-1,pg_char_to_encoding(current_setting('server_encoding')))
 ORDER BY c.oid LIMIT 1`).Scan(&collation)
				if err == sql.ErrNoRows {
					t.Skip("no alternate deterministic collation available")
				}
				require.NoError(t, err)
				sessionKey = "session_id COLLATE " + collation
			}
			_, err := f.admin.Exec(`DROP INDEX hosted_embedding_failed; CREATE INDEX hosted_embedding_failed ON hosted_embedding_requirements(tenant_id,generation_id,` + sessionKey + `) WHERE state='failed'`)
			require.NoError(t, err)
			var before, after, definition string
			dataQuery := `SELECT jsonb_build_array((SELECT to_jsonb(r) FROM hosted_embedding_requirements r),(SELECT to_jsonb(s) FROM hosted_embedding_sources s),(SELECT to_jsonb(g) FROM hosted_embedding_generations g),(SELECT to_jsonb(s) FROM hosted_embedding_state s))::text`
			require.NoError(t, f.runtime.QueryRow(dataQuery).Scan(&before))
			require.NoError(t, f.admin.QueryRow(`SELECT pg_get_indexdef('hosted_embedding_failed'::regclass)`).Scan(&definition))
			result, err := store.RetryFailed(t.Context(), generation.ID, 1)
			assert.ErrorIs(t, err, ErrHostedEmbeddingRecoveryIndexUnavailable)
			assert.Equal(t, HostedEmbeddingRetryResult{GenerationID: generation.ID}, result)
			require.NoError(t, f.runtime.QueryRow(dataQuery).Scan(&after))
			assert.Equal(t, before, after)
			_, err = ProvisionHostedEmbeddings(t.Context(), f.admin, f.schema, f.tenant, embeddingRecipe(), "initial", f.role)
			assert.ErrorIs(t, err, ErrHostedEmbeddingRecoveryIndexUnavailable)
			var afterDefinition string
			require.NoError(t, f.admin.QueryRow(`SELECT pg_get_indexdef('hosted_embedding_failed'::regclass)`).Scan(&afterDefinition))
			assert.Equal(t, definition, afterDefinition)
			require.NoError(t, f.runtime.QueryRow(dataQuery).Scan(&after))
			assert.Equal(t, before, after)
		})
	}
}
