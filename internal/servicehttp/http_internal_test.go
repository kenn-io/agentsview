package servicehttp

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/service"
)

func TestNewHTTPBackendUsesLongRunningClient(t *testing.T) {
	t.Parallel()
	svc := NewHTTPBackend("http://example.test", "", false, "")
	backend, ok := svc.(*httpBackend)
	require.True(t, ok)
	require.NotNil(t, backend.client)
	require.NotNil(t, backend.longRunningClient)

	assert.Equal(t, 30*time.Second, backend.client.Timeout)
	assert.Zero(t, backend.longRunningClient.Timeout)
}

func TestHTTPBackendRecallCapabilityRespectsReadOnlyMode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		readOnly bool
		want     bool
	}{
		{name: "writable daemon", want: true},
		{name: "read-only daemon", readOnly: true, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			svc := NewHTTPBackend("http://example.test", "", tt.readOnly, "")
			assert.Equal(t, tt.want, service.SupportsRecallQueries(svc))
		})
	}
}

func TestListForwardsListOptions(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "America/New_York", r.URL.Query().Get("timezone"))
		assert.Equal(t, "next-page", r.URL.Query().Get("cursor"))
		assert.Equal(t, "true", r.URL.Query().Get("include_source"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sessions":[]}`))
	}))
	t.Cleanup(srv.Close)
	_, err := NewHTTPBackend(srv.URL, "", false, "").List(t.Context(), service.ListFilter{Timezone: "America/New_York", Cursor: "next-page", IncludeSource: true})
	require.NoError(t, err)
}

func TestSearchContentUsesLongRunningClient(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(50 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"matches":[]}`))
		}))
		transport := srv.Client().Transport

		svc := NewHTTPBackend(srv.URL, "", false, "")
		backend, ok := svc.(*httpBackend)
		require.True(t, ok)
		backend.client.Transport = transport
		backend.longRunningClient.Transport = transport
		backend.client.Timeout = 10 * time.Millisecond

		result, err := svc.SearchContent(t.Context(), service.ContentSearchRequest{
			Pattern: "slow first query",
			Mode:    "semantic",
		})
		require.NoError(t, err)
		assert.Empty(t, result.Matches)
	})
}

func TestSearchContentForwardsRecallContract(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		assert.Equal(t, "terms", query.Get("mode"))
		assert.Equal(t, "all", query.Get("scope"))
		assert.Equal(t, "target-session", query.Get("session_id"))
		assert.Equal(t, "feature/memory", query.Get("git_branch_exact"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"matches":[]}`))
	}))
	t.Cleanup(srv.Close)

	_, err := NewHTTPBackend(srv.URL, "", false, "").SearchContent(
		t.Context(), service.ContentSearchRequest{
			Pattern: "alpha beta", Mode: "terms", Scope: "all",
			SessionID: "target-session", GitBranchExact: "feature/memory",
		},
	)
	require.NoError(t, err)
}

func TestMessagesForwardsRevisionBinding(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "rev-1", r.URL.Query().Get("expected_revision"))
		assert.Equal(t, "archive-binding", r.URL.Query().Get("evidence_source"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"messages":[],"count":0,"transcript_revision":"rev-1","evidence_source":"archive-binding"}`))
	}))
	t.Cleanup(srv.Close)

	result, err := NewHTTPBackend(srv.URL, "", false, "").Messages(
		t.Context(), "session", service.MessageFilter{
			ExpectedRevision: "rev-1", EvidenceSource: "archive-binding",
		},
	)
	require.NoError(t, err)
	assert.Equal(t, "rev-1", result.TranscriptRevision)
	assert.Equal(t, "archive-binding", result.EvidenceSource)
}

func TestMessagesMapsSourceChangedConflict(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"status":409,"detail":"source_changed: transcript revision does not match"}`))
	}))
	t.Cleanup(srv.Close)

	_, err := NewHTTPBackend(srv.URL, "", false, "").Messages(
		t.Context(), "session", service.MessageFilter{ExpectedRevision: "old"},
	)
	require.ErrorIs(t, err, service.ErrSourceChanged)
}

func TestUsageSummaryUsesLongRunningClient(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/api/v1/usage/summary", r.URL.Path)
			time.Sleep(50 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"daily":[{"date":"2026-09-01"}]}`))
		}))
		transport := srv.Client().Transport
		backend := NewHTTPBackend(srv.URL, "", false, "").(*httpBackend)
		backend.client.Transport = transport
		backend.longRunningClient.Transport = transport
		backend.client.Timeout = 10 * time.Millisecond
		result, err := backend.UsageSummary(t.Context(), service.UsageRequest{})
		require.NoError(t, err)
		require.Len(t, result.Daily, 1)
		assert.Equal(t, "2026-09-01", result.Daily[0].Date)
	})
}

func TestUsagePairwiseComparisonUsesLongRunningClient(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/api/v1/usage/pairwise-comparison", r.URL.Path)
			time.Sleep(50 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"left":{"totalTokens":42}}`))
		}))
		transport := srv.Client().Transport
		backend := NewHTTPBackend(srv.URL, "", false, "").(*httpBackend)
		backend.client.Transport = transport
		backend.longRunningClient.Transport = transport
		backend.client.Timeout = 10 * time.Millisecond
		result, err := backend.UsagePairwiseComparison(t.Context(), service.UsagePairwiseComparisonRequest{})
		require.NoError(t, err)
		assert.Equal(t, 42, result.Left.TotalTokens)
	})
}

func TestQueryRecallSemanticModesUseLongRunningClient(t *testing.T) {
	tests := []struct {
		name      string
		inputMode string
		wantMode  string
	}{
		{name: "vector", inputMode: " VECTOR ", wantMode: "vector"},
		{name: "hybrid", inputMode: "Hybrid", wantMode: "hybrid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					time.Sleep(50 * time.Millisecond)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"mode":"` + tt.wantMode + `","recall_entries":[]}`))
				}))
				transport := srv.Client().Transport

				svc := NewHTTPBackend(srv.URL, "", false, "")
				backend, ok := svc.(*httpBackend)
				require.True(t, ok)
				backend.client.Transport = transport
				backend.longRunningClient.Transport = transport
				backend.client.Timeout = 10 * time.Millisecond

				result, err := svc.QueryRecallEntries(t.Context(), service.RecallQuery{
					Query: "connection storm", Mode: tt.inputMode,
				})
				require.NoError(t, err)
				assert.Equal(t, tt.wantMode, result.Mode)
				assert.Empty(t, result.RecallEntries)
			})
		})
	}
}
