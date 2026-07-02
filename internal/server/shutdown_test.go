package server

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
)

// TestShutdownClosesOnDemandEngine guards the lifecycle of the
// server-owned lazily-created sync engine: Shutdown must close it so
// pending debounced signal recomputes flush before the owner closes
// the DB. Injected engines are closed by their owner, not here.
func TestShutdownClosesOnDemandEngine(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	srv := New(config.Config{
		Host:         "127.0.0.1",
		Port:         0,
		WriteTimeout: 30 * time.Second,
	}, database, nil)

	require.NotNil(t, srv.syncEngineForLocal(database),
		"on-demand engine should be created lazily")

	require.NoError(t, srv.Shutdown(context.Background()))

	srv.mu.RLock()
	defer srv.mu.RUnlock()
	assert.Nil(t, srv.onDemandEngine,
		"shutdown must close and release the on-demand engine")
}
