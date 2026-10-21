package main

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

func TestFileReferencesMCPDaemonArchive(t *testing.T) {
	dir := runtimeTestDir(t)
	local := dbtest.OpenTestDB(t)
	require.NoError(t, local.UpsertSession(t.Context(), db.Session{ID: "example", Agent: "pi", Project: "local-only"}))
	forbidStartBackgroundServeForTransport(t, "MCP uses the selected daemon archive")
	host, port, _ := newMCPCacheDaemon(t, map[string]http.HandlerFunc{
		"/api/v1/sessions/example/file-references": func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
			assert.Equal(t, "docbank", r.Header.Get(service.FileReferencesIntentHeader))
			assert.Equal(t, "50", r.URL.Query().Get("limit"))
			w.Header().Set("Content-Type", "application/json")
			_, err := w.Write([]byte(`{"binding":"daemon-archive","revision":"daemon-revision","coverage":"complete","capability":"available","integration":"disabled","occurrences":[],"total":0,"next_offset":null}`))
			if !assert.NoError(t, err) {
				return
			}
		},
	})
	writeDaemonRuntimeForTest(t, dir, host, port, "test", false)
	svc := newMCPDaemonService(config.Config{DataDir: dir, DBPath: local.Path(), AuthToken: "test-token"})
	out, err := svc.FileReferences(t.Context(), service.FileReferencesInput{SessionID: "example", Limit: 50})
	require.NoError(t, err)
	assert.Equal(t, "daemon-archive", out.Binding)
	assert.Equal(t, "daemon-revision", out.Revision)
}
