package db

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUpdateSessionSignals(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "sig-1", "proj", func(s *Session) {
		s.MessageCount = 5
	})

	update := SessionSignalUpdate{
		ToolFailureSignalCount: 3,
		ToolRetryCount:         2,
		EditChurnCount:         1,
		ConsecutiveFailureMax:  4,
		Outcome:                "completed",
		OutcomeConfidence:      "high",
		EndedWithRole:          "assistant",
		FinalFailureStreak:     0,
		SignalsPendingSince:    nil,
		CompactionCount:        2,
		ContextPressureMax:     new(0.85),
		HealthScore:            new(72),
		HealthGrade:            new("B"),
		QualitySignals: QualitySignals{
			Version:                     CurrentQualitySignalVersion,
			ShortPromptCount:            2,
			UnstructuredStart:           true,
			MissingSuccessCriteriaCount: 1,
			MissingVerificationCount:    1,
			DuplicatePromptCount:        3,
			NoCodeContextCount:          1,
			RunawayToolLoopCount:        1,
		},
	}
	require.NoError(t, d.UpdateSessionSignals("sig-1", update),
		"UpdateSessionSignals")

	got, err := d.GetSessionFull(ctx, "sig-1")
	require.NoError(t, err, "GetSessionFull")
	require.NotNil(t, got, "session not found after update")

	assert.Equal(t, 3, got.ToolFailureSignalCount, "ToolFailureSignalCount")
	assert.Equal(t, 2, got.ToolRetryCount, "ToolRetryCount")
	assert.Equal(t, 1, got.EditChurnCount, "EditChurnCount")
	assert.Equal(t, 4, got.ConsecutiveFailureMax, "ConsecutiveFailureMax")
	assert.Equal(t, "completed", got.Outcome, "Outcome")
	assert.Equal(t, "high", got.OutcomeConfidence, "OutcomeConfidence")
	assert.Equal(t, "assistant", got.EndedWithRole, "EndedWithRole")
	assert.Equal(t, 0, got.FinalFailureStreak, "FinalFailureStreak")
	assert.Equal(t, 2, got.CompactionCount, "CompactionCount")
	assert.Equal(t, CurrentQualitySignalVersion, got.QualitySignalVersion,
		"QualitySignalVersion")
	assert.Equal(t, 2, got.ShortPromptCount, "ShortPromptCount")
	assert.True(t, got.UnstructuredStart, "UnstructuredStart")
	assert.Equal(t, 1, got.MissingSuccessCriteriaCount,
		"MissingSuccessCriteriaCount")
	assert.Equal(t, 1, got.MissingVerificationCount,
		"MissingVerificationCount")
	assert.Equal(t, 3, got.DuplicatePromptCount, "DuplicatePromptCount")
	assert.Equal(t, 1, got.NoCodeContextCount, "NoCodeContextCount")
	assert.Equal(t, 1, got.RunawayToolLoopCount, "RunawayToolLoopCount")

	assert.Nil(t, got.SignalsPendingSince, "SignalsPendingSince")
	require.NotNil(t, got.ContextPressureMax, "ContextPressureMax")
	assert.Equal(t, 0.85, *got.ContextPressureMax, "ContextPressureMax")
	require.NotNil(t, got.HealthScore, "HealthScore")
	assert.Equal(t, 72, *got.HealthScore, "HealthScore")
	require.NotNil(t, got.HealthGrade, "HealthGrade")
	assert.Equal(t, "B", *got.HealthGrade, "HealthGrade")

	// Update again with pending since set and nullable fields
	// cleared.
	pending := "2024-06-01T00:00:00Z"
	update2 := SessionSignalUpdate{
		Outcome:             "unknown",
		OutcomeConfidence:   "low",
		SignalsPendingSince: &pending,
	}
	require.NoError(t, d.UpdateSessionSignals("sig-1", update2),
		"UpdateSessionSignals (2nd)")

	got2, err := d.GetSessionFull(ctx, "sig-1")
	require.NoError(t, err, "GetSessionFull (2nd)")

	// Verify signals_pending_since is loaded by GetSessionFull
	// (was previously absent from the column lists).
	require.NotNil(t, got2.SignalsPendingSince, "SignalsPendingSince")
	assert.Equal(t, pending, *got2.SignalsPendingSince, "SignalsPendingSince")

	pendingIDs, err := d.PendingSignalSessions(
		ctx, "2024-07-01T00:00:00Z",
	)
	require.NoError(t, err, "PendingSignalSessions")
	assert.Equal(t, []string{"sig-1"}, pendingIDs, "PendingSignalSessions")

	assert.Nil(t, got2.ContextPressureMax, "ContextPressureMax")
	assert.Nil(t, got2.HealthScore, "HealthScore")
	assert.Nil(t, got2.HealthGrade, "HealthGrade")
	assert.Nil(t, got2.StoredQualitySignals(), "StoredQualitySignals")
}

