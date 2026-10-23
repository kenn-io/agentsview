package servicehttp

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/remotecred"
	"go.kenn.io/agentsview/internal/service"
)

func TestPolicyClientCapabilities(t *testing.T) {
	for _, body := range []string{`{"api_version":4,"session_stats_available":true}`, `null`, `{}`, `{"api_version":0}`, `{"api_version":-1}`, ``} {
		t.Run(body, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "Bearer remote-only", r.Header.Get("Authorization"))
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			}))
			defer ts.Close()
			caps, err := ProbeHTTPServerCapabilitiesWithClient(t.Context(), ts.URL, "remote-only", ts.Client())
			if body == `{"api_version":4,"session_stats_available":true}` {
				require.NoError(t, err)
				require.True(t, caps.SessionStatsAvailable)
			} else {
				require.Error(t, err)
				require.ErrorContains(t, err, ts.URL)
				require.ErrorContains(t, err, "--local")
			}
		})
	}
}

func TestRemotePolicyUsedByLazyProbe(t *testing.T) {
	for _, tc := range []struct {
		name   string
		filter service.ListFilter
		path   string
		failed bool
	}{
		{"filtered probe failure", service.ListFilter{Labels: []string{"urgent"}}, "/api/v1/version", true},
		{"plain read", service.ListFilter{}, "/api/v1/sessions", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paths := []string{}
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				assert.Equal(t, tc.path, r.URL.Path)
				switch r.URL.Path {
				case "/api/v1/version":
					http.Error(w, "private token=secret", http.StatusNotImplemented)
				case "/api/v1/sessions":
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"sessions":[{"id":"example"}],"total":1}`))
				default:
					assert.Failf(t, "unexpected request", "path: %s", r.URL.Path)
					w.WriteHeader(http.StatusInternalServerError)
				}
			}))
			defer source.Close()
			client, err := remotecred.Client(source.URL, false)
			require.NoError(t, err)
			backend := NewHTTPBackendWithClient(source.URL, "remote-only", true, "", client)
			result, err := backend.List(t.Context(), tc.filter)
			assert.Equal(t, []string{tc.path}, paths)
			if tc.failed {
				require.ErrorIs(t, err, errHTTPNotImplemented)
				assert.NotContains(t, err.Error(), "secret")
				assert.NotContains(t, err.Error(), "private")
				require.ErrorContains(t, err, "--local")
				require.ErrorContains(t, err, source.URL)
			} else {
				require.NoError(t, err)
				require.Len(t, result.Sessions, 1)
				assert.Equal(t, "example", result.Sessions[0].ID)
			}
		})
	}
}

func TestRemotePolicyRedactsFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		readBody  string
		truncated bool
		want      error
		decode    bool
	}{
		{name: "unauthorized", status: 401, body: "private secret"},
		{name: "bad gateway", status: 502, body: "private secret"},
		{name: "missing endpoint", status: 404, body: "private secret", want: errHTTPNotFound},
		{name: "unsupported endpoint", status: 501, body: "private secret", want: errHTTPNotImplemented},
		{name: "malformed JSON", status: 200, body: `{"response-secret":`, decode: true},
		{name: "wrong version type", status: 200, body: `{"api_version":"response-secret"}`, readBody: `{"sessions":"response-secret"}`, decode: true},
		{name: "truncated body", status: 200, body: `{"api_version":4}`, truncated: true, want: io.ErrUnexpectedEOF},
	} {
		for _, probe := range []bool{true, false} {
			t.Run(tc.name+"/probe="+strconv.FormatBool(probe), func(t *testing.T) {
				hits := 0
				ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hits++
					body := tc.body
					if !probe && tc.readBody != "" {
						body = tc.readBody
					}
					assert.Equal(t, "Bearer remote-only", r.Header.Get("Authorization"))
					w.Header().Set("Content-Type", "application/json; private=header-secret")
					if tc.truncated {
						w.Header().Set("Content-Length", strconv.Itoa(len(body)+10))
					}
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(body))
				}))
				defer ts.Close()
				target := ts.URL + "/private?token=url-secret"
				client, err := remotecred.Client(target, false)
				require.NoError(t, err)
				if probe {
					_, err = ProbeHTTPServerCapabilitiesWithClient(t.Context(), target, "remote-only", client)
				} else {
					backend := NewHTTPBackendWithClient(target, "remote-only", true, "", client)
					_, err = backend.List(t.Context(), service.ListFilter{})
				}
				require.Error(t, err)
				assert.Equal(t, 1, hits)
				require.ErrorContains(t, err, ts.URL)
				require.ErrorContains(t, err, "--local")
				assert.NotContains(t, err.Error(), "private")
				assert.NotContains(t, err.Error(), "secret")
				if tc.want != nil {
					require.ErrorIs(t, err, tc.want)
				}
				if tc.decode {
					var decode *runtime.ResponseDecodeError
					require.ErrorAs(t, err, &decode)
				}
			})
		}
	}
}

func TestRemoteRecallUnavailableIsRedacted(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotImplemented) }))
	defer ts.Close()
	client, err := remotecred.Client(ts.URL, false)
	require.NoError(t, err)
	backend := NewHTTPBackendWithClient(ts.URL+"/private?token=secret", "remote-only", false, "", client)
	for _, call := range []func() error{
		func() error { _, e := backend.ListRecallEntries(t.Context(), service.RecallFilter{}); return e },
		func() error { _, e := backend.GetRecallEntry(t.Context(), "entry"); return e },
		func() error {
			_, e := backend.QueryRecallEntries(t.Context(), service.RecallQuery{Query: "query"})
			return e
		},
	} {
		err = call()
		require.Error(t, err)
		require.NotContains(t, err.Error(), "private")
		require.NotContains(t, err.Error(), "secret")
		require.Contains(t, err.Error(), "--local")
	}
}
