package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewHTTPBackendUsesLongRunningClient(t *testing.T) {
	t.Parallel()
	svc := NewHTTPBackend("http://example.test", "", false)
	backend, ok := svc.(*httpBackend)
	require.True(t, ok)
	require.NotNil(t, backend.client)
	require.NotNil(t, backend.longRunningClient)

	assert.Equal(t, 30*time.Second, backend.client.Timeout)
	assert.Zero(t, backend.longRunningClient.Timeout)
}

func TestHTTPBackendRecallCapabilityRespectsReadOnlyMode(t *testing.T) {
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
			svc := NewHTTPBackend("http://example.test", "", tt.readOnly)
			assert.Equal(t, tt.want, SupportsRecallQueries(svc))
		})
	}
}

func TestSearchContentUsesLongRunningClient(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"matches":[]}`))
	}))
	t.Cleanup(srv.Close)

	svc := NewHTTPBackend(srv.URL, "", false)
	backend, ok := svc.(*httpBackend)
	require.True(t, ok)
	backend.client.Timeout = 10 * time.Millisecond

	result, err := svc.SearchContent(context.Background(), ContentSearchRequest{
		Pattern: "slow first query",
		Mode:    "semantic",
	})
	require.NoError(t, err)
	assert.Empty(t, result.Matches)
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
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				time.Sleep(50 * time.Millisecond)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"mode":"` + tt.wantMode + `","recall_entries":[]}`))
			}))
			t.Cleanup(srv.Close)

			svc := NewHTTPBackend(srv.URL, "", false)
			backend, ok := svc.(*httpBackend)
			require.True(t, ok)
			backend.client.Timeout = 10 * time.Millisecond

			result, err := svc.QueryRecallEntries(context.Background(), RecallQuery{
				Query: "connection storm", Mode: tt.inputMode,
			})
			require.NoError(t, err)
			assert.Equal(t, tt.wantMode, result.Mode)
			assert.Empty(t, result.RecallEntries)
		})
	}
}
