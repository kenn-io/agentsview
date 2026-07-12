// ABOUTME: Tests the bounded recovery path for abandoned daemon-launch syncs.
// ABOUTME: Uses a scratch database and synthetic timeout without starting a daemon.
package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

func TestRunDeferredStartupSyncFallbackPerformsSkippedSync(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	engine := syncpkg.NewEngine(database, syncpkg.EngineConfig{
		Machine:                 "local",
		DeferStartupMaintenance: true,
	})
	t.Cleanup(engine.Close)
	timeout := make(chan time.Time, 1)
	timeout <- time.Now()

	ran, err := runDeferredStartupSyncFallback(
		t.Context(), engine, nil, timeout,
	)

	require.NoError(t, err)
	assert.True(t, ran)
	assert.False(t, engine.LastSyncStartedAt().IsZero(),
		"timeout fallback must perform the skipped local sync")
}
