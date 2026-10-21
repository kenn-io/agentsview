package mcp

import (
	"context"
	"crypto/sha256"
	jsonv1 "encoding/json"
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
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/fileevidence"
	"go.kenn.io/agentsview/internal/integrations/docbank"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/servicehttp"
)

type fileResolver struct {
	page   docbank.Page
	calls  int
	lookup func(context.Context, docbank.Lookup) (docbank.Page, error)
}

func (r *fileResolver) Lookup(ctx context.Context, q docbank.Lookup) (docbank.Page, error) {
	r.calls++
	if r.lookup != nil {
		return r.lookup(ctx, q)
	}
	return r.page, nil
}
func (*fileResolver) Close() error { return nil }
func seedFileReferences(t *testing.T, n int) *db.DB {
	t.Helper()
	d := dbtest.OpenTestDB(t)
	require.NoError(t, d.UpsertSession(t.Context(), db.Session{ID: "example", Agent: "pi", Project: "example"}))
	messages := make([]db.Message, n)
	for i := range messages {
		messages[i] = db.Message{SessionID: "example", Ordinal: i, Role: "user", Content: "attachment", FileEvidenceCoverage: fileevidence.Coverage{Version: 1, State: "complete"}, FileEvidenceCandidates: []fileevidence.Candidate{{Record: fileevidence.Record{Position: fileevidence.Position{Entry: strconv.Itoa(i), Call: -1, Result: -1, Block: 0}, Kind: "document", MediaType: "application/pdf", ProofMethod: fileevidence.ProofInlinePayloadV1, Retention: "document_metadata", SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("document"))), Size: new(int64(8))}, Payload: []byte("document"), Support: "message", SupportingText: "attachment"}}}
	}
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "example", messages))
	return d
}

func TestFileReferencesMCPParity(t *testing.T) {
	d := seedFileReferences(t, 2)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	resolver := &fileResolver{page: docbank.Page{State: "not_found", CheckedAt: &now, ReferenceTotal: new(0)}}
	svc := service.NewDirectBackend(d, nil, service.WithFileReferencesResolver(resolver))
	expected, err := svc.(service.FileReferencesService).FileReferences(t.Context(), service.FileReferencesInput{SessionID: "example", Limit: 1})
	require.NoError(t, err)
	require.Len(t, expected.Occurrences, 1)
	assert.Equal(t, "not_found", expected.Occurrences[0].State)
	for _, profile := range []Profile{ProfileFull, ProfileMemory} {
		ss, cs := newInMemoryPair(t, newServer(ServeOptions{Service: svc, Profile: profile}))
		if profile == ProfileFull {
			result, err := cs.CallTool(t.Context(), callParams("get_session_file_references", map[string]any{"session_id": "example", "limit": 1}))
			require.NoError(t, err)
			require.False(t, result.IsError)
			raw, err := json.Marshal(result.StructuredContent)
			require.NoError(t, err)
			var got service.FileReferencesResult
			require.NoError(t, json.Unmarshal(raw, &got))
			assert.Equal(t, expected, &got)
		}
		before := resolver.calls
		ordinary, err := cs.CallTool(t.Context(), callParams(ToolGetMessages, map[string]any{"session_id": "example"}))
		require.NoError(t, err)
		require.False(t, ordinary.IsError)
		raw, err := json.Marshal(ordinary)
		require.NoError(t, err)
		assert.NotContains(t, string(raw), "file_evidence")
		assert.NotContains(t, string(raw), "sha256")
		assert.Equal(t, before, resolver.calls)
		require.NoError(t, cs.Close())
		require.NoError(t, ss.Wait())
	}
}

