package server

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/importer"
)

func readImportEvents(t *testing.T, body io.Reader, handle func(string, string)) {
	t.Helper()
	scanner := bufio.NewScanner(body)
	event := ""
	terminal := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			event = strings.TrimPrefix(line, "event: ")
		}
		if strings.HasPrefix(line, "data: ") {
			handle(event, strings.TrimPrefix(line, "data: "))
			terminal = terminal || event == "done" || event == "error"
		}
	}
	require.NoError(t, scanner.Err())
	require.True(t, terminal, "import stream must finish with done or error")
}

func TestClaudeAISyncRelay(t *testing.T) {
	for _, tt := range []struct {
		name string
		seed bool
	}{
		{name: "large result stores session"},
		{name: "shorter result updates in place", seed: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := testServer(t, 5*time.Second)
			if tt.seed {
				stats, err := importer.ImportClaudeAI(t.Context(), srv.db, strings.NewReader(`[{"uuid":"relay","created_at":"2026-03-01T10:00:00Z","updated_at":"2026-03-01T10:04:00Z","chat_messages":[{"sender":"human","text":"Previous prompt"},{"sender":"assistant","text":"Previous answer"}]}]`), nil)
				require.NoError(t, err)
				require.Equal(t, 1, stats.Imported)
			}
			httpServer := httptest.NewServer(srv.mux)
			defer httpServer.Close()
			postResult := func(id, body string, want int) {
				response, err := http.Post(httpServer.URL+"/api/v1/import/claude-ai/sync/results/"+id+"?status=200", "application/octet-stream", strings.NewReader(body))
				require.NoError(t, err)
				defer response.Body.Close()
				data, err := io.ReadAll(response.Body)
				require.NoError(t, err)
				require.Equal(t, want, response.StatusCode, "%s", data)
			}
			postResult("unknown", `{}`, http.StatusNotFound)
			response, err := http.Post(httpServer.URL+"/api/v1/import/claude-ai/sync", "application/json", nil)
			require.NoError(t, err)
			defer response.Body.Close()
			require.Equal(t, http.StatusOK, response.StatusCode)
			answered := ""
			var stats importer.ImportStats
			readImportEvents(t, response.Body, func(event, data string) {
				switch event {
				case "fetch":
					var request struct {
						ID   string `json:"id"`
						Path string `json:"path"`
					}
					require.NoError(t, json.Unmarshal([]byte(data), &request))
					answered = request.ID
					switch request.Path {
					case "/api/organizations":
						postResult(request.ID, `[{"uuid":"org","capabilities":["chat"]}]`, http.StatusNoContent)
					case "/api/organizations/org/chat_conversations_v2?limit=50&offset=0":
						postResult(request.ID, `{"data":[{"uuid":"relay","current_leaf_message_uuid":"m","name":"Relay","created_at":"2026-03-01T10:00:00Z","updated_at":"2026-03-01T10:05:00Z"}],"has_more":false}`, http.StatusNoContent)
					case "/api/organizations/org/chat_conversations/relay?tree=True&rendering_mode=messages&consistency=strong&render_all_tools=true&include_inline_comparison=true":
						postResult(request.ID, `{"uuid":"relay","current_leaf_message_uuid":"m","name":"Relay","created_at":"2026-03-01T10:00:00Z","updated_at":"2026-03-01T10:05:00Z","padding":"`+strings.Repeat("x", 2<<20)+`","chat_messages":[{"uuid":"m","parent_message_uuid":"00000000-0000-4000-8000-000000000000","sender":"human","text":"Archived relay message","created_at":"2026-03-01T10:00:00Z"}]}`, http.StatusNoContent)
					default:
						t.Fatalf("unexpected path %s", request.Path)
					}
				case "error":
					t.Fatalf("sync failed: %s", data)
				case "done":
					require.NoError(t, json.Unmarshal([]byte(data), &stats))
				}
			})
			if tt.seed {
				assert.Equal(t, 1, stats.Updated)
			} else {
				assert.Equal(t, 1, stats.Imported)
			}
			assert.Zero(t, stats.Errors)
			messages, err := srv.db.GetAllMessages(t.Context(), "claude-ai:relay")
			require.NoError(t, err)
			require.Len(t, messages, 1)
			assert.Equal(t, "Archived relay message", messages[0].Content)
			postResult(answered, `{}`, http.StatusNotFound)
		})

	}

	t.Run("expired sign-in reaches stream error", func(t *testing.T) {
		srv := testServer(t, 5*time.Second)
		httpServer := httptest.NewServer(srv.mux)
		defer httpServer.Close()
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, httpServer.URL+"/api/v1/import/claude-ai/sync", nil)
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		require.NoError(t, err)
		defer response.Body.Close()
		gotError := false
		readImportEvents(t, response.Body, func(event, data string) {
			switch event {
			case "fetch":
				var request struct {
					ID string `json:"id"`
				}
				require.NoError(t, json.Unmarshal([]byte(data), &request))
				answer, err := http.NewRequestWithContext(t.Context(), http.MethodPost, httpServer.URL+"/api/v1/import/claude-ai/sync/results/"+request.ID+"?status=401", strings.NewReader("{}"))
				require.NoError(t, err)
				answer.Header.Set("Content-Type", "application/octet-stream")
				result, err := http.DefaultClient.Do(answer)
				require.NoError(t, err)
				require.Equal(t, http.StatusNoContent, result.StatusCode)
				require.NoError(t, result.Body.Close())
			case "error":
				assert.JSONEq(t, `{"error":"Sign in to Claude.ai, then Sync again"}`, data)
				gotError = true
			case "done":
				require.FailNow(t, "expired credentials completed Sync")
			}
		})
		assert.True(t, gotError)
	})

	t.Run("browser failure reaches stream error", func(t *testing.T) {
		srv := testServer(t, 5*time.Second)
		httpServer := httptest.NewServer(srv.mux)
		defer httpServer.Close()
		response, err := http.Post(httpServer.URL+"/api/v1/import/claude-ai/sync", "application/json", nil)
		require.NoError(t, err)
		defer response.Body.Close()
		answered := ""
		gotError := false
		readImportEvents(t, response.Body, func(event, data string) {
			switch event {
			case "fetch":
				var request struct {
					ID string `json:"id"`
				}
				require.NoError(t, json.Unmarshal([]byte(data), &request))
				answered = request.ID
				result, err := http.Post(httpServer.URL+"/api/v1/import/claude-ai/sync/results/"+request.ID+"?status=0", "application/octet-stream", strings.NewReader("TypeError: Failed to fetch"))
				require.NoError(t, err)
				require.Equal(t, http.StatusNoContent, result.StatusCode)
				require.NoError(t, result.Body.Close())
			case "error":
				assert.JSONEq(t, `{"error":"TypeError: Failed to fetch"}`, data)
				gotError = true
			}
		})
		assert.True(t, gotError)
		result, err := http.Post(httpServer.URL+"/api/v1/import/claude-ai/sync/results/"+answered+"?status=200", "application/octet-stream", strings.NewReader("{}"))
		require.NoError(t, err)
		defer result.Body.Close()
		assert.Equal(t, http.StatusNotFound, result.StatusCode)
	})

	t.Run("unanswered fetch expires", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			srv := testServer(t, 5*time.Second)
			recorder := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/import/claude-ai/sync", nil)
			req.Header.Set("Content-Type", "application/json")
			start := time.Now()
			srv.mux.ServeHTTP(recorder, req)
			assert.Equal(t, 2*time.Minute, time.Since(start))
			assert.Contains(t, recorder.Body.String(), "claude browser fetch timed out")
			assert.Contains(t, recorder.Body.String(), "event: error")
			var request struct {
				ID string `json:"id"`
			}
			for _, line := range strings.Split(recorder.Body.String(), "\n") {
				if strings.HasPrefix(line, "data: ") {
					require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &request))
					break
				}
			}
			require.NotEmpty(t, request.ID)
			late := httptest.NewRecorder()
			answer := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/import/claude-ai/sync/results/"+request.ID+"?status=200", strings.NewReader("{}"))
			answer.Header.Set("Content-Type", "application/octet-stream")
			srv.mux.ServeHTTP(late, answer)
			assert.Equal(t, http.StatusNotFound, late.Code)
		})
	})
}

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

