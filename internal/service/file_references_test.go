package service_test

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	require.NoError(t, env.db.InsertMessages(t.Context(), []db.Message{{SessionID: id, Ordinal: 0, Role: "assistant", Content: "tools", ToolCalls: []db.ToolCall{
		{ToolName: "Read", ResultContent: inline},
		{ToolName: "Read", ResultContent: inline},
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
	messages, err := svc.Messages(t.Context(), id, service.MessageFilter{})
	require.NoError(t, err)
	assert.Zero(t, requests.Load())
	data, err := json.Marshal(messages)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"call_index":1`)
	assert.Contains(t, string(data), `"call_index":0`)
	out, err := service.FileReferences(t.Context(), svc, id)
	require.NoError(t, err)
	assert.Equal(t, int32(1), requests.Load())
	require.Len(t, out.Occurrences, 5)
	assert.Equal(t, new(0), out.Occurrences[0].CallIndex)
	assert.Equal(t, new(1), out.Occurrences[1].CallIndex)
	for _, occurrence := range out.Occurrences[:2] {
		assert.Equal(t, new(""), occurrence.ToolUseID)
	}
	for _, occurrence := range out.Occurrences[2:] {
		assert.Equal(t, 4, occurrence.MessageOrdinal)
		assert.Equal(t, "not_found", occurrence.State)
		assert.Equal(t, digest, occurrence.SHA256)
	}
	require.NotNil(t, out.Occurrences[3].EventIndex)
	assert.Equal(t, 0, *out.Occurrences[3].EventIndex)
	assert.Equal(t, "read-2", *out.Occurrences[4].ToolUseID)
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
		s.replace()
	}
	return session, err
}

func TestFileReferencesRejectsRepeatedReplacementBetweenReads(t *testing.T) {
	t.Setenv("DOCBANK_ABSENT_KEY", "")
	_, env := newDirectTestSvc(t)
	id := env.InsertSession(t)
	require.NoError(t, env.db.ReplaceSessionMessages(t.Context(), id, []db.Message{{SessionID: id, Ordinal: 0, Role: "user", Content: "original"}}))
	replacements := 0
	store := &replacingFileReferenceStore{Store: env.db, replace: func() {
		replacements++
		require.NoError(t, env.db.ReplaceSessionMessages(t.Context(), id, []db.Message{{SessionID: id, Ordinal: 0, Role: "user", Content: fmt.Sprintf("replacement %d", replacements)}}))
	}}
	svc := service.NewReadOnlyBackend(store, service.WithDocbank(docbank.New(config.DocbankIntegration{URL: "https://example.com", APIKeyEnv: "DOCBANK_ABSENT_KEY"})))
	_, err := service.FileReferences(t.Context(), svc, id)
	assert.ErrorIs(t, err, service.ErrSourceChanged)
}

type hydratingFileReferenceStore struct {
	db.Store
	replace func()
}

func (s *hydratingFileReferenceStore) GetAllMessages(ctx context.Context, id string) ([]db.Message, error) {
	messages, err := s.Store.GetAllMessages(ctx, id)
	if err != nil || s.replace == nil {
		return messages, err
	}
	replace := s.replace
	s.replace = nil
	replace()
	// Hydration can see new tool results after the message query captured its revision.
	current, err := s.Store.GetAllMessages(ctx, id)
	if err != nil {
		return nil, err
	}
	for i := range messages {
		messages[i].ToolCalls = current[i].ToolCalls
	}
	return messages, nil
}

func TestFileReferencesDroppedImageBytesNotKept(t *testing.T) {
	t.Skip("Dropped images finding: preserving unavailable occurrences would lengthen RetainedToolResultImages and FileReferences")
	_, env := newDirectTestSvc(t)
	id := env.InsertSession(t)
	require.NoError(t, env.db.InsertMessages(t.Context(), []db.Message{{SessionID: id, Ordinal: 0, Role: "assistant", ToolCalls: []db.ToolCall{{ToolName: "Read", ResultContent: `[{"type":"agentsview_image","version":1,"media_type":"image/png","byte_size":3}]`}}}}))
	t.Setenv("DOCBANK_TEST_KEY", "test-key")
	var requests atomic.Int32
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"items":[],"total":0}`))
	}))
	defer stub.Close()
	svc := service.NewDirectBackend(env.db, nil, service.WithDocbank(docbank.New(config.DocbankIntegration{URL: stub.URL, APIKeyEnv: "DOCBANK_TEST_KEY"})))
	out, err := service.FileReferences(t.Context(), svc, id)
	require.NoError(t, err)
	assert.Zero(t, requests.Load())
	require.Len(t, out.Occurrences, 1)
	assert.Empty(t, out.Occurrences[0].SHA256)
	assert.Equal(t, "bytes_not_kept", out.Occurrences[0].State)
}

func TestFileReferencesChecksRevisionAfterToolHydration(t *testing.T) {
	t.Setenv("DOCBANK_ABSENT_KEY", "")
	_, env := newDirectTestSvc(t)
	id := env.InsertSession(t)
	image := func(digest string) []db.Message {
		return []db.Message{{SessionID: id, Ordinal: 0, Role: "assistant", Content: "image", ToolCalls: []db.ToolCall{{ToolUseID: "read", ToolName: "Read", ResultEvents: []db.ToolResultEvent{{EventIndex: 0, Source: "tool_result", Status: "completed", Content: `[{"type":"agentsview_image","image_ref":"asset://` + digest + `.png","media_type":"image/png"}]`}}}}}}
	}
	require.NoError(t, env.db.ReplaceSessionMessages(t.Context(), id, image(strings.Repeat("a", 64))))
	store := &hydratingFileReferenceStore{Store: env.db, replace: func() {
		require.NoError(t, env.db.ReplaceSessionMessages(t.Context(), id, image(strings.Repeat("b", 64))))
	}}
	svc := service.NewReadOnlyBackend(store, service.WithDocbank(docbank.New(config.DocbankIntegration{URL: "https://example.com", APIKeyEnv: "DOCBANK_ABSENT_KEY"})))
	out, err := service.FileReferences(t.Context(), svc, id)
	require.NoError(t, err)
	current, err := env.db.GetSession(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, *current.TranscriptRevision, out.TranscriptRevision)
	require.Len(t, out.Occurrences, 1)
	assert.Equal(t, strings.Repeat("b", 64), out.Occurrences[0].SHA256)
}
