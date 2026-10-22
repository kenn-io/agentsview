package service_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
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

func TestFileReferencesDigestCapAndBudget(t *testing.T) {
	_, env := newDirectTestSvc(t)
	id := env.InsertSession(t)
	var blocks []map[string]any
	for i := range 52 {
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
	assert.False(t, out.LookupFailed)
	assert.Equal(t, "limit_exceeded", out.Occurrences[50].State)
	assert.Equal(t, "limit_exceeded", out.Occurrences[51].State)
}

func TestFileReferencesLookupFailure(t *testing.T) {
	_, env := newDirectTestSvc(t)
	id := env.InsertSession(t)
	const digest = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	require.NoError(t, env.db.InsertMessages(t.Context(), []db.Message{{SessionID: id, Ordinal: 0, Role: "user", Content: "![a](asset://" + digest + ".png) ![b](asset://" + fmt.Sprintf("%064x", 1) + ".png)"}}))
	t.Setenv("DOCBANK_TEST_KEY", "test-key")
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("sha256") == digest {
			_, _ = w.Write([]byte(`{"items":[],"total":0}`))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer stub.Close()
	svc := service.NewDirectBackend(env.db, nil, service.WithDocbank(docbank.New(config.DocbankIntegration{URL: stub.URL, APIKeyEnv: "DOCBANK_TEST_KEY"})))
	out, err := service.FileReferences(t.Context(), svc, id)
	require.NoError(t, err)
	assert.True(t, out.LookupFailed)
	require.Len(t, out.Occurrences, 2)
	assert.Equal(t, "not_found", out.Occurrences[0].State)
	assert.Equal(t, "unavailable", out.Occurrences[1].State)
}

func TestFileReferencesRequestBudget(t *testing.T) {
	_, env := newDirectTestSvc(t)
	id := env.InsertSession(t)
	var calls []db.ToolCall
	for i := range 16 {
		calls = append(calls, db.ToolCall{ToolUseID: strconv.Itoa(i), ToolName: "Read", ResultContent: fmt.Sprintf(`[{"type":"agentsview_image","image_ref":"asset://%064x.png","media_type":"image/png","byte_size":3}]`, i)})
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
	assert.True(t, out.LookupFailed)
	require.Len(t, out.Occurrences, 16)
	for _, occurrence := range out.Occurrences {
		assert.Equal(t, "unavailable", occurrence.State)
	}
}

func TestFileReferencesCarriesRevisionAcrossReplacement(t *testing.T) {
	_, env := newDirectTestSvc(t)
	id := env.InsertSession(t)
	image := func(digest string) []db.Message {
		return []db.Message{{SessionID: id, Ordinal: 0, Role: "user", Content: "![image](asset://" + digest + ".png)"}}
	}
	require.NoError(t, env.db.ReplaceSessionMessages(t.Context(), id, image(strings.Repeat("a", 64))))
	svc := service.NewDirectBackend(env.db, nil, service.WithDocbank(docbank.New(config.DocbankIntegration{URL: "https://example.com", APIKeyEnv: "DOCBANK_ABSENT_KEY"})))
	t.Setenv("DOCBANK_ABSENT_KEY", "")
	first, err := svc.Messages(t.Context(), id, service.MessageFilter{})
	require.NoError(t, err)
	require.NoError(t, env.db.ReplaceSessionMessages(t.Context(), id, image(strings.Repeat("b", 64))))
	out, err := service.FileReferences(t.Context(), svc, id)
	require.NoError(t, err)
	current, err := svc.Messages(t.Context(), id, service.MessageFilter{})
	require.NoError(t, err)
	data, err := json.Marshal(out)
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(data, &wire))
	assert.Equal(t, current.TranscriptRevision, wire["transcript_revision"])
	assert.NotEqual(t, first.TranscriptRevision, wire["transcript_revision"])
	require.Len(t, out.Occurrences, 1)
	assert.Equal(t, strings.Repeat("b", 64), out.Occurrences[0].SHA256)
}

type replacingFileReferenceStore struct {
	db.Store
	replace func()
}

func (s *replacingFileReferenceStore) GetSession(ctx context.Context, id string) (*db.Session, error) {
	session, err := s.Store.GetSession(ctx, id)
	if s.replace != nil {
		replace := s.replace
		s.replace = nil
		replace()
	}
	return session, err
}

func TestFileReferencesRejectsReplacementBetweenReads(t *testing.T) {
	_, env := newDirectTestSvc(t)
	id := env.InsertSession(t)
	require.NoError(t, env.db.ReplaceSessionMessages(t.Context(), id, []db.Message{{SessionID: id, Ordinal: 0, Role: "user", Content: "original"}}))
	store := &replacingFileReferenceStore{Store: env.db, replace: func() {
		require.NoError(t, env.db.ReplaceSessionMessages(t.Context(), id, []db.Message{{SessionID: id, Ordinal: 0, Role: "user", Content: "replacement"}}))
	}}
	svc := service.NewReadOnlyBackend(store, service.WithDocbank(docbank.New(config.DocbankIntegration{})))
	_, err := service.FileReferences(t.Context(), svc, id)
	assert.ErrorIs(t, err, service.ErrSourceChanged)
}

func TestFileReferencesIdlessCalls(t *testing.T) {
	_, env := newDirectTestSvc(t)
	id := env.InsertSession(t)
	require.NoError(t, env.db.InsertMessages(t.Context(), []db.Message{{SessionID: id, Ordinal: 0, Role: "assistant", Content: "tools", ToolCalls: []db.ToolCall{
		{ToolName: "Read", ResultContent: `[{"type":"input_image","image_url":"data:image/png;base64,YWJj"}]`},
		{ToolName: "Read", ResultContent: `[{"type":"input_image","image_url":"data:image/png;base64,YWJk"}]`},
	}}}))
	t.Setenv("DOCBANK_TEST_KEY", "key")
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"items":[],"total":0}`)) }))
	defer remote.Close()
	svc := service.NewDirectBackend(env.db, nil, service.WithDocbank(docbank.New(config.DocbankIntegration{URL: remote.URL, APIKeyEnv: "DOCBANK_TEST_KEY"})))
	out, err := service.FileReferences(t.Context(), svc, id)
	require.NoError(t, err)
	data, err := json.Marshal(out)
	require.NoError(t, err)
	var body struct {
		Occurrences []struct {
			CallIndex *int `json:"call_index"`
		} `json:"occurrences"`
	}
	require.NoError(t, json.Unmarshal(data, &body))
	require.Len(t, body.Occurrences, 2)
	assert.Equal(t, new(0), body.Occurrences[0].CallIndex)
	assert.Equal(t, new(1), body.Occurrences[1].CallIndex)
	messages, err := svc.Messages(t.Context(), id, service.MessageFilter{})
	require.NoError(t, err)
	data, err = json.Marshal(messages)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"call_index":1`)
}

