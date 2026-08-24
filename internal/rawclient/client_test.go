package rawclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeAPIError(t *testing.T) {
	t.Parallel()
	offset := int64(5)
	tests := []struct {
		name   string
		status int
		body   string
		want   APIError
		wantOK bool
	}{
		{
			name:   "offset conflict carries authoritative offset",
			status: http.StatusConflict,
			body: `{"code":"upload_offset_conflict","error":"raw upload offset changed",` +
				`"upload_offset":5}`,
			want: APIError{
				Status:              http.StatusConflict,
				Code:                "upload_offset_conflict",
				Message:             "raw upload offset changed",
				CurrentUploadOffset: &offset,
			},
			wantOK: true,
		},
		{
			name:   "head conflict carries current head",
			status: http.StatusConflict,
			body: `{"code":"head_conflict","error":"raw source head changed",` +
				`"current_manifest_id":"rm_1","current_receipt":"rr_1","current_generation":3}`,
			want: APIError{
				Status:            http.StatusConflict,
				Code:              "head_conflict",
				Message:           "raw source head changed",
				CurrentManifestID: "rm_1",
				CurrentReceipt:    "rr_1",
				CurrentGeneration: 3,
			},
			wantOK: true,
		},
		{
			name:   "missing object conflict",
			status: http.StatusConflict,
			body:   `{"code":"missing_object","error":"raw manifest references a missing object"}`,
			want: APIError{
				Status:  http.StatusConflict,
				Code:    "missing_object",
				Message: "raw manifest references a missing object",
			},
			wantOK: true,
		},
		{
			name:   "unauthorized",
			status: http.StatusUnauthorized,
			body:   `{"code":"unauthorized","error":"Unauthorized"}`,
			want: APIError{
				Status:  http.StatusUnauthorized,
				Code:    "unauthorized",
				Message: "Unauthorized",
			},
			wantOK: true,
		},
		{
			name:   "non-json body still reports status and code",
			status: http.StatusInternalServerError,
			body:   `upstream broke`,
			want: APIError{
				Status:  http.StatusInternalServerError,
				Code:    CodeInternal,
				Message: "raw sync request failed",
			},
			wantOK: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got APIError
			err := decodeAPIError(tt.status, json.RawMessage(tt.body))
			require.Error(t, err)
			ok := AsAPIError(err, &got)
			require.Equal(t, tt.wantOK, ok)
			if tt.want.CurrentUploadOffset == nil {
				assert.Nil(t, got.CurrentUploadOffset)
			} else {
				require.NotNil(t, got.CurrentUploadOffset)
				assert.Equal(t, *tt.want.CurrentUploadOffset, *got.CurrentUploadOffset)
			}
			assert.Equal(t, tt.want.Status, got.Status)
			assert.Equal(t, tt.want.Code, got.Code)
			assert.Equal(t, tt.want.Message, got.Message)
			assert.Equal(t, tt.want.CurrentManifestID, got.CurrentManifestID)
			assert.Equal(t, tt.want.CurrentReceipt, got.CurrentReceipt)
			assert.Equal(t, tt.want.CurrentGeneration, got.CurrentGeneration)
		})
	}
}

func TestAsAPIErrorRejectsForeignErrors(t *testing.T) {
	t.Parallel()
	var got APIError
	assert.False(t, AsAPIError(errors.New("boom"), &got))
}

func TestClientRefusesCredentialRedirects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		httpClient func() *http.Client
	}{
		{name: "default client"},
		{
			name: "supplied client without redirect policy",
			httpClient: func() *http.Client {
				return &http.Client{Timeout: time.Second}
			},
		},
		{
			name: "supplied client with permissive redirect policy",
			httpClient: func() *http.Client {
				return &http.Client{
					Timeout: time.Second,
					CheckRedirect: func(*http.Request, []*http.Request) error {
						return nil
					},
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var redirected atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				redirected.Add(1)
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"token":"avdt_redirected","device_id":"dev_test",`+
					`"scopes":["negotiate","upload","commit"],"expires_at":%q}`,
					time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano))
			}))
			t.Cleanup(target.Close)
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
			}))
			t.Cleanup(endpoint.Close)

			var supplied *http.Client
			if tt.httpClient != nil {
				supplied = tt.httpClient()
			}
			client, err := NewClient(Config{
				BaseURL: endpoint.URL, DeviceID: "dev_test",
				Credential: "avdc_test", HTTPClient: supplied,
			})
			require.NoError(t, err)
			_, err = client.tokens.token(t.Context())
			assert.Error(t, err)
			assert.EqualValues(t, 0, redirected.Load(),
				"redirect target must not receive the device credential")
		})
	}
}
