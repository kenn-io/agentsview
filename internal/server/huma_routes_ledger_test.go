package server_test

import (
	"encoding/json/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/ledger"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/servicehttp"
)

func withLedger(zones ...string) setupOption {
	return func(c *config.Config) {
		c.Ledger.Enabled = true
		c.Ledger.Source = "host-a"
		for _, z := range zones {
			c.Ledger.Zones = append(c.Ledger.Zones, config.LedgerZoneConfig{ID: z})
		}
	}
}

type ledgerTestHostTransport struct {
	base http.RoundTripper
}

func (t ledgerTestHostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Host = "127.0.0.1:0"
	req.Header.Set("Origin", "http://127.0.0.1:0")
	return t.base.RoundTrip(req)
}

func TestLedgerRoutes(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T, te *testEnv)
	}{
		{"append_query_status_verify", func(t *testing.T, te *testEnv) {
			t.Helper()
			w := te.post(t, "/api/v1/ledger/events",
				`{"events":[{"event_class":"state-change","payload":{"subsystem":"api","summary":"hello","n":18446744073709551615,"f":1e+16}}]}`)
			require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
			var appended struct {
				Zone      string `json:"zone"`
				Source    string `json:"source"`
				SourceSeq uint64 `json:"source_seq"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &appended))
			assert.Equal(t, "default", appended.Zone)
			assert.Equal(t, "host-a", appended.Source, "the source is the server's own")
			assert.Equal(t, uint64(1), appended.SourceSeq)

			w = te.get(t, "/api/v1/ledger/events?subsystem=api&since=1h")
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var q struct {
				Results []struct {
					Zone   string `json:"zone"`
					Events []struct {
						Serde      string `json:"serde"`
						EventClass string `json:"event_class"`
						Subsystem  string `json:"subsystem"`
					} `json:"events"`
				} `json:"results"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &q))
			require.Len(t, q.Results, 1)
			require.Len(t, q.Results[0].Events, 1)
			assert.Equal(t, "state_change", q.Results[0].Events[0].EventClass)
			ev, err := ledger.ParseEventJSON([]byte(q.Results[0].Events[0].Serde))
			require.NoError(t, err)
			assert.Contains(t, q.Results[0].Events[0].Serde, `"n":18446744073709551615`)
			assert.Contains(t, q.Results[0].Events[0].Serde, `"f":1e+16`)
			assert.Equal(t, "hello", ledger.Summary(ev))

			w = te.get(t, "/api/v1/ledger/status")
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.Contains(t, w.Body.String(), `"events":1`)

			w = te.post(t, "/api/v1/ledger/verify", `{}`)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.Contains(t, w.Body.String(), `"newly_verified":1`)
		}},
		{"unconfigured_stored_zone_is_listed", func(t *testing.T, te *testEnv) {
			t.Helper()
			w := ledger.NewWriter(te.db, "pushed-zone", "host-b", nil)
			_, err := w.Append(t.Context(), []ledger.Event{{
				EventClass: ledger.ClassHealth, PayloadTier: ledger.TierStructured,
				Payload: map[string]any{"subsystem": "x", "summary": "from a laptop"},
			}})
			require.NoError(t, err)
			rec := te.get(t, "/api/v1/ledger/status")
			require.Equal(t, http.StatusOK, rec.Code)
			assert.Contains(t, rec.Body.String(), `"zone":"pushed-zone"`)
			rec = te.get(t, "/api/v1/ledger/events?since=1h")
			require.Equal(t, http.StatusOK, rec.Code)
			assert.Contains(t, rec.Body.String(), `"zone":"pushed-zone"`)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { tt.run(t, setup(t, withLedger())) })
	}
}

func TestLedgerSubsystemFilterIsRepeatableInOpenAPI(t *testing.T) {
	te := setup(t, withLedger())
	w := te.get(t, "/api/openapi.json")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var spec struct {
		Paths map[string]map[string]struct {
			Parameters []struct {
				Name    string `json:"name"`
				In      string `json:"in"`
				Explode bool   `json:"explode"`
			} `json:"parameters"`
		} `json:"paths"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &spec))
	get := spec.Paths["/api/v1/ledger/events"]["get"]
	found := false
	for _, parameter := range get.Parameters {
		if parameter.Name == "subsystem" && parameter.In == "query" {
			found = true
			assert.True(t, parameter.Explode,
				"repeated subsystem query values must be represented separately")
		}
	}
	assert.True(t, found, "the query operation should document subsystem")
}

func TestLedgerAppendGeneratedClientAcceptsCreatedResponse(t *testing.T) {
	te := setup(t, withLedger())
	httpServer := httptest.NewServer(te.handler)
	t.Cleanup(httpServer.Close)
	api, err := apiclient.NewHTTPClient(httpServer.URL, "", &http.Client{
		Transport: ledgerTestHostTransport{base: http.DefaultTransport},
	})
	require.NoError(t, err)
	response, err := api.PostAPIV1LedgerEventsWithResponse(
		t.Context(), &apiclient.PostAPIV1LedgerEventsRequestOptions{
			Body: &apiclient.LedgerAppendRequest{
				Events: []apiclient.LedgerAppendEvent{{EventClass: string(ledger.ClassHealth)}},
			},
		},
	)
	require.NoError(t, err)
	require.NotNil(t, response)
	assert.Equal(t, http.StatusCreated, response.StatusCode)
	require.NotNil(t, response.JSON201)
	assert.Equal(t, "default", response.JSON201.Zone)
	assert.Equal(t, "host-a", response.JSON201.Source)
	assert.GreaterOrEqual(t, response.JSON201.Checksum, int64(0))
	assert.LessOrEqual(t, response.JSON201.Checksum, int64(^uint32(0)))
}

func TestLedgerAppendRejections(t *testing.T) {
	tests := []struct {
		name     string
		opts     []setupOption
		body     string
		forward  bool
		wantCode int
		wantBody string
	}{
		{"ledger_off", nil, `{"events":[{"event_class":"health"}]}`, false, http.StatusConflict, "ledger_disabled"},
		{"forwarded_without_auth", []setupOption{withLedger()}, `{"events":[{"event_class":"health"}]}`, true, http.StatusForbidden, "only permitted from localhost"},
		{"bad_class", []setupOption{withLedger()}, `{"events":[{"event_class":"review"}]}`, false, http.StatusBadRequest, "unknown event class 'review'"},
		{"unknown_zone", []setupOption{withLedger()}, `{"zone":"ops","events":[{"event_class":"health"}]}`, false, http.StatusBadRequest, `zone \"ops\" is not configured`},
		{"no_events", []setupOption{withLedger()}, `{"events":[]}`, false, http.StatusBadRequest, "expected array length >= 1"},
		{"bad_uuid", []setupOption{withLedger()}, `{"events":[{"event_class":"health","correlation_id":"nope"}]}`, false, http.StatusBadRequest, "correlation_id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			te := setup(t, tt.opts...)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/ledger/events", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Origin", "http://127.0.0.1:0")
			if tt.forward {
				req.Header.Set("X-Forwarded-For", "203.0.113.7")
			}
			w := httptest.NewRecorder()
			te.handler.ServeHTTP(w, req)
			assert.Equal(t, tt.wantCode, w.Code, w.Body.String())
			assert.Contains(t, w.Body.String(), tt.wantBody)
			st, err := te.db.LedgerStatus(t.Context(), "default")
			require.NoError(t, err)
			assert.Equal(t, 0, st.Segments, "nothing was written")
		})
	}
}

