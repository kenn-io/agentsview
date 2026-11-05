package main

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	agentsync "go.kenn.io/agentsview/internal/sync"
)

func TestDaemonIngestionWakeIncludesPendingAttachedHistory(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(fmt.Sprintf("unavailable=%v", unavailable), func(t *testing.T) {
			cfg := config.Config{Host: "127.0.0.1", DataDir: t.TempDir(), ArchiveSources: []config.ArchiveSource{archiveSourceCommandFixture(t)}, AgentDirs: map[parser.AgentType][]string{parser.AgentGemini: {t.TempDir()}}}
			database := dbtest.OpenTestDB(t)
			engine := agentsync.NewEngine(t.Context(), database, localSyncEngineConfig(cfg))
			t.Cleanup(engine.Close)
			ingestion := newDaemonIngestion(t.Context(), cfg, engine, database, nil, nil)
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
			ts := serveIngestionArchive(t, cfg, database, engine, ingestion)
			headers, body := searchIngestionArchive(t, ts.URL, "archived")
			if unavailable {
				assert.NotContains(t, body, "archived message")
				assert.Equal(t, "true", headers.Get(apiclient.ArchiveStaleHeader), "unfinished attachment imports must warn")
				assert.True(t, ingestion.Pending())
				require.NoError(t, os.Rename(cfg.ArchiveSources[0].Repository+"-offline", cfg.ArchiveSources[0].Repository))
			} else {
				assert.Contains(t, body, "archived message")
				assert.Empty(t, headers.Get(apiclient.ArchiveStaleHeader))
			}
			// Verify the attachment is valid through the existing scheduled source path.
			_, err = attached.DrainScheduled(t.Context())
			require.NoError(t, err)
			_, body = searchIngestionArchive(t, ts.URL, "archived")
			require.Contains(t, body, "archived message")
		})
	}
}
