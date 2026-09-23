package rawclient

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/rawsync"
)

func TestStatusClientScopesAndRefresh(t *testing.T) {
	var exchanges, statusCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/raw-sync/tokens":
			assert.Equal(t, http.MethodPost, r.Method)
			body, err := io.ReadAll(r.Body)
			assert.NoError(t, err)
			assert.Equal(t, `{"scopes":["status"]}`, string(body))
			assert.Equal(t, "Bearer credential-value", r.Header.Get("Authorization"))
			assert.Equal(t, "device-a", r.Header.Get("X-AgentsView-Device-ID"))
			writeStatusToken(w, int(exchanges.Add(1)), "device-a")
		case "/api/v1/raw-sync/status":
			assert.Equal(t, http.MethodGet, r.Method)
			assert.Empty(t, r.Header.Get("X-AgentsView-Device-ID"))
			assert.NotEqual(t, "Bearer credential-value", r.Header.Get("Authorization"))
			if statusCalls.Add(1) == 1 {
				writeStatusHTTPError(w, http.StatusUnauthorized, `{"error":"expired"}`)
				return
			}
			writeStatusJSON(w, rawsync.Status{})
		default:
			writeStatusHTTPError(w, http.StatusNotFound, "")
		}
	}))
	t.Cleanup(server.Close)
	client, err := NewStatusClient(Config{
		BaseURL: server.URL, DeviceID: "device-a", Credential: "credential-value",
	})
	require.NoError(t, err)

	_, err = client.Status(t.Context())
	require.NoError(t, err)
	_, err = client.Status(t.Context())
	require.NoError(t, err)
	assert.EqualValues(t, 2, exchanges.Load())
	assert.EqualValues(t, 3, statusCalls.Load())

	t.Run("refresh 404 keeps typed status", func(t *testing.T) {
		var exchangeCount atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/raw-sync/tokens":
				if exchangeCount.Add(1) == 1 {
					writeStatusToken(w, 1, "device-a")
					return
				}
				writeStatusHTTPError(w, http.StatusNotFound, "old server")
			case "/api/v1/raw-sync/status":
				writeStatusHTTPError(w, http.StatusUnauthorized, "")
			}
		}))
		t.Cleanup(server.Close)
		client, err := NewStatusClient(Config{
			BaseURL: server.URL, DeviceID: "device-a", Credential: "credential-value",
		})
		require.NoError(t, err)
		_, err = client.Status(t.Context())
		var apiErr APIError
		require.Error(t, err)
		require.True(t, AsAPIError(err, &apiErr))
		assert.Equal(t, http.StatusNotFound, apiErr.Status)
	})
}

func TestStatusClientResponses(t *testing.T) {
	tests := []struct {
		name          string
		statusCode    int
		body          string
		wantStatus    int
		wantStatusAPI bool
	}{
		{
			name: "json 404", statusCode: http.StatusNotFound,
			body: `{"error":"missing"}`, wantStatus: http.StatusNotFound,
			wantStatusAPI: true,
		},
		{
			name: "non-json 500", statusCode: http.StatusInternalServerError,
			body: "proxy text", wantStatus: http.StatusInternalServerError,
			wantStatusAPI: true,
		},
		{name: "malformed 200", statusCode: http.StatusOK, body: "{"},
		{name: "empty 200", statusCode: http.StatusOK, body: ""},
		{name: "null 200", statusCode: http.StatusOK, body: "null"},
		{
			name:       "unknown members and missing completion",
			statusCode: http.StatusOK,
			body: `{"source_heads":[{"device_id":"device-a","configured_root_id":"root-a",` +
				`"provider":"codex","source_key":"source.jsonl","generation":1,` +
				`"last_accepted_at":null,"parse_pending":false,"parse_leased":false,` +
				`"parse_failed":false,"unknown_member":true}],"parse_jobs":{},` +
				`"active_device_count":0,"devices":[],"uploads":{}}`,
		},
		{name: "201", statusCode: http.StatusCreated, body: `{}`},
		{name: "204", statusCode: http.StatusNoContent, body: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newStatusResponseServer(t, tt.statusCode, tt.body)
			client, err := NewStatusClient(Config{
				BaseURL: server.URL, DeviceID: "device-a", Credential: "credential-value",
			})
			require.NoError(t, err)

			status, err := client.Status(t.Context())
			if tt.name == "unknown members and missing completion" {
				require.NoError(t, err)
				require.Len(t, status.SourceHeads, 1)
				assert.Nil(t, status.SourceHeads[0].LastParseCompletedAt)
				return
			}
			assert.Equal(t, rawsync.Status{}, status)
			require.Error(t, err)
			var apiErr APIError
			assert.Equal(t, tt.wantStatusAPI, AsAPIError(err, &apiErr))
			if tt.wantStatusAPI {
				assert.Equal(t, tt.wantStatus, apiErr.Status)
			}
		})
	}

	t.Run("transport failure", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/raw-sync/tokens" {
				writeStatusToken(w, 1, "device-a")
				return
			}
			writeStatusJSON(w, rawsync.Status{})
		}))
		baseURL := server.URL
		server.Close()
		client, err := NewStatusClient(Config{
			BaseURL: baseURL, DeviceID: "device-a", Credential: "credential-value",
		})
		require.NoError(t, err)
		_, err = client.Status(t.Context())
		require.Error(t, err)
		var apiErr APIError
		assert.False(t, AsAPIError(err, &apiErr))
	})

	t.Run("cancellation", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}))
		t.Cleanup(server.Close)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		client, err := NewStatusClient(Config{
			BaseURL: server.URL, DeviceID: "device-a", Credential: "credential-value",
		})
		require.NoError(t, err)
		_, err = client.Status(ctx)
		assert.ErrorIs(t, err, context.Canceled)
	})
}

