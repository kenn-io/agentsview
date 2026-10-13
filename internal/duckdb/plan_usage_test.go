//go:build !(windows && arm64)

package duckdb

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

func duckPlanUsage(weekly float64) *db.SessionPlanUsage {
	return &db.SessionPlanUsage{
		HasPlanUsage:  true,
		DataAsOf:      "2026-10-01T10:00:00.123456789Z",
		DataStatus:    "complete",
		UsageSource:   "query_v2",
		WeeklyPercent: new(weekly),
		Credits:       new("1.5"),
		Groups: []db.SessionPlanUsageGroup{
			{Model: "gpt-5-codex", ReasoningEffort: "high", Speed: "standard", WeeklyPercent: new(weekly), FiveHourPercent: new(0.5)},
		},
		Final:           true,
		OutcomeKind:     "left_out",
		OutcomeAt:       "2026-10-02T08:30:00Z",
		CountedUnder:    "codex-cloud:root",
		HasCoverage:     true,
		LastCaptureAt:   "2026-10-01T09:59:00.5Z",
		UncapturedTurns: 4,
	}
}

func pushPlanUsageMirror(t *testing.T, local *db.DB, path string) {
	t.Helper()
	_, err := Push(t.Context(), path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
}

func readPlanUsage(t *testing.T, path, sessionID string) *db.SessionPlanUsage {
	t.Helper()
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	got, err := store.GetSessionPlanUsage(t.Context(), sessionID)
	require.NoError(t, err)
	return got
}

func TestDuckDBPlanUsageRoundTrip(t *testing.T) {
	local, path := newPushFixture(t, 2)
	require.NoError(t, local.ReplaceSessionPlanUsage(t.Context(), "sess-1", duckPlanUsage(12.5)))
	pushPlanUsageMirror(t, local, path)
	assert.Equal(t, duckPlanUsage(12.5), readPlanUsage(t, path, "sess-1"))
	assert.Nil(t, readPlanUsage(t, path, "sess-2"), "a session without plan usage has no row")

	require.NoError(t, local.ReplaceSessionPlanUsage(t.Context(), "sess-1", duckPlanUsage(40)))
	pushPlanUsageMirror(t, local, path)
	assert.Equal(t, duckPlanUsage(40), readPlanUsage(t, path, "sess-1"),
		"an incremental push replaces the row")

	require.NoError(t, local.ReplaceSessionPlanUsage(t.Context(), "sess-1", nil))
	pushPlanUsageMirror(t, local, path)
	assert.Nil(t, readPlanUsage(t, path, "sess-1"), "a row removed from the archive must leave the mirror")
}

func TestDuckDBHardDeleteClearsPlanUsage(t *testing.T) {
	local, path := newPushFixture(t, 1)
	require.NoError(t, local.ReplaceSessionPlanUsage(t.Context(), "sess-1", duckPlanUsage(12.5)))
	pushPlanUsageMirror(t, local, path)
	require.NotNil(t, readPlanUsage(t, path, "sess-1"))

	require.NoError(t, local.SoftDeleteSession(t.Context(), "sess-1"))
	deleted, err := local.DeleteSessionIfTrashed(t.Context(), "sess-1")
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)
	pushPlanUsageMirror(t, local, path)

	conn, err := Open(t.Context(), path)
	require.NoError(t, err)
	defer conn.Close()
	var rows int
	require.NoError(t, conn.QueryRowContext(t.Context(),
		`SELECT count(*) FROM session_plan_usage WHERE session_id = 'sess-1'`).Scan(&rows))
	assert.Zero(t, rows)
}

// A mirror written before v19 has no session_plan_usage table. The next
// push must rebuild it rather than fail or serve without plan usage.
func TestDuckDBSchemaVersionRebuild(t *testing.T) {
	local, path := newPushFixture(t, 1)
	pushPlanUsageMirror(t, local, path)
	conn, err := Open(t.Context(), path)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `DROP TABLE session_plan_usage`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	setMirrorMetadataValue(t, path, schemaVersionMetadataKey, strconv.Itoa(SchemaVersion-1))
	require.NoError(t, local.ReplaceSessionPlanUsage(t.Context(), "sess-1", duckPlanUsage(7)))

	result, err := Push(t.Context(), path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	assert.True(t, result.Diagnostics.Full, "rebuild reason: %s", result.Diagnostics.RebuildReason)
	assert.Equal(t, duckPlanUsage(7), readPlanUsage(t, path, "sess-1"))
}

func TestDuckDBSessionUsageIncludesPlanUsage(t *testing.T) {
	local, path := newPushFixture(t, 1)
	require.NoError(t, local.ReplaceSessionPlanUsage(t.Context(), "sess-1", duckPlanUsage(12.5)))
	pushPlanUsageMirror(t, local, path)
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	usage, err := store.GetSessionUsage(t.Context(), "sess-1", false)
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, duckPlanUsage(12.5), usage.PlanUsage)
}

// The session row alone cannot reveal a plan usage change to a caller that
// already holds the session, so the fingerprint must cover the row.
func TestDuckDBFingerprintTracksPlanUsage(t *testing.T) {
	local, _ := newPushFixture(t, 1)
	sess, err := local.GetSessionFull(t.Context(), "sess-1")
	require.NoError(t, err)
	require.NotNil(t, sess)
	s := newInMemoryTestSync(t, local, storage.MirrorPushOptions{})
	fingerprint := func() string {
		t.Helper()
		fps, err := s.sessionFingerprints(t.Context(), []db.Session{*sess})
		require.NoError(t, err)
		return fps["sess-1"]
	}
	before := fingerprint()
	require.NoError(t, local.ReplaceSessionPlanUsage(t.Context(), "sess-1", duckPlanUsage(1)))
	first := fingerprint()
	assert.NotEqual(t, before, first)
	require.NoError(t, local.ReplaceSessionPlanUsage(t.Context(), "sess-1", duckPlanUsage(2)))
	assert.NotEqual(t, first, fingerprint())
}
