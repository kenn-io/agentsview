package servicehttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

// fixedServer answers every request with one status and JSON body and
// counts how many requests reached it.
func fixedServer(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

func TestGetMapsDaemonAnswers(t *testing.T) {
	t.Run("missing session is nil without error", func(t *testing.T) {
		srv, _ := fixedServer(t, http.StatusNotFound, `{"error":"session not found"}`)
		detail, err := NewHTTPBackend(srv.URL, "", false, "").Get(t.Context(), "claude:gone")
		require.NoError(t, err)
		assert.Nil(t, detail)
	})
	t.Run("server failure keeps status and message", func(t *testing.T) {
		srv, _ := fixedServer(t, http.StatusInternalServerError, `{"error":"database is locked"}`)
		detail, err := NewHTTPBackend(srv.URL, "", false, "").Get(t.Context(), "claude:s1")
		require.Error(t, err)
		assert.Nil(t, detail)
		status, message, ok := HTTPErrorDetails(err)
		require.True(t, ok)
		assert.Equal(t, http.StatusInternalServerError, status)
		assert.Equal(t, "database is locked", message)
		assert.False(t, IsRequestRejected(err), "a 5xx means the daemon failed, not that it rejected the request")
	})
}

func TestHTTPErrorDetailsWithoutJSONBodyHasNoMessage(t *testing.T) {
	srv, _ := fixedServer(t, http.StatusBadGateway, `<html>upstream down</html>`)
	_, err := NewHTTPBackend(srv.URL, "", false, "").ToolCalls(t.Context(), "claude:s1")
	status, message, ok := HTTPErrorDetails(err)
	require.True(t, ok)
	assert.Equal(t, http.StatusBadGateway, status)
	assert.Empty(t, message, "raw non-JSON bodies must not leak into the CLI message")
}

func TestSearchNotImplementedIsSearchUnavailable(t *testing.T) {
	srv, _ := fixedServer(t, http.StatusNotImplemented, `{"error":"no FTS index"}`)
	_, err := NewHTTPBackend(srv.URL, "", false, "").Search(
		t.Context(), service.SearchRequest{Query: "retry"},
	)
	require.ErrorIs(t, err, service.ErrSearchUnavailable)
}

// A 501 from search/content explains why semantic search is unavailable;
// the client keeps the server's reason while classifying the error.
func TestSearchContentNotImplementedKeepsServerReason(t *testing.T) {
	sentinel := db.ErrSemanticUnavailable.Error()
	tests := []struct {
		name    string
		body    string
		wantMsg string
	}{
		{
			name:    "bare sentinel",
			body:    `{"error":"` + sentinel + `"}`,
			wantMsg: sentinel,
		},
		{
			name:    "empty body",
			body:    ``,
			wantMsg: sentinel,
		},
		{
			name:    "sentinel with building progress",
			body:    `{"error":"` + sentinel + `: index is building: 40% complete"}`,
			wantMsg: sentinel + ": index is building: 40% complete",
		},
		{
			name:    "remote reason",
			body:    `{"error":"semantic search not available: no pushed embeddings generation"}`,
			wantMsg: "semantic search not available: no pushed embeddings generation",
		},
		{
			name:    "unexpected wording",
			body:    `{"error":"vector extension missing"}`,
			wantMsg: "semantic search not available: vector extension missing",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := fixedServer(t, http.StatusNotImplemented, tt.body)
			_, err := NewHTTPBackend(srv.URL, "", false, "").SearchContent(
				t.Context(), service.ContentSearchRequest{Pattern: "auth", Mode: "semantic"},
			)
			require.ErrorIs(t, err, db.ErrSemanticUnavailable)
			assert.EqualError(t, err, tt.wantMsg)
		})
	}
}