// chatGPTExportZip packs conversations as a one-file ChatGPT export zip.
func chatGPTExportZip(t *testing.T, conversations string) []byte {
	t.Helper()
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	entry, err := zw.Create("conversations-000.json")
	require.NoError(t, err)
	_, err = entry.Write([]byte(conversations))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return archive.Bytes()
}

// postChatGPTExport posts conversations as a one-file export zip and decodes the JSON body.
func postChatGPTExport(t *testing.T, srv *Server, conversations string) map[string]any {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "export.zip")
	require.NoError(t, err)
	_, err = part.Write(chatGPTExportZip(t, conversations))
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

// postImport posts data to an import route and decodes the stats from the JSON body or the stream's done event.
func postImport(t *testing.T, srv *Server, path, filename string, data []byte, stream bool) importer.ImportStats {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", filename)
	require.NoError(t, err)
	_, _ = part.Write(data)
	require.NoError(t, writer.Close())
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var stats importer.ImportStats
	payload := rec.Body.String()
	if stream {
		_, done, ok := strings.Cut(payload, "event: done\ndata: ")
		require.True(t, ok, "no done event in %s", payload)
		payload, _, _ = strings.Cut(done, "\n")
	}
	require.NoError(t, json.Unmarshal([]byte(payload), &stats))
	return stats
}

