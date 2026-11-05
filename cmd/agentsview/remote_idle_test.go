package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
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

func TestDaemonIngestionWakeIncludesRemoteHistory(t *testing.T) {
	for _, forbidden := range []bool{false, true} {
		t.Run(fmt.Sprintf("remote_forbidden=%v", forbidden), func(t *testing.T) {
			content := testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-02T00:00:00Z", "overnight remote message", "remote-overnight").String()
			var published atomic.Bool
			var denied atomic.Bool
			var requests atomic.Int32
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if denied.Load() {
					w.WriteHeader(http.StatusForbidden)
					_, _ = io.WriteString(w, "<Error><Code>AccessDenied</Code></Error>")
					return
				}
				if r.URL.Path == "/bucket" || r.URL.Path == "/bucket/" {
					w.Header().Set("Content-Type", "application/xml")
					contents := ""
					if published.Load() {
						contents = fmt.Sprintf("<Contents><Key>sessions/project/overnight.jsonl</Key><LastModified>2026-01-02T00:00:00.000Z</LastModified><ETag>&quot;abc123&quot;</ETag><Size>%d</Size><StorageClass>STANDARD</StorageClass></Contents>", len(content))
					}
					_, _ = fmt.Fprintf(w, `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>bucket</Name><Prefix>sessions/</Prefix><KeyCount>1</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated>%s</ListBucketResult>`, contents)
					return
				}
				if strings.HasSuffix(r.URL.Path, "overnight.jsonl") {
					w.Header().Set("Last-Modified", "Fri, 02 Jan 2026 00:00:00 GMT")
					w.Header().Set("ETag", `"abc123"`)
					w.Header().Set("Content-Length", strconv.Itoa(len(content)))
					_, _ = io.WriteString(w, content)
					return
				}
				http.NotFound(w, r)
			}))
			t.Cleanup(remote.Close)
			t.Setenv("AWS_S3_ENDPOINT", remote.URL)
			t.Setenv("AWS_ACCESS_KEY_ID", "test-key")
			t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
			t.Setenv("AWS_REGION", "us-east-1")
			cfg := config.Config{Host: "127.0.0.1", AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {"s3://bucket/sessions"}}}
			local := t.TempDir()
			if forbidden {
				cfg.AgentDirs[parser.AgentClaude] = append(cfg.AgentDirs[parser.AgentClaude], local)
			}
			database := dbtest.OpenTestDB(t)
			engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{AgentDirs: cfg.AgentDirs, Machine: "test-machine"})
			t.Cleanup(engine.Close)
			published.Store(forbidden)
			engine.SyncAll(t.Context(), nil)
			ingestion := newDaemonIngestion(t.Context(), cfg, engine, database, server.NewIdleTracker(0, func() {}), nil)
			t.Cleanup(ingestion.Stop)
			ingestion.Suspend()
			ingestion.applying.Wait()
			published.Store(true)
			denied.Store(forbidden)
			requests.Store(0)
			if forbidden {
				dbtest.WriteTestFile(t, filepath.Join(local, "project", "morning.jsonl"), []byte(testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-02T00:00:00Z", "hello from morning", "morning").String()))
			}
			assert.Equal(t, !forbidden, ingestion.EnsureFresh(t.Context()))
			assert.Equal(t, forbidden, ingestion.Pending())
			if forbidden {
				archived, err := database.GetAllMessages(t.Context(), "overnight")
				require.NoError(t, err)
				require.Len(t, archived, 1, "history under the denied scope stays archived")
				endpoint := serveIngestionArchive(t, cfg, database, engine, ingestion)
				headers, _ := searchIngestionArchive(t, endpoint.URL, "overnight")
				assert.Equal(t, "true", headers.Get("X-Agentsview-Archive-Stale"))
			}
			assert.Positive(t, requests.Load(), "wake attempts remote imports")
			id, message := "overnight", "overnight remote message"
			if forbidden {
				id, message = "morning", "hello from morning"
			}
			messages, err := database.GetAllMessages(t.Context(), id)
			require.NoError(t, err)
			require.Len(t, messages, 1)
			assert.Equal(t, message, messages[0].Content)
			if forbidden {
				denied.Store(false)
				require.NoError(t, ingestion.syncScheduled(t.Context()))
				assert.False(t, ingestion.Pending())
				messages, err = database.GetAllMessages(t.Context(), "overnight")
				require.NoError(t, err)
				require.Len(t, messages, 1)
				assert.Equal(t, "overnight remote message", messages[0].Content)
			}
		})
	}
}