func TestQueryRecallErrorMapping(t *testing.T) {
	tests := []struct {
		name          string
		mode          string
		status        int
		body          string
		wantIs        error
		wantNotIs     error
		wantMsgSubstr string
	}{
		{
			name: "vector endpoint outage is transient", mode: "vector",
			status: http.StatusServiceUnavailable, body: `{"error":"embeddings endpoint timed out"}`,
			wantIs: db.ErrSemanticTransient, wantNotIs: db.ErrSemanticUnavailable,
			wantMsgSubstr: "embeddings endpoint timed out",
		},
		{
			name: "vector on a remote read-only daemon", mode: "vector",
			status: http.StatusNotImplemented, body: `{"error":"not available in remote mode"}`,
			wantIs: db.ErrReadOnly, wantNotIs: db.ErrSemanticUnavailable,
		},
		{
			name: "hybrid without embeddings", mode: "hybrid",
			status: http.StatusNotImplemented, body: `{"error":"semantic search not available: no generation"}`,
			wantIs: db.ErrSemanticUnavailable, wantMsgSubstr: "no generation",
		},
		{
			name: "lexical 501 is read-only", mode: "",
			status: http.StatusNotImplemented, body: `{"error":"semantic search not available: no generation"}`,
			wantIs: db.ErrReadOnly, wantNotIs: db.ErrSemanticUnavailable,
		},
		{
			name: "lexical 503 is not a semantic outage", mode: "lexical",
			status: http.StatusServiceUnavailable, body: `{"error":"busy"}`,
			wantNotIs: db.ErrSemanticTransient, wantMsgSubstr: "HTTP 503",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := fixedServer(t, tt.status, tt.body)
			_, err := NewHTTPBackend(srv.URL, "", false, "").QueryRecallEntries(
				t.Context(), service.RecallQuery{Query: "connection storm", Mode: tt.mode},
			)
			require.Error(t, err)
			if tt.wantIs != nil {
				require.ErrorIs(t, err, tt.wantIs)
			}
			if tt.wantNotIs != nil {
				require.NotErrorIs(t, err, tt.wantNotIs)
			}
			if tt.wantMsgSubstr != "" {
				assert.Contains(t, err.Error(), tt.wantMsgSubstr)
			}
		})
	}
}

// A daemon that silently answers a vector query with lexical results must not
// be presented to the caller as a semantic answer.
func TestQueryRecallRejectsDowngradedMode(t *testing.T) {
	srv, _ := fixedServer(t, http.StatusOK, `{"mode":"lexical","recall_entries":[]}`)
	result, err := NewHTTPBackend(srv.URL, "", false, "").QueryRecallEntries(
		t.Context(), service.RecallQuery{Query: "connection storm", Mode: "vector"},
	)
	require.EqualError(t, err, "recall query: requested recall mode vector but daemon returned lexical")
	assert.Nil(t, result)
}

func TestReadOnlyBackendRefusesWritesWithoutRequest(t *testing.T) {
	tests := []struct {
		name string
		call func(context.Context, *httpBackend) error
	}{
		{"sync", func(ctx context.Context, b *httpBackend) error {
			_, err := b.Sync(ctx, service.SyncInput{ID: "claude:s1"})
			return err
		}},
		{"recall import", func(ctx context.Context, b *httpBackend) error {
			_, err := b.ImportRecallEntries(ctx, strings.NewReader("{}\n"), db.RecallImportOptions{})
			return err
		}},
		{"secret scan", func(ctx context.Context, b *httpBackend) error {
			_, err := b.ScanSecrets(ctx, service.SecretScanInput{}, nil)
			return err
		}},
		{"diagnostic", func(ctx context.Context, b *httpBackend) error {
			_, err := b.AppendDiagnostic(ctx, db.DiagnosticEvent{Source: "cli", Message: "hello"})
			return err
		}},
		{"labels", func(ctx context.Context, b *httpBackend) error {
			_, err := b.UpdateSessionLabels(ctx, "claude:s1", []string{"keep"}, nil, false)
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, requests := fixedServer(t, http.StatusOK, `{}`)
			backend := NewHTTPBackend(srv.URL, "", true, "").(*httpBackend)
			err := tt.call(t.Context(), backend)
			require.ErrorIs(t, err, db.ErrReadOnly)
			assert.Zero(t, requests.Load(), "a read-only backend must not send writes")
		})
	}
}