func TestHandleImportReplaceQuery(t *testing.T) {
	const claudeAIMessage = `{"uuid":"m%d","text":"turn %d","sender":"human","content":[{"type":"text","text":"turn %d"}],"created_at":"2026-03-01T10:0%d:00.000000Z"}`
	claudeAIExport := func(n int) []byte {
		msgs := make([]string, n)
		for i := range msgs {
			msgs[i] = fmt.Sprintf(claudeAIMessage, i, i, i, i)
		}
		return []byte(`[{"uuid":"replace-001","name":"Replace","created_at":"2026-03-01T10:00:00.000000Z",` +
			`"updated_at":"2026-03-01T10:05:00.000000Z","chat_messages":[` + strings.Join(msgs, ",") + `]}]`)
	}
	claudeAI := struct{ path, file, id string }{"/api/v1/import/claude-ai", "conversations.json", "claude-ai:replace-001"}
	chatGPT := struct{ path, file, id string }{"/api/v1/import/chatgpt", "export.zip", "chatgpt:cg-1"}
	for _, tt := range []struct {
		name             string
		route            struct{ path, file, id string }
		initial, refused []byte
		stream           bool
	}{
		{"claude-ai json", claudeAI, claudeAIExport(2), claudeAIExport(1), false},
		{"claude-ai sse", claudeAI, claudeAIExport(2), claudeAIExport(1), true},
		{"chatgpt json", chatGPT, chatGPTExportZip(t, chatGPTRefusalConvWithAppend), chatGPTExportZip(t, chatGPTRefusalConv), false},
		{"chatgpt sse", chatGPT, chatGPTExportZip(t, chatGPTRefusalConvWithAppend), chatGPTExportZip(t, chatGPTRefusalConv), true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := testServer(t, 5*time.Second)
			require.Equal(t, 1, postImport(t, srv, tt.route.path, tt.route.file, tt.initial, tt.stream).Imported)
			require.Equal(t, 1, postImport(t, srv, tt.route.path, tt.route.file, tt.refused, tt.stream).Errors)

			query := "?" + url.Values{"replace": {tt.route.id}}.Encode()
			stats := postImport(t, srv, tt.route.path+query, tt.route.file, tt.refused, tt.stream)
			assert.Equal(t, 1, stats.Updated)
			assert.Zero(t, stats.Errors)
			trashed, err := srv.db.(*db.DB).ListTrashedSessions(t.Context())
			require.NoError(t, err)
			require.Len(t, trashed, 1)
			assert.True(t, strings.HasPrefix(trashed[0].ID, tt.route.id+":replaced:"), trashed[0].ID)
		})
	}
}

