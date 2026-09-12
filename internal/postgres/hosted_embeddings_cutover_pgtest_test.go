//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Removing the durable default must fail even if all workers still use the
// automatic path: older running workers rely on database policy too.
func TestHostedEmbeddingCutoverDurableDefault(t *testing.T) {
	f, _, _ := embeddingFixture(t)
	var mode sql.NullString
	require.NoError(t, f.runtime.QueryRow(`SELECT to_jsonb(g)->>'activation_mode' FROM hosted_embedding_generations g`).Scan(&mode))
	assert.Equal(t, sql.NullString{String: "automatic", Valid: true}, mode)
	// The old startup contract must reject the upgraded immutable function body.
	assert.ErrorContains(t, checkHostedCatalogEmbeddingPolicy(t.Context(), f.runtime, f.schema, f.tenant, true), "hosted_embedding_recipe_immutable")
}

// Bypassing automatic/manual selection, readiness, or desired selection must
// leave the prior generation visible to search.
func TestHostedEmbeddingCutoverManual(t *testing.T) {
	f, s, previous, snap := embeddingOne(t)
	ctx := t.Context()
	require.NoError(t, s.Publish(ctx, snap, embeddingOneVector()))
	activated, err := s.ActivateAutomatic(ctx, previous.ID)
	require.NoError(t, err)
	require.True(t, activated)
	manual, err := ProvisionHostedEmbeddingsWithOptions(ctx, f.admin, f.schema, f.tenant, embeddingRecipe(), "shadow", f.role, HostedEmbeddingProvisionOptions{ActivationMode: "manual"})
	require.NoError(t, err)
	assert.Equal(t, "manual", manual.ActivationMode)
	assert.Equal(t, previous.Recipe.Fingerprint, manual.Recipe.Fingerprint)
	activated, err = s.Activate(ctx, manual.ID)
	require.NoError(t, err)
	assert.False(t, activated)
	reconcileEmbedding(t, s)
	leases, err := s.Claim(ctx, "shadow", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	shadow, err := s.ReadSession(ctx, leases[0])
	require.NoError(t, err)
	require.NoError(t, s.Publish(ctx, shadow, embeddingOneVector()))
	status, err := s.Status(ctx)
	require.NoError(t, err)
	assert.True(t, status.ActivationReady)
	restarted, err := NewHostedEmbeddingStore(ctx, f.runtime, HostedEmbeddingOptions{Schema: f.schema, Tenant: f.tenant})
	require.NoError(t, err)
	for _, store := range []*HostedEmbeddingStore{s, restarted} {
		activated, err = store.ActivateAutomatic(ctx, manual.ID)
		require.NoError(t, err)
		assert.False(t, activated)
	}
	for _, id := range []int64{-1, 0, manual.ID + 1, previous.ID} {
		activated, err = s.Activate(ctx, id)
		require.NoError(t, err)
		assert.False(t, activated)
	}
	active, err := s.Active(ctx)
	require.NoError(t, err)
	require.NotNil(t, active)
	assert.Equal(t, previous.ID, active.ID)
	activated, err = s.Activate(ctx, manual.ID)
	require.NoError(t, err)
	assert.True(t, activated)
	active, err = s.Active(ctx)
	require.NoError(t, err)
	require.NotNil(t, active)
	assert.Equal(t, manual.ID, active.ID)
	activated, err = s.Activate(ctx, manual.ID)
	require.NoError(t, err)
	assert.True(t, activated)
}

// Old workers issue this SQL directly, so Go policy alone cannot protect the
// shadow. Approval must match both identifiers and expire on commit/rollback.
func TestHostedEmbeddingCutoverLegacySQLGuard(t *testing.T) {
	f, s, previous := embeddingFixture(t)
	ctx := t.Context()
	reconcileEmbedding(t, s)
	ok, err := s.Activate(ctx, previous.ID)
	require.NoError(t, err)
	require.True(t, ok)
	manual, err := ProvisionHostedEmbeddingsWithOptions(ctx, f.admin, f.schema, f.tenant, embeddingRecipe(), "shadow", f.role, HostedEmbeddingProvisionOptions{ActivationMode: "manual"})
	require.NoError(t, err)
	reconcileEmbedding(t, s)
	conn, err := f.runtime.Conn(ctx)
	require.NoError(t, err)
	defer conn.Close()
	update := `UPDATE hosted_embedding_state SET active_generation_id=$1,active_valid=true WHERE singleton=1 AND desired_generation_id=$1`
	for _, approval := range [][2]string{{"", ""}, {"wrong", strconv.FormatInt(manual.ID, 10)}, {f.tenant, strconv.FormatInt(previous.ID, 10)}} {
		tx, err := conn.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `SELECT set_config('agentsview.embedding_activation_tenant',$1,true),set_config('agentsview.embedding_activation_generation',$2,true)`, approval[0], approval[1])
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, update, manual.ID)
		assert.ErrorContains(t, err, "manual embedding activation requires explicit approval")
		require.NoError(t, tx.Rollback())
	}
	for _, commit := range []bool{false, true} {
		tx, err := conn.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `SELECT set_config('agentsview.embedding_activation_tenant',$1,true),set_config('agentsview.embedding_activation_generation',$2,true)`, f.tenant, strconv.FormatInt(manual.ID, 10))
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, update, manual.ID)
		require.NoError(t, err)
		if commit {
			require.NoError(t, tx.Commit())
		} else {
			require.NoError(t, tx.Rollback())
		}
		_, err = conn.ExecContext(ctx, `UPDATE hosted_embedding_state SET active_valid=false`)
		require.NoError(t, err)
		tx, err = conn.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, update, manual.ID)
		assert.ErrorContains(t, err, "manual embedding activation requires explicit approval")
		require.NoError(t, tx.Rollback())
	}
	// INSERT has the same compatibility boundary; ordinary cursor writes do not.
	tx, err := conn.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `DELETE FROM hosted_embedding_state`)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `INSERT INTO hosted_embedding_state(tenant_id,singleton,active_generation_id,active_valid) VALUES($1,1,$2,true)`, f.tenant, manual.ID)
	assert.ErrorContains(t, err, "manual embedding activation requires explicit approval")
	require.NoError(t, tx.Rollback())
	_, err = conn.ExecContext(ctx, `UPDATE hosted_embedding_state SET claim_after_generation_id=1`)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `UPDATE hosted_embedding_generations SET activation_mode='automatic' WHERE id=$1`, manual.ID)
	assert.Error(t, err)
}