func TestFileReferencesActualWireCapsAndRawContinuation(t *testing.T) {
	for _, count := range []int{3, 100, 4097} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			d := seedFileReferences(t, 50)
			var requests atomic.Int32
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				assert.Equal(t, "test-key", r.Header.Get("X-Api-Key"))
				assert.Equal(t, "/api/v1/content-references", r.URL.Path)
				assert.Equal(t, "100", r.URL.Query().Get("limit"))
				assert.Len(t, r.URL.Query(), 3)
				offset, err := strconv.Atoi(r.URL.Query().Get("offset"))
				if !assert.NoError(t, err) {
					return
				}
				items := []any{}
				total := count
				if count == 3 {
					total = 4
				}
				nameBytes := 4096
				if count == 4097 {
					total, nameBytes = 1, 4097
				}
				for i := offset; i < total; i++ {
					id := i + 1
					if count == 3 && i == 1 {
						id = 1
					} // A,A,B,C on the raw remote page.
					version := fmt.Sprintf("00000000-0000-4000-8000-%012d", id)
					items = append(items, map[string]any{"version": map[string]any{"id": version, "node_id": id, "blob_hash": r.URL.Query().Get("sha256"), "size": 8}, "node": map[string]any{"id": id, "kind": "file", "current_version_id": version, "name": strings.Repeat("<", nameBytes)}, "path": strings.Repeat("\u2028", 1365), "is_current": true})
				}
				raw, err := json.Marshal(map[string]any{"items": items, "total": total, "offset": offset, "limit": 100})
				if !assert.NoError(t, err) {
					return
				}
				if !assert.Less(t, len(raw), 1<<20) {
					return
				}
				_, err = w.Write(raw)
				if !assert.NoError(t, err) {
					return
				}
			}))
			defer remote.Close()
			t.Setenv("DOCBANK_WIRE_TEST_KEY", "test-key")
			resolver, err := docbank.NewResolver(t.Context(), config.DocbankIntegration{Enabled: true, URL: remote.URL, APIKeyEnv: "DOCBANK_WIRE_TEST_KEY"})
			require.NoError(t, err)
			defer resolver.Close()
			svc := service.NewDirectBackend(d, nil, service.WithFileReferencesResolver(resolver))
			direct, err := svc.(service.FileReferencesService).FileReferences(t.Context(), service.FileReferencesInput{SessionID: "example", Limit: 50})
			require.NoError(t, err)
			require.Len(t, direct.Occurrences, 50)
			assert.Equal(t, int32(1), requests.Load(), "one digest lookup despite repeated occurrences")
			canonical, err := json.Marshal(direct)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(canonical, direct))
			cfg := config.Config{Host: "127.0.0.1", DataDir: t.TempDir(), RequireAuth: true, AuthToken: "test-token"}
			srv := server.New(cfg, d, nil, server.WithFileReferencesResolver(resolver))
			defer srv.Shutdown(t.Context())
			request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/sessions/example/file-references?limit=50", nil)
			request.Header.Set("Authorization", "Bearer test-token")
			request.Host = "127.0.0.1:0"
			request.Header.Set(service.FileReferencesIntentHeader, "docbank")
			response := httptest.NewRecorder()
			srv.Handler().ServeHTTP(response, request)
			require.Equal(t, 200, response.Code, response.Body.String())
			require.LessOrEqual(t, response.Body.Len(), service.FileReferencesMaxBytes)
			var fromHTTP service.FileReferencesResult
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &fromHTTP))
			assert.Equal(t, direct, &fromHTTP)
			daemon := httptest.NewServer(srv.Handler())
			defer daemon.Close()
			adapter := servicehttp.NewHTTPBackend(daemon.URL, "test-token", true, "")
			fromAdapter, err := adapter.FileReferences(t.Context(), service.FileReferencesInput{SessionID: "example", Limit: 50})
			require.NoError(t, err)
			assert.Equal(t, direct, fromAdapter)
			ss, cs := newInMemoryPair(t, newServer(ServeOptions{Service: adapter}))
			defer ss.Wait()
			defer cs.Close()
			result, err := cs.CallTool(t.Context(), callParams("get_session_file_references", map[string]any{"session_id": "example", "limit": 50}))
			require.NoError(t, err)
			require.False(t, result.IsError)
			wire, err := jsonv1.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
			require.NoError(t, err)
			require.LessOrEqual(t, len(wire), service.FileReferencesMaxBytes)
			var fromMCP service.FileReferencesResult
			raw, err := json.Marshal(result.StructuredContent)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &fromMCP))
			assert.Equal(t, direct, &fromMCP)
			if count == 4097 {
				for _, occurrence := range direct.Occurrences {
					assert.Equal(t, "unavailable", occurrence.State)
					assert.Empty(t, occurrence.Matches)
					assert.Nil(t, occurrence.ReferenceTotal)
				}
				return
			}
			trimmed := false
			for _, occurrence := range direct.Occurrences {
				if occurrence.NextReferenceOffset == nil {
					continue
				}
				trimmed = true
				next := *occurrence.NextReferenceOffset
				selected, err := svc.(service.FileReferencesService).FileReferences(t.Context(), service.FileReferencesInput{SessionID: "example", Binding: direct.Binding, OccurrenceID: occurrence.OccurrenceID, ReferenceOffset: next})
				require.NoError(t, err)
				require.Len(t, selected.Occurrences, 1)
				require.NotEmpty(t, selected.Occurrences[0].Matches)
				assert.GreaterOrEqual(t, selected.Occurrences[0].Matches[0].NodeID, int64(next+1))
				if count == 3 {
					require.Len(t, occurrence.Matches, 1, "cap splits A,A,B,C after A")
					assert.Equal(t, 2, next)
					assert.Equal(t, int64(3), selected.Occurrences[0].Matches[0].NodeID)
				}
				break
			}
			assert.True(t, trimmed, "the cap must exercise raw-offset continuation")
			exhausted, err := svc.FileReferences(t.Context(), service.FileReferencesInput{SessionID: "example", Binding: direct.Binding, OccurrenceID: direct.Occurrences[0].OccurrenceID, ReferenceOffset: 500})
			require.NoError(t, err)
			require.Len(t, exhausted.Occurrences, 1)
			assert.Equal(t, "found", exhausted.Occurrences[0].State)
			assert.Empty(t, exhausted.Occurrences[0].Matches)
		})
	}
}

