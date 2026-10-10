package server

import (
	"archive/zip"
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/remotesync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestArchiveOnlyRejectsManualSync(t *testing.T) {
	f := newSyncRouteFixture(t)
	require.NoError(t, f.db.EnableArchiveOnly(t.Context()))
	for _, path := range []string{"/api/v1/sync", "/api/v1/sync/remotes", "/api/v1/sessions/sync", "/api/v1/push/pg", "/api/v1/push/duckdb"} {
		t.Run(path, func(t *testing.T) {
			var body any = map[string]string{"id": "archived"}
			if path == "/api/v1/sync/remotes" {
				body = remoteSyncRequest{}
			}
			if path == "/api/v1/push/pg" || path == "/api/v1/push/duckdb" {
				body = daemonPushRequest{}
			}
			response := serveJSON(t, f.handler, http.MethodPost, path, body)
			assert.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), "archive-only")
		})
	}
	f.srv.cfg.AuthToken = "test-token"
	transferServer := New(f.srv.cfg, f.db, nil, WithArtifactExchangeRunner(func(context.Context, ArtifactExchangeRequest) (artifact.SyncResult, error) {
		panic("archive-only exchange must not run")
	}))
	// Source-transfer requests must fail before resolving roots or touching a target.
	handler := transferServer.Handler()
	for _, path := range []string{"/api/v1/remote-sync/manifest", "/api/v1/remote-sync/archive", "/api/v1/artifacts/exchange"} {
		response := serveJSON(t, handler, http.MethodPost, path, map[string]any{}, func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer test-token")
			remotesync.SetProtocolHeader(r.Header)
		})
		assert.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
		assert.Contains(t, response.Body.String(), "archive-only")
	}

	// Read commands wait for startup through this endpoint. An archive-only
	// daemon has no startup ingestion to wait for.
	response := serveJSON(t, f.handler, http.MethodPost, "/api/v1/sync?startup_only=true", nil)
	assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
}

// chatGPTImageExportZip packs a ChatGPT export whose message references an
// image, so a successful import writes an asset.
func chatGPTImageExportZip(t *testing.T) []byte {
	t.Helper()
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	for name, data := range testjsonl.ChatGPTImageExport() {
		entry, err := zw.Create(name)
		require.NoError(t, err)
		_, err = entry.Write([]byte(data))
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	return archive.Bytes()
}

func TestArchiveOnlyRejectsOrdinaryImports(t *testing.T) {
	claudeAI := []byte(`[{"uuid":"archive-import","name":"Import","summary":"",` +
		`"created_at":"2026-03-01T10:00:00.000000Z","updated_at":"2026-03-01T10:05:00.000000Z",` +
		`"account":{"uuid":"acct-1"},"chat_messages":[{"uuid":"m1","text":"Imported",` +
		`"content":[{"type":"text","text":"Imported"}],"sender":"human",` +
		`"created_at":"2026-03-01T10:00:00.000000Z","updated_at":"2026-03-01T10:00:00.000000Z",` +
		`"attachments":[],"files":[]}]}]`)
	chatGPT := chatGPTImageExportZip(t)
	for _, tt := range []struct {
		name, path, filename string
		data                 []byte
		stream               bool
	}{
		{name: "claude-ai", path: "/api/v1/import/claude-ai", filename: "conversations.json", data: claudeAI},
		{name: "claude-ai-stream", path: "/api/v1/import/claude-ai", filename: "conversations.json", data: claudeAI, stream: true},
		{name: "chatgpt", path: "/api/v1/import/chatgpt", filename: "export.zip", data: chatGPT},
		{name: "chatgpt-stream", path: "/api/v1/import/chatgpt", filename: "export.zip", data: chatGPT, stream: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// The same upload imports into an ordinary archive.
			ordinary := testServer(t, 5*time.Second)
			stats := postImport(t, ordinary, tt.path, tt.filename, tt.data, tt.stream)
			require.Equal(t, 1, stats.Imported)

			srv := testServer(t, 5*time.Second)
			local, ok := srv.db.(*db.DB)
			require.True(t, ok)
			first := "preserved history"
			require.NoError(t, local.UpsertSession(t.Context(), db.Session{
				ID: "preserved", Agent: "claude", Project: "project-a", Machine: "retired", FirstMessage: &first,
			}))
			require.NoError(t, local.EnableArchiveOnly(t.Context()))
			assetsDir := filepath.Join(srv.cfg.DataDir, "assets")
			require.NoError(t, os.MkdirAll(assetsDir, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(assetsDir, "existing.png"), []byte("existing"), 0o600))

			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			part, err := writer.CreateFormFile("file", tt.filename)
			require.NoError(t, err)
			_, err = part.Write(tt.data)
			require.NoError(t, err)
			require.NoError(t, writer.Close())
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, tt.path, &body)
			req.Header.Set("Content-Type", writer.FormDataContentType())
			if tt.stream {
				req.Header.Set("Accept", "text/event-stream")
			}
			rec := httptest.NewRecorder()
			srv.mux.ServeHTTP(rec, req)

			assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "archive-only")
			assert.NotContains(t, rec.Body.String(), "event:",
				"the rejection must not open an SSE stream")
			var ids []string
			rows, err := local.Reader().Query(t.Context(), "SELECT id FROM sessions ORDER BY id")
			require.NoError(t, err)
			defer rows.Close()
			for rows.Next() {
				var id string
				require.NoError(t, rows.Scan(&id))
				ids = append(ids, id)
			}
			require.NoError(t, rows.Err())
			assert.Equal(t, []string{"preserved"}, ids, "a preserved archive does not gain imported conversations")
			preserved, err := local.GetSessionFull(t.Context(), "preserved")
			require.NoError(t, err)
			require.NotNil(t, preserved)
			require.NotNil(t, preserved.FirstMessage)
			assert.Equal(t, first, *preserved.FirstMessage)
			entries, err := os.ReadDir(assetsDir)
			require.NoError(t, err)
			require.Len(t, entries, 1, "a preserved archive does not gain imported assets")
			data, err := os.ReadFile(filepath.Join(assetsDir, "existing.png"))
			require.NoError(t, err)
			assert.Equal(t, "existing", string(data))
		})
	}
}