func TestStatusClientIdentityAndRedirects(t *testing.T) {
	t.Run("empty token fails before GET", func(t *testing.T) {
		var gets atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/raw-sync/tokens" {
				writeStatusTokenValue(w, "", "device-a")
				return
			}
			gets.Add(1)
			writeStatusJSON(w, rawsync.Status{})
		}))
		t.Cleanup(server.Close)
		client, err := NewStatusClient(Config{
			BaseURL: server.URL, DeviceID: "device-a", Credential: "credential-marker",
		})
		require.NoError(t, err)
		_, err = client.Status(t.Context())
		require.Error(t, err)
		assert.Zero(t, gets.Load())
		assert.NotContains(t, err.Error(), "credential-marker")
	})

	t.Run("wrong device fails before GET", func(t *testing.T) {
		var gets atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/raw-sync/tokens" {
				writeStatusTokenValue(w, "avdt_token", "other-device")
				return
			}
			gets.Add(1)
			writeStatusJSON(w, rawsync.Status{})
		}))
		t.Cleanup(server.Close)
		client, err := NewStatusClient(Config{
			BaseURL: server.URL, DeviceID: "device-a", Credential: "credential-marker",
		})
		require.NoError(t, err)
		_, err = client.Status(t.Context())
		require.Error(t, err)
		assert.Zero(t, gets.Load())
		assert.NotContains(t, err.Error(), "credential-marker")
	})

	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(fmt.Sprintf("token redirect %d", status), func(t *testing.T) {
			var redirected atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				redirected.Add(1)
			}))
			t.Cleanup(target.Close)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/raw-sync/tokens" {
					http.Redirect(w, r, target.URL, status)
					return
				}
				writeStatusJSON(w, rawsync.Status{})
			}))
			t.Cleanup(server.Close)
			client, err := NewStatusClient(Config{
				BaseURL: server.URL, DeviceID: "device-a", Credential: "credential-marker",
			})
			require.NoError(t, err)
			_, err = client.Status(t.Context())
			require.Error(t, err)
			assert.Zero(t, redirected.Load())
			assert.NotContains(t, err.Error(), "credential-marker")
		})
		t.Run(fmt.Sprintf("status redirect %d", status), func(t *testing.T) {
			var redirected atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				redirected.Add(1)
			}))
			t.Cleanup(target.Close)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/raw-sync/tokens":
					writeStatusToken(w, 1, "device-a")
				case "/api/v1/raw-sync/status":
					http.Redirect(w, r, target.URL, status)
				}
			}))
			t.Cleanup(server.Close)
			client, err := NewStatusClient(Config{
				BaseURL: server.URL, DeviceID: "device-a", Credential: "credential-marker",
			})
			require.NoError(t, err)
			_, err = client.Status(t.Context())
			require.Error(t, err)
			assert.Zero(t, redirected.Load())
			assert.NotContains(t, err.Error(), "credential-marker")
		})
	}
}

func newStatusResponseServer(t *testing.T, statusCode int, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/raw-sync/tokens" {
			writeStatusToken(w, 1, "device-a")
			return
		}
		w.WriteHeader(statusCode)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server
}

func writeStatusToken(w http.ResponseWriter, number int, deviceID string) {
	writeStatusTokenValue(w, fmt.Sprintf("avdt_%d", number), deviceID)
}

func writeStatusTokenValue(w http.ResponseWriter, token, deviceID string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, fmt.Sprintf(
		`{"token":%q,"device_id":%q,"scopes":["status"],"expires_at":%q}`,
		token, deviceID, time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
	))
}

func writeStatusJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	data, err := json.Marshal(value)
	if err != nil {
		writeStatusHTTPError(w, http.StatusInternalServerError, "")
		return
	}
	_, _ = w.Write(data)
}

func writeStatusHTTPError(w http.ResponseWriter, status int, body string) {
	w.WriteHeader(status)
	if body != "" {
		_, _ = io.WriteString(w, body)
	}
}
