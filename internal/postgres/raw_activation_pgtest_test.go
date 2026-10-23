//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/rawparity"
)

func activationOptions() RawActivationOptions {
	return RawActivationOptions{Version: "parser-1", AcceptDifferences: true, Embeddings: RawEmbeddingsDisabled}
}
func activateFixture(t require.TestingT, ctx context.Context, f projectionFixture, device, provider string) RawActivationAudit {
	audit, err := ActivateRawRollout(ctx, f.admin, f.schema, f.tenant, device, provider, activationOptions())
	require.NoError(t, err)
	return audit
}
func assertActivationGuardFree(t *testing.T, f projectionFixture) {
	t.Helper()
	// pgx returns cancellation before its asynchronous physical connection
	// cleanup finishes. A leaked pooled guard must still fail this bounded wait.
	require.Eventually(t, func() bool {
		var held int
		err := f.admin.QueryRowContext(t.Context(), `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND granted AND classid=((hashtextextended($1,0)>>32)&4294967295)::oid AND objid=(hashtextextended($1,0)&4294967295)::oid`, "agentsview-rollout:"+f.schema).Scan(&held)
		return err == nil && held == 0
	}, 20*time.Second, 10*time.Millisecond, "no pooled connection may retain the session guard")
}

// This exercises the production entry point, including a deliberately different
// transcript. Deleting carry/map/publication or storing a modified pass fails it.
func TestRawActivationAuditCarryAndRollback(t *testing.T) {
	f, _ := supersessionFixture(t)
	require.NoError(t, ChangeRawRollout(t.Context(), f.admin, f.schema, f.tenant, "", "", "shadow"))
	before := activationContentSnapshot(t, f.runtime)
	audit := activateFixture(t, t.Context(), f, "device-a", "")
	assert.False(t, audit.Parity.Pass)
	assert.Equal(t, 1, audit.Parity.Counts["differs"])
	assert.True(t, audit.AcceptDifferences)
	assert.Equal(t, "not_configured", audit.Embeddings)
	assert.Greater(t, audit.ActivatedCorpusRevision, audit.CheckedCorpusRevision)
	assert.Equal(t, before, activationContentSnapshot(t, f.runtime), "gate's accepted content and proofs remain unchanged")
	var encoded []byte
	var revision int64
	require.NoError(t, f.admin.QueryRow(`SELECT parity,activated_corpus_revision FROM raw_rollout_scopes WHERE device_id='device-a'`).Scan(&encoded, &revision))
	var stored RawActivationAudit
	require.NoError(t, json.Unmarshal(encoded, &stored))
	assert.Equal(t, audit, stored)
	assert.Equal(t, audit.ActivatedCorpusRevision, revision)
	var name string
	var hidden, trashed bool
	require.NoError(t, f.runtime.QueryRow(`SELECT rollout_hidden FROM sessions WHERE id='codex:portable'`).Scan(&hidden))
	assert.True(t, hidden)
	require.NoError(t, f.runtime.QueryRow(`SELECT display_name,deleted_at IS NOT NULL FROM sessions WHERE provenance_kind='raw'`).Scan(&name, &trashed))
	assert.Equal(t, "legacy title", name)
	assert.True(t, trashed)
	require.NoError(t, f.sink.SetCuration(t.Context(), "codex:portable", "display_name", "raw edit"))
	require.NoError(t, f.sink.SetCuration(t.Context(), "codex:portable", "trashed", false))
	require.NoError(t, ChangeRawRollout(t.Context(), f.admin, f.schema, f.tenant, "device-a", "", "shadow"))
	require.NoError(t, f.runtime.QueryRow(`SELECT display_name,deleted_at IS NOT NULL,rollout_hidden FROM sessions WHERE id='codex:portable'`).Scan(&name, &trashed, &hidden))
	assert.Equal(t, "legacy title", name)
	assert.True(t, trashed)
	assert.False(t, hidden)
	activateFixture(t, t.Context(), f, "device-a", "")
	require.NoError(t, f.runtime.QueryRow(`SELECT display_name,deleted_at IS NOT NULL FROM sessions WHERE provenance_kind='raw'`).Scan(&name, &trashed))
	assert.Equal(t, "raw edit", name)
	assert.False(t, trashed)
	assertActivationGuardFree(t, f)
}