func TestClaudeAISyncRelayOversizeContinues(t *testing.T) {
	for _, oversizedStatus := range []int{200, 413} {
		t.Run(fmt.Sprint(oversizedStatus), func(t *testing.T) {
			srv := testServer(t, 5*time.Second)
			httpServer := httptest.NewServer(srv.mux)
			defer httpServer.Close()
			response, err := http.Post(httpServer.URL+"/api/v1/import/claude-ai/sync", "application/json", nil)
			require.NoError(t, err)
			defer response.Body.Close()
			var stats importer.ImportStats
			done := false
			readImportEvents(t, response.Body, func(event, data string) {
				switch event {
				case "fetch":
					var request struct {
						ID   string `json:"id"`
						Path string `json:"path"`
					}
					require.NoError(t, json.Unmarshal([]byte(data), &request))
					var body string
					status := 200
					switch request.Path {
					case "/api/organizations":
						body = `[{"uuid":"org","capabilities":["chat"]}]`
					case "/api/organizations/org/chat_conversations_v2?limit=50&offset=0":
						body = `{"data":[{"uuid":"large","current_leaf_message_uuid":"m","updated_at":"2026-03-01T10:05:00Z"},{"uuid":"later","current_leaf_message_uuid":"m","updated_at":"2026-03-01T10:05:00Z"}],"has_more":false}`
					case "/api/organizations/org/chat_conversations/large?tree=True&rendering_mode=messages&consistency=strong&render_all_tools=true&include_inline_comparison=true":
						status = oversizedStatus
						if status == 200 {
							body = strings.Repeat("x", (32<<20)+2)
						}
					case "/api/organizations/org/chat_conversations/later?tree=True&rendering_mode=messages&consistency=strong&render_all_tools=true&include_inline_comparison=true":
						body = `{"uuid":"later","created_at":"2026-03-01T10:00:00Z","updated_at":"2026-03-01T10:05:00Z","current_leaf_message_uuid":"m","chat_messages":[{"uuid":"m","parent_message_uuid":"00000000-0000-4000-8000-000000000000","sender":"assistant","text":"Later reply","created_at":"2026-03-01T10:05:00Z"}]}`
					default:
						t.Fatalf("unexpected path %s", request.Path)
					}
					result, err := http.Post(httpServer.URL+"/api/v1/import/claude-ai/sync/results/"+request.ID+"?status="+fmt.Sprint(status), "application/octet-stream", strings.NewReader(body))
					require.NoError(t, err)
					assert.Equal(t, http.StatusNoContent, result.StatusCode)
					require.NoError(t, result.Body.Close())
				case "error":
					t.Fatalf("sync failed: %s", data)
				case "done":
					require.NoError(t, json.Unmarshal([]byte(data), &stats))
					done = true
				}
			})
			require.True(t, done)
			assert.Equal(t, 1, stats.Errors)
			assert.Equal(t, 1, stats.Imported)
			session, err := srv.db.GetSession(t.Context(), "claude-ai:later")
			require.NoError(t, err)
			require.NotNil(t, session)
			assert.Equal(t, 1, session.MessageCount)
			messages, err := srv.db.GetAllMessages(t.Context(), session.ID)
			require.NoError(t, err)
			require.Len(t, messages, 1)
			assert.Equal(t, "Later reply", messages[0].Content)
		})
	}
}

