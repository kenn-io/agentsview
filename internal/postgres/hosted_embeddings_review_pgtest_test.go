//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func finishEmbeddingRequirements(t *testing.T, s *HostedEmbeddingStore) {
	t.Helper()
	reconcileEmbedding(t, s)
	for round := 0; round < 10; round++ {
		leases, e := s.Claim(t.Context(), "finish", 64, time.Minute)
		require.NoError(t, e)
		if len(leases) == 0 {
			return
		}
		for _, lease := range leases {
			snap, e := s.ReadSession(t.Context(), lease)
			require.NoError(t, e)
			var vectors []HostedEmbeddingVector
			for _, doc := range snap.Documents {
				for _, chunk := range doc.Chunks {
					vectors = append(vectors, HostedEmbeddingVector{doc.Key, chunk.Index, []float32{1, 0, 0}})
				}
			}
			require.NoError(t, s.Publish(t.Context(), snap, vectors))
		}
	}
	t.Fatal("embedding fixture did not settle")
}

func TestHostedEmbeddingRetiredGenerationRecovery(t *testing.T) {
	for _, ownerResume := range []bool{false, true} {
		t.Run(fmt.Sprintf("owner_resume_%t", ownerResume), func(t *testing.T) {
			f, s, a := embeddingFixture(t)
			_, e := f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent) VALUES('changed','p','m','codex'),('deleted','p','m','codex');INSERT INTO messages(session_id,ordinal,role,content) VALUES('changed',0,'user','original'),('deleted',0,'user','remove me')`)
			require.NoError(t, e)
			finishEmbeddingRequirements(t, s)
			ok, e := s.Activate(t.Context(), a.ID)
			require.NoError(t, e)
			require.True(t, ok)
			b, e := ProvisionHostedEmbeddings(t.Context(), f.admin, f.schema, f.tenant, embeddingRecipe(), "second", f.role)
			require.NoError(t, e)
			finishEmbeddingRequirements(t, s)
			ok, e = s.Activate(t.Context(), b.ID)
			require.NoError(t, e)
			require.True(t, ok)
			_, e = f.runtime.Exec(`UPDATE messages SET content='changed while retired' WHERE session_id='changed';DELETE FROM sessions WHERE id='deleted';INSERT INTO sessions(id,project,machine,agent) VALUES('new','p','m','codex');INSERT INTO messages(session_id,ordinal,role,content) VALUES('new',0,'user','new while retired')`)
			require.NoError(t, e)
			finishEmbeddingRequirements(t, s)
			var dirty int
			require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM hosted_embedding_sources WHERE dirty`).Scan(&dirty))
			require.Zero(t, dirty)
			if ownerResume {
				_, e = ProvisionHostedEmbeddings(t.Context(), f.admin, f.schema, f.tenant, embeddingRecipe(), "initial", f.role)
			} else {
				e = s.SelectDesired(t.Context(), a.ID)
			}
			require.NoError(t, e)
			ok, e = s.Activate(t.Context(), a.ID)
			require.NoError(t, e)
			assert.False(t, ok, "retired completion cannot prove the current corpus")
			reconcileEmbedding(t, s)
			ok, e = s.Activate(t.Context(), a.ID)
			require.NoError(t, e)
			assert.False(t, ok, "new and edited documents still need publication")
			var deletedState string
			var deletedRevision, currentRevision int64
			require.NoError(t, f.runtime.QueryRow(`SELECT r.state,r.completed_revision,j.revision FROM hosted_embedding_requirements r JOIN hosted_embedding_sources j USING(tenant_id,session_id) WHERE r.generation_id=$1 AND r.session_id='deleted'`, a.ID).Scan(&deletedState, &deletedRevision, &currentRevision))
			assert.Equal(t, "complete", deletedState)
			assert.Equal(t, currentRevision, deletedRevision)
			finishEmbeddingRequirements(t, s)
			ok, e = s.Activate(t.Context(), a.ID)
			require.NoError(t, e)
			assert.True(t, ok)
			rows, e := f.runtime.Query(`SELECT session_id,content FROM hosted_embedding_documents WHERE generation_id=$1 ORDER BY session_id`, a.ID)
			require.NoError(t, e)
			var actual []string
			for rows.Next() {
				var id, content string
				require.NoError(t, rows.Scan(&id, &content))
				actual = append(actual, id+": "+content)
			}
			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
			assert.Equal(t, []string{"changed: changed while retired", "new: new while retired"}, actual)
		})
	}
}

