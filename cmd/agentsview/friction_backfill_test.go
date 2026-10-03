package main

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/sync"
)

func TestRecomputeStaleFrictionRunsBackfill(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	dbtest.SeedSession(t, database, "s1", "proj")
	dbtest.SeedMessages(t, database,
		dbtest.UserMsg("s1", 0, "please fix the failing build"),
		dbtest.AsstMsg("s1", 1, "I'll hardcode it for now."),
	)
	engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{Machine: "local"})
	t.Cleanup(engine.Close)

	recomputeStaleFriction(t.Context(), engine)

	s, err := database.GetSessionFull(t.Context(), "s1")
	require.NoError(t, err)
	assert.Equal(t, friction.RulesVersion, s.FrictionRulesVersion)
	stale, err := database.StaleFrictionSessions(t.Context(), friction.RulesVersion, 10)
	require.NoError(t, err)
	assert.Empty(t, stale)
}

func TestPendingSignalsRespectsFrictionBudget(t *testing.T) {
	for _, mode := range []string{"within-budget", "rows", "bytes"} {
		t.Run(mode, func(t *testing.T) {
			store := dbtest.OpenTestDB(t)
			const id = "pending-session"
			ended := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
			n := 3
			if mode == "rows" {
				n = 2001
			}
			require.NoError(t, store.UpsertSession(t.Context(), db.Session{
				ID: id, Agent: "claude", Project: "project-a", Machine: "local", EndedAt: &ended,
				MessageCount: n, UserMessageCount: 1,
			}))
			msgs := make([]db.Message, n)
			for i := range msgs {
				msgs[i] = db.Message{SessionID: id, Ordinal: i, Role: "assistant", Content: "working", Timestamp: ended}
			}
			msgs[0].Role, msgs[0].Content = "user", "finish the task"
			msgs[n-1].Content = "I'll come back to this later."
			if mode == "bytes" {
				msgs[1].Content = strings.Repeat("x", (4<<20)+1)
			}
			require.NoError(t, store.ReplaceSessionMessages(t.Context(), id, msgs))
			require.NoError(t, store.UpdateSessionSignals(t.Context(), id, db.SessionSignalUpdate{
				QualitySignals:      db.QualitySignals{Version: db.CurrentQualitySignalVersion},
				SignalsPendingSince: &ended,
				Friction:            &db.SessionFrictionUpdate{RulesVersion: "older-rules"},
			}))
			engine := sync.NewEngine(t.Context(), store, sync.EngineConfig{})
			t.Cleanup(engine.Close)
			loads := store.MessagesLoadCount()

			recomputePendingSessions(engine, store)

			stored, err := store.GetSessionFull(t.Context(), id)
			require.NoError(t, err)
			require.NotNil(t, stored)
			assert.Nil(t, stored.SignalsPendingSince, "ordinary signal recomputation still completes")
			assert.Equal(t, loads+1, store.MessagesLoadCount(), "friction must reuse the signal history load")
			findings, err := store.SessionFrictionFindings(t.Context(), id)
			require.NoError(t, err)
			if mode == "within-budget" {
				assert.Equal(t, friction.RulesVersion, stored.FrictionRulesVersion)
				require.Len(t, findings, 1)
				assert.Equal(t, "deferral", findings[0].Kind)
			} else {
				assert.Equal(t, "older-rules", stored.FrictionRulesVersion, "oversized friction stays pending")
				assert.Empty(t, findings)
			}
		})
	}
}