// A writable client talking to a daemon whose store cannot serve the route
// (a 501) reports db.ErrReadOnly so callers can explain the remote mode.
func TestNotImplementedDaemonMapsToReadOnly(t *testing.T) {
	tests := []struct {
		name string
		call func(context.Context, *httpBackend) error
	}{
		{"sync", func(ctx context.Context, b *httpBackend) error {
			_, err := b.Sync(ctx, service.SyncInput{ID: "claude:s1"})
			return err
		}},
		{"stats", func(ctx context.Context, b *httpBackend) error {
			_, err := b.Stats(ctx, service.StatsFilter{})
			return err
		}},
		{"usage summary", func(ctx context.Context, b *httpBackend) error {
			_, err := b.UsageSummary(ctx, service.UsageRequest{})
			return err
		}},
		{"usage comparison", func(ctx context.Context, b *httpBackend) error {
			_, err := b.UsagePairwiseComparison(ctx, service.UsagePairwiseComparisonRequest{})
			return err
		}},
		{"recall list", func(ctx context.Context, b *httpBackend) error {
			_, err := b.ListRecallEntries(ctx, service.RecallFilter{})
			return err
		}},
		{"recall get", func(ctx context.Context, b *httpBackend) error {
			_, err := b.GetRecallEntry(ctx, "entry-1")
			return err
		}},
		{"recall import", func(ctx context.Context, b *httpBackend) error {
			_, err := b.ImportRecallEntries(ctx, strings.NewReader("{}\n"), db.RecallImportOptions{})
			return err
		}},
		{"secret scan", func(ctx context.Context, b *httpBackend) error {
			_, err := b.ScanSecrets(ctx, service.SecretScanInput{}, nil)
			return err
		}},
		{"friction digests", func(ctx context.Context, b *httpBackend) error {
			_, err := b.FrictionDigests(ctx, "", "")
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, requests := fixedServer(t, http.StatusNotImplemented, `{"error":"not available in remote mode"}`)
			backend := NewHTTPBackend(srv.URL, "", false, "").(*httpBackend)
			err := tt.call(t.Context(), backend)
			require.ErrorIs(t, err, db.ErrReadOnly)
			assert.Equal(t, int32(1), requests.Load())
		})
	}
}

func TestMessagesStatusMapping(t *testing.T) {
	t.Run("501 means revision-bound reads are unavailable", func(t *testing.T) {
		srv, _ := fixedServer(t, http.StatusNotImplemented, `{"error":"revision-bound reads unavailable"}`)
		_, err := NewHTTPBackend(srv.URL, "", false, "").Messages(
			t.Context(), "claude:s1", service.MessageFilter{ExpectedRevision: "rev-1"},
		)
		require.ErrorIs(t, err, service.ErrRevisionBoundReadUnavailable)
	})
	t.Run("409 without source_changed stays a plain conflict", func(t *testing.T) {
		srv, _ := fixedServer(t, http.StatusConflict, `{"error":"cursor expired"}`)
		_, err := NewHTTPBackend(srv.URL, "", false, "").Messages(
			t.Context(), "claude:s1", service.MessageFilter{},
		)
		require.Error(t, err)
		require.NotErrorIs(t, err, service.ErrSourceChanged)
		status, message, ok := HTTPErrorDetails(err)
		require.True(t, ok)
		assert.Equal(t, http.StatusConflict, status)
		assert.Equal(t, "cursor expired", message)
	})
}

// An older daemon ignores raw_suffix and answers with substring matches;
// the client must not hand those back as exact raw-suffix matches.
func TestFindSessionIDsByRawSuffixRequiresServerAcknowledgement(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    []string
		wantErr string
	}{
		{name: "older server", body: `{"ids":["claude:abc123","claude:xabc"]}`, wantErr: "server does not acknowledge raw session ID lookup"},
		{name: "explicit false", body: `{"ids":["claude:abc123"],"raw_suffix":false}`, wantErr: "server does not acknowledge raw session ID lookup"},
		{name: "acknowledged", body: `{"ids":["claude:abc"],"raw_suffix":true}`, want: []string{"claude:abc"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := fixedServer(t, http.StatusOK, tt.body)
			ids, err := NewHTTPBackend(srv.URL, "", false, "").FindSessionIDsByRawSuffix(t.Context(), "abc", 5)
			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				assert.Nil(t, ids)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, ids)
		})
	}
}