func TestHostedEmbeddingCutoverProvisionMode(t *testing.T) {
	f, _, _ := embeddingFixture(t)
	ctx := t.Context()
	manual, err := ProvisionHostedEmbeddingsWithOptions(ctx, f.admin, f.schema, f.tenant, embeddingRecipe(), "shadow", f.role, HostedEmbeddingProvisionOptions{ActivationMode: "manual"})
	require.NoError(t, err)
	same, err := ProvisionHostedEmbeddings(ctx, f.admin, f.schema, f.tenant, embeddingRecipe(), "shadow", f.role)
	require.NoError(t, err)
	assert.Equal(t, manual, same)
	selected, err := ProvisionHostedEmbeddings(t.Context(), f.admin, f.schema, f.tenant, embeddingRecipe(), "selected", f.role)
	require.NoError(t, err)
	for _, mode := range []string{"automatic", "invalid", "Manual"} {
		_, err = ProvisionHostedEmbeddingsWithOptions(ctx, f.admin, f.schema, f.tenant, embeddingRecipe(), "shadow", f.role, HostedEmbeddingProvisionOptions{ActivationMode: mode})
		assert.Error(t, err)
	}
	var desired int64
	require.NoError(t, f.runtime.QueryRow(`SELECT desired_generation_id FROM hosted_embedding_state`).Scan(&desired))
	assert.Equal(t, selected.ID, desired)
	_, err = f.admin.Exec(`UPDATE hosted_embedding_generations SET activation_mode='automatic' WHERE id=$1`, manual.ID)
	assert.Error(t, err)
}

// These simulate supported pre-upgrade metadata, not a query failure injection.
const cutoverLegacyImmutable = `BEGIN IF TG_OP<>'UPDATE' OR ROW(OLD.tenant_id,OLD.id,OLD.instance_key,OLD.recipe_fingerprint,OLD.recipe_json,OLD.dimensions) IS DISTINCT FROM ROW(NEW.tenant_id,NEW.id,NEW.instance_key,NEW.recipe_fingerprint,NEW.recipe_json,NEW.dimensions) THEN RAISE EXCEPTION 'embedding recipe is immutable'; END IF; RETURN NEW; END; `

