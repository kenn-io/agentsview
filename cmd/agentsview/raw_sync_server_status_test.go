package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/rawcheckpoint"
	"go.kenn.io/agentsview/internal/rawsync"
)

func TestRawSyncServerStatusJSON(t *testing.T) {
	acceptedA := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	completedA := acceptedA.Add(2 * time.Second)
	acceptedB := time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)
	completedB := acceptedB.Add(20 * time.Minute)
	status := rawsync.Status{
		SourceHeads: []rawsync.SourceHeadStatus{
			{
				DeviceID:             "device-a",
				ConfiguredRootID:     "root-a",
				SourceKey:            "a.jsonl",
				Generation:           1,
				LastAcceptedAt:       &acceptedA,
				LastParseCompletedAt: &completedA,
			},
			{
				DeviceID:             "device-b",
				ConfiguredRootID:     "root-b",
				SourceKey:            "b.jsonl",
				Generation:           2,
				LastAcceptedAt:       &acceptedB,
				LastParseCompletedAt: &completedB,
			},
		},
		ParseJobs: rawsync.ParseJobCounts{
			Ready: 2, Leased: 3, Retrying: 4, Complete: 90, Failed: 5, Superseded: 60,
		},
		ActiveDeviceCount: 1,
		Devices:           []rawsync.DeviceStatus{{DeviceID: "device-a"}},
	}
	server := newRawSyncStatusTestServer(t, status)
	t.Setenv("AGENTSVIEW_RAW_SYNC_CREDENTIAL", "credential-value")

	output, err := executeCommand(
		newRootCommand(), "raw-sync", "server-status",
		"--server", server.URL, "--device-id", "device-a", "--allow-insecure-http",
	)

	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(output), &got))
	assert.ElementsMatch(t, []string{
		"source_heads", "parse_jobs", "active_device_count", "devices", "uploads",
		"pipeline_depth", "parse_lag_seconds",
	}, mapKeys(got))
	assert.Equal(t, float64(9), got["pipeline_depth"])
	assert.Equal(t, float64(2), got["parse_lag_seconds"])
	heads, ok := got["source_heads"].([]any)
	require.True(t, ok)
	for _, head := range heads {
		headObject, ok := head.(map[string]any)
		require.True(t, ok)
		assert.Contains(t, headObject, "last_parse_completed_at")
	}
	assert.Equal(t, byte('\n'), output[len(output)-1])
}

func TestRawSyncServerStatusParseLag(t *testing.T) {
	acceptedA := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	completedA := acceptedA.Add(2 * time.Second)
	acceptedB := time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)
	completedB := acceptedB.Add(20 * time.Minute)
	tests := []struct {
		name  string
		heads []rawsync.SourceHeadStatus
		want  *float64
	}{
		{
			name: "most recently completed head",
			heads: []rawsync.SourceHeadStatus{
				{LastAcceptedAt: &acceptedA, LastParseCompletedAt: &completedA},
				{LastAcceptedAt: &acceptedB, LastParseCompletedAt: &completedB},
			},
			want: new(2.0),
		},
		{
			name: "ties use server order",
			heads: []rawsync.SourceHeadStatus{
				{LastAcceptedAt: &acceptedA, LastParseCompletedAt: &completedA},
				{LastAcceptedAt: &acceptedB, LastParseCompletedAt: &completedA},
			},
			want: new(2.0),
		},
		{
			name: "missing acceptance is skipped",
			heads: []rawsync.SourceHeadStatus{
				{LastParseCompletedAt: &completedB},
				{LastAcceptedAt: &acceptedA, LastParseCompletedAt: &completedA},
			},
			want: new(2.0),
		},
		{name: "all evidence missing", heads: []rawsync.SourceHeadStatus{}},
		{
			name:  "legacy field is null",
			heads: []rawsync.SourceHeadStatus{{LastAcceptedAt: &acceptedA}},
		},
		{
			name: "signed difference",
			heads: []rawsync.SourceHeadStatus{
				{LastAcceptedAt: &completedA, LastParseCompletedAt: &acceptedA},
			},
			want: new(-2.0),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rawSyncParseLagSeconds(tt.heads)
			if tt.want == nil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, *tt.want, *got)
			if tt.name == "most recently completed head" {
				t.Log("selected lag 2.0 seconds; historical lag 1200.0 seconds ignored")
			}
		})
	}
}

