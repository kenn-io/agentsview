package requestsign

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVerifierBoundsEventStreamsAndRejectUndeclaredTrailers(t *testing.T) {
	key := signingKey(t)
	state := filepath.Join(t.TempDir(), "replay.db")
	require.NoError(t, InitReplay(state))
	replay, err := OpenReplay(state)
	require.NoError(t, err)
	defer replay.Close()
	streamExited := make(chan struct{}, 9)
	var handler http.Handler
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	defer ts.Close()
	verifier, err := NewVerifier(VerifyConfig{ExternalURL: ts.URL, Replay: replay, Keys: func() (map[string]Key, error) { return map[string]Key{key.ID: key}, nil }})
	require.NoError(t, err)
	handler = verifier.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/events" {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			streamExited <- struct{}{}
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}), nil, nil)
	signer, err := NewSigner(ts.URL, key)
	require.NoError(t, err)
	client, err := signer.Client(ts.Client())
	require.NoError(t, err)
	var streams []*http.Response
	for range 8 {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/api/v1/events", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer test")
		resp, err := client.Do(req) //nolint:bodyclose // Keep the event stream open while testing its permit.
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		streams = append(streams, resp) //nolint:bodyclose // Closed after the permit-release assertions.
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/api/v1/events", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer test")
	resp, err := client.Do(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, "event streams must have an independent concurrency limit")
	require.NoError(t, resp.Body.Close())

	require.NoError(t, streams[0].Body.Close())
	select {
	case <-streamExited:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "closing an event stream should stop its handler")
	}
	req, err = http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/api/v1/events", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer test")
	resp, err = client.Do(req) //nolint:bodyclose // Keep the replacement stream open while testing permits.
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode, "a finished event stream should release its permit")
	streams = append(streams, resp) //nolint:bodyclose // Closed after the permit-release assertions.

	req, err = http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/api", strings.NewReader("body"))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer test")
	resp, err = client.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusNoContent, resp.StatusCode, "open event streams must not monopolize ordinary handler capacity")
	for _, stream := range streams {
		require.NoError(t, stream.Body.Close())
	}
	for range 8 {
		select {
		case <-streamExited:
		case <-time.After(10 * time.Second):
			require.FailNow(t, "closed event streams should stop their handlers")
		}
	}

	// Go accepts chunked trailers even without a Trailer declaration. Verify
	// after EOF, using the original request, before consuming its nonce.
	req, err = http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/api", strings.NewReader("body"))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer test")
	require.NoError(t, signer.Sign(req))
	address := strings.TrimPrefix(ts.URL, "https://")
	dialer := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: ts.Client().Transport.(*http.Transport).TLSClientConfig}
	conn, err := dialer.DialContext(t.Context(), "tcp", address)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	_, err = fmt.Fprintf(conn, "POST /api HTTP/1.1\r\nHost: %s\r\nTransfer-Encoding: chunked\r\n", address)
	require.NoError(t, err)
	for name, values := range req.Header {
		for _, value := range values {
			_, err = fmt.Fprintf(conn, "%s: %s\r\n", name, value)
			require.NoError(t, err)
		}
	}
	_, err = io.WriteString(conn, "\r\n4\r\nbody\r\n0\r\nUnsigned-Field: changed\r\n\r\n")
	require.NoError(t, err)
	resp, err = http.ReadResponse(bufio.NewReader(conn), req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}
