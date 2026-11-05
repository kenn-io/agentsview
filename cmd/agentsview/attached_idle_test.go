package main

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/server"
	agentsync "go.kenn.io/agentsview/internal/sync"
)

func TestDaemonIngestionWakeIncludesPendingAttachedHistory(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(fmt.Sprintf("unavailable=%v", unavailable), func(t *testing.T) {
			cfg := config.Config{Host: "127.0.0.1", DataDir: t.TempDir(), ArchiveSources: []config.ArchiveSource{archiveSourceCommandFixture(t)}, AgentDirs: map[parser.AgentType][]string{parser.AgentGemini: {t.TempDir()}}}
			database := dbtest.OpenTestDB(t)
			engine := agentsync.NewEngine(t.Context(), database, localSyncEngineConfig(cfg))
			t.Cleanup(engine.Close)
			ingestion := newDaemonIngestion(t.Context(), cfg, engine, database, server.NewIdleTracker(0, func() {}), nil)
			t.Cleanup(ingestion.Stop)
			ingestion.OpenWatcherDispatch()
			attached, err := ingestion.attachSources(cfg)
			require.NoError(t, err)
			t.Cleanup(attached.Close)
			// Pending history has not reached its first scheduled import when serve idles.
			ingestion.Suspend()
			ingestion.applying.Wait()
			if unavailable {
				require.NoError(t, os.Rename(cfg.ArchiveSources[0].Repository, cfg.ArchiveSources[0].Repository+"-offline"))
			}
			fresh := ingestion.EnsureFresh(t.Context())
			assert.Equal(t, !unavailable, fresh)
			assert.Equal(t, unavailable, ingestion.Pending())
			messages, err := database.GetAllMessages(t.Context(), attachedPushSession)
			require.NoError(t, err)
			if unavailable {
				assert.Empty(t, messages)
			} else {
				require.Len(t, messages, 1)
				assert.Equal(t, "archived message", messages[0].Content)
			}
		})
	}
}
