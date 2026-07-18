package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWithTimeout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		operation      string
		timeout        time.Duration
		handler        http.HandlerFunc
		wantStatus     int
		wantBody       string
		wantHeaderKey  string
		wantHeaderVal  string
		assertResponse func(t *testing.T, resp *http.Response)
	}{
		{
			name:      "timeout",
			operation: "GET /test",
			timeout:   10 * time.Millisecond,
			handler: func(w http.ResponseWriter, r *http.Request) {
				time.Sleep(50 * time.Millisecond)
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("too slow"))
			},
			assertResponse: func(t *testing.T, resp *http.Response) {
				assertTimeoutResponse(
					t, resp,
					"GET /test",
					"10ms",
					"--write-timeout",
				)
			},
		},
		{
			name:      "success",
			operation: "GET /test",
			timeout:   100 * time.Millisecond,
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Custom", "value")
				w.WriteHeader(http.StatusCreated)
				w.Write([]byte(`{"status":"ok"}`))
			},
			wantStatus:    http.StatusCreated,
			wantBody:      `{"status":"ok"}`,
			wantHeaderKey: "X-Custom",
			wantHeaderVal: "value",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newTestServerMinimal(t, tt.timeout)
			wrapped := s.withTimeout(tt.operation, tt.handler)

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			w := httptest.NewRecorder()
			wrapped.ServeHTTP(w, req)

			resp := w.Result()
			defer resp.Body.Close()

			if tt.assertResponse != nil {
				tt.assertResponse(t, resp)
				return
			}

			assertRecorderStatus(t, w, tt.wantStatus)

			if tt.wantHeaderKey != "" {
				assert.Equal(t, tt.wantHeaderVal,
					resp.Header.Get(tt.wantHeaderKey),
					"header %s", tt.wantHeaderKey)
			}

			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			assert.Equal(t, tt.wantBody, string(body))
		})
	}
}

func TestTimeoutBodyParity(t *testing.T) {
	t.Parallel()

	operation := "GET /api/v1/sessions"
	timeout := 30 * time.Second

	msg := timeoutErrorBody(operation, timeout)

	var je jsonError
	require.NoError(t, json.Unmarshal([]byte(msg), &je))
	assert.Equal(t, "request timed out", je.Error)
	assert.Contains(t, je.Detail, operation)
	assert.Contains(t, je.Detail, "30s")
	assert.Contains(t, je.Detail, "--write-timeout")
}
