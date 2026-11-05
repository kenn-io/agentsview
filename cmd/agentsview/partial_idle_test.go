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
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/server"
	agentsync "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestPartialDiscoveryImportsHealthyLocalHistory(t *testing.T) {
	const id = "22222222-2222-4222-8222-222222222222"
	const stamp = "2026-01-02T00:00:00Z"
	for _, agent := range []parser.AgentType{parser.AgentClaude, parser.AgentCodex, parser.AgentCursor, parser.AgentIcodemate} {
		t.Run(string(agent), func(t *testing.T) {
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, "<Error><Code>AccessDenied</Code></Error>")
			}))
			t.Cleanup(remote.Close)
			t.Setenv("AWS_S3_ENDPOINT", remote.URL)
			t.Setenv("AWS_ACCESS_KEY_ID", "test-key")
			t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
			t.Setenv("AWS_REGION", "us-east-1")
			local := t.TempDir()
			relative := "project/" + id + ".jsonl"
			content := testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID(stamp, "healthy local history", id).String()
			storedID := id
			if agent == parser.AgentIcodemate {
				storedID = "icodemate:" + id
			}
			if agent == parser.AgentCodex {
				storedID = "codex:" + id
				relative = "rollout-2026-01-02T00-00-00-" + id + ".jsonl"
				content = testjsonl.CodexSessionMetaJSON(id, "/demo/project", "codex_cli_rs", stamp) + "\n" + testjsonl.CodexMsgJSON("user", "healthy local history", stamp) + "\n"
			}
			if agent == parser.AgentCursor {
				relative = "project/agent-transcripts/" + id + ".jsonl"
				content = "{\"role\":\"user\",\"message\":{\"content\":\"healthy local history\"}}\n"
				storedID = "cursor:" + id
			}
			dbtest.WriteTestFile(t, filepath.Join(local, filepath.FromSlash(relative)), []byte(content))
			database := dbtest.OpenTestDB(t)
			engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{AgentDirs: map[parser.AgentType][]string{agent: {"s3://bucket/sessions", local}}, Machine: "test-machine"})
			t.Cleanup(engine.Close)
			stats := engine.SyncAll(t.Context(), nil)
			assert.False(t, stats.ProcessingComplete())
			messages, err := database.GetAllMessages(t.Context(), storedID)
			require.NoError(t, err)
			require.Len(t, messages, 1)
			assert.Equal(t, "healthy local history", messages[0].Content)
		})
	}
}

func TestIdleRecoveryAttemptsHealthyAttachmentsAfterOtherFailure(t *testing.T) {
	for _, stage := range []string{"remote", "local"} {
		for _, mode := range []string{"wake", "scheduled"} {
			t.Run(stage+"/"+mode, func(t *testing.T) {
				var denied atomic.Bool
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
				local := t.TempDir()
				cfg := config.Config{Host: "127.0.0.1", DataDir: t.TempDir(), ArchiveSources: []config.ArchiveSource{archiveSourceCommandFixture(t)}, AgentDirs: map[parser.AgentType][]string{parser.AgentGemini: {local}}}
				if stage == "remote" {
					cfg.AgentDirs[parser.AgentClaude] = []string{"s3://bucket/sessions"}
				} else {
					writeReloadGeminiSession(t, local, "repair")
					require.NoError(t, os.WriteFile(filepath.Join(local, "tmp", "project", "chats", "session-repair.json"), []byte("{broken json"), 0o600))
				}
				database := dbtest.OpenTestDB(t)
				engine := agentsync.NewEngine(t.Context(), database, localSyncEngineConfig(cfg))
				t.Cleanup(engine.Close)
				ingestion := newDaemonIngestion(t.Context(), cfg, engine, database, server.NewIdleTracker(0, func() {}), nil)
				t.Cleanup(ingestion.Stop)
				ingestion.OpenWatcherDispatch()
				attached, err := ingestion.attachSources(cfg)
				require.NoError(t, err)
				t.Cleanup(attached.Close)
				ingestion.Suspend()
				ingestion.applying.Wait()
				if mode == "wake" {
					require.False(t, ingestion.EnsureFresh(t.Context()))
				} else {
					require.Error(t, ingestion.RunSync(t.Context(), func(context.Context) error { return errors.New("requested pass failed") }))
					require.Error(t, ingestion.syncScheduled(t.Context()))
				}
				require.True(t, ingestion.Pending())
				messages, err := database.GetAllMessages(t.Context(), attachedPushSession)
				require.NoError(t, err)
				require.Len(t, messages, 1)
				assert.Equal(t, "archived message", messages[0].Content)
				endpoint := serveIngestionArchive(t, cfg, database, engine, ingestion)
				headers, body := searchIngestionArchive(t, endpoint.URL, "archived message")
				assert.Equal(t, "true", headers.Get("X-Agentsview-Archive-Stale"))
				assert.Contains(t, body, "archived message")
				denied.Store(false)
				if stage == "local" {
					writeReloadGeminiSession(t, local, "repair")
				}
				require.NoError(t, ingestion.syncScheduled(t.Context()))
				require.False(t, ingestion.Pending())
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