func TestFileReferencesHTTPRevalidatesAfterRemote(t *testing.T) {
	for _, mutation := range []string{"generation", "policy"} {
		t.Run(mutation, func(t *testing.T) {
			d := seedFileReferences(t, 1)
			entered, release := make(chan struct{}), make(chan struct{})
			resolver := &fileResolver{lookup: func(context.Context, docbank.Lookup) (docbank.Page, error) {
				close(entered)
				<-release
				return docbank.Page{State: "found", ReferenceTotal: new(1), MatchOffsets: []int{0}, Matches: []docbank.Match{{NodeID: 1, Name: "stale-result"}}}, nil
			}}
			srv := server.New(config.Config{Host: "127.0.0.1", DataDir: t.TempDir()}, d, nil, server.WithFileReferencesResolver(resolver))
			defer srv.Shutdown(t.Context())
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/sessions/example/file-references", nil)
			req.Host = "127.0.0.1:0"
			req.Header.Set(service.FileReferencesIntentHeader, "docbank")
			response := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { defer close(done); srv.Handler().ServeHTTP(response, req) }()
			<-entered
			if mutation == "generation" {
				require.NoError(t, d.SetDatabaseIDForTest(t.Context(), "replacement"))
			} else {
				messages, err := d.GetAllMessages(t.Context(), "example")
				require.NoError(t, err)
				d.SetArchiveContent(config.ArchiveContentUsage)
				require.NoError(t, d.ReplaceSessionMessages(t.Context(), "example", messages))
			}
			close(release)
			<-done
			assert.Equal(t, 409, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), "source_changed")
			assert.NotContains(t, response.Body.String(), "stale-result")
		})
	}
}

func TestFileReferencesOrdinaryReadsNeverResolve(t *testing.T) {
	d := seedFileReferences(t, 1)
	require.NoError(t, d.UpsertSession(t.Context(), db.Session{ID: "claude", Agent: "claude", Project: "example"}))
	// This normalized text could equal a different LF-only file in Docbank.
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "claude", []db.Message{{SessionID: "claude", Role: "assistant", Content: "Read example.txt", ToolCalls: []db.ToolCall{{ToolName: "Read", Category: "Read", InputJSON: `{"file_path":"example.txt"}`, ResultContent: "alpha\n"}}}}))
	var requests atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected lookup", http.StatusInternalServerError)
	}))
	defer remote.Close()
	t.Setenv("DOCBANK_ORDINARY_TEST_KEY", "test-key")
	resolver, err := docbank.NewResolver(t.Context(), config.DocbankIntegration{Enabled: true, URL: remote.URL, APIKeyEnv: "DOCBANK_ORDINARY_TEST_KEY"})
	require.NoError(t, err)
	defer resolver.Close()
	svc := service.NewDirectBackend(d, nil, service.WithFileReferencesResolver(resolver))
	_, err = svc.Messages(t.Context(), "example", service.MessageFilter{})
	require.NoError(t, err)
	_, err = svc.SearchContent(t.Context(), service.ContentSearchRequest{Pattern: "attachment", Mode: "substring", IncludeOneShot: true})
	require.NoError(t, err)
	_, err = svc.ToolCalls(t.Context(), "claude")
	require.NoError(t, err)
	out, err := svc.FileReferences(t.Context(), service.FileReferencesInput{SessionID: "claude"})
	require.NoError(t, err)
	assert.Empty(t, out.Occurrences)
	assert.Equal(t, "unavailable", out.Coverage)
	srv := server.New(config.Config{Host: "127.0.0.1", DataDir: t.TempDir()}, d, nil, server.WithFileReferencesResolver(resolver))
	defer srv.Shutdown(t.Context())
	for _, path := range []string{"messages", "md", "export"} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/sessions/example/"+path, nil)
		req.Host = "127.0.0.1:0"
		res := httptest.NewRecorder()
		srv.Handler().ServeHTTP(res, req)
		require.Equal(t, 200, res.Code)
	}
	ss, cs := newInMemoryPair(t, newServer(ServeOptions{Service: svc}))
	defer ss.Wait()
	defer cs.Close()
	result, err := cs.CallTool(t.Context(), callParams(ToolGetMessages, map[string]any{"session_id": "example"}))
	require.NoError(t, err)
	require.False(t, result.IsError)
	assert.Zero(t, requests.Load())
}