func TestRawSyncServerStatusLocalIndependence(t *testing.T) {
	dataDir := t.TempDir()
	checkpointDir := filepath.Join(dataDir, "raw-sync")
	require.NoError(t, os.MkdirAll(checkpointDir, 0o700))
	checkpointPath := filepath.Join(checkpointDir, "checkpoint.db")
	checkpointBytes := []byte("corrupt checkpoint")
	require.NoError(t, os.WriteFile(checkpointPath, checkpointBytes, 0o600))
	configBytes := []byte("[not valid")
	configPath := filepath.Join(dataDir, "config.toml")
	require.NoError(t, os.WriteFile(configPath, configBytes, 0o600))
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	t.Setenv("AGENTSVIEW_RAW_SYNC_CREDENTIAL", "credential-value")
	server := newRawSyncStatusTestServer(t, rawsync.Status{})

	output, err := executeCommand(
		newRootCommand(), "raw-sync", "server-status", "--server", server.URL,
		"--device-id", "device-a", "--allow-insecure-http",
	)

	require.NoError(t, err)
	assert.NotEmpty(t, output)
	assert.Equal(t, checkpointBytes, mustReadFile(t, checkpointPath))
	assert.Equal(t, configBytes, mustReadFile(t, configPath))

	missingDataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", missingDataDir)
	output, err = executeCommand(
		newRootCommand(), "raw-sync", "server-status", "--server", server.URL,
		"--device-id", "device-a", "--allow-insecure-http",
	)
	require.NoError(t, err)
	assert.NotEmpty(t, output)
	_, err = os.Stat(filepath.Join(missingDataDir, "raw-sync"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestRawSyncServerStatusFallback(t *testing.T) {
	tests := []struct {
		name    string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{
			name: "get json 404",
			handler: rawSyncStatusErrorHandler(
				[]int{http.StatusOK}, []int{http.StatusNotFound}, `{"error":"old server"}`,
			),
		},
		{
			name: "get html 404",
			handler: rawSyncStatusErrorHandler(
				[]int{http.StatusOK}, []int{http.StatusNotFound}, "<html>not found</html>",
			),
		},
		{
			name: "get empty 404",
			handler: rawSyncStatusErrorHandler(
				[]int{http.StatusOK}, []int{http.StatusNotFound}, "",
			),
		},
		{
			name: "initial exchange 404",
			handler: rawSyncStatusErrorHandler(
				[]int{http.StatusNotFound}, nil, `{"error":"old server"}`,
			),
		},
		{
			name: "refresh exchange 404",
			handler: rawSyncStatusErrorHandler(
				[]int{http.StatusOK, http.StatusNotFound}, []int{http.StatusUnauthorized}, "",
			),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dataDir := t.TempDir()
			t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
			t.Setenv("AGENTSVIEW_RAW_SYNC_CREDENTIAL", "credential-value")
			server := httptest.NewServer(http.HandlerFunc(tt.handler))
			t.Cleanup(server.Close)

			output, err := executeCommand(
				newRootCommand(), "raw-sync", "server-status", "--server", server.URL,
				"--device-id", "device-a", "--allow-insecure-http",
			)

			require.NoError(t, err)
			assert.JSONEq(t, `{"device_id":"","pending_generations":0,`+
				`"pending_objects":0,"pending_object_bytes":0,`+
				`"outbox":{"used_bytes":0,"reserved_bytes":0,"limit_bytes":0},`+
				`"permanent_failures":0}`, output)
			_, statErr := os.Stat(filepath.Join(dataDir, "raw-sync"))
			assert.ErrorIs(t, statErr, os.ErrNotExist)
		})
	}

	t.Run("populated checkpoint", func(t *testing.T) {
		dataDir := t.TempDir()
		checkpointPath := filepath.Join(dataDir, "raw-sync", "checkpoint.db")
		require.NoError(t, os.MkdirAll(filepath.Dir(checkpointPath), 0o700))
		store, err := rawcheckpoint.Open(t.Context(), checkpointPath)
		require.NoError(t, err)
		require.NoError(t, store.EnsureDevice(t.Context(), "local-device"))
		require.NoError(t, store.Close())
		t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
		t.Setenv("AGENTSVIEW_RAW_SYNC_CREDENTIAL", "credential-value")
		server := httptest.NewServer(rawSyncStatusErrorHandler(
			[]int{http.StatusOK}, []int{http.StatusNotFound}, "",
		))
		t.Cleanup(server.Close)

		output, err := executeCommand(
			newRootCommand(), "raw-sync", "server-status", "--server", server.URL,
			"--device-id", "device-a", "--allow-insecure-http",
		)

		require.NoError(t, err)
		assert.Contains(t, output, `"device_id":"local-device"`)
	})

	t.Run("corrupt checkpoint remains an error", func(t *testing.T) {
		dataDir := t.TempDir()
		checkpointPath := filepath.Join(dataDir, "raw-sync", "checkpoint.db")
		require.NoError(t, os.MkdirAll(filepath.Dir(checkpointPath), 0o700))
		contents := []byte("corrupt checkpoint")
		require.NoError(t, os.WriteFile(checkpointPath, contents, 0o600))
		t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
		t.Setenv("AGENTSVIEW_RAW_SYNC_CREDENTIAL", "credential-value")
		server := httptest.NewServer(rawSyncStatusErrorHandler(
			[]int{http.StatusOK}, []int{http.StatusNotFound}, "",
		))
		t.Cleanup(server.Close)

		output, err := executeCommand(
			newRootCommand(), "raw-sync", "server-status", "--server", server.URL,
			"--device-id", "device-a", "--allow-insecure-http",
		)

		assert.Empty(t, output)
		require.Error(t, err)
		assert.Equal(t, contents, mustReadFile(t, checkpointPath))
	})
}

func TestRawSyncServerStatusErrors(t *testing.T) {
	statuses := []int{
		http.StatusBadRequest, http.StatusForbidden, http.StatusMethodNotAllowed,
		http.StatusGone, http.StatusTeapot, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusServiceUnavailable,
	}
	for _, status := range statuses {
		for _, route := range []string{"exchange", "get"} {
			t.Run(fmt.Sprintf("%s-%d", route, status), func(t *testing.T) {
				dataDir := t.TempDir()
				checkpointPath := filepath.Join(dataDir, "raw-sync", "checkpoint.db")
				require.NoError(t, os.MkdirAll(filepath.Dir(checkpointPath), 0o700))
				checkpointBytes := []byte("must not be read")
				require.NoError(t, os.WriteFile(checkpointPath, checkpointBytes, 0o600))
				t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
				t.Setenv("AGENTSVIEW_RAW_SYNC_CREDENTIAL", "credential-marker")
				exchange := []int{http.StatusOK}
				get := []int{http.StatusOK}
				if route == "exchange" {
					exchange[0] = status
				} else {
					get[0] = status
				}
				server := httptest.NewServer(rawSyncStatusErrorHandler(
					exchange, get, "404 not found credential-marker",
				))
				t.Cleanup(server.Close)

				output, err := executeCommand(
					newRootCommand(), "raw-sync", "server-status", "--server", server.URL,
					"--device-id", "device-a", "--allow-insecure-http",
				)

				assert.Empty(t, output)
				require.Error(t, err)
				assert.NotContains(t, err.Error(), "credential-marker")
				assert.Equal(t, checkpointBytes, mustReadFile(t, checkpointPath))
			})
		}
	}

	for _, refreshStatus := range []int{
		http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError,
	} {
		t.Run(fmt.Sprintf("refresh-%d", refreshStatus), func(t *testing.T) {
			dataDir := t.TempDir()
			t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
			t.Setenv("AGENTSVIEW_RAW_SYNC_CREDENTIAL", "credential-marker")
			server := httptest.NewServer(rawSyncStatusErrorHandler(
				[]int{http.StatusOK, refreshStatus}, []int{http.StatusUnauthorized},
				"404 not found credential-marker",
			))
			t.Cleanup(server.Close)

			output, err := executeCommand(
				newRootCommand(), "raw-sync", "server-status", "--server", server.URL,
				"--device-id", "device-a", "--allow-insecure-http",
			)

			assert.Empty(t, output)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "credential-marker")
		})
	}

	t.Run("second GET 401", func(t *testing.T) {
		var gets atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/raw-sync/tokens":
				writeRawSyncToken(w, r, int(gets.Load())+1)
			case "/api/v1/raw-sync/status":
				gets.Add(1)
				writeRawSyncHTTPError(w, http.StatusUnauthorized, "")
			default:
				writeRawSyncHTTPError(w, http.StatusNotFound, "")
			}
		}))
		t.Cleanup(server.Close)
		t.Setenv("AGENTSVIEW_RAW_SYNC_CREDENTIAL", "credential-value")

		output, err := executeCommand(
			newRootCommand(), "raw-sync", "server-status", "--server", server.URL,
			"--device-id", "device-a", "--allow-insecure-http",
		)

		assert.Empty(t, output)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "HTTP 401")
	})
}

