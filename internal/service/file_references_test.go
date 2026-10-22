package service_test

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/integrations/docbank"
	"go.kenn.io/agentsview/internal/service"
)

func TestFileReferencesDisabled(t *testing.T) {
	svc, _ := newDirectTestSvc(t)
	out, err := service.FileReferences(t.Context(), svc, "missing")
	require.NoError(t, err)
	assert.Equal(t, "disabled", out.Integration)
	assert.Empty(t, out.Occurrences)
}

func TestFileReferencesRetainedImages(t *testing.T) {
	_, env := newDirectTestSvc(t)
	id := env.InsertSession(t)
	const digest = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	const inline = `[{"type":"input_image","image_url":"data:image/png;base64,YWJj"}]`
	const offloaded = `[{"type":"agentsview_image","image_ref":"asset://` + digest + `.png","media_type":"image/png","byte_size":3}]`
	require.NoError(t, env.db.InsertMessages(t.Context(), []db.Message{{SessionID: id, Ordinal: 4, Role: "assistant", Content: "image", HasToolUse: true, ToolCalls: []db.ToolCall{
		{ToolUseID: "read-1", ToolName: "Read", ResultContent: inline, ResultEvents: []db.ToolResultEvent{{EventIndex: 0, Source: "tool_result", Status: "completed", Content: offloaded}}},
		{ToolUseID: "read-2", ToolName: "Read", ResultContent: inline},
	}}}))
	t.Setenv("DOCBANK_TEST_KEY", "test-key")
	var requests atomic.Int32
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		assert.Equal(t, digest, r.URL.Query().Get("sha256"))
		_, _ = w.Write([]byte(`{"items":[],"total":0}`))
	}))
	defer stub.Close()
	svc := service.NewDirectBackend(env.db, nil, service.WithDocbank(docbank.New(config.DocbankIntegration{URL: stub.URL, APIKeyEnv: "DOCBANK_TEST_KEY"})))
	_, err := svc.Messages(t.Context(), id, service.MessageFilter{})
	require.NoError(t, err)
	assert.Zero(t, requests.Load())
	out, err := service.FileReferences(t.Context(), svc, id)
	require.NoError(t, err)
	assert.Equal(t, int32(1), requests.Load())
	require.Len(t, out.Occurrences, 3)
	for _, occurrence := range out.Occurrences {
		assert.Equal(t, 4, occurrence.MessageOrdinal)
		assert.Equal(t, "not_found", occurrence.State)
		assert.Equal(t, digest, occurrence.SHA256)
	}
	require.NotNil(t, out.Occurrences[1].EventIndex)
	assert.Equal(t, 0, *out.Occurrences[1].EventIndex)
	assert.Equal(t, "read-2", *out.Occurrences[2].ToolUseID)
	_, err = service.FileReferences(t.Context(), svc, "missing")
	assert.ErrorIs(t, err, service.ErrSessionNotFound)
}

func TestFileReferencesClaudeSource(t *testing.T) {
	_, env := newDirectTestSvc(t)
	path := filepath.Join(t.TempDir(), "source.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(`{"type":"user","uuid":"pasted","message":{"content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YWJj"}}]}}
{"type":"user","uuid":"no-row","message":{"content":[]}}
{"type":"assistant","uuid":"call","message":{"content":[]}}
{"type":"user","uuid":"result","message":{"content":[{"type":"tool_result","tool_use_id":"read-1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YWJj"}}]}]}}
`), 0600))
	require.NoError(t, env.db.UpsertSession(t.Context(), db.Session{ID: "claude-images", Project: "p1", Agent: "claude", FilePath: new(path)}))
	require.NoError(t, env.db.InsertMessages(t.Context(), []db.Message{{SessionID: "claude-images", Ordinal: 0, Role: "assistant", Content: "read", SourceUUID: "call", ToolCalls: []db.ToolCall{{ToolUseID: "read-1", ToolName: "Read"}}}}))
	t.Setenv("DOCBANK_TEST_KEY", "test-key")
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"items":[],"total":0}`)) }))
	defer stub.Close()
	svc := service.NewDirectBackend(env.db, nil, service.WithDocbank(docbank.New(config.DocbankIntegration{URL: stub.URL, APIKeyEnv: "DOCBANK_TEST_KEY"})))
	out, err := service.FileReferences(t.Context(), svc, "claude-images")
	require.NoError(t, err)
	assert.Equal(t, "available", out.Source)
	require.Len(t, out.Occurrences, 2)
	assert.Nil(t, out.Occurrences[0].ToolUseID)
	assert.Equal(t, 0, out.Occurrences[0].MessageOrdinal)
	assert.Equal(t, "read-1", *out.Occurrences[1].ToolUseID)
	assert.Equal(t, 0, out.Occurrences[1].MessageOrdinal)
	require.NoError(t, os.Remove(path))
	out, err = service.FileReferences(t.Context(), svc, "claude-images")
	require.NoError(t, err)
	assert.Equal(t, "unavailable", out.Source)
	assert.Empty(t, out.Occurrences)
}