func TestSessionUsageVersionGateAndMissingSession(t *testing.T) {
	t.Run("old server cannot combine subagent usage", func(t *testing.T) {
		srv, requests := fixedServer(t, http.StatusOK, `{}`)
		backend := NewHTTPBackendForServer(srv.URL, "", HTTPServerCapabilities{APIVersion: 5})
		_, err := backend.SessionUsage(t.Context(), service.SessionUsageRequest{ID: "claude:s1"})
		require.ErrorContains(t, err, "server API version 5 does not support combined subagent usage")
		assert.Zero(t, requests.Load())
	})
	t.Run("old server still serves own-session usage", func(t *testing.T) {
		srv, requests := fixedServer(t, http.StatusOK, `{"session_id":"claude:s1","agent":"claude","total_output_tokens":42}`)
		backend := NewHTTPBackendForServer(srv.URL, "", HTTPServerCapabilities{APIVersion: 5})
		report, err := backend.SessionUsage(t.Context(), service.SessionUsageRequest{ID: "claude:s1", OwnOnly: true})
		require.NoError(t, err)
		require.NotNil(t, report)
		assert.Equal(t, 42, report.TotalOutputTokens)
		assert.Equal(t, int32(1), requests.Load())
	})
	t.Run("missing session is nil without error", func(t *testing.T) {
		srv, _ := fixedServer(t, http.StatusNotFound, `{"error":"session not found"}`)
		report, err := NewHTTPBackend(srv.URL, "", false, "").SessionUsage(
			t.Context(), service.SessionUsageRequest{ID: "claude:gone"},
		)
		require.NoError(t, err)
		assert.Nil(t, report)
	})
}

func TestWatchMapsHTTPFailures(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		wantErr string
	}{
		{name: "missing session", status: http.StatusNotFound, wantErr: "watch: session not found: claude:gone"},
		{name: "server failure", status: http.StatusInternalServerError, wantErr: "watch: HTTP 500"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := fixedServer(t, tt.status, `{"error":"nope"}`)
			events, err := NewHTTPBackend(srv.URL, "", false, "").Watch(t.Context(), "claude:gone")
			require.EqualError(t, err, tt.wantErr)
			assert.Nil(t, events)
		})
	}
}

// A missing session is the caller's mistake, so it classifies as a rejected
// request and as service.ErrSessionNotFound rather than a daemon failure.
func TestFileReferencesMissingSessionIsRejectedRequest(t *testing.T) {
	srv, _ := fixedServer(t, http.StatusNotFound, `{"error":"session not found"}`)
	backend := NewHTTPBackend(srv.URL, "", false, "").(*httpBackend)
	_, err := backend.FileReferences(t.Context(), "claude:gone")
	require.ErrorIs(t, err, service.ErrSessionNotFound)
	assert.True(t, IsRequestRejected(err))
}

func TestAppendDiagnosticOnOlderDaemonExplainsUnavailable(t *testing.T) {
	srv, _ := fixedServer(t, http.StatusNotFound, `404 page not found`)
	backend := NewHTTPBackend(srv.URL, "", false, "").(*httpBackend)
	_, err := backend.AppendDiagnostic(t.Context(), db.DiagnosticEvent{Source: "cli", Message: "hello"})
	require.ErrorContains(t, err, "diagnostic writes are unavailable")
	assert.True(t, IsRequestRejected(err))
}

func TestFrictionDigestLatestWithNoDigestsIsNotFound(t *testing.T) {
	srv, requests := fixedServer(t, http.StatusOK, `{"digests":[]}`)
	backend := NewHTTPBackend(srv.URL, "", false, "").(*httpBackend)
	view, err := backend.FrictionDigest(t.Context(), "")
	require.ErrorIs(t, err, service.ErrFrictionDigestNotFound)
	assert.Nil(t, view)
	assert.Equal(t, int32(1), requests.Load(), "no digest date means no detail read")
}