func TestLedgerMutationRoutesRejectReadOnlyStore(t *testing.T) {
	te := setupPGMode(t, withLedger())
	w := te.post(t, "/api/v1/ledger/events",
		`{"events":[{"event_class":"health"}]}`)
	assert.Equal(t, http.StatusNotImplemented, w.Code, w.Body.String())
	status, err := te.db.LedgerStatus(t.Context(), "default")
	require.NoError(t, err)
	assert.Zero(t, status.Segments, "append must not reach the read-only store")

	_, err = ledger.NewWriter(te.db, "default", "host-a", nil).Append(t.Context(), []ledger.Event{{
		EventClass: ledger.ClassHealth, PayloadTier: ledger.TierMetadataOnly,
	}})
	require.NoError(t, err)
	w = te.post(t, "/api/v1/ledger/verify", `{}`)
	assert.Equal(t, http.StatusNotImplemented, w.Code, w.Body.String())
	checkpoint, err := te.db.GetLedgerVerifyState(t.Context(), "default", "host-a")
	require.NoError(t, err)
	assert.Nil(t, checkpoint, "verification must not reach the read-only store")
}

func TestLedgerQueryRejections(t *testing.T) {
	te := setup(t, withLedger())
	tests := []struct{ query, want string }{
		{"since=yesterday", "invalid date: yesterday (expected Nh, Nd, Nw, or YYYY-MM-DD)"},
		{"class=nope", "unknown event class 'nope'"},
		{"zone=..%2Fx", `invalid zone`},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			w := te.get(t, "/api/v1/ledger/events?"+tt.query)
			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.Contains(t, w.Body.String(), tt.want)
		})
	}
}

func TestVersionReportsLedgerAvailable(t *testing.T) {
	for _, tt := range []struct {
		name string
		opts []setupOption
		want string
	}{
		{"off", nil, `"ledger_available":true`},
		{"on", []setupOption{withLedger()}, `"ledger_available":true`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := setup(t, tt.opts...).get(t, "/api/v1/version")
			require.Equal(t, http.StatusOK, w.Code)
			assert.Contains(t, w.Body.String(), tt.want)
		})
	}
}

func TestReadOnlyPostgresServerAdvertisesStoredLedgerQueries(t *testing.T) {
	te := setup(t)
	_, err := ledger.NewWriter(te.db, "default", "host-a", nil).Append(t.Context(), []ledger.Event{{
		EventClass: ledger.ClassHealth, PayloadTier: ledger.TierMetadataOnly,
		Timestamp: time.Now().UTC(), Payload: map[string]any{"summary": "stored event"},
	}})
	require.NoError(t, err)

	listener, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	cfg := config.Config{
		Host: "127.0.0.1", DataDir: te.dataDir,
		PublicURL: "http://" + listener.Addr().String(),
	}
	srv := server.New(cfg,
		readOnlyTestStore{Store: te.db}, nil,
		server.WithVersion(server.VersionInfo{ReadOnly: true}),
	)
	httpServer := httptest.NewUnstartedServer(srv.Handler())
	require.NoError(t, httpServer.Listener.Close())
	httpServer.Listener = listener
	httpServer.Start()
	t.Cleanup(httpServer.Close)

	capabilities, err := servicehttp.ProbeHTTPServerCapabilities(t.Context(), httpServer.URL, "")
	require.NoError(t, err)
	assert.True(t, capabilities.ReadOnly)
	assert.True(t, capabilities.LedgerQueries,
		"stored ledger reads are available even when local appends are disabled")

	backend := servicehttp.NewHTTPBackendForServer(httpServer.URL, "", capabilities)
	assert.True(t, service.SupportsLedgerQueries(backend))
	results, err := service.LedgerQuery(t.Context(), backend, ledger.Query{Zone: "default", Limit: 10})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Len(t, results[0].Events, 1)
	assert.Equal(t, "stored event", ledger.Summary(results[0].Events[0]))
}