func TestRawActivationRefusalIsAtomic(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mode   RawEmbeddingMode
		accept bool
		want   string
	}{
		{"differences", RawEmbeddingsDisabled, false, "parity"},
		{"unknown", RawEmbeddingsUnknown, true, "unknown"},
		{"configured", RawEmbeddingsConfigured, true, "coverage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := supersessionFixture(t)
			require.NoError(t, ChangeRawRollout(t.Context(), f.admin, f.schema, f.tenant, "", "", "shadow"))
			before := supersessionPublicationAt(t, f.runtime)
			options := activationOptions()
			options.Embeddings = tc.mode
			options.AcceptDifferences = tc.accept
			_, err := ActivateRawRollout(t.Context(), f.admin, f.schema, f.tenant, "device-a", "", options)
			require.ErrorContains(t, err, tc.want)
			assert.Equal(t, before, supersessionPublicationAt(t, f.runtime))
			var count int
			require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM raw_legacy_supersessions`).Scan(&count))
			assert.Zero(t, count)
			require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM raw_rollout_scopes WHERE state='active'`).Scan(&count))
			assert.Zero(t, count)
			require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM raw_curation`).Scan(&count))
			assert.Zero(t, count)
			assertActivationGuardFree(t, f)
		})
	}
}

func TestRawActivationOwnerAndBinding(t *testing.T) {
	f, _ := supersessionFixture(t)
	_, err := ActivateRawRollout(t.Context(), f.runtime, f.schema, f.tenant, "", "", activationOptions())
	require.ErrorContains(t, err, "owner")
	_, err = ActivateRawRollout(t.Context(), f.admin, f.schema, "other-tenant", "", "", activationOptions())
	require.Error(t, err)
	assertActivationGuardFree(t, f)
}

// Waiting inside RR would see the pre-wait legacy transcript and wrongly pass.
func TestRawActivationGuardPrecedesSnapshot(t *testing.T) {
	f, _ := supersessionFixture(t)
	require.NoError(t, ChangeRawRollout(t.Context(), f.admin, f.schema, f.tenant, "", "", "shadow"))
	guard, err := f.admin.Conn(t.Context())
	require.NoError(t, err)
	defer guard.Close()
	_, err = guard.ExecContext(t.Context(), `SELECT pg_advisory_lock(hashtextextended($1,0))`, "agentsview-rollout:"+f.schema)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := ActivateRawRollout(ctx, f.admin, f.schema, f.tenant, "device-a", "", activationOptions())
		done <- err
	}()
	require.Eventually(t, func() bool {
		var n int
		err := f.admin.QueryRow(`SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND NOT granted AND classid=((hashtextextended($1,0)>>32)&4294967295)::oid AND objid=(hashtextextended($1,0)&4294967295)::oid`, "agentsview-rollout:"+f.schema).Scan(&n)
		return err == nil && n > 0
	}, 5*time.Second, 10*time.Millisecond)
	// Readiness changes are deliberately not coupled to corpus publication here:
	// this fixture isolates snapshot acquisition from the later corpus fence.
	_, err = f.runtime.Exec(`UPDATE raw_source_projections SET membership_complete=false`)
	require.NoError(t, err)
	_, err = guard.ExecContext(ctx, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, "agentsview-rollout:"+f.schema)
	require.NoError(t, err)
	require.ErrorContains(t, <-done, "parity")
	assertActivationGuardFree(t, f)
}

func TestRawActivationCancelledGuardAcquisition(t *testing.T) {
	f, _ := supersessionFixture(t)
	blocker, err := f.admin.Conn(t.Context())
	require.NoError(t, err)
	defer blocker.Close()
	_, err = blocker.ExecContext(t.Context(), `SELECT pg_advisory_lock(hashtextextended($1,0))`, "agentsview-rollout:"+f.schema)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := ActivateRawRollout(ctx, f.admin, f.schema, f.tenant, "", "", activationOptions())
		done <- err
	}()
	require.Eventually(t, func() bool {
		var n int
		err := f.admin.QueryRow(`SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND NOT granted AND classid=((hashtextextended($1,0)>>32)&4294967295)::oid AND objid=(hashtextextended($1,0)&4294967295)::oid`, "agentsview-rollout:"+f.schema).Scan(&n)
		return err == nil && n > 0
	}, 5*time.Second, 10*time.Millisecond)
	cancel()
	require.Error(t, <-done)
	_, err = blocker.ExecContext(t.Context(), `SELECT pg_advisory_unlock(hashtextextended($1,0))`, "agentsview-rollout:"+f.schema)
	require.NoError(t, err)
	assertActivationGuardFree(t, f)
}

func TestRawActivationInternalParityDoesNotRelaxPublicReader(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "physical", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("raw only")))
	tx, err := f.admin.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = rawparity.Run(t.Context(), RawParityReader{Tx: tx, Version: "parser-1"}, rawparity.Scope{})
	require.ErrorContains(t, err, "READ ONLY")
	report, err := rawparity.Run(t.Context(), rawActivationReader{RawParityReader{Tx: tx, Version: "parser-1"}}, rawparity.Scope{})
	require.NoError(t, err)
	assert.True(t, report.Pass)
	assert.Equal(t, 1, report.Counts["raw_only"])
}

func activationContentSnapshot(t *testing.T, q supersessionQuery) map[string]string {
	t.Helper()
	state := rawCarryContentSnapshot(t, q)
	var stateSessionJSON string
	require.NoError(t, q.QueryRowContext(t.Context(), `SELECT COALESCE(jsonb_agg(v ORDER BY v::text),'[]'::jsonb)::text FROM (SELECT to_jsonb(s)-ARRAY['display_name','deleted_at','deletion_cause','rollout_hidden'] AS v FROM sessions s WHERE provenance_kind='raw') content`).Scan(&stateSessionJSON))
	state["sessions"] = stateSessionJSON
	return state
}

func TestRawActivationRequiresInitializedPublicationFence(t *testing.T) {
	f := newProjectionFixture(t)
	_, err := ActivateRawRollout(t.Context(), f.admin, f.schema, f.tenant, "", "", activationOptions())
	require.ErrorContains(t, err, "publication state is not initialized")
	var scopes, publication int
	require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM raw_rollout_scopes`).Scan(&scopes))
	require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM raw_corpus_state`).Scan(&publication))
	assert.Zero(t, scopes)
	assert.Zero(t, publication, "activation must never create an unfenced publication row")
	assertActivationGuardFree(t, f)
}