// TestUpdateSessionSignalsBumpsLocalModifiedAt ensures that
// signal updates bump local_modified_at so the session is
// re-selected by the next pg push. Without this bump, sessions
// backfilled by BackfillSignals (e.g. after a PG schema
// migration adds new signal columns) would never propagate to
// PG-backed deployments.
func TestUpdateSessionSignalsBumpsLocalModifiedAt(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "lm-1", "proj")

	// Snapshot local_modified_at after the initial upsert.
	beforeRow, err := d.GetSessionFull(ctx, "lm-1")
	require.NoError(t, err, "GetSessionFull")
	require.NotNil(t, beforeRow, "session not found before update")
	before := ""
	if beforeRow.LocalModifiedAt != nil {
		before = *beforeRow.LocalModifiedAt
	}

	// SQLite's strftime('now') ticks at millisecond precision.
	// Sleep a few ms so a re-set produces a strictly later value.
	time.Sleep(5 * time.Millisecond)

	require.NoError(t, d.UpdateSessionSignals("lm-1", SessionSignalUpdate{
		ToolFailureSignalCount: 1,
		Outcome:                "completed",
		OutcomeConfidence:      "high",
		EndedWithRole:          "assistant",
	}), "UpdateSessionSignals")

	afterRow, err := d.GetSessionFull(ctx, "lm-1")
	require.NoError(t, err, "GetSessionFull (after)")
	require.NotNil(t, afterRow.LocalModifiedAt,
		"local_modified_at not set after signal update")
	require.NotEmpty(t, *afterRow.LocalModifiedAt,
		"local_modified_at not set after signal update")
	assert.Greater(t, *afterRow.LocalModifiedAt, before,
		"local_modified_at not bumped")
}

func TestPendingSignalSessions(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	cutoff := "2024-06-01T12:00:00Z"

	// Session with pending_since before cutoff -- should match.
	insertSession(t, d, "ps-old", "proj")
	old := SessionSignalUpdate{
		Outcome:             "unknown",
		OutcomeConfidence:   "low",
		SignalsPendingSince: new("2024-06-01T10:00:00Z"),
	}
	require.NoError(t, d.UpdateSessionSignals("ps-old", old),
		"UpdateSessionSignals ps-old")

	// Session with pending_since after cutoff -- should NOT match.
	insertSession(t, d, "ps-new", "proj")
	newer := SessionSignalUpdate{
		Outcome:             "unknown",
		OutcomeConfidence:   "low",
		SignalsPendingSince: new("2024-06-01T14:00:00Z"),
	}
	require.NoError(t, d.UpdateSessionSignals("ps-new", newer),
		"UpdateSessionSignals ps-new")

	// Session with no pending_since -- should NOT match.
	insertSession(t, d, "ps-none", "proj")

	ids, err := d.PendingSignalSessions(ctx, cutoff)
	require.NoError(t, err, "PendingSignalSessions")
	require.Len(t, ids, 1)
	assert.Equal(t, "ps-old", ids[0])
}

// TestBackfillSignalsMarkerOnlyOnSuccess guards the
// completion-marker contract: the one-shot marker must only be
// set when every session was processed successfully. Partial
// runs (e.g. a concurrent resync that disconnects the DB
// mid-backfill) must leave the marker unset so the next
// startup retries.
func TestBackfillSignalsMarkerOnlyOnSuccess(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "ok-1", "p")
	insertSession(t, d, "ok-2", "p")
	insertSession(t, d, "fail-1", "p")

	// One session fails -- marker must NOT be set.
	failOnce := true
	compute := func(_ context.Context, id string) error {
		if id == "fail-1" && failOnce {
			failOnce = false
			return fmt.Errorf("simulated failure")
		}
		return d.UpdateSessionSignals(id, SessionSignalUpdate{
			QualitySignals: QualitySignals{
				Version: CurrentQualitySignalVersion,
			},
		})
	}

	err := d.BackfillSignals(
		ctx,
		compute,
	)
	require.Error(t, err, "expected error from partial backfill")

	// Marker check: a second BackfillSignals call must NOT
	// short-circuit since the marker is unset.
	calls := 0
	err = d.BackfillSignals(
		ctx,
		func(ctx context.Context, id string) error {
			calls++
			return compute(ctx, id)
		},
	)
	require.NoError(t, err, "retry")
	assert.Equal(t, 3, calls,
		"second backfill should see all sessions (marker not set after partial run)")

	// Now the marker should be set; a third call short-circuits.
	calls = 0
	err = d.BackfillSignals(
		ctx,
		func(_ context.Context, _ string) error {
			calls++
			return nil
		},
	)
	require.NoError(t, err, "third call")
	assert.Equal(t, 0, calls,
		"third backfill should see 0 sessions (marker set after clean run)")
}

func TestBackfillSignalsRecomputesStaleQualityVersions(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "stale", "p")
	insertSession(t, d, "current", "p")
	insertSession(t, d, "empty", "p", func(s *Session) {
		s.MessageCount = 0
	})

	if err := d.UpdateSessionSignals("stale", SessionSignalUpdate{
		QualitySignals: QualitySignals{
			Version: CurrentQualitySignalVersion - 1,
		},
	}); err != nil {
		t.Fatalf("UpdateSessionSignals stale: %v", err)
	}
	if err := d.UpdateSessionSignals("current", SessionSignalUpdate{
		QualitySignals: QualitySignals{
			Version: CurrentQualitySignalVersion,
		},
	}); err != nil {
		t.Fatalf("UpdateSessionSignals current: %v", err)
	}
	if err := d.MarkSignalsBackfillDone(); err != nil {
		t.Fatalf("MarkSignalsBackfillDone: %v", err)
	}

	var calls []string
	if err := d.BackfillSignals(
		ctx,
		func(_ context.Context, id string) error {
			calls = append(calls, id)
			return nil
		},
	); err != nil {
		t.Fatalf("BackfillSignals: %v", err)
	}
	if len(calls) != 1 || calls[0] != "stale" {
		t.Fatalf("calls = %v, want [stale]", calls)
	}
}
