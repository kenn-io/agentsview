//go:build pgtest

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRawRolloutProjectionAndCurationWaitBeforeRowLocks(t *testing.T) {
	f := newProjectionFixture(t)
	m, accepted := f.accept(t, "device-a", "initial", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("initial")))
	target, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	m, _ = f.accept(t, "device-a", "capture", accepted.Receipt)
	lease := f.lease(t, m)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	// While a worker waits on rollout, none of the later row locks may be held.
	probeRows := func(projection bool) {
		probe, err := f.admin.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer probe.Rollback()
		var found int
		if projection {
			require.NoError(t, probe.QueryRowContext(ctx, `SELECT 1 FROM raw_source_heads WHERE manifest_id=$1 FOR UPDATE NOWAIT`, m.ManifestID).Scan(&found))
			require.Equal(t, 1, found)
			require.NoError(t, probe.QueryRowContext(ctx, `SELECT 1 FROM raw_ingest_jobs WHERE id=$1 FOR UPDATE NOWAIT`, lease.ID).Scan(&found))
			require.Equal(t, 1, found)
		}
		require.NoError(t, probe.QueryRowContext(ctx, `SELECT 1 FROM raw_session_groups WHERE group_id=$1 FOR UPDATE NOWAIT`, target.GroupID).Scan(&found))
		require.Equal(t, 1, found)
	}
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
	probeRows(true)
	require.NoError(t, gate.Commit())
	require.NoError(t, <-projected)
	var content, state string
	require.NoError(t, f.runtime.QueryRow(`SELECT m.content FROM messages m JOIN sessions s ON s.id=m.session_id WHERE s.raw_group_id=$1 AND m.ordinal=0`, target.GroupID).Scan(&content))
	require.Equal(t, "hello", content)
	require.NoError(t, f.runtime.QueryRow(`SELECT state FROM raw_ingest_jobs WHERE id=$1`, lease.ID).Scan(&state))
	require.Equal(t, "complete", state)

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
	probeRows(false)
	require.NoError(t, gate.Commit())
	require.NoError(t, <-curated)
	var starred bool
	require.NoError(t, f.runtime.QueryRow(`SELECT EXISTS(SELECT 1 FROM starred_sessions)`).Scan(&starred))
	require.True(t, starred)
}
