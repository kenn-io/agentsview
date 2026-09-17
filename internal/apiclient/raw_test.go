package apiclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRawRequestLeavesArchiveAndErrorBodiesUnread(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusBadRequest} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			release := make(chan struct{})
			var once sync.Once
			finish := func() { once.Do(func() { close(release) }) }
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, "/api/v1/remote-sync/archive", r.URL.Path)
				w.Header().Set("Content-Type", "application/x-tar")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "first")
				w.(http.Flusher).Flush()
				<-release
				_, _ = io.WriteString(w, "last")
			}))
			defer server.Close()
			defer finish()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			response, err := RawRequest(server.URL, server.Client(), func(client *Client) error {
				_, err := client.PostAPIV1RemoteSyncArchiveWithResponse(ctx, &PostAPIV1RemoteSyncArchiveRequestOptions{})
				return err
			})
			require.NoError(t, err)
			require.NotNil(t, response)
			defer response.Body.Close()
			assert.Equal(t, status, response.StatusCode)
			first := make([]byte, 5)
			_, err = io.ReadFull(response.Body, first)
			require.NoError(t, err)
			assert.Equal(t, "first", string(first))
			finish()
			rest, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			assert.Equal(t, "last", string(rest))
		})
	}
}
