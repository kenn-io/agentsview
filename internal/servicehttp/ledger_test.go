package servicehttp

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/ledger"
	"go.kenn.io/agentsview/internal/service"
)

func TestHTTPBackendLedgerCapabilityAndQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/version":
			_, _ = w.Write([]byte(`{"api_version":10,"ledger_available":true}`))
		case "/api/v1/ledger/events":
			assert.Equal(t, "api", r.URL.Query().Get("subsystem"))
			assert.Equal(t, "5", r.URL.Query().Get("limit"))
			_, _ = w.Write([]byte(`{"since":"7d","results":[{"zone":"default","events":[{"serde":"{\"event_id\":\"00000000-0000-0000-0000-000000000001\",\"zone\":\"default\",\"source\":\"host-a\",\"source_seq\":1,\"timestamp\":\"2026-01-01T00:00:00.000000Z\",\"correlation_id\":null,\"causation_id\":null,\"actor_ref\":null,\"object_ref\":null,\"event_class\":\"health\",\"payload_tier\":\"structured\",\"payload\":{\"n\":18446744073709551615}}"}]}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	capabilities, err := ProbeHTTPServerCapabilities(t.Context(), server.URL, "")
	require.NoError(t, err)
	require.True(t, capabilities.LedgerQueries)
	svc := NewHTTPBackendForServer(server.URL, "", capabilities)
	require.True(t, service.SupportsLedgerQueries(svc))
	results, err := service.LedgerQuery(t.Context(), svc, ledger.Query{
		Since: time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC), Subsystems: []string{"api"}, Limit: 5,
	})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Len(t, results[0].Events, 1)
	serialized, err := ledger.FormatJSON(results)
	require.NoError(t, err)
	assert.Contains(t, string(serialized), `18446744073709551615`)
	assert.False(t, service.SupportsLedgerQueries(NewHTTPBackendForServer(server.URL, "", HTTPServerCapabilities{})))
}