func TestFileReferencesDigestCapAndBudget(t *testing.T) {
	_, env := newDirectTestSvc(t)
	id := env.InsertSession(t)
	var blocks []map[string]any
	for i := 0; i < 52; i++ {
		blocks = append(blocks, map[string]any{"type": "agentsview_image", "image_ref": fmt.Sprintf("asset://%064x.png", i), "media_type": "image/png", "byte_size": 3})
	}
	content, err := json.Marshal(blocks)
	require.NoError(t, err)
	require.NoError(t, env.db.InsertMessages(t.Context(), []db.Message{{SessionID: id, Ordinal: 0, Role: "assistant", Content: "images", ToolCalls: []db.ToolCall{{ToolUseID: "read", ToolName: "Read", ResultContent: string(content)}}}}))
	t.Setenv("DOCBANK_TEST_KEY", "test-key")
	var requests, active, peak atomic.Int32
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		_, _ = w.Write([]byte(`{"items":[],"total":0}`))
	}))
	defer stub.Close()
	svc := service.NewDirectBackend(env.db, nil, service.WithDocbank(docbank.New(config.DocbankIntegration{URL: stub.URL, APIKeyEnv: "DOCBANK_TEST_KEY"})))
	out, err := service.FileReferences(t.Context(), svc, id)
	require.NoError(t, err)
	assert.Equal(t, int32(50), requests.Load())
	assert.LessOrEqual(t, peak.Load(), int32(4))
	require.Len(t, out.Occurrences, 52)
	assert.Equal(t, "unavailable", out.Occurrences[50].State)
	assert.Equal(t, "unavailable", out.Occurrences[51].State)
	// A caller's canceled budget stops further network work and preserves unknown evidence.
	ctx, cancel := context.WithCancel(t.Context())
	hanging := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { cancel(); <-r.Context().Done() }))
	defer hanging.Close()
	svc = service.NewDirectBackend(env.db, nil, service.WithDocbank(docbank.New(config.DocbankIntegration{URL: hanging.URL, APIKeyEnv: "DOCBANK_TEST_KEY"})))
	out, err = service.FileReferences(ctx, svc, id)
	require.NoError(t, err)
	require.Len(t, out.Occurrences, 52)
	for _, occurrence := range out.Occurrences {
		assert.Equal(t, "unavailable", occurrence.State)
	}
}

func TestFileReferencesRequestBudget(t *testing.T) {
	_, env := newDirectTestSvc(t)
	id := env.InsertSession(t)
	var calls []db.ToolCall
	for i := 0; i < 12; i++ {
		calls = append(calls, db.ToolCall{ToolUseID: fmt.Sprint(i), ToolName: "Read", ResultContent: fmt.Sprintf(`[{"type":"agentsview_image","image_ref":"asset://%064x.png","media_type":"image/png","byte_size":3}]`, i)})
	}
	require.NoError(t, env.db.InsertMessages(t.Context(), []db.Message{{SessionID: id, Ordinal: 0, Role: "assistant", Content: "images", ToolCalls: calls}}))
	t.Setenv("DOCBANK_TEST_KEY", "test-key")
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer stub.Close()
	svc := service.NewDirectBackend(env.db, nil, service.WithDocbank(docbank.New(config.DocbankIntegration{URL: stub.URL, APIKeyEnv: "DOCBANK_TEST_KEY"})))
	start := time.Now()
	out, err := service.FileReferences(t.Context(), svc, id)
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 7*time.Second)
	require.Len(t, out.Occurrences, 12)
	for _, occurrence := range out.Occurrences {
		assert.Equal(t, "unavailable", occurrence.State)
	}
}

func TestFileReferencesUnplacedSourceImage(t *testing.T) {
	_, env := newDirectTestSvc(t)
	path := filepath.Join(t.TempDir(), "session.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(`{"type":"user","uuid":"tail","message":{"content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YWJj"}}]}}`), 0600))
	require.NoError(t, env.db.UpsertSession(t.Context(), db.Session{ID: "session", Agent: "claude", Project: "example", FilePath: new(path)}))
	t.Setenv("DOCBANK_TEST_KEY", "test-key")
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("unplaced image must not be looked up")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer stub.Close()
	svc := service.NewDirectBackend(env.db, nil, service.WithDocbank(docbank.New(config.DocbankIntegration{URL: stub.URL, APIKeyEnv: "DOCBANK_TEST_KEY"})))
	out, err := service.FileReferences(t.Context(), svc, "session")
	require.NoError(t, err)
	assert.Equal(t, "unavailable", out.Source)
	assert.Empty(t, out.Occurrences)
}

// The source metadata lookup uses the same bounded source phase as the JSONL scan.
type expiredSourceStore struct{ db.Store }

func (s expiredSourceStore) GetSessionFull(ctx context.Context, _ string) (*db.Session, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestFileReferencesSourceBudgetDoesNotCancelLookups(t *testing.T) {
	_, env := newDirectTestSvc(t)
	require.NoError(t, env.db.UpsertSession(t.Context(), db.Session{ID: "session", Agent: "claude", Project: "example"}))
	require.NoError(t, env.db.InsertMessages(t.Context(), []db.Message{{SessionID: "session", Ordinal: 4, Role: "user", Content: `![image](asset://ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad.png)`}}))
	t.Setenv("DOCBANK_TEST_KEY", "test-key")
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", r.URL.Query().Get("sha256"))
		_, _ = w.Write([]byte(`{"items":[],"total":0}`))
	}))
	defer stub.Close()
	svc := service.NewReadOnlyBackend(expiredSourceStore{env.db}, service.WithDocbank(docbank.New(config.DocbankIntegration{URL: stub.URL, APIKeyEnv: "DOCBANK_TEST_KEY"})))
	out, err := service.FileReferences(t.Context(), svc, "session")
	require.NoError(t, err)
	assert.Equal(t, "unavailable", out.Source)
	require.Len(t, out.Occurrences, 1)
	assert.Equal(t, "not_found", out.Occurrences[0].State)
}
