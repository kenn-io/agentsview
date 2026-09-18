package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/export"
)

func TestActivityReportSourceProbeChangesWithReportInputs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	ctx := t.Context()
	initial, err := database.ActivityReportSourceProbe(ctx)
	require.NoError(err)

	insertSession(t, database, "probe-session", "probe-project")
	afterSession, err := database.ActivityReportSourceProbe(ctx)
	require.NoError(err)
	assert.NotEqual(initial, afterSession)

	seedMessage(t, database, "probe-session", 0, "user",
		"2026-07-01T00:00:00Z", "")
	afterMessage, err := database.ActivityReportSourceProbe(ctx)
	require.NoError(err)
	assert.Greater(afterMessage.MaxMessageID, afterSession.MaxMessageID)

	_, err = database.getWriter().Exec(`INSERT INTO usage_events(
		session_id, source, model, output_tokens, occurred_at, dedup_key
	) VALUES (?, 'test', 'model', 1, ?, 'probe')`,
		"probe-session", "2026-07-01T00:00:01Z")
	require.NoError(err)
	afterUsage, err := database.ActivityReportSourceProbe(ctx)
	require.NoError(err)
	assert.Greater(afterUsage.MaxUsageID, afterMessage.MaxUsageID)

	_, err = database.getWriter().Exec(`INSERT INTO model_pricing(
		model_pattern, updated_at
	) VALUES ('probe-model', '2026-07-01T00:00:02Z')`)
	require.NoError(err)
	afterPricing, err := database.ActivityReportSourceProbe(ctx)
	require.NoError(err)
	assert.Equal("2026-07-01T00:00:02Z", afterPricing.MaxPricingUpdated)

	require.NoError(database.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			Project: "probe-project", Machine: "test-machine",
			RootPath:   "/fixtures/probe-project",
			GitRemote:  "https://example.com/acme/probe-project.git",
			ObservedAt: time.Date(2026, 7, 1, 0, 0, 3, 0, time.UTC),
		},
	))
	afterIdentity, err := database.ActivityReportSourceProbe(ctx)
	require.NoError(err)
	assert.NotEqual(afterPricing, afterIdentity,
		"identity-only changes must invalidate Activity report generations")
}
