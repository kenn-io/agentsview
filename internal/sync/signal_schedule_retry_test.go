package sync

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestSignalSchedulerRetriesExhaustedSnapshotConflicts(t *testing.T) {
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{})
	t.Cleanup(engine.Close)
	const sessionID = "snapshot-retry"
	session := db.Session{ID: sessionID, Agent: "claude", Project: "project", Machine: "local", MessageCount: 1}
	_, err := database.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{
		Session: session, ReplaceMessages: true,
		Messages: []db.Message{{SessionID: sessionID, Ordinal: 0, Role: "assistant", Content: "initial"}},
	}}, nil)
	require.NoError(t, err)
	h := newSchedulerHarness(10*time.Second, 2*time.Second)
	defer h.sched.stop()
	runs, conflicts := 0, 0
	h.sched.run = func(_ context.Context, id string) {
		runs++
		_, err := engine.recomputeSignalsFromDBWithHook(t.Context(), id, func(int) {
			if runs != 1 {
				return
			}
			conflicts++
			// Session uploads use this same database boundary without the engine lock.
			_, writeErr := database.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{
				Session: session, ReplaceMessages: true,
				Messages: []db.Message{{SessionID: sessionID, Ordinal: 0, Role: "assistant", Content: fmt.Sprintf("replacement %d", conflicts), IsCompactBoundary: conflicts == 3}},
			}}, nil)
			require.NoError(t, writeErr)
		})
		if err != nil {
			h.sched.deferRetry(id)
		}
	}
	h.sched.markDirty(t.Context(), sessionID)
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
	h.sched.run = func(_ context.Context, id string) {
		runs++
		h.sched.deferRetry(id)
	}
	h.sched.markDirty(t.Context(), "session")
	require.Equal(t, 1, runs)
	require.Equal(t, 1, h.armedCount())
	h.sched.stop()
	require.Equal(t, 2, runs, "shutdown makes one final attempt")
	require.Zero(t, h.armedCount())
	h.sched.flushAll()
	require.Equal(t, 2, runs, "failed shutdown work must not remain queued")
}

// A push can be canceled while it flushes deferred signal recomputes. The
// flush must stop, and the recompute must stay queued for a later flush.
func TestCanceledPushFlushKeepsSignalRecomputeQueued(t *testing.T) {
	fx := newEngineFixture(t)
	path := fx.writeClaudeSession(t, "proj", "sig-flush-cancel.jsonl", "hello")
	fx.engine.SyncAll(t.Context(), nil)
	sid := fx.sessionIDFor(t, path)

	fx.appendClaudeMessage(t, path, "key AKIA7QHWN2DKR4FYPLJM leaked")
	fx.engine.SyncPaths([]string{path})
	require.Equal(t, 1, secretLeakCount(t, fx, sid))
	fx.appendClaudeMessage(t, path, "key AKIA9XKQV3ZTN8WMB2RC leaked")
	fx.engine.SyncPaths([]string{path})
	require.Equal(t, 1, secretLeakCount(t, fx, sid),
		"second write within interval should defer the recompute")

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	seen := -1
	require.NoError(t, fx.engine.RunExclusiveFlushed(canceled, func() error {
		seen = secretLeakCount(t, fx, sid)
		return nil
	}))
	assert.Equal(t, 1, seen, "a canceled push must stop the flush")

	fx.engine.FlushSignals()
	assert.Equal(t, 2, secretLeakCount(t, fx, sid),
		"the canceled recompute must stay queued for the next flush")
}
