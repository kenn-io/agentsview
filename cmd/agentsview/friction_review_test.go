package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/sync"
)

func TestFrictionReviewerReviewsStaleSessionsAfterNotify(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	dbtest.SeedSession(t, database, "s1", "proj")
	dbtest.SeedMessages(t, database,
		dbtest.UserMsg("s1", 0, "please fix the failing build"),
		dbtest.AsstMsg("s1", 1, "I'll hardcode it for now."),
	)
	engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{
		Machine: "local", FrictionEnabled: true,
	})
	t.Cleanup(engine.Close)
	reviewer := newFrictionReviewer()
	reviewer.start(t.Context(), engine, nil)
	t.Cleanup(reviewer.stop)

	reviewer.Notify()

	require.Eventually(t, func() bool {
		s, err := database.GetSessionFull(t.Context(), "s1")
		return err == nil && s.FrictionRulesVersion == friction.RulesVersion
	}, 10*time.Second, 10*time.Millisecond)
	findings, err := database.SessionFrictionFindings(t.Context(), "s1")
	require.NoError(t, err)
	require.Len(t, findings, 1)
	assert.Equal(t, "workaround", findings[0].Kind)
}
