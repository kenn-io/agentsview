//go:build pgtest

package main

import (
	"bytes"
	"context"
	"log"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/review"
	"go.kenn.io/agentsview/internal/postgres"
)

func TestPGFrictionRunnerKeepsSessionReviewWithoutDiagnosticSelect(t *testing.T) {
	pgURL := os.Getenv("TEST_PG_URL")
	if pgURL == "" {
		t.Skip("TEST_PG_URL not configured")
	}
	const (
		schema   = "agentsview_friction_diag_read_test"
		role     = "agentsview_friction_diag_reader"
		password = "agentsview_friction_diag_reader_pw"
	)
	ctx := t.Context()
	admin, err := postgres.Open(ctx, pgURL, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanupCtx := context.WithoutCancel(ctx)
		_, _ = admin.ExecContext(cleanupCtx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
		_, _ = admin.ExecContext(cleanupCtx, "DROP OWNED BY "+role)
		_, _ = admin.ExecContext(cleanupCtx, "DROP ROLE IF EXISTS "+role)
		assert.NoError(t, admin.Close())
	})
	_, err = admin.ExecContext(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	require.NoError(t, err)
	_, _ = admin.ExecContext(ctx, "DROP OWNED BY "+role)
	_, err = admin.ExecContext(ctx, "DROP ROLE IF EXISTS "+role)
	require.NoError(t, err)
	require.NoError(t, postgres.EnsureSchema(ctx, admin, schema))
	_, err = admin.ExecContext(ctx, "CREATE ROLE "+role+" LOGIN PASSWORD '"+password+"'")
	require.NoError(t, err)
	for _, statement := range []string{
		"GRANT USAGE ON SCHEMA " + schema + " TO " + role,
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA " + schema + " TO " + role,
		"GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA " + schema + " TO " + role,
		"REVOKE SELECT ON " + schema + ".diagnostic_events FROM " + role,
	} {
		_, err = admin.ExecContext(ctx, statement)
		require.NoError(t, err, statement)
	}
	roleURL, err := url.Parse(pgURL)
	require.NoError(t, err)
	roleURL.User = url.UserPassword(role, password)
	store, err := postgres.NewStore(ctx, roleURL.String(), schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, store.Close()) })

	previousLogWriter := log.Writer()
	var logOutput bytes.Buffer
	log.SetOutput(&logOutput)
	t.Cleanup(func() { log.SetOutput(previousLogWriter) })
	store.DetectFrictionAvailability(ctx)
	assert.True(t, store.FrictionAvailable(), "session review writes remain available")
	assert.False(t, store.FrictionDiagnosticsAvailable())
	assert.Contains(t, logOutput.String(), "GRANT SELECT ON diagnostic_events")
	_, err = store.DiagnosticSignalsForDate(ctx, "2026-10-01", time.UTC)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "42501", pgErr.Code, "the test role lacks SELECT on diagnostic_events")

	_, err = admin.ExecContext(ctx, `INSERT INTO sessions (
		id, machine, project, agent, ended_at, friction_rules_version
	) VALUES ('session', 'machine', 'project', 'claude', '2026-10-01T12:00:00Z', $1)`, friction.RulesVersion)
	require.NoError(t, err)
	_, err = admin.ExecContext(ctx, `INSERT INTO friction_findings (
		session_id, kind, detector, title, fingerprint, seq, rules_version
	) VALUES ('session', 'correction', 'correction.coding', 'Use the existing helper', 'fl1:session', 0, $1)`, friction.RulesVersion)
	require.NoError(t, err)

	cfg := config.Config{Friction: config.FrictionConfig{
		Enabled: true, Timezone: "UTC", BackfillDays: 3,
	}}
	runner, err := newFrictionRunner(cfg, store, func() time.Time {
		return time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	})
	require.NoError(t, err)
	require.NotNil(t, runner)
	_, err = runner.CatchUp(ctx)
	require.NoError(t, err)
	digest, err := store.GetFrictionDigest(ctx, "2026-10-01")
	require.NoError(t, err)
	require.NotNil(t, digest)
	assert.Equal(t, 1, digest.SessionsScanned)

	_, err = admin.ExecContext(ctx, `INSERT INTO diagnostic_events (
		id, source, message, occurred_at, occurred_at_exact, accepted_at
	) VALUES ('diag-event', 'worker', 'worker failed', '2026-10-01T13:00:00Z', '2026-10-01T13:00:00Z', '2026-10-01T13:00:00Z')`)
	require.NoError(t, err)
	_, err = admin.ExecContext(ctx, "GRANT SELECT ON "+schema+".diagnostic_events TO "+role)
	require.NoError(t, err)
	store.DetectFrictionAvailability(ctx)
	require.True(t, store.FrictionDiagnosticsAvailable())
	runner, err = newFrictionRunner(cfg, store, func() time.Time {
		return time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	})
	require.NoError(t, err)
	require.NotNil(t, runner)
	_, err = runner.CatchUp(ctx)
	require.NoError(t, err)
	digest, err = store.GetFrictionDigest(ctx, "2026-10-01")
	require.NoError(t, err)
	require.NotNil(t, digest)
	snapshot, err := review.DecodeSnapshot(digest.SnapshotJSON)
	require.NoError(t, err)
	var includedDiagnostic bool
	for _, signal := range snapshot.Signals {
		includedDiagnostic = includedDiagnostic || signal.SubjectID == "diagnostic:diag-event"
	}
	assert.True(t, includedDiagnostic, "diagnostics return to the review once SELECT is granted")
}
