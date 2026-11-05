package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	agentsync "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestDaemonIngestionWakeIncludesRemoteHistory(t *testing.T) {
	content := testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-02T00:00:00Z", "overnight remote message", "remote-overnight").String()
	var published atomic.Bool
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	database := dbtest.OpenTestDB(t)
	engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{AgentDirs: cfg.AgentDirs, Machine: "test-machine"})
	t.Cleanup(engine.Close)
	engine.SyncAll(t.Context(), nil)
	ingestion := newDaemonIngestion(t.Context(), cfg, engine, database, nil, nil)
	t.Cleanup(ingestion.Stop)
	ingestion.Suspend()
	ingestion.applying.Wait()
	published.Store(true)
	ts := serveIngestionArchive(t, cfg, database, engine, ingestion)
	headers, body := searchIngestionArchive(t, ts.URL, "overnight")
	// Idle catch-up reads remote roots, so the reader gets the overnight message
	// without a stale warning.
	require.Contains(t, body, "overnight remote message")
	assert.Empty(t, headers.Get(apiclient.ArchiveStaleHeader))
}