func TestRawSyncServerStatusConfig(t *testing.T) {
	var requests atomic.Int32
	server := newRawSyncStatusTestServerWithHandler(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/api/v1/raw-sync/tokens" {
			writeRawSyncToken(w, r, 1)
			return
		}
		writeRawSyncJSON(w, rawsync.Status{})
	})
	t.Setenv("AGENTSVIEW_RAW_SYNC_URL", "http://invalid.example")
	t.Setenv("AGENTSVIEW_RAW_SYNC_DEVICE_ID", "env-device")
	t.Setenv("AGENTSVIEW_RAW_SYNC_CREDENTIAL", "credential-value")

	output, err := executeCommand(
		newRootCommand(), "raw-sync", "server-status", "--server", server.URL,
		"--device-id", "flag-device", "--allow-insecure-http",
	)
	require.NoError(t, err)
	assert.NotEmpty(t, output)
	assert.Positive(t, requests.Load())

	t.Setenv("AGENTSVIEW_RAW_SYNC_URL", server.URL)
	requests.Store(0)
	output, err = executeCommand(
		newRootCommand(), "raw-sync", "server-status", "--server", "   ",
		"--device-id", "\t", "--allow-insecure-http",
	)
	require.NoError(t, err)
	assert.NotEmpty(t, output)
	assert.Positive(t, requests.Load())

	t.Setenv("AGENTSVIEW_RAW_SYNC_URL", "")
	t.Setenv("AGENTSVIEW_RAW_SYNC_DEVICE_ID", "")
	requests.Store(0)
	_, err = executeCommand(newRootCommand(), "raw-sync", "server-status")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--server or AGENTSVIEW_RAW_SYNC_URL is required")
	assert.Zero(t, requests.Load())

	t.Setenv("AGENTSVIEW_RAW_SYNC_URL", server.URL)
	t.Setenv("AGENTSVIEW_RAW_SYNC_DEVICE_ID", "env-device")
	_, err = executeCommand(newRootCommand(), "raw-sync", "server-status", "extra")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown command")
}

