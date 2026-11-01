//go:build pgtest

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRawRolloutVisibilityAfterWait(t *testing.T) {
	f := newProjectionFixture(t)
	m, accepted := f.accept(t, "device-a", "initial", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("initial")))
	m, _ = f.accept(t, "device-a", "capture", accepted.Receipt)
	lease := f.lease(t, m)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	gate, err := f.admin.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer gate.Rollback()
	_, err = gate.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "agentsview-rollout:"+f.schema)
	require.NoError(t, err)
	projected := make(chan error, 1)
	go func() { projected <- f.sink.Project(ctx, lease, m, projectionOutcome("hello")) }()
	require.Eventually(t, func() bool {
		var count int
		err := f.admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE usename=$1 AND wait_event='advisory'`, f.role).Scan(&count)
		return err == nil && count == 1
	}, 5*time.Second, 10*time.Millisecond)
	_, err = gate.ExecContext(ctx, `INSERT INTO raw_rollout_scopes(tenant_id,state) VALUES($1,'shadow')`, f.tenant)
	require.NoError(t, err)
	require.NoError(t, gate.Commit())
	require.NoError(t, <-projected)
	var hidden bool
	require.NoError(t, f.runtime.QueryRow(`SELECT rollout_hidden FROM sessions WHERE provenance_kind='raw'`).Scan(&hidden))
	require.True(t, hidden, "new physical row must inherit shadow")

	gate, err = f.admin.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer gate.Rollback()
	_, err = gate.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "agentsview-rollout:"+f.schema)
	require.NoError(t, err)
	curated := make(chan error, 1)
	go func() { curated <- f.sink.SetCuration(ctx, "codex:portable", "starred", true) }()
	require.Eventually(t, func() bool {
		var count int
		err := f.admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE usename=$1 AND wait_event='advisory'`, f.role).Scan(&count)
		return err == nil && count == 1
	}, 5*time.Second, 10*time.Millisecond)
	_, err = gate.ExecContext(ctx, `DELETE FROM raw_rollout_scopes`)
	require.NoError(t, err)
	require.NoError(t, gate.Commit())
	require.NoError(t, <-curated)
	require.NoError(t, f.runtime.QueryRow(`SELECT rollout_hidden FROM sessions WHERE provenance_kind='raw'`).Scan(&hidden))
	require.False(t, hidden, "curation recompute must see the cleared policy")
}

func TestRawRolloutWriterWaitsForPublication(t *testing.T) {
	for _, action := range []string{"shadow", "clear"} {
		t.Run(action, func(t *testing.T) {
			f := newProjectionFixture(t)
			m, _ := f.accept(t, "device-a", "capture", "")
			lease := f.lease(t, m)
			write := func(ctx context.Context) error { return f.sink.Project(ctx, lease, m, projectionOutcome("hello")) }
			table := "messages"
			if action == "clear" {
				require.NoError(t, write(t.Context()))
				require.NoError(t, ChangeRawRollout(t.Context(), f.admin, f.schema, f.tenant, "", "", "shadow"))
				write = func(ctx context.Context) error { return f.sink.SetCuration(ctx, "codex:portable", "starred", true) }
				table = "starred_sessions"
			}
			before := f.revisions(t)
			f.parkOn(t, "INSERT", table, "true")
			scoped := make(chan error, 1)
			f.whileParked(t, write, func(ctx context.Context) error {
				go func() { scoped <- ChangeRawRollout(t.Context(), f.admin, f.schema, f.tenant, "", "", action) }()
				require.Eventually(t, func() bool {
					var waiting int
					err := f.admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND NOT granted AND classid=((hashtextextended($1,0)>>32)&4294967295)::oid AND objid=(hashtextextended($1,0)&4294967295)::oid`, "agentsview-rollout:"+f.schema).Scan(&waiting)
					return err == nil && waiting == 1
				}, 2*time.Second, 10*time.Millisecond)
				require.Equal(t, before, f.revisions(t), "neither writer may publish before materialization finishes")
				return nil
			})
			require.NoError(t, <-scoped)
			after := f.revisions(t)
			require.Equal(t, before.corpus+2, after.corpus)
			identityDelta := int64(2)
			if action == "clear" {
				identityDelta = 1
			}
			require.Equal(t, before.identity+identityDelta, after.identity)
			var hidden bool
			require.NoError(t, f.runtime.QueryRow(`SELECT rollout_hidden FROM sessions WHERE provenance_kind='raw'`).Scan(&hidden))
			require.Equal(t, action == "shadow", hidden)
		})
	}
}
