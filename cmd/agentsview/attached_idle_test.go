package main

import (
	"fmt"
	"os"
	"sync/atomic"
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
			writeReloadGeminiSession(t, cfg.AgentDirs[parser.AgentGemini][0], "local")
			database := dbtest.OpenTestDB(t)
			engineCfg := localSyncEngineConfig(cfg)
			var calls atomic.Int32
			for _, factory := range parser.ProviderFactories() {
				if factory.Definition().Type == parser.AgentGemini {
					engineCfg.ProviderFactories = []parser.ProviderFactory{countingIngestionFactory{factory, &calls}}
				}
			}
			engine := agentsync.NewEngine(t.Context(), database, engineCfg)
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
				firstPass := calls.Load()
				assert.Positive(t, firstPass)
				for range 5 {
					assert.False(t, ingestion.EnsureFresh(t.Context()))
				}
				assert.Equal(t, firstPass, calls.Load(), "reads during a source failure must not repeat discovery")
				require.NoError(t, os.Rename(cfg.ArchiveSources[0].Repository+"-offline", cfg.ArchiveSources[0].Repository))
				require.NoError(t, ingestion.syncScheduled(t.Context()))
				assert.False(t, ingestion.Pending())
				messages, err = database.GetAllMessages(t.Context(), attachedPushSession)
				require.NoError(t, err)
				require.Len(t, messages, 1)
				assert.Equal(t, "archived message", messages[0].Content)
			} else {
				require.Len(t, messages, 1)
				assert.Equal(t, "archived message", messages[0].Content)
			}
		})
	}
}