func TestValidateRawSyncConnection(t *testing.T) {
	require.NoError(t, validateRawSyncConnection(
		"http://[::ffff:127.0.0.1]:8080", "device-a", "credential", true,
	))
}

func TestRawSyncServerStatusHelp(t *testing.T) {
	help, err := executeCommand(newRootCommand(), "raw-sync", "server-status", "--help")
	require.NoError(t, err)
	for _, want := range []string{
		"server-status", "--server", "--device-id", "--allow-insecure-http",
		"AGENTSVIEW_RAW_SYNC_CREDENTIAL", "HTTP 404", "raw-sync status",
	} {
		assert.Contains(t, help, want)
	}
	assert.NotContains(t, help, "--credential")
}

func TestWriteRawSyncStatusWriterError(t *testing.T) {
	sentinel := errors.New("writer failed")
	writer := failingRawSyncWriter{err: sentinel}
	err := writeRawSyncStatus(&writer, rawsync.Status{})
	assert.ErrorIs(t, err, sentinel)
	writer = failingRawSyncWriter{err: sentinel}
	err = writeRawSyncStatus(&writer, rawSyncServerStatus{ParseLagSeconds: new(2.0)})
	assert.ErrorIs(t, err, sentinel)
}

type failingRawSyncWriter struct{ err error }