func removeEmbeddingCutover(t *testing.T, f hostedFixture) {
	t.Helper()
	_, err := f.admin.Exec(`DROP TRIGGER hosted_embedding_activation_guard ON hosted_embedding_state; DROP FUNCTION hosted_embedding_activation_guard(); ALTER TABLE hosted_embedding_generations DROP COLUMN activation_mode; CREATE OR REPLACE FUNCTION hosted_embedding_recipe_immutable() RETURNS trigger LANGUAGE plpgsql SECURITY INVOKER AS $old$` + cutoverLegacyImmutable + `$old$`)
	require.NoError(t, err)
}

func TestHostedEmbeddingCutoverOwnerUpgradePreservesData(t *testing.T) {
	f, s, g, snap := embeddingOne(t)
	ctx := t.Context()
	require.NoError(t, s.Publish(ctx, snap, embeddingOneVector()))
	ok, err := s.Activate(ctx, g.ID)
	require.NoError(t, err)
	require.True(t, ok)
	var before string
	require.NoError(t, f.runtime.QueryRow(`SELECT md5(string_agg(row(c.input_hash,c.embedding::text)::text,',' ORDER BY c.doc_key,c.chunk_index)) FROM hosted_embedding_chunks_g1 c`).Scan(&before))
	removeEmbeddingCutover(t, f)
	assert.Error(t, CheckHostedTenant(ctx, f.runtime, f.schema, f.tenant))
	same, err := ProvisionHostedEmbeddings(ctx, f.admin, f.schema, f.tenant, embeddingRecipe(), "initial", f.role)
	require.NoError(t, err)
	assert.Equal(t, g, same)
	require.NoError(t, CheckHostedTenant(ctx, f.runtime, f.schema, f.tenant))
	var after string
	var chunks, complete int
	require.NoError(t, f.runtime.QueryRow(`SELECT md5(string_agg(row(c.input_hash,c.embedding::text)::text,',' ORDER BY c.doc_key,c.chunk_index)),count(*) FROM hosted_embedding_chunks_g1 c`).Scan(&after, &chunks))
	assert.Equal(t, before, after)
	assert.Equal(t, 1, chunks)
	require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM hosted_embedding_requirements WHERE state='complete' AND completed_revision=required_revision`).Scan(&complete))
	assert.Equal(t, 1, complete)
	active, err := s.Active(ctx)
	require.NoError(t, err)
	require.NotNil(t, active)
	assert.Equal(t, g.ID, active.ID)
}

func TestHostedEmbeddingCutoverRejectsTampering(t *testing.T) {
	for _, tc := range []struct{ name, ddl string }{
		{"missing column", `ALTER TABLE hosted_embedding_generations DROP COLUMN activation_mode`},
		{"nullable column", `ALTER TABLE hosted_embedding_generations ALTER COLUMN activation_mode DROP NOT NULL`},
		{"wrong type", `ALTER TABLE hosted_embedding_generations ALTER COLUMN activation_mode TYPE varchar(20)`},
		{"missing check", `ALTER TABLE hosted_embedding_generations DROP CONSTRAINT hosted_embedding_activation_mode_check`},
		{"unvalidated check", `ALTER TABLE hosted_embedding_generations DROP CONSTRAINT hosted_embedding_activation_mode_check; ALTER TABLE hosted_embedding_generations ADD CONSTRAINT hosted_embedding_activation_mode_check CHECK(activation_mode IN ('automatic','manual')) NOT VALID`},
		{"wrong check", `ALTER TABLE hosted_embedding_generations DROP CONSTRAINT hosted_embedding_activation_mode_check; ALTER TABLE hosted_embedding_generations ADD CONSTRAINT hosted_embedding_activation_mode_check CHECK(activation_mode IN ('automatic','manual','other'))`},
		{"missing guard", `DROP TRIGGER hosted_embedding_activation_guard ON hosted_embedding_state`},
		{"column restricted guard", `DROP TRIGGER hosted_embedding_activation_guard ON hosted_embedding_state; CREATE TRIGGER hosted_embedding_activation_guard BEFORE INSERT OR UPDATE OF claim_after_generation_id ON hosted_embedding_state FOR EACH ROW EXECUTE FUNCTION hosted_embedding_activation_guard()`},
		{"disabled guard", `ALTER TABLE hosted_embedding_state DISABLE TRIGGER hosted_embedding_activation_guard`},
		{"altered guard", `CREATE OR REPLACE FUNCTION hosted_embedding_activation_guard() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RETURN NEW; END;$$`},
		{"definer guard", `ALTER FUNCTION hosted_embedding_activation_guard() SECURITY DEFINER`},
		{"column restricted immutable", `DROP TRIGGER hosted_embedding_recipe_immutable ON hosted_embedding_generations; CREATE TRIGGER hosted_embedding_recipe_immutable BEFORE UPDATE OF backfill_finished OR DELETE ON hosted_embedding_generations FOR EACH ROW EXECUTE FUNCTION hosted_embedding_recipe_immutable()`},
		{"column restricted session notifier", `DROP TRIGGER hosted_embedding_session_notify ON sessions; CREATE TRIGGER hosted_embedding_session_notify AFTER INSERT OR UPDATE OF display_name OR DELETE ON sessions FOR EACH ROW EXECUTE FUNCTION hosted_embedding_session_notify()`},
		{"column restricted message notifier", `DROP TRIGGER hosted_embedding_message_notify ON messages; CREATE TRIGGER hosted_embedding_message_notify AFTER INSERT OR UPDATE OF ordinal OR DELETE ON messages FOR EACH ROW EXECUTE FUNCTION hosted_embedding_message_notify()`},
		{"legacy immutable", `CREATE OR REPLACE FUNCTION hosted_embedding_recipe_immutable() RETURNS trigger LANGUAGE plpgsql SECURITY INVOKER AS $old$` + cutoverLegacyImmutable + `$old$`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, initial := embeddingFixture(t)
			_, err := f.admin.Exec(tc.ddl)
			require.NoError(t, err)
			assert.Error(t, CheckHostedTenant(t.Context(), f.runtime, f.schema, f.tenant))
			_, err = ProvisionHostedEmbeddings(t.Context(), f.admin, f.schema, f.tenant, embeddingRecipe(), "new", f.role)
			assert.Error(t, err)
			var count int
			require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM hosted_embedding_generations`).Scan(&count))
			assert.Equal(t, 1, count)
			var active sql.NullInt64
			var desired int64
			require.NoError(t, f.runtime.QueryRow(`SELECT active_generation_id,desired_generation_id FROM hosted_embedding_state`).Scan(&active, &desired))
			assert.False(t, active.Valid)
			assert.Equal(t, initial.ID, desired)
		})
	}
	t.Run("runtime mode update grant", func(t *testing.T) {
		f, _, _ := embeddingFixture(t)
		_, err := f.admin.Exec(`GRANT UPDATE(activation_mode) ON hosted_embedding_generations TO "` + f.role + `"`)
		require.NoError(t, err)
		assert.Error(t, CheckHostedTenant(t.Context(), f.runtime, f.schema, f.tenant))
	})
	t.Run("old baseline altered", func(t *testing.T) {
		f, _, _ := embeddingFixture(t)
		removeEmbeddingCutover(t, f)
		_, err := f.admin.Exec(`ALTER TABLE hosted_embedding_documents NO FORCE ROW LEVEL SECURITY`)
		require.NoError(t, err)
		_, err = ProvisionHostedEmbeddings(t.Context(), f.admin, f.schema, f.tenant, embeddingRecipe(), "new", f.role)
		assert.Error(t, err)
		var mode sql.NullString
		require.NoError(t, f.runtime.QueryRow(`SELECT to_jsonb(g)->>'activation_mode' FROM hosted_embedding_generations g`).Scan(&mode))
		assert.False(t, mode.Valid)
	})
}

