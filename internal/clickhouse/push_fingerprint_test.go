package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// TestSnapshotFingerprintTracksPlanUsage pins both halves of the plan
// usage fingerprint contract: a session without plan usage keeps the
// fingerprint it had before the table existed, so an upgrade re-pushes
// nothing, and a plan usage change makes the session a push candidate.
func TestSnapshotFingerprintTracksPlanUsage(t *testing.T) {
	s := &Sync{machine: "m"}
	snapshot := &db.SessionMirrorSnapshot{Session: db.Session{
		ID: "plan", Project: "alpha", Machine: "m1", Agent: "codex-cloud",
		CreatedAt: "2026-10-01T00:00:00Z",
	}}
	fingerprint := func() string {
		t.Helper()
		fp, err := s.snapshotFingerprint(snapshot)
		require.NoError(t, err)
		return fp
	}

	// Recorded before session_plan_usage existed.
	const withoutPlanUsage = "30cbc4cbdcb2e8df98366ef537afdeedcd5c98d5dc0ac91c8fd9e629e8bbf76b"
	assert.Equal(t, withoutPlanUsage, fingerprint())

	snapshot.PlanUsage = &db.SessionPlanUsage{HasPlanUsage: true, WeeklyPercent: new(1.0)}
	first := fingerprint()
	assert.NotEqual(t, withoutPlanUsage, first)
	snapshot.PlanUsage = &db.SessionPlanUsage{HasPlanUsage: true, WeeklyPercent: new(2.0)}
	assert.NotEqual(t, first, fingerprint())
}
