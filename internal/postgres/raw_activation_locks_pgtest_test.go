//go:build pgtest

package postgres

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every ordinary resource class must block BEFORE corpus. A fence taken before
// alias, group, session, child/FK, or relation access fails the NOWAIT observer.
func TestRawActivationPrelocksBeforeCorpus(t *testing.T) {
	for _, lock := range []string{
		`SELECT pg_advisory_xact_lock(hashtext(current_schema()),(hashtextextended('codex:portable',0)&255)::int)`,
		`SELECT group_id FROM raw_session_groups FOR UPDATE`,
		`SELECT id FROM sessions WHERE id='codex:portable' FOR UPDATE`,
		`SELECT id FROM sessions WHERE provenance_kind='raw' FOR UPDATE`,
		`SELECT id FROM pinned_messages WHERE session_id='codex:portable' FOR UPDATE`,
		`SELECT session_id FROM starred_sessions WHERE session_id='codex:portable' FOR UPDATE`,
		`LOCK TABLE raw_pins IN ACCESS EXCLUSIVE MODE`,
		`ALTER SEQUENCE pinned_messages_id_seq CACHE 1`,
	} {
		t.Run(lock, func(t *testing.T) {
			f, _ := supersessionFixture(t)
			require.NoError(t, ChangeRawRollout(t.Context(), f.admin, f.schema, f.tenant, "", "", "shadow"))
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			blocker, err := f.admin.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer blocker.Rollback()
			var blockerPID int
			require.NoError(t, blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID))
			_, err = blocker.ExecContext(ctx, lock)
			require.NoError(t, err)
			done := make(chan error, 1)
			go func() {
				_, err := ActivateRawRollout(ctx, f.admin, f.schema, f.tenant, "device-a", "", activationOptions())
				done <- err
			}()
			waitRawActivationBlockedBy(t, f, blockerPID)
			observer, err := f.admin.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer observer.Rollback()
			_, err = observer.ExecContext(ctx, `SELECT singleton FROM raw_corpus_state WHERE singleton=1 FOR UPDATE NOWAIT`)
			require.NoError(t, err, "activation must not own corpus while waiting for ordinary resources")
			require.NoError(t, observer.Rollback())
			require.NoError(t, blocker.Rollback())
			require.NoError(t, <-done)
			assertActivationGuardFree(t, f)
		})
	}
}

// A deferred constraint failure is a known failed commit. A fresh connection
// must be able to take the same guard, and no activation data may survive.
func TestRawActivationCommitFailureCleansGuard(t *testing.T) {
	f, _ := supersessionFixture(t)
	require.NoError(t, ChangeRawRollout(t.Context(), f.admin, f.schema, f.tenant, "", "", "shadow"))
	_, err := f.admin.Exec(`CREATE FUNCTION reject_activation_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic deferred failure'; END $$; CREATE CONSTRAINT TRIGGER reject_activation_commit AFTER INSERT OR UPDATE ON raw_rollout_scopes DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_activation_commit()`)
	require.NoError(t, err)
	before := supersessionPublicationAt(t, f.runtime)
	_, err = ActivateRawRollout(t.Context(), f.admin, f.schema, f.tenant, "device-a", "", activationOptions())
	require.ErrorContains(t, err, "synthetic deferred failure")
	assert.Equal(t, before, supersessionPublicationAt(t, f.runtime))
	assertActivationGuardFree(t, f)
	_, err = f.admin.Exec(`DROP TRIGGER reject_activation_commit ON raw_rollout_scopes`)
	require.NoError(t, err)
	activateFixture(t, t.Context(), f, "device-a", "")
	assertActivationGuardFree(t, f)
}

func TestRawActivationCancelledTransactionCleansGuard(t *testing.T) {
	f, _ := supersessionFixture(t)
	require.NoError(t, ChangeRawRollout(t.Context(), f.admin, f.schema, f.tenant, "", "", "shadow"))
	blocker, err := f.runtime.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer blocker.Rollback()
	var blockerPID int
	require.NoError(t, blocker.QueryRow(`SELECT pg_backend_pid()`).Scan(&blockerPID))
	_, err = blocker.Exec(`SELECT id FROM sessions WHERE id='codex:portable' FOR UPDATE`)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := ActivateRawRollout(ctx, f.admin, f.schema, f.tenant, "device-a", "", activationOptions())
		done <- err
	}()
	waitRawActivationBlockedBy(t, f, blockerPID)
	cancel()
	require.Error(t, <-done)
	require.NoError(t, blocker.Rollback())
	assertActivationGuardFree(t, f)
	activateFixture(t, t.Context(), f, "device-a", "")
}

