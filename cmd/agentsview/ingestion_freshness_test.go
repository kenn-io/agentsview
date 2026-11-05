package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/server"
	agentsync "go.kenn.io/agentsview/internal/sync"
)

func TestDaemonIngestionFailedCatchupKeepsWarning(t *testing.T) {
	for _, source := range []parser.AgentType{parser.AgentGemini, parser.AgentClaude, parser.AgentCodex, parser.AgentCursor, parser.AgentIcodemate} {
		t.Run(string(source), func(t *testing.T) {
			if source == parser.AgentGemini && (runtime.GOOS == "windows" || os.Geteuid() == 0) {
				t.Skip("fixture needs enforced POSIX file permissions")
			}
			database := dbtest.OpenTestDB(t)
			root := t.TempDir()
			cfg := config.Config{Host: "127.0.0.1", AgentDirs: map[parser.AgentType][]string{parser.AgentGemini: {root}}}
			var denied atomic.Bool
			if source != parser.AgentGemini {
				remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					if denied.Load() {
						w.WriteHeader(http.StatusForbidden)
						_, _ = w.Write([]byte("<Error><Code>AccessDenied</Code></Error>"))
						return
					}
					_, _ = w.Write([]byte("<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>"))
				}))
				t.Cleanup(remote.Close)
				t.Setenv("AWS_S3_ENDPOINT", remote.URL)
				t.Setenv("AWS_ACCESS_KEY_ID", "test-key")
				t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
				t.Setenv("AWS_REGION", "us-east-1")
				cfg.AgentDirs[source] = []string{"s3://bucket/sessions"}
			}
			writeReloadGeminiSession(t, root, "before")
			engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{AgentDirs: cfg.AgentDirs, Machine: "test-machine"})
			t.Cleanup(engine.Close)
			engine.SyncAll(t.Context(), nil)
			ingestion := newDaemonIngestion(t.Context(), cfg, engine, database, server.NewIdleTracker(0, func() {}), nil)
			t.Cleanup(ingestion.Stop)
			ingestion.Suspend()
			ingestion.applying.Wait()
			if source == parser.AgentGemini {
				writeReloadGeminiSession(t, root, "overnight")
				path := filepath.Join(root, "tmp", "project", "chats", "session-overnight.json")
				require.NoError(t, os.Chmod(path, 0))
				t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
				_, err := os.ReadFile(path)
				require.Error(t, err, "fixture must cause an actual source-read failure")
			} else {
				denied.Store(true)
			}
			ts := serveIngestionArchive(t, cfg, database, engine, ingestion)
			headers, body := searchIngestionArchive(t, ts.URL, "overnight")
			assert.NotContains(t, body, "hello from overnight")
			assert.Equal(t, "true", headers.Get(apiclient.ArchiveStaleHeader), "failed source catch-up must warn the reader about missing overnight messages")
			assert.True(t, ingestion.Pending())
		})
	}
}