func (w *failingRawSyncWriter) Write([]byte) (int, error) {
	return 0, w.err
}

func newRawSyncStatusTestServer(t *testing.T, status rawsync.Status) *httptest.Server {
	t.Helper()
	return newRawSyncStatusTestServerWithHandler(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/raw-sync/tokens":
			writeRawSyncToken(w, r, 1)
		case "/api/v1/raw-sync/status":
			writeRawSyncJSON(w, status)
		default:
			writeRawSyncHTTPError(w, http.StatusNotFound, "")
		}
	})
}

func newRawSyncStatusTestServerWithHandler(
	t *testing.T,
	handler http.HandlerFunc,
) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func rawSyncStatusErrorHandler(
	exchangeStatuses, getStatuses []int,
	body string,
) http.HandlerFunc {
	var exchanges atomic.Int32
	var gets atomic.Int32
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/raw-sync/tokens":
			index := int(exchanges.Add(1)) - 1
			status := exchangeStatuses[min(index, len(exchangeStatuses)-1)]
			if status != http.StatusOK {
				writeRawSyncHTTPError(w, status, body)
				return
			}
			writeRawSyncToken(w, r, index+1)
		case "/api/v1/raw-sync/status":
			index := int(gets.Add(1)) - 1
			status := getStatuses[min(index, len(getStatuses)-1)]
			if status != http.StatusOK {
				writeRawSyncHTTPError(w, status, body)
				return
			}
			writeRawSyncJSON(w, rawsync.Status{})
		default:
			writeRawSyncHTTPError(w, http.StatusNotFound, "")
		}
	}
}

func writeRawSyncToken(w http.ResponseWriter, r *http.Request, number int) {
	if r.Method != http.MethodPost {
		writeRawSyncHTTPError(w, http.StatusMethodNotAllowed, "")
		return
	}
	deviceID := r.Header.Get("X-AgentsView-Device-ID")
	if deviceID == "" {
		deviceID = "device-a"
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, fmt.Sprintf(
		`{"token":"avdt_%d","device_id":%q,"scopes":["status"],"expires_at":%q}`,
		number, deviceID, time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
	))
}

func writeRawSyncJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	data, err := json.Marshal(value)
	if err != nil {
		writeRawSyncHTTPError(w, http.StatusInternalServerError, "")
		return
	}
	_, _ = w.Write(data)
}

func writeRawSyncHTTPError(w http.ResponseWriter, status int, body string) {
	w.WriteHeader(status)
	if body != "" {
		_, _ = io.WriteString(w, body)
	}
}

func mapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}

var _ io.Writer = (*failingRawSyncWriter)(nil)
