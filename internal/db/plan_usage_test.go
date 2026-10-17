package db

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rootPlanUsage is a usage root with a final row, one group, a newer
// "left out" outcome, and live capture coverage.
func rootPlanUsage() *SessionPlanUsage {
	return &SessionPlanUsage{
		HasPlanUsage:    true,
		DataAsOf:        "2026-10-02T09:30:00Z",
		DataStatus:      "available",
		UsageSource:     "wham",
		WeeklyPercent:   new(2.25),
		FiveHourPercent: new(12.5),
		Credits:         new("0E-10"),
		Groups: []SessionPlanUsageGroup{{
			Model: "gpt-6", ReasoningEffort: "high", Speed: "standard", ProductExperience: "codex",
			WeeklyPercent: new(2.25), Credits: new("1.5"),
		}},
		Final:           true,
		OutcomeKind:     "left_out",
		OutcomeAt:       "2026-10-03T08:00:00Z",
		HasCoverage:     true,
		LastCaptureAt:   "2026-10-01T12:30:00Z",
		UncapturedTurns: 2,
	}
}

func TestPlanUsageReplaceAndRead(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "codex-cloud:thread-a", "proj")
	insertSession(t, d, "codex-cloud:thread-b", "proj")

	got, err := d.GetSessionPlanUsage(ctx, "codex-cloud:thread-a")
	require.NoError(t, err)
	assert.Nil(t, got, "no row before the first write")

	require.NoError(t, d.ReplaceSessionPlanUsage(ctx, "codex-cloud:thread-a", rootPlanUsage()))
	got, err = d.GetSessionPlanUsage(ctx, "codex-cloud:thread-a")
	require.NoError(t, err)
	assert.Equal(t, rootPlanUsage(), got)
	usage, err := d.GetSessionUsage(ctx, "codex-cloud:thread-a", false)
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, rootPlanUsage(), usage.PlanUsage, "the session usage reader returns the row")

	// The thread turns out to be a descendant: its plan figures go, its
	// coverage stays.
	descendant := &SessionPlanUsage{
		HasPlanUsage:    true,
		OutcomeKind:     "counted_under",
		OutcomeAt:       "2026-10-04T08:00:00Z",
		CountedUnder:    "codex-cloud:thread-root",
		HasCoverage:     true,
		LastCaptureAt:   "2026-10-01T12:30:00Z",
		UncapturedTurns: 2,
	}
	require.NoError(t, d.ReplaceSessionPlanUsage(ctx, "codex-cloud:thread-a", descendant))
	got, err = d.GetSessionPlanUsage(ctx, "codex-cloud:thread-a")
	require.NoError(t, err)
	assert.Equal(t, descendant, got)

	other, err := d.GetSessionPlanUsage(ctx, "codex-cloud:thread-b")
	require.NoError(t, err)
	assert.Nil(t, other, "a write touches only its own session")

	require.NoError(t, d.ReplaceSessionPlanUsage(ctx, "codex-cloud:thread-a", nil))
	got, err = d.GetSessionPlanUsage(ctx, "codex-cloud:thread-a")
	require.NoError(t, err)
	assert.Nil(t, got, "a nil write removes the row")
	usage, err = d.GetSessionUsage(ctx, "codex-cloud:thread-a", false)
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Nil(t, usage.PlanUsage)
}

func TestPlanUsageBumpsLocalModified(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "codex-cloud:thread-a", "proj")
	const old = "2000-01-01T00:00:00.000Z"
	setOld := func() {
		_, err := d.getWriter().Exec(ctx,
			"UPDATE sessions SET local_modified_at = ? WHERE id = ?", old, "codex-cloud:thread-a")
		require.NoError(t, err)
	}
	localModified := func() string {
		s, err := d.GetSessionFull(ctx, "codex-cloud:thread-a")
		require.NoError(t, err)
		require.NotNil(t, s)
		require.NotNil(t, s.LocalModifiedAt)
		return *s.LocalModifiedAt
	}

	setOld()
	require.NoError(t, d.ReplaceSessionPlanUsage(ctx, "codex-cloud:thread-a", rootPlanUsage()))
	assert.Greater(t, localModified(), old, "a write is a push candidate")

	setOld()
	require.NoError(t, d.ReplaceSessionPlanUsage(ctx, "codex-cloud:thread-a", nil))
	assert.Greater(t, localModified(), old, "removing a row is a push candidate")

	setOld()
	require.NoError(t, d.ReplaceSessionPlanUsage(ctx, "codex-cloud:thread-a", nil))
	assert.Equal(t, old, localModified(), "removing nothing changes nothing")
}

