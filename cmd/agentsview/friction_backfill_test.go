package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