func TestClaudeAISyncMutationNotifications(t *testing.T) {
	for _, ending := range []string{"done", "detail error", "cancel"} {
		t.Run(ending, func(t *testing.T) {
			mutations := make(chan struct{}, 2)
			recall := make(chan struct{}, 2)
			srv := testServer(t, 5*time.Second,
				WithSessionMutationNotifier(func() { mutations <- struct{}{} }),
				WithRecallCorpusMutationNotifier(func() { recall <- struct{}{} }),
			)
			srv.broadcaster = NewBroadcaster(0)
			events, unsubscribe := srv.broadcaster.Subscribe()
			defer unsubscribe()
			httpServer := httptest.NewServer(srv.mux)
			defer httpServer.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, httpServer.URL+"/api/v1/import/claude-ai/sync", nil)
			require.NoError(t, err)
			response, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer response.Body.Close()
			scanner := bufio.NewScanner(response.Body)
			terminal := ""
			for scanner.Scan() {
				line := scanner.Text()
				if line == "event: done" || line == "event: error" {
					terminal = strings.TrimPrefix(line, "event: ")
				}
				if !strings.HasPrefix(line, "data: ") || !strings.Contains(line, `"path"`) {
					continue
				}
				var fetch struct {
					ID   string `json:"id"`
					Path string `json:"path"`
				}
				require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &fetch))
				body, status := "", 200
				switch {
				case fetch.Path == "/api/organizations":
					body = `[{"uuid":"org","capabilities":["chat"]}]`
				case strings.Contains(fetch.Path, "chat_conversations_v2"):
					body = `{"data":[{"uuid":"one","current_leaf_message_uuid":"m","updated_at":"2026-03-01T10:05:00Z"}`
					if ending != "done" {
						body += `,{"uuid":"two","current_leaf_message_uuid":"m","updated_at":"2026-03-01T10:05:00Z"}`
					}
					body += `],"has_more":false}`
				case strings.Contains(fetch.Path, "/one?"):
					body = `{"uuid":"one","created_at":"2026-03-01T10:00:00Z","updated_at":"2026-03-01T10:05:00Z","current_leaf_message_uuid":"m","chat_messages":[{"uuid":"m","parent_message_uuid":"00000000-0000-4000-8000-000000000000","sender":"human","text":"Committed chat"}]}`
				case strings.Contains(fetch.Path, "/two?"):
					if ending == "cancel" {
						cancel()
						terminal = "cancel"
						break
					}
					status, body = 0, "browser disconnected"
				default:
					t.Fatalf("unexpected fetch %s", fetch.Path)
				}
				if terminal == "cancel" {
					break
				}
				answer, err := http.Post(httpServer.URL+"/api/v1/import/claude-ai/sync/results/"+fetch.ID+"?status="+fmt.Sprint(status), "application/octet-stream", strings.NewReader(body))
				require.NoError(t, err)
				require.Equal(t, http.StatusNoContent, answer.StatusCode)
				require.NoError(t, answer.Body.Close())
			}
			wantTerminal := ending
			if ending == "detail error" {
				wantTerminal = "done"
			}
			assert.Equal(t, wantTerminal, terminal)
			for _, ch := range []<-chan struct{}{mutations, recall} {
				select {
				case <-ch:
				case <-time.After(5 * time.Second):
					t.Fatal("committed chat did not notify mutation consumer")
				}
			}
			select {
			case event := <-events:
				assert.Equal(t, "sessions", event.Scope)
			case <-time.After(5 * time.Second):
				t.Fatal("committed chat did not broadcast sessions")
			}
			messages, err := srv.db.GetAllMessages(t.Context(), "claude-ai:one")
			require.NoError(t, err)
			require.Len(t, messages, 1)
			assert.Equal(t, "Committed chat", messages[0].Content)
		})
	}
}
