package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/importer"
)

func TestHandleImportClaudeAI(t *testing.T) {
	srv := testServer(t, 5*time.Second)

	conversations := `[
      {
        "uuid": "api-test-001",
        "name": "API Test",
        "summary": "",
        "created_at": "2026-03-01T10:00:00.000000Z",
        "updated_at": "2026-03-01T10:05:00.000000Z",
        "account": {"uuid": "acct-1"},
        "chat_messages": [
          {
            "uuid": "m1",
            "text": "Test message",
            "content": [{"type":"text","text":"Test message"}],
            "sender": "human",
            "created_at": "2026-03-01T10:00:00.000000Z",
            "updated_at": "2026-03-01T10:00:00.000000Z",
            "attachments": [],
            "files": []
          }
        ]
      }
    ]`

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "conversations.json")
	require.NoError(t, err)
	_, _ = part.Write([]byte(conversations))
	writer.Close()

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/import/claude-ai",
		&body,
	)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var stats importer.ImportStats
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&stats))
	assert.Equal(t, 1, stats.Imported)
	assert.Zero(t, stats.Updated)
}

// TestHandleImportRejectsWriterClosedBeforeStream pins the maintenance-mode
// UX: while a worker pass holds the write barrier, import requests fail before
// the stream body opens with the transient 503 + Retry-After instead of a
// misleading 500 or an HTTP-200 SSE error event.
func TestHandleImportRejectsWriterClosedBeforeStream(t *testing.T) {
	for _, tt := range []struct {
		name     string
		path     string
		filename string
	}{
		{name: "claude-ai", path: "/api/v1/import/claude-ai", filename: "conversations.json"},
		{name: "chatgpt", path: "/api/v1/import/chatgpt", filename: "export.zip"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := testServer(t, 5*time.Second)
			local, ok := srv.db.(*db.DB)
			require.True(t, ok)
			require.NoError(t, local.CloseWriter())
			t.Cleanup(func() { require.NoError(t, local.ReopenWriter()) })

			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			part, err := writer.CreateFormFile("file", tt.filename)
			require.NoError(t, err)
			_, _ = part.Write([]byte("[]"))
			writer.Close()

			req := httptest.NewRequest(http.MethodPost, tt.path, &body)
			req.Header.Set("Content-Type", writer.FormDataContentType())
			req.Header.Set("Accept", "text/event-stream")
			rec := httptest.NewRecorder()
			srv.mux.ServeHTTP(rec, req)

			require.Equal(t, http.StatusServiceUnavailable, rec.Code,
				"body: %s", rec.Body.String())
			assert.Equal(t, writerClosedRetryAfterSeconds,
				rec.Header().Get("Retry-After"))
			assert.NotContains(t, rec.Body.String(), "event:",
				"the rejection must not open an SSE stream")
		})
	}
}

func TestHandleImportChatGPT_RequiresZip(t *testing.T) {
	srv := testServer(t, 5*time.Second)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "data.json")
	require.NoError(t, err)
	_, _ = part.Write([]byte("[]"))
	writer.Close()

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/import/chatgpt",
		&body,
	)
	req.Header.Set(
		"Content-Type", writer.FormDataContentType(),
	)

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
}

func TestHandleImportClaudeAI_SSE(t *testing.T) {
	srv := testServer(t, 5*time.Second)

	conversations := `[{
      "uuid": "sse-test-001",
      "name": "SSE Test",
      "created_at": "2026-03-01T10:00:00.000000Z",
      "updated_at": "2026-03-01T10:05:00.000000Z",
      "chat_messages": [{
        "uuid": "m1", "text": "hello", "sender": "human",
        "content": [{"type":"text","text":"hello"}],
        "created_at": "2026-03-01T10:00:00.000000Z"
      }]
    }]`

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile(
		"file", "conversations.json",
	)
	require.NoError(t, err)
	_, _ = part.Write([]byte(conversations))
	writer.Close()

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/import/claude-ai",
		&body,
	)
	req.Header.Set(
		"Content-Type", writer.FormDataContentType(),
	)
	req.Header.Set("Accept", "text/event-stream")

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	require.Contains(t, rec.Header().Get("Content-Type"), "text/event-stream")

	// Parse the done event from the SSE body.
	var stats importer.ImportStats
	lines := strings.Split(rec.Body.String(), "\n")
	for i, line := range lines {
		if line == "event: done" && i+1 < len(lines) {
			data := strings.TrimPrefix(
				lines[i+1], "data: ",
			)
			require.NoError(t, json.Unmarshal([]byte(data), &stats))
		}
	}
	assert.Equal(t, 1, stats.Imported)
}

func TestHandleImportClaudeAI_NoFile(t *testing.T) {
	srv := testServer(t, 5*time.Second)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	writer.Close()

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/import/claude-ai",
		&body,
	)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
}
