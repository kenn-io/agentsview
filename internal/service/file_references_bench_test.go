package service_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/integrations/docbank"
	"go.kenn.io/agentsview/internal/service"
)

func BenchmarkFileReferences20000Messages(b *testing.B) {
	d, err := db.Open(b.Context(), filepath.Join(b.TempDir(), "archive.db"))
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, d.Close()) })
	const id = "long-session"
	require.NoError(b, d.UpsertSession(b.Context(), db.Session{ID: id, Project: "bench", Agent: "opencode", MessageCount: 20_000}))
	messages := make([]db.Message, 20_000)
	ordinary := strings.Repeat("package example\nfunc Example() {}\n", 64)
	for i := range messages {
		messages[i] = db.Message{SessionID: id, Ordinal: i, Role: "assistant", Content: "Read the next source file.", HasToolUse: true, ToolCalls: []db.ToolCall{{ToolUseID: fmt.Sprintf("read-%d", i), ToolName: "Read", Category: "Read", ResultEvents: []db.ToolResultEvent{{EventIndex: 0, Source: "tool_result", Status: "completed", Content: ordinary}}, ResultContent: ordinary}}}
	}
	for i := range 3 {
		messages[i*7_000].ToolCalls[0].ResultEvents[0].Content = fmt.Sprintf(`[{"type":"agentsview_image","version":1,"image_ref":"asset://%064x.png","media_type":"image/png","byte_size":3}]`, i+1)
		messages[i*7_000].ToolCalls[0].ResultContent = messages[i*7_000].ToolCalls[0].ResultEvents[0].Content
	}
	require.NoError(b, d.InsertMessages(b.Context(), messages))
	b.Setenv("DOCBANK_TEST_KEY", "test-key")
	var requests atomic.Int64
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		assert.Contains(b, []string{fmt.Sprintf("%064x", 1), fmt.Sprintf("%064x", 2), fmt.Sprintf("%064x", 3)}, r.URL.Query().Get("sha256"))
		_, _ = w.Write([]byte(`{"items":[],"total":0}`))
	}))
	defer stub.Close()
	svc := service.NewDirectBackend(d, nil, service.WithDocbank(docbank.New(config.DocbankIntegration{URL: stub.URL, APIKeyEnv: "DOCBANK_TEST_KEY"})))
	b.ReportAllocs()
	for b.Loop() {
		out, err := service.FileReferences(b.Context(), svc, id)
		require.NoError(b, err)
		require.Len(b, out.Occurrences, 6)
		require.False(b, out.LookupFailed)
	}
	require.Equal(b, int64(b.N)*3, requests.Load())
}