func TestFileReferencesOpenCodeFiles(t *testing.T) {
	raw, err := os.ReadFile("../parser/testdata/opencode_v2/tool_files.json")
	require.NoError(t, err)
	var source struct {
		Content []struct {
			State struct {
				Content jsontext.Value `json:"content"`
			} `json:"state"`
		} `json:"content"`
	}
	require.NoError(t, json.Unmarshal(raw, &source))
	_, env := newDirectTestSvc(t)
	id := env.InsertSession(t)
	require.NoError(t, env.db.InsertMessages(t.Context(), []db.Message{{SessionID: id, Ordinal: 0, Role: "assistant", Content: "files", ToolCalls: []db.ToolCall{{ToolName: "read", ResultContent: string(source.Content[1].State.Content)}}}}))
	t.Setenv("DOCBANK_TEST_KEY", "key")
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"items":[],"total":0}`)) }))
	defer remote.Close()
	svc := service.NewDirectBackend(env.db, nil, service.WithDocbank(docbank.New(config.DocbankIntegration{URL: remote.URL, APIKeyEnv: "DOCBANK_TEST_KEY"})))
	out, err := service.FileReferences(t.Context(), svc, id)
	require.NoError(t, err)
	require.Len(t, out.Occurrences, 2)
	assert.Equal(t, "application/pdf", out.Occurrences[0].MediaType)
	assert.Equal(t, new(int64(327)), out.Occurrences[0].Size)
	assert.Equal(t, "text/plain", out.Occurrences[1].MediaType)
	assert.Equal(t, new(int64(6)), out.Occurrences[1].Size)
	assert.Equal(t, "b6a98d9ce9a2d9149288fa3df42d377c3e42737afdcdaf714e33c0a100b51060", out.Occurrences[1].SHA256)
}
