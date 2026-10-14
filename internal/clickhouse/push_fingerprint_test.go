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

	// What main, which has no session_plan_usage, computes for this session.
	const withoutPlanUsage = "3ffa01ac4b265fffea3c52f5731cafd35b542c2db053e8d5f993bac323c0d07b"
	assert.Equal(t, withoutPlanUsage, fingerprint())

	snapshot.PlanUsage = &db.SessionPlanUsage{HasPlanUsage: true, WeeklyPercent: new(1.0)}
	first := fingerprint()
	assert.NotEqual(t, withoutPlanUsage, first)
	snapshot.PlanUsage = &db.SessionPlanUsage{HasPlanUsage: true, WeeklyPercent: new(2.0)}
	assert.NotEqual(t, first, fingerprint())
}