func TestHostedEmbeddingDesiredRetryPreservesProgress(t *testing.T) {
	f, s, g := embeddingFixture(t)
	_, e := f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent) VALUES('a','p','m','codex'),('b','p','m','codex');INSERT INTO messages(session_id,ordinal,role,content) VALUES('a',0,'user','first'),('b',0,'user','second')`)
	require.NoError(t, e)
	_, e = s.Reconcile(t.Context(), 1)
	require.NoError(t, e)
	leases, e := s.Claim(t.Context(), "in-flight", 1, time.Minute)
	require.NoError(t, e)
	require.Len(t, leases, 1)
	_, e = s.ReadSession(t.Context(), leases[0])
	require.NoError(t, e)
	require.NoError(t, s.SelectDesired(t.Context(), g.ID))
	same, e := ProvisionHostedEmbeddings(t.Context(), f.admin, f.schema, f.tenant, embeddingRecipe(), "initial", f.role)
	require.NoError(t, e)
	assert.Equal(t, g.ID, same.ID)
	var cursor, token, state string
	var finished bool
	require.NoError(t, f.runtime.QueryRow(`SELECT backfill_after_session_id,backfill_finished FROM hosted_embedding_generations WHERE id=$1`, g.ID).Scan(&cursor, &finished))
	assert.Equal(t, "a", cursor)
	assert.False(t, finished)
	require.NoError(t, f.runtime.QueryRow(`SELECT lease_token,state FROM hosted_embedding_requirements WHERE generation_id=$1 AND session_id='a'`, g.ID).Scan(&token, &state))
	assert.Equal(t, leases[0].Token, token)
	assert.Equal(t, "leased", state)
}

// The ACL change is uncommitted and visible only to this one connection. Other
// fixtures retain public USAGE, and closing/rolling back restores the ACL.
func embeddingRevokedUsagePool(t *testing.T, f hostedFixture) *sql.DB {
	t.Helper()
	config, e := pgx.ParseConfig(testPGURL(t))
	require.NoError(t, e)
	quotedSchema, _ := quoteIdentifier(f.schema)
	quotedRole, _ := quoteIdentifier(f.role)
	pool := stdlib.OpenDB(*config, stdlib.OptionAfterConnect(func(ctx context.Context, conn *pgx.Conn) error {
		for _, query := range []string{`BEGIN`, `REVOKE USAGE ON SCHEMA public FROM PUBLIC`, `REVOKE USAGE ON SCHEMA public FROM ` + quotedRole, `SET SESSION AUTHORIZATION ` + quotedRole} {
			if _, e := conn.Exec(ctx, query); e != nil {
				return e
			}
		}
		_, e := conn.Exec(ctx, `SELECT set_config('search_path',$1,false),set_config('agentsview.tenant_id',$2,false)`, quotedSchema+", pg_temp", f.tenant)
		return e
	}))
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	t.Cleanup(func() {
		_, e := pool.ExecContext(context.Background(), `ROLLBACK`)
		assert.NoError(t, e)
		assert.NoError(t, pool.Close())
	})
	return pool
}
func TestHostedEmbeddingRuntimeRejectsRevokedVectorUsage(t *testing.T) {
	f, s, _ := embeddingFixture(t)
	pool := embeddingRevokedUsagePool(t, f)
	var allowed bool
	require.NoError(t, pool.QueryRow(`SELECT has_schema_privilege('public','USAGE')`).Scan(&allowed))
	require.False(t, allowed)
	_, e := NewHostedEmbeddingStore(t.Context(), pool, HostedEmbeddingOptions{Schema: f.schema, Tenant: f.tenant})
	assert.Error(t, e)
	existing := *s
	existing.pg = pool
	assert.Error(t, existing.CheckWritable(t.Context()))
}

type embeddingExplainPlan struct {
	Node        string                 `json:"Node Type"`
	ActualRows  float64                `json:"Actual Rows"`
	ActualLoops float64                `json:"Actual Loops"`
	Removed     float64                `json:"Rows Removed by Filter"`
	IndexCond   string                 `json:"Index Cond"`
	Plans       []embeddingExplainPlan `json:"Plans"`
}

func embeddingPlanWork(plan embeddingExplainPlan) (float64, []string) {
	var work float64
	var conditions []string
	if len(plan.Plans) == 0 {
		work = (plan.ActualRows + plan.Removed) * plan.ActualLoops
	}
	if plan.IndexCond != "" {
		conditions = append(conditions, plan.IndexCond)
	}
	for _, child := range plan.Plans {
		n, c := embeddingPlanWork(child)
		work += n
		conditions = append(conditions, c...)
	}
	return work, conditions
}
func embeddingExplain(t *testing.T, f hostedFixture, query string, args ...any) (float64, []string) {
	t.Helper()
	tx, e := f.runtime.BeginTx(t.Context(), nil)
	require.NoError(t, e)
	defer func() { _ = tx.Rollback() }()
	var raw []byte
	require.NoError(t, tx.QueryRowContext(t.Context(), `EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) `+query, args...).Scan(&raw))
	var result []struct {
		Plan embeddingExplainPlan `json:"Plan"`
	}
	require.NoError(t, json.Unmarshal(raw, &result))
	require.Len(t, result, 1)
	return embeddingPlanWork(result[0].Plan)
}
func TestHostedEmbeddingFutureQueuesHaveBoundedIndexRanges(t *testing.T) {
	for _, count := range []int{1000, 10000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			f, s, a := embeddingFixture(t)
			finishEmbeddingRequirements(t, s)
			ok, e := s.Activate(t.Context(), a.ID)
			require.NoError(t, e)
			require.True(t, ok)
			_, e = ProvisionHostedEmbeddings(t.Context(), f.admin, f.schema, f.tenant, embeddingRecipe(), "second", f.role)
			require.NoError(t, e)
			_, e = f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent) SELECT 'future-'||n,'p','m','codex' FROM generate_series(1,$1) n`, count)
			require.NoError(t, e)
			_, e = f.runtime.Exec(`UPDATE hosted_embedding_sources SET dirty=false`)
			require.NoError(t, e)
			_, e = f.runtime.Exec(`INSERT INTO hosted_embedding_requirements(tenant_id,generation_id,session_id,required_revision,eligible,state,attempts,available_at,lease_expires_at) SELECT j.tenant_id,g.id,j.session_id,1,true,CASE WHEN n%2=0 THEN 'leased' ELSE 'retry' END,1,clock_timestamp()+interval '1 day',clock_timestamp()+interval '1 day' FROM hosted_embedding_sources j CROSS JOIN hosted_embedding_generations g CROSS JOIN LATERAL (SELECT split_part(j.session_id,'-',2)::integer n) x WHERE j.session_id LIKE 'future-%'`)
			require.NoError(t, e)
			_, e = f.admin.Exec(`ANALYZE hosted_embedding_requirements;ANALYZE hosted_embedding_sources`)
			require.NoError(t, e)
			for _, runnable := range []bool{false, true} {
				if runnable {
					_, e = f.runtime.Exec(`UPDATE hosted_embedding_requirements SET available_at='2000-01-01',lease_expires_at='2000-01-01' WHERE session_id IN ('future-1','future-2')`)
					require.NoError(t, e)
				}
				for _, v := range []struct{ name, query, column string }{{"expiry", hostedEmbeddingExpiredSQL, "lease_expires_at"}, {"due", hostedEmbeddingDueSQL, "available_at"}} {
					t.Run(fmt.Sprintf("%s/runnable_%t", v.name, runnable), func(t *testing.T) {
						work, conditions := embeddingExplain(t, f, v.query, f.tenant, 1, 5, a.ID, time.Now())
						t.Logf("future sources=%d leaf tuples=%.0f index conditions=%v", count, work, conditions)
						assert.LessOrEqual(t, work, float64(16), "future rows must be excluded by index range, not scanned and filtered")
						assert.Contains(t, fmt.Sprint(conditions), v.column)
					})
				}
				leases, e := s.Claim(t.Context(), "queue", 1, time.Minute)
				require.NoError(t, e)
				if runnable {
					require.Len(t, leases, 1)
				} else {
					assert.Empty(t, leases)
				}
			}

		})
	}
}
func TestHostedEmbeddingClaimRotatesServicedGenerations(t *testing.T) {
	f, s, a := embeddingFixture(t)
	finishEmbeddingRequirements(t, s)
	ok, e := s.Activate(t.Context(), a.ID)
	require.NoError(t, e)
	require.True(t, ok)
	b, e := ProvisionHostedEmbeddings(t.Context(), f.admin, f.schema, f.tenant, embeddingRecipe(), "second", f.role)
	require.NoError(t, e)
	_, e = f.runtime.Exec(`INSERT INTO hosted_embedding_sources(tenant_id,session_id,revision,dirty) SELECT $1,'a-'||n,1,false FROM generate_series(1,5) n UNION ALL SELECT $1,'z',1,false`, f.tenant)
	require.NoError(t, e)
	_, e = f.runtime.Exec(`INSERT INTO hosted_embedding_requirements(tenant_id,generation_id,session_id,required_revision,eligible,state,available_at) SELECT tenant_id,CASE WHEN session_id='z' THEN $2::bigint ELSE $1::bigint END,session_id,1,true,'ready','2000-01-01'::timestamptz FROM hosted_embedding_sources`, a.ID, b.ID)
	require.NoError(t, e)
	other, e := NewHostedEmbeddingStore(t.Context(), f.runtime, HostedEmbeddingOptions{Schema: f.schema, Tenant: f.tenant})
	require.NoError(t, e)
	var generations []int64
	for _, worker := range []*HostedEmbeddingStore{s, other} {
		leases, e := worker.Claim(t.Context(), "fair", 1, time.Minute)
		require.NoError(t, e)
		require.Len(t, leases, 1)
		generations = append(generations, leases[0].GenerationID)
	}
	assert.ElementsMatch(t, []int64{a.ID, b.ID}, generations)
}

