//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func embeddingOne(t *testing.T) (hostedFixture, *HostedEmbeddingStore, HostedEmbeddingGeneration, *HostedEmbeddingSnapshot) {
	t.Helper()
	f, s, g := embeddingFixture(t)
	_, e := f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent) VALUES('s','p','m','codex');INSERT INTO messages(session_id,ordinal,role,content) VALUES('s',0,'user','hello')`)
	require.NoError(t, e)
	reconcileEmbedding(t, s)
	l, e := s.Claim(t.Context(), "worker", 1, time.Minute)
	require.NoError(t, e)
	require.Len(t, l, 1)
	snap, e := s.ReadSession(t.Context(), l[0])
	require.NoError(t, e)
	return f, s, g, snap
}
func embeddingOneVector() []HostedEmbeddingVector {
	return []HostedEmbeddingVector{{DocumentKey: "o:s:0", ChunkIndex: 0, Values: []float32{1, 0, 0}}}
}
func TestHostedEmbeddingLeaseReplacementAndFailure(t *testing.T) {
	f, s, g, snap := embeddingOne(t)
	_, e := f.runtime.Exec(`UPDATE hosted_embedding_requirements SET lease_expires_at=clock_timestamp()-interval '1 second'`)
	require.NoError(t, e)
	leases, e := s.Claim(t.Context(), "replacement", 1, time.Minute)
	require.NoError(t, e)
	require.Len(t, leases, 1)
	assert.Greater(t, leases[0].AttemptFence, snap.Lease.AttemptFence)
	assert.ErrorIs(t, s.Publish(t.Context(), snap, embeddingOneVector()), ErrHostedEmbeddingLeaseLost)
	_, e = s.Heartbeat(t.Context(), snap.Lease, time.Minute)
	assert.ErrorIs(t, e, ErrHostedEmbeddingLeaseLost)
	assert.ErrorIs(t, s.Fail(t.Context(), snap.Lease, "encoder_timeout", false), ErrHostedEmbeddingLeaseLost)
	require.NoError(t, s.Fail(t.Context(), leases[0], "private endpoint detail", false))
	ok, e := s.Activate(t.Context(), g.ID)
	require.NoError(t, e)
	assert.False(t, ok)
	status, e := s.Status(t.Context())
	require.NoError(t, e)
	assert.Equal(t, int64(1), status.Desired.Failed)
	assert.Equal(t, map[string]int64{"encoder_unavailable": 1}, status.Desired.Errors)
}
func TestHostedEmbeddingDelayedSourceAndDeletion(t *testing.T) {
	f, s, g, snap := embeddingOne(t)
	_, e := f.runtime.Exec(`UPDATE messages SET content='new text' WHERE session_id='s'`)
	require.NoError(t, e)
	assert.ErrorIs(t, s.Publish(t.Context(), snap, embeddingOneVector()), ErrHostedEmbeddingStale)
	_, e = s.ReadSession(t.Context(), snap.Lease)
	assert.ErrorIs(t, e, ErrHostedEmbeddingStale)
	reconcileEmbedding(t, s)
	l, e := s.Claim(t.Context(), "current", 1, time.Minute)
	require.NoError(t, e)
	require.Len(t, l, 1)
	newSnap, e := s.ReadSession(t.Context(), l[0])
	require.NoError(t, e)
	assert.Equal(t, "new text", newSnap.Documents[0].Unit.Content)
	_, e = f.runtime.Exec(`DELETE FROM sessions WHERE id='s'`)
	require.NoError(t, e)
	assert.ErrorIs(t, s.Publish(t.Context(), newSnap, embeddingOneVector()), ErrHostedEmbeddingStale)
	reconcileEmbedding(t, s)
	ok, e := s.Activate(t.Context(), g.ID)
	require.NoError(t, e)
	assert.True(t, ok)
	var revision int64
	require.NoError(t, f.runtime.QueryRow(`SELECT revision FROM hosted_embedding_sources WHERE session_id='s'`).Scan(&revision))
	assert.Greater(t, revision, newSnap.Lease.Revision)
}
func waitEmbeddingBlocked(t *testing.T, admin *sql.DB, pid int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for {
		var blocked bool
		e := admin.QueryRowContext(ctx, `SELECT cardinality(pg_blocking_pids($1))>0`, pid).Scan(&blocked)
		require.NoError(t, e)
		if blocked {
			return
		}
		runtime.Gosched()
	}
}
func TestHostedEmbeddingPublishDoesNotLockUncommittedSource(t *testing.T) {
	for _, v := range []struct{ name, query string }{{"message edit", `UPDATE messages SET content='changed while publishing' WHERE session_id='s'`}, {"session deletion", `DELETE FROM sessions WHERE id='s'`}} {
		t.Run(v.name, func(t *testing.T) { testEmbeddingPublishConcurrentSource(t, v.query) })
	}
}
func testEmbeddingPublishConcurrentSource(t *testing.T, query string) {
	f, s, _, snap := embeddingOne(t)
	// Hold the publication inside its first document INSERT, after corpus fence.
	gate, e := f.admin.BeginTx(t.Context(), nil)
	require.NoError(t, e)
	defer func() { _ = gate.Rollback() }()
	_, e = gate.Exec(`SELECT pg_advisory_xact_lock(hashtext($1),731)`, f.schema)
	require.NoError(t, e)
	_, e = f.admin.Exec(`CREATE FUNCTION embedding_test_gate() RETURNS trigger LANGUAGE plpgsql AS $b$ BEGIN PERFORM pg_advisory_xact_lock(hashtext(TG_TABLE_SCHEMA),731); RETURN NEW; END $b$;CREATE TRIGGER embedding_test_gate BEFORE INSERT ON hosted_embedding_documents FOR EACH ROW EXECUTE FUNCTION embedding_test_gate()`)
	require.NoError(t, e)
	done := make(chan error, 1)
	go func() { done <- s.Publish(t.Context(), snap, embeddingOneVector()) }()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for {
		var blocked bool
		e = f.admin.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE query LIKE 'INSERT INTO hosted_embedding_documents%' AND wait_event='advisory')`).Scan(&blocked)
		require.NoError(t, e)
		if blocked {
			break
		}
		runtime.Gosched()
	}
	writer, e := f.runtime.Conn(t.Context())
	require.NoError(t, e)
	defer writer.Close()
	var pid int
	require.NoError(t, writer.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid))
	writeDone := make(chan error, 1)
	go func() {
		_, err := writer.ExecContext(ctx, query)
		writeDone <- err
	}()
	waitEmbeddingBlocked(t, f.admin, pid)
	require.NoError(t, gate.Commit())
	require.NoError(t, <-done)
	require.NoError(t, <-writeDone)
	var visible int
	require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM hosted_embedding_documents d JOIN hosted_embedding_sources j ON j.session_id=d.session_id AND j.tenant_id=d.tenant_id AND j.revision=d.source_revision`).Scan(&visible))
	assert.Zero(t, visible)
}
func TestHostedEmbeddingActivationFencesWriter(t *testing.T) {
	f, s, g, snap := embeddingOne(t)
	require.NoError(t, s.Publish(t.Context(), snap, embeddingOneVector()))
	fence, e := f.runtime.BeginTx(t.Context(), nil)
	require.NoError(t, e)
	defer func() { _ = fence.Rollback() }()
	_, e = fence.Exec(`SELECT singleton FROM raw_corpus_state FOR UPDATE`)
	require.NoError(t, e)
	writer, e := f.runtime.Conn(t.Context())
	require.NoError(t, e)
	defer writer.Close()
	var pid int
	require.NoError(t, writer.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := writer.ExecContext(ctx, `UPDATE sessions SET is_automated=true WHERE id='s'`)
		done <- err
	}()
	waitEmbeddingBlocked(t, f.admin, pid)
	ready, e := embeddingActivationReady(ctx, fence, g.ID)
	require.NoError(t, e)
	assert.True(t, ready)
	_, e = fence.Exec(`UPDATE hosted_embedding_state SET active_generation_id=$1`, g.ID)
	require.NoError(t, e)
	require.NoError(t, fence.Commit())
	require.NoError(t, <-done)
	ok, e := s.Activate(t.Context(), g.ID)
	require.NoError(t, e)
	assert.False(t, ok)
	reconcileEmbedding(t, s)
	ok, e = s.Activate(t.Context(), g.ID)
	require.NoError(t, e)
	assert.True(t, ok)
}
func TestHostedEmbeddingCatalogTamper(t *testing.T) {
	for _, ddl := range []string{`ALTER TABLE hosted_embedding_sources ALTER COLUMN dirty DROP NOT NULL`, `ALTER TABLE raw_embedding_outbox ALTER COLUMN embedding_consumed DROP NOT NULL`, `DROP INDEX hosted_embedding_due`, `ALTER TABLE hosted_embedding_chunks_g1 ALTER COLUMN embedding TYPE public.halfvec(4)`, `ALTER TABLE hosted_embedding_documents DROP CONSTRAINT hosted_fk_1`, `ALTER TABLE hosted_embedding_sources NO FORCE ROW LEVEL SECURITY`, `ALTER FUNCTION hosted_embedding_message_notify() SECURITY DEFINER`, `DROP TRIGGER hosted_embedding_session_notify ON sessions`} {
		t.Run(fmt.Sprint(ddl), func(t *testing.T) {
			f, _, _ := embeddingFixture(t)
			var extensionSchema string
			require.NoError(t, f.admin.QueryRow(`SELECT n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid=e.extnamespace WHERE e.extname='vector'`).Scan(&extensionSchema))
			quoted, _ := quoteIdentifier(extensionSchema)
			_, e := f.admin.Exec(strings.ReplaceAll(ddl, "public.halfvec", quoted+".halfvec"))
			require.NoError(t, e)
			assert.Error(t, CheckHostedTenant(t.Context(), f.runtime, f.schema, f.tenant))
		})
	}
}

func TestHostedEmbeddingExpiryDuringPublicationRollsBack(t *testing.T) {
	f, s, _, snap := embeddingOne(t)
	// Controlled expiry at the document-write boundary tests the final server
	// clock guard, including rollback of the inserted document and vector rows.
	_, e := f.admin.Exec(`CREATE FUNCTION embedding_expire_test() RETURNS trigger LANGUAGE plpgsql AS $b$ BEGIN UPDATE hosted_embedding_requirements SET lease_expires_at=clock_timestamp()-interval '1 microsecond' WHERE generation_id=NEW.generation_id AND session_id=NEW.session_id; RETURN NEW; END $b$;CREATE TRIGGER embedding_expire_test BEFORE INSERT ON hosted_embedding_documents FOR EACH ROW EXECUTE FUNCTION embedding_expire_test()`)
	require.NoError(t, e)
	assert.ErrorIs(t, s.Publish(t.Context(), snap, embeddingOneVector()), ErrHostedEmbeddingLeaseLost)
	var docs, chunks int
	require.NoError(t, f.runtime.QueryRow(`SELECT (SELECT count(*) FROM hosted_embedding_documents),(SELECT count(*) FROM hosted_embedding_chunks_g1)`).Scan(&docs, &chunks))
	assert.Zero(t, docs)
	assert.Zero(t, chunks)
	var state string
	require.NoError(t, f.runtime.QueryRow(`SELECT state FROM hosted_embedding_requirements WHERE session_id='s'`).Scan(&state))
	assert.Equal(t, "leased", state)
}
func TestHostedEmbeddingRawCurationAndReplacement(t *testing.T) {
	f := newProjectionFixture(t)
	g, e := ProvisionHostedEmbeddings(t.Context(), f.admin, f.schema, f.tenant, embeddingRecipe(), "initial", f.role)
	require.NoError(t, e)
	s, e := NewHostedEmbeddingStore(t.Context(), f.runtime, HostedEmbeddingOptions{Schema: f.schema, Tenant: f.tenant})
	require.NoError(t, e)
	m, accepted := f.accept(t, "device-a", "capture-a", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("same prompt")))
	reconcileEmbedding(t, s)
	leases, e := s.Claim(t.Context(), "worker", 1, time.Minute)
	require.NoError(t, e)
	require.Len(t, leases, 1)
	snap, e := s.ReadSession(t.Context(), leases[0])
	require.NoError(t, e)
	require.Len(t, snap.Documents, 2)
	vectors := []HostedEmbeddingVector{{snap.Documents[0].Key, 0, []float32{1, 0, 0}}, {snap.Documents[1].Key, 0, []float32{0, 1, 0}}}
	require.NoError(t, s.Publish(t.Context(), snap, vectors))
	ok, e := s.Activate(t.Context(), g.ID)
	require.NoError(t, e)
	assert.True(t, ok)
	// A lower outbox key commits after reconciliation; there is no MAX cursor.
	delayed, e := f.runtime.BeginTx(t.Context(), nil)
	require.NoError(t, e)
	defer func() { _ = delayed.Rollback() }()
	_, e = delayed.Exec(`INSERT INTO raw_embedding_outbox(session_id,selection_revision,corpus_revision,content_revision,action) SELECT session_id,0,0,content_revision,'remove' FROM raw_content_revisions WHERE session_id=$1`, snap.Lease.SessionID)
	require.NoError(t, e)
	reconcileEmbedding(t, s)
	require.NoError(t, delayed.Commit())
	ok, e = s.Activate(t.Context(), g.ID)
	require.NoError(t, e)
	assert.False(t, ok)
	reconcileEmbedding(t, s)
	var retained int
	require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM hosted_embedding_documents WHERE session_id=$1`, snap.Lease.SessionID).Scan(&retained))
	assert.Equal(t, 2, retained)
	require.NoError(t, f.sink.SetCuration(t.Context(), "codex:portable", "trashed", true))
	var rev int64
	require.NoError(t, f.runtime.QueryRow(`SELECT revision FROM hosted_embedding_sources WHERE session_id=$1`, snap.Lease.SessionID).Scan(&rev))
	assert.Greater(t, rev, snap.Lease.Revision)
	require.NoError(t, f.sink.SetCuration(t.Context(), "codex:portable", "trashed", false))
	require.NoError(t, f.runtime.QueryRow(`SELECT revision FROM hosted_embedding_sources WHERE session_id=$1`, snap.Lease.SessionID).Scan(&rev))
	assert.Greater(t, rev, snap.Lease.Revision+1)
	next, _ := f.accept(t, "device-a", "capture-next", accepted.Receipt)
	outcome := projectionOutcome("same prompt")
	outcome.Outcome.Results[0].Result.Messages[1].Content = "changed reply"
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, next), next, outcome))
	reconcileEmbedding(t, s)
	leases, e = s.Claim(t.Context(), "worker", 2, time.Minute)
	require.NoError(t, e)
	require.Len(t, leases, 1)
	assert.NotEqual(t, snap.Lease.SessionID, leases[0].SessionID)
	replacement, e := s.ReadSession(t.Context(), leases[0])
	require.NoError(t, e)
	reused, e := s.ReusableVectors(t.Context(), replacement)
	require.NoError(t, e)
	require.Len(t, reused, 1)
	assert.Equal(t, replacement.Documents[0].Key, reused[0].DocumentKey)
	assert.Equal(t, []float32{1, 0, 0}, reused[0].Values)
	var oldDocs int
	require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM hosted_embedding_documents WHERE session_id=$1`, snap.Lease.SessionID).Scan(&oldDocs))
	assert.Zero(t, oldDocs)
}

func TestHostedEmbeddingReadSnapshotRejectsConcurrentEdit(t *testing.T) {
	f, s, _, snap := embeddingOne(t)
	gate, e := f.admin.BeginTx(t.Context(), nil)
	require.NoError(t, e)
	defer func() { _ = gate.Rollback() }()
	_, e = gate.Exec(`SELECT pg_advisory_xact_lock(hashtext($1),733)`, f.schema)
	require.NoError(t, e)
	_, e = f.admin.Exec(`CREATE FUNCTION embedding_read_gate() RETURNS boolean LANGUAGE plpgsql VOLATILE AS $b$ BEGIN PERFORM pg_advisory_xact_lock(hashtext(current_schema()),733); RETURN true; END $b$; ALTER POLICY hosted_tenant_policy ON messages USING (tenant_id=current_setting('agentsview.tenant_id',true) AND embedding_read_gate())`)
	require.NoError(t, e)
	done := make(chan error, 1)
	go func() { _, err := s.ReadSession(t.Context(), snap.Lease); done <- err }()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for {
		var blocked bool
		e = f.admin.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE query LIKE 'SELECT ordinal,%' AND wait_event='advisory')`).Scan(&blocked)
		require.NoError(t, e)
		if blocked {
			break
		}
		runtime.Gosched()
	}
	// Owner bypasses the test-only read barrier. The source notifier still runs.
	_, e = f.admin.Exec(`UPDATE messages SET content='revision B' WHERE session_id='s'`)
	require.NoError(t, e)
	require.NoError(t, gate.Commit())
	assert.ErrorIs(t, <-done, ErrHostedEmbeddingStale)
	var dirty bool
	require.NoError(t, f.runtime.QueryRow(`SELECT dirty FROM hosted_embedding_sources WHERE session_id='s'`).Scan(&dirty))
	assert.True(t, dirty)
}
