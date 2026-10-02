package server

import (
	"archive/zip"
	"bytes"
	"encoding/json/v2"
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

	req := httptest.NewRequestWithContext(t.Context(),
		http.MethodPost,
		"/api/v1/import/claude-ai",
		&body,
	)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	raw := rec.Body.Bytes()
	var stats importer.ImportStats
	require.NoError(t, json.Unmarshal(raw, &stats))
	assert.Equal(t, 1, stats.Imported)
	assert.Zero(t, stats.Updated)

	var wire map[string]any
	require.NoError(t, json.Unmarshal(raw, &wire))
	for _, key := range []string{"imported", "updated", "skipped", "errors"} {
		assert.Contains(t, wire, key)
	}
	assert.NotContains(t, wire, "refusals", "a clean import must not send refusals")
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

			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, tt.path, &body)
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

	req := httptest.NewRequestWithContext(t.Context(),
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

	req := httptest.NewRequestWithContext(t.Context(),
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

	req := httptest.NewRequestWithContext(t.Context(),
		http.MethodPost,
		"/api/v1/import/claude-ai",
		&body,
	)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
}

const chatGPTRefusalConv = `[{
  "id":"cg-1","conversation_id":"cg-1","title":"Test",
  "create_time":1706745600.0,"update_time":1706745660.0,
  "current_node":"n1","mapping":{
    "r":{"id":"r","parent":null,"children":["n1"],"message":null},
    "n1":{"id":"n1","parent":"r","children":[],"message":{
      "id":"m1","create_time":1706745600.0,
      "author":{"role":"user","name":null,"metadata":{}},
      "content":{"content_type":"text","parts":["Hello"]},
      "status":"finished_successfully","metadata":{}}}
  }
}]`

const chatGPTRefusalConvWithAppend = `[{
  "id":"cg-1","conversation_id":"cg-1","title":"Test",
  "create_time":1706745600.0,"update_time":1706745660.0,
  "current_node":"n2","mapping":{
    "r":{"id":"r","parent":null,"children":["n1"],"message":null},
    "n1":{"id":"n1","parent":"r","children":["n2"],"message":{
      "id":"m1","create_time":1706745600.0,
      "author":{"role":"user","name":null,"metadata":{}},
      "content":{"content_type":"text","parts":["Hello"]},
      "status":"finished_successfully","metadata":{}}},
    "n2":{"id":"n2","parent":"n1","children":[],"message":{
      "id":"m2","create_time":1706745660.0,
      "author":{"role":"assistant","name":null,"metadata":{}},
      "content":{"content_type":"text","parts":["A newly appended answer with searchable phrase"]},
      "status":"finished_successfully","metadata":{}}}
  }
}]`

// postChatGPTExport posts conversations as a one-file export zip and decodes the JSON body.
func postChatGPTExport(t *testing.T, srv *Server, conversations string) map[string]any {
	t.Helper()
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	entry, err := zw.Create("conversations-000.json")
	require.NoError(t, err)
	_, err = entry.Write([]byte(conversations))
	require.NoError(t, err)
	require.NoError(t, zw.Close())

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "export.zip")
	require.NoError(t, err)
	_, err = part.Write(archive.Bytes())
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/import/chatgpt", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var wire map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &wire))
	return wire
}

func TestHandleImportChatGPTReportsRefusalReasons(t *testing.T) {
	for _, tc := range []struct {
		name   string
		data   string
		reason string
	}{
		{name: "diverged", data: strings.Replace(chatGPTRefusalConvWithAppend, `"Hello"`, `"Changed archived message"`, 1), reason: "diverged"},
		{name: "shorter_export", data: chatGPTRefusalConv, reason: "shorter_export"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := testServer(t, 5*time.Second)
			first := postChatGPTExport(t, srv, chatGPTRefusalConvWithAppend)
			require.InDelta(t, float64(1), first["imported"], 0, "first import: %v", first)

			wire := postChatGPTExport(t, srv, tc.data)
			assert.InDelta(t, float64(1), wire["errors"], 0)
			assert.Equal(t, []any{
				map[string]any{"session_id": "chatgpt:cg-1", "reason": tc.reason},
			}, wire["refusals"])
		})
	}
}

func TestHandleImportClaudeAIStreamsRefusalsOnlyWhenDone(t *testing.T) {
	srv := testServer(t, 5*time.Second)
	message := func(id, text, sender, at string) string {
		return `{"uuid":"` + id + `","text":"` + text + `","sender":"` + sender + `",` +
			`"content":[{"type":"text","text":"` + text + `"}],"created_at":"` + at + `"}`
	}
	conversation := func(messages ...string) string {
		return `[{"uuid":"refusal-sse-001","name":"Refusal SSE",` +
			`"created_at":"2026-03-01T10:00:00.000000Z","updated_at":"2026-03-01T10:05:00.000000Z",` +
			`"chat_messages":[` + strings.Join(messages, ",") + `]}]`
	}
	first := message("m1", "hello", "human", "2026-03-01T10:00:00.000000Z")
	second := message("m2", "hi there", "assistant", "2026-03-01T10:01:00.000000Z")

	post := func(conversations string, stream bool) *httptest.ResponseRecorder {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		part, err := writer.CreateFormFile("file", "conversations.json")
		require.NoError(t, err)
		_, err = part.Write([]byte(conversations))
		require.NoError(t, err)
		require.NoError(t, writer.Close())
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/import/claude-ai", &body)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		if stream {
			req.Header.Set("Accept", "text/event-stream")
		}
		rec := httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		return rec
	}
	post(conversation(first, second), false)

	rec := post(conversation(first), true)
	var done *importer.ImportStats
	progress := 0
	lines := strings.Split(rec.Body.String(), "\n")
	for i, line := range lines {
		if i+1 >= len(lines) {
			break
		}
		data := strings.TrimPrefix(lines[i+1], "data: ")
		switch line {
		case "event: progress":
			progress++
			assert.NotContains(t, data, `"refusals"`)
		case "event: done":
			done = new(importer.ImportStats)
			require.NoError(t, json.Unmarshal([]byte(data), done))
		}
	}
	require.Positive(t, progress, "body: %s", rec.Body.String())
	require.NotNil(t, done, "body: %s", rec.Body.String())
	assert.Equal(t, 1, done.Errors)
	assert.Equal(t, []importer.ImportRefusal{
		{SessionID: "claude-ai:refusal-sse-001", Reason: importer.RefusalShorterExport},
	}, done.Refusals)
}