// If approval or pointer writes escape the activation transaction, cancellation
// while the state UPDATE is blocked would publish a manual generation anyway.
func TestHostedEmbeddingCutoverCancellation(t *testing.T) {
	f, s, previous := embeddingFixture(t)
	ctx := t.Context()
	reconcileEmbedding(t, s)
	ok, err := s.Activate(ctx, previous.ID)
	require.NoError(t, err)
	require.True(t, ok)
	manual, err := ProvisionHostedEmbeddingsWithOptions(ctx, f.admin, f.schema, f.tenant, embeddingRecipe(), "shadow", f.role, HostedEmbeddingProvisionOptions{ActivationMode: "manual"})
	require.NoError(t, err)
	reconcileEmbedding(t, s)
	gate, err := f.admin.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer gate.Rollback()
	_, err = gate.Exec(`SELECT pg_advisory_xact_lock(hashtext($1),739)`, f.schema)
	require.NoError(t, err)
	_, err = f.admin.Exec(`CREATE FUNCTION embedding_cutover_test_gate() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN PERFORM pg_advisory_xact_lock(hashtext(TG_TABLE_SCHEMA),739); RETURN NEW; END;$$; CREATE TRIGGER embedding_cutover_test_gate BEFORE UPDATE ON hosted_embedding_state FOR EACH ROW EXECUTE FUNCTION embedding_cutover_test_gate()`)
	require.NoError(t, err)
	activationCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type outcome struct {
		ok  bool
		err error
	}
	done := make(chan outcome, 1)
	go func() { ok, err := s.Activate(activationCtx, manual.ID); done <- outcome{ok, err} }()
	waitCtx, stop := context.WithTimeout(ctx, 10*time.Second)
	defer stop()
	for {
		var blocked bool
		require.NoError(t, f.admin.QueryRowContext(waitCtx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE usename=$1 AND query LIKE 'UPDATE hosted_embedding_state SET active_generation_id=%' AND wait_event='advisory')`, f.role).Scan(&blocked))
		if blocked {
			break
		}
		runtime.Gosched()
	}
	cancel()
	result := <-done
	assert.Error(t, result.err)
	assert.False(t, result.ok)
	require.NoError(t, gate.Rollback())
	_, err = f.admin.Exec(`DROP TRIGGER embedding_cutover_test_gate ON hosted_embedding_state; DROP FUNCTION embedding_cutover_test_gate()`)
	require.NoError(t, err)
	active, err := s.Active(ctx)
	require.NoError(t, err)
	require.NotNil(t, active)
	assert.Equal(t, previous.ID, active.ID)
	_, err = f.runtime.Exec(`UPDATE hosted_embedding_state SET active_generation_id=$1,active_valid=true`, manual.ID)
	assert.ErrorContains(t, err, "manual embedding activation requires explicit approval")
	ok, err = s.Activate(ctx, manual.ID)
	require.NoError(t, err)
	assert.True(t, ok)
}

// Metadata DDL before the corpus fence would hold a registry lock here while an
// existing worker owns the fence, creating the owner/worker lock inversion.
func TestHostedEmbeddingCutoverUpgradeWaitsForCorpusFence(t *testing.T) {
	f, _, _ := embeddingFixture(t)
	removeEmbeddingCutover(t, f)
	ctx := t.Context()
	fence, err := f.runtime.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer fence.Rollback()
	_, err = fence.Exec(`SELECT singleton FROM raw_corpus_state FOR UPDATE`)
	require.NoError(t, err)
	owner, err := Open(testPGURL(t), f.schema, false)
	require.NoError(t, err)
	defer owner.Close()
	owner.SetMaxOpenConns(1)
	var pid int
	require.NoError(t, owner.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid))
	upgradeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := ProvisionHostedEmbeddings(upgradeCtx, owner, f.schema, f.tenant, embeddingRecipe(), "initial", f.role)
		done <- err
	}()
	waitEmbeddingBlocked(t, f.admin, pid)
	var ddlLocks bool
	require.NoError(t, f.admin.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND relation IN ('hosted_embedding_generations'::regclass,'hosted_embedding_state'::regclass) AND mode='AccessExclusiveLock')`, pid).Scan(&ddlLocks))
	assert.False(t, ddlLocks)
	_, err = fence.Exec(`UPDATE hosted_embedding_generations SET backfill_after_session_id='worker-cursor'`)
	require.NoError(t, err)
	require.NoError(t, fence.Commit())
	require.NoError(t, <-done)
	require.NoError(t, CheckHostedTenant(ctx, f.runtime, f.schema, f.tenant))
	var cursor string
	require.NoError(t, f.runtime.QueryRow(`SELECT backfill_after_session_id FROM hosted_embedding_generations`).Scan(&cursor))
	assert.Equal(t, "worker-cursor", cursor)
}