// Sequence state survives abort, so the first injected failure is deterministic.
// Returning stale-snapshot work instead of restarting the whole attempt fails
// the changed readiness check on the second attempt.
func TestRawActivationRetriesFreshSnapshot(t *testing.T) {
	for _, code := range []string{"40001", "40P01"} {
		t.Run(code, func(t *testing.T) {
			f, _ := supersessionFixture(t)
			require.NoError(t, ChangeRawRollout(t.Context(), f.admin, f.schema, f.tenant, "", "", "shadow"))
			_, err := f.admin.Exec(fmt.Sprintf(`CREATE SEQUENCE activation_fault; CREATE FUNCTION activation_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF nextval('activation_fault')=1 THEN PERFORM pg_advisory_xact_lock(hashtext(TG_TABLE_SCHEMA)); RAISE EXCEPTION 'synthetic retry' USING ERRCODE='%s'; END IF; RETURN NEW; END $$; CREATE TRIGGER activation_fault BEFORE INSERT ON raw_rollout_scopes FOR EACH ROW EXECUTE FUNCTION activation_fault()`, code))
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			gate, err := f.admin.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer gate.Rollback()
			_, err = gate.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, f.schema)
			require.NoError(t, err)
			done := make(chan error, 1)
			go func() {
				_, err := ActivateRawRollout(ctx, f.admin, f.schema, f.tenant, "device-a", "", activationOptions())
				done <- err
			}()
			require.Eventually(t, func() bool {
				var called bool
				err := f.admin.QueryRow(`SELECT is_called FROM activation_fault`).Scan(&called)
				return err == nil && called
			}, 5*time.Second, 10*time.Millisecond)
			_, err = f.runtime.Exec(`UPDATE raw_source_projections SET membership_complete=false`)
			require.NoError(t, err)
			require.NoError(t, gate.Rollback())
			require.ErrorContains(t, <-done, "parity")
			assertActivationGuardFree(t, f)
		})
	}
}

// Use the actual blocker PID instead of unrelated global lock activity.
func waitRawActivationBlockedBy(t *testing.T, f projectionFixture, blocker int) int {
	t.Helper()
	var pid int
	require.Eventually(t, func() bool {
		err := f.admin.QueryRowContext(t.Context(), `SELECT pid FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)) ORDER BY pid LIMIT 1`, blocker).Scan(&pid)
		return err == nil
	}, 5*time.Second, 10*time.Millisecond)
	return pid
}

// Queue a second row writer behind activation. It holds the retry before corpus
// so the test cannot race through the retry budget. Readiness changes while the
// FIRST snapshot is parked; only a new complete attempt can see the refusal.
func TestRawActivationCorpusNowaitRetriesFreshSnapshot(t *testing.T) {
	f, _ := supersessionFixture(t)
	require.NoError(t, ChangeRawRollout(t.Context(), f.admin, f.schema, f.tenant, "", "", "shadow"))
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	corpus, err := f.admin.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer corpus.Rollback()
	_, err = corpus.ExecContext(ctx, `SELECT singleton FROM raw_corpus_state WHERE singleton=1 FOR UPDATE`)
	require.NoError(t, err)
	first, err := f.admin.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer first.Rollback()
	var firstPID int
	require.NoError(t, first.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&firstPID))
	_, err = first.ExecContext(ctx, `SELECT group_id FROM raw_session_groups FOR UPDATE`)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() {
		_, err := ActivateRawRollout(ctx, f.admin, f.schema, f.tenant, "device-a", "", activationOptions())
		done <- err
	}()
	activationPID := waitRawActivationBlockedBy(t, f, firstPID)
	second, err := f.admin.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer second.Rollback()
	var secondPID int
	require.NoError(t, second.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&secondPID))
	queued := make(chan error, 1)
	go func() {
		_, err := second.ExecContext(ctx, `SELECT group_id FROM raw_session_groups FOR UPDATE`)
		queued <- err
	}()
	require.Eventually(t, func() bool {
		var waiting bool
		err := f.admin.QueryRowContext(ctx, `SELECT cardinality(pg_blocking_pids($1))>0`, secondPID).Scan(&waiting)
		return err == nil && waiting
	}, 5*time.Second, 10*time.Millisecond)
	_, err = f.runtime.ExecContext(ctx, `UPDATE raw_source_projections SET membership_complete=false`)
	require.NoError(t, err)
	require.NoError(t, first.Rollback())
	require.NoError(t, <-queued, "first attempt must abort its NOWAIT conflict and release group")
	retryPID := waitRawActivationBlockedBy(t, f, secondPID)
	assert.NotZero(t, activationPID)
	assert.NotZero(t, retryPID)
	require.NoError(t, corpus.Rollback())
	require.NoError(t, second.Rollback())
	require.ErrorContains(t, <-done, "parity")
	assertActivationGuardFree(t, f)
}

// Kill only this fixture's activation backend while it owns the session guard.
// The failed physical connection must not poison subsequent pool use.
func TestRawActivationLostBackendDoesNotPoisonPool(t *testing.T) {
	f, _ := supersessionFixture(t)
	require.NoError(t, ChangeRawRollout(t.Context(), f.admin, f.schema, f.tenant, "", "", "shadow"))
	blocker, err := f.runtime.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer blocker.Rollback()
	var blockerPID int
	require.NoError(t, blocker.QueryRow(`SELECT pg_backend_pid()`).Scan(&blockerPID))
	_, err = blocker.Exec(`SELECT id FROM sessions WHERE id='codex:portable' FOR UPDATE`)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := ActivateRawRollout(ctx, f.admin, f.schema, f.tenant, "device-a", "", activationOptions())
		done <- err
	}()
	pid := waitRawActivationBlockedBy(t, f, blockerPID)
	var terminated bool
	require.NoError(t, f.admin.QueryRowContext(ctx, `SELECT pg_terminate_backend($1)`, pid).Scan(&terminated))
	require.True(t, terminated)
	require.Error(t, <-done)
	require.NoError(t, blocker.Rollback())
	assertActivationGuardFree(t, f)
	activateFixture(t, ctx, f, "device-a", "")
	assertActivationGuardFree(t, f)
}
