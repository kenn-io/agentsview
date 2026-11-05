package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/archivesource"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/server"
	agentsync "go.kenn.io/agentsview/internal/sync"
)

func TestIdleRecoveryImportsAvailableHistory(t *testing.T) {
	for _, stage := range []string{"remote", "local", "attached"} {
		for _, mode := range []string{"wake", "scheduled"} {
			t.Run(stage+"/"+mode, func(t *testing.T) {
				local := t.TempDir()
				cfg := config.Config{Host: "127.0.0.1", DataDir: t.TempDir(), ArchiveSources: []config.ArchiveSource{archiveSourceCommandFixture(t)}, AgentDirs: map[parser.AgentType][]string{parser.AgentGemini: {local}}}
				var denied atomic.Bool
				switch stage {
				case "remote":
					denied.Store(true)
					remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if denied.Load() {
							w.WriteHeader(http.StatusForbidden)
							_, _ = io.WriteString(w, "<Error><Code>AccessDenied</Code></Error>")
							return
						}
						_, _ = io.WriteString(w, `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>bucket</Name><IsTruncated>false</IsTruncated></ListBucketResult>`)
					}))
					t.Cleanup(remote.Close)
					t.Setenv("AWS_S3_ENDPOINT", remote.URL)
					t.Setenv("AWS_ACCESS_KEY_ID", "test-key")
					t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
					t.Setenv("AWS_REGION", "us-east-1")
					cfg.AgentDirs[parser.AgentClaude] = []string{"s3://bucket/sessions"}
				case "local":
					writeReloadGeminiSession(t, local, "repair")
					require.NoError(t, os.WriteFile(filepath.Join(local, "tmp", "project", "chats", "session-repair.json"), []byte("{broken json"), 0o600))
				}
				database := dbtest.OpenTestDB(t)
				engineCfg := localSyncEngineConfig(cfg)
				var calls atomic.Int32
				if stage == "attached" {
					writeReloadGeminiSession(t, local, "local")
					for _, factory := range parser.ProviderFactories() {
						if factory.Definition().Type == parser.AgentGemini {
							engineCfg.ProviderFactories = []parser.ProviderFactory{countingIngestionFactory{factory, &calls}}
						}
					}
				}
				engine := agentsync.NewEngine(t.Context(), database, engineCfg)
				t.Cleanup(engine.Close)
				ingestion := newDaemonIngestion(t.Context(), cfg, engine, database, server.NewIdleTracker(0, func() {}), nil)
				t.Cleanup(ingestion.Stop)
				ingestion.OpenWatcherDispatch()
				attached, err := archivesource.NewManager(t.Context(), cfg, database, engine, ingestion.FlushPending)
				require.NoError(t, err)
				ingestion.attached = attached
				t.Cleanup(attached.Close)
				ingestion.Suspend()
				ingestion.applying.Wait()
				if stage == "attached" {
					require.NoError(t, os.Rename(cfg.ArchiveSources[0].Repository, cfg.ArchiveSources[0].Repository+"-offline"))
				}
				if mode == "wake" {
					require.False(t, ingestion.EnsureFresh(t.Context()))
				} else {
					require.Error(t, ingestion.RunSync(t.Context(), func(context.Context) error { return errors.New("requested pass failed") }))
					require.Error(t, ingestion.syncScheduled(t.Context()))
				}
				require.True(t, ingestion.Pending())
				messages, err := database.GetAllMessages(t.Context(), attachedPushSession)
				require.NoError(t, err)
				if stage == "attached" {
					assert.Empty(t, messages)
					firstPass := calls.Load()
					assert.Positive(t, firstPass)
					for range 5 {
						assert.False(t, ingestion.EnsureFresh(t.Context()))
					}
					assert.Equal(t, firstPass, calls.Load(), "reads during a source failure must not repeat discovery")
					require.NoError(t, os.Rename(cfg.ArchiveSources[0].Repository+"-offline", cfg.ArchiveSources[0].Repository))
				} else {
					require.Len(t, messages, 1)
					assert.Equal(t, "archived message", messages[0].Content)
				}
				if stage == "remote" {
					denied.Store(false)
				}
				if stage == "local" {
					require.False(t, ingestion.EnsureFresh(t.Context()))
					// The unchanged malformed file becomes a cached terminal
					// parse failure on the next scheduled pass.
					require.NoError(t, ingestion.syncScheduled(t.Context()))
					require.True(t, ingestion.EnsureFresh(t.Context()))
					require.False(t, sessionImported(t, database, "gemini:repair"))
					ingestion.Suspend()
					ingestion.applying.Wait()
					writeReloadGeminiSession(t, local, "repair")
				}
				require.NoError(t, ingestion.syncScheduled(t.Context()))
				require.False(t, ingestion.Pending())
				if stage == "attached" {
					messages, err = database.GetAllMessages(t.Context(), attachedPushSession)
					require.NoError(t, err)
					require.Len(t, messages, 1)
					assert.Equal(t, "archived message", messages[0].Content)
				}
				if stage == "local" {
					messages, err = database.GetAllMessages(t.Context(), "gemini:repair")
					require.NoError(t, err)
					require.Len(t, messages, 1)
					assert.Equal(t, "hello from repair", messages[0].Content)
				}
			})
		}
	}
}
