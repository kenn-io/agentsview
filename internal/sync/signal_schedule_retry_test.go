package sync

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestSignalSchedulerRetriesExhaustedSnapshotConflicts(t *testing.T) {
	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{})
	t.Cleanup(engine.Close)
	const sessionID = "snapshot-retry"
	session := db.Session{ID: sessionID, Agent: "claude", Project: "project", Machine: "local", MessageCount: 1}
	_, err := database.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: session, ReplaceMessages: true,
		Messages: []db.Message{{SessionID: sessionID, Ordinal: 0, Role: "assistant", Content: "initial"}},
	}}, nil)
	require.NoError(t, err)
	h := newSchedulerHarness(10*time.Second, 2*time.Second)
	defer h.sched.stop()
	runs, conflicts := 0, 0
	h.sched.run = func(id string) {
		runs++
		_, err := engine.recomputeSignalsFromDBWithHook(t.Context(), id, func(int) {
			if runs != 1 {
				return
			}
			conflicts++
			// Session uploads use this same database boundary without the engine lock.
			_, writeErr := database.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
				Session: session, ReplaceMessages: true,
				Messages: []db.Message{{SessionID: sessionID, Ordinal: 0, Role: "assistant", Content: fmt.Sprintf("replacement %d", conflicts), IsCompactBoundary: conflicts == 3}},
			}}, nil)
			require.NoError(t, writeErr)
		})
		if err != nil {
			h.sched.deferRetry(id)
		}
	}
	h.sched.markDirty(sessionID)
	require.Equal(t, 3, conflicts)
	require.Equal(t, 1, runs, "a failed recompute must not recurse inline")
	require.Equal(t, 1, h.armedCount())
	h.advance(2 * time.Second)
	h.fireTimer(t)
	require.Equal(t, 2, runs)
	stored, err := database.GetSessionFull(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	require.Equal(t, db.CurrentQualitySignalVersion, stored.QualitySignalVersion)
	require.Equal(t, 1, stored.CompactionCount, "signals must include the final upload's compact boundary")
	require.Zero(t, h.armedCount(), "successful retry must leave no recurring timer")
}

func TestSignalSchedulerDoesNotRetryFailedShutdownFlush(t *testing.T) {
	h := newSchedulerHarness(10*time.Second, 2*time.Second)
	runs := 0
	h.sched.run = func(id string) {
		runs++
		h.sched.deferRetry(id)
	}
	h.sched.markDirty("session")
	require.Equal(t, 1, runs)
	require.Equal(t, 1, h.armedCount())
	h.sched.stop()
	require.Equal(t, 2, runs, "shutdown makes one final attempt")
	require.Zero(t, h.armedCount())
	h.sched.flushAll()
	require.Equal(t, 2, runs, "failed shutdown work must not remain queued")
}