func TestHostedEmbeddingExhaustedCandidateDoesNotBlockQueue(t *testing.T) {
	f, s, g := embeddingFixture(t)
	_, e := f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent) VALUES('a','p','m','codex'),('b','p','m','codex');INSERT INTO messages(session_id,ordinal,role,content) VALUES('a',0,'user','first'),('b',0,'user','second')`)
	require.NoError(t, e)
	reconcileEmbedding(t, s)
	// A previous worker used a higher retry ceiling. The current ceiling must
	// retire its exhausted head row without starving the next runnable source.
	_, e = f.runtime.Exec(`UPDATE hosted_embedding_requirements SET state='retry',attempts=5,available_at='2000-01-01' WHERE session_id='a'`)
	require.NoError(t, e)
	leases, e := s.Claim(t.Context(), "lower-ceiling", 1, time.Minute)
	require.NoError(t, e)
	assert.Empty(t, leases)
	var state string
	require.NoError(t, f.runtime.QueryRow(`SELECT state FROM hosted_embedding_requirements WHERE generation_id=$1 AND session_id='a'`, g.ID).Scan(&state))
	assert.Equal(t, "failed", state)
	leases, e = s.Claim(t.Context(), "lower-ceiling", 1, time.Minute)
	require.NoError(t, e)
	require.Len(t, leases, 1)
	assert.Equal(t, "b", leases[0].SessionID)
}