func TestPlanUsageFingerprints(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "codex-cloud:thread-a", "proj")
	insertSession(t, d, "codex-cloud:thread-b", "proj")
	require.NoError(t, d.ReplaceSessionPlanUsage(ctx, "codex-cloud:thread-a", rootPlanUsage()))

	before, err := d.PlanUsageFingerprints(ctx, []string{"codex-cloud:thread-a", "codex-cloud:thread-b"})
	require.NoError(t, err)
	assert.NotEmpty(t, before["codex-cloud:thread-a"])
	assert.Empty(t, before["codex-cloud:thread-b"], "no row, no fingerprint")
	require.Contains(t, before, "codex-cloud:thread-b")

	changed := []func(*SessionPlanUsage){
		func(p *SessionPlanUsage) { p.OutcomeAt = "2026-10-03T09:00:00Z" },
		func(p *SessionPlanUsage) { p.Groups[0].Credits = new("2") },
		func(p *SessionPlanUsage) { p.UncapturedTurns = 3 },
		func(p *SessionPlanUsage) { p.FiveHourPercent = nil },
	}
	for i, change := range changed {
		p := rootPlanUsage()
		change(p)
		require.NoError(t, d.ReplaceSessionPlanUsage(ctx, "codex-cloud:thread-a", p))
		after, err := d.PlanUsageFingerprints(ctx, []string{"codex-cloud:thread-a"})
		require.NoError(t, err)
		assert.NotEqual(t, before["codex-cloud:thread-a"], after["codex-cloud:thread-a"], "change %d", i)
	}
}

func TestPlanUsageWrittenByBatchAndRemovedWithSession(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	session := Session{ID: "codex-cloud:thread-a", Project: "proj", Machine: defaultMachine, Agent: "codex-cloud"}
	_, err := d.WriteSessionBatch([]SessionBatchWrite{{Session: session, PlanUsage: rootPlanUsage()}})
	require.NoError(t, err)
	got, err := d.GetSessionPlanUsage(ctx, session.ID)
	require.NoError(t, err)
	assert.Equal(t, rootPlanUsage(), got)

	_, err = d.WriteSessionBatch([]SessionBatchWrite{{Session: session}})
	require.NoError(t, err)
	got, err = d.GetSessionPlanUsage(ctx, session.ID)
	require.NoError(t, err)
	assert.Nil(t, got, "a full write without plan usage clears the old row")

	require.NoError(t, d.ReplaceSessionPlanUsage(ctx, session.ID, rootPlanUsage()))
	_, err = d.getWriter().Exec(ctx, "DELETE FROM sessions WHERE id = ?", session.ID)
	require.NoError(t, err)
	var rows int
	require.NoError(t, d.getReader().QueryRowContext(ctx,
		"SELECT count(*) FROM session_plan_usage").Scan(&rows))
	assert.Zero(t, rows, "deleting the session deletes its row")
}

func TestFullResyncKeepsOrphanAndTrashPlanUsage(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.db")
	source := testDBAtPath(t, sourcePath, "source")
	insertSession(t, source, "codex-cloud:orphan", "proj")
	insertSession(t, source, "codex-cloud:trashed", "proj")
	require.NoError(t, source.ReplaceSessionPlanUsage(ctx, "codex-cloud:orphan", rootPlanUsage()))
	coverageOnly := &SessionPlanUsage{HasCoverage: true, LastCaptureAt: "2026-10-01T12:30:00Z", UncapturedTurns: 1}
	require.NoError(t, source.ReplaceSessionPlanUsage(ctx, "codex-cloud:trashed", coverageOnly))
	require.NoError(t, source.SoftDeleteSession(ctx, "codex-cloud:trashed"))
	require.NoError(t, source.Close())

	destination := testDBAtPath(t, filepath.Join(dir, "destination.db"), "destination")
	defer destination.Close()
	require.NoError(t, destination.CopySyncStateFrom(sourcePath))
	trashed, err := destination.CopyTrashedDataFrom(sourcePath)
	require.NoError(t, err)
	require.Equal(t, []string{"codex-cloud:trashed"}, trashed)
	copied, err := destination.CopyOrphanedDataFrom(sourcePath)
	require.NoError(t, err)
	require.Equal(t, 1, copied)

	got, err := destination.GetSessionPlanUsage(ctx, "codex-cloud:orphan")
	require.NoError(t, err)
	assert.Equal(t, rootPlanUsage(), got)
	got, err = destination.GetSessionPlanUsage(ctx, "codex-cloud:trashed")
	require.NoError(t, err)
	assert.Equal(t, coverageOnly, got)
}
