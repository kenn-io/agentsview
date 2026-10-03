package requestsign

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func signingKey(t *testing.T) Key {
	t.Helper()
	secret := make([]byte, 64)
	_, err := rand.Read(secret)
	require.NoError(t, err)
	return Key{ID: "test-key", Secret: secret, Grant: "reader"}
}

// A separately constructed base catches matching signer/verifier mistakes.
func TestSignerWireOverHTTPS(t *testing.T) {
	key := signingKey(t)
	var received *http.Request
	var body []byte
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r.Clone(r.Context())
		var err error
		body, err = io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()
	signer, err := NewSigner(ts.URL+"/native", key)
	require.NoError(t, err)
	client, err := signer.Client(ts.Client())
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/native/api/v1/recall/query?a=1&a=2", strings.NewReader("{}"))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer test-auth")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.Equal(t, []byte("{}"), body)
	require.NotNil(t, received)
	assert.Equal(t, "sha-256=:RBNvo1WzZ4oRRq0W9+hknpT7T8If536DEMBg9hyq/4o=:", received.Header.Get("Content-Digest"))
	input := received.Header.Get("Signature-Input")
	require.True(t, strings.HasPrefix(input, `sig1=("@method" "@target-uri" "content-digest" "content-type" "authorization" "x-agentsview-device-id" "upload-offset" "x-agentsview-search-intent");created=`))
	params := strings.TrimPrefix(input, "sig1=")
	base := `"@method": POST` + "\n" + `"@target-uri": ` + ts.URL + "/native/api/v1/recall/query?a=1&a=2\n" +
		`"content-digest": sha-256=:RBNvo1WzZ4oRRq0W9+hknpT7T8If536DEMBg9hyq/4o=:` + "\n" +
		`"content-type": application/json` + "\n" + `"authorization": Bearer test-auth` + "\n" +
		`"x-agentsview-device-id": ` + "\n" + `"upload-offset": ` + "\n" + `"x-agentsview-search-intent": ` + "\n" + `"@signature-params": ` + params
	mac := hmac.New(sha256.New, key.Secret)
	_, err = io.WriteString(mac, base)
	require.NoError(t, err)
	assert.Equal(t, "sig1=:"+base64.StdEncoding.EncodeToString(mac.Sum(nil))+":", received.Header.Get("Signature"))
}

func TestSignerPinsDestinationAndRejectsAmbiguity(t *testing.T) {
	key := signingKey(t)
	signer, err := NewSigner("https://example.com/native", key)
	require.NoError(t, err)
	for _, target := range []string{"http://example.com/native/api/v1/ping", "https://other.example/native/api/v1/ping", "https://example.com/native-alias/api/v1/ping", "https://example.com/native/api/../config", "https://example.com/native/api/%70ing", "https://example.com/native//api", "https://example.com/native/api?bad=%XX"} {
		t.Run(target, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
			require.NoError(t, err)
			assert.Error(t, signer.Sign(req))
		})
	}
	for _, change := range []func(*http.Request){
		func(r *http.Request) { r.Header.Add("Authorization", "Bearer second") },
		func(r *http.Request) { r.Header.Set("Content-Encoding", "gzip") },
		func(r *http.Request) { r.Trailer = http.Header{"Something": []string{"value"}} },
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com/native/api", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer test")
		change(req)
		require.Error(t, signer.Sign(req))
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://example.com/native/api", bytes.NewReader(make([]byte, (4<<20)+1)))
	require.NoError(t, err)
	require.Error(t, signer.Sign(req))
}

func TestClientFromEnvironmentLeavesLoopbackDaemonUnsigned(t *testing.T) {
	t.Setenv("AGENTSVIEW_SIGNING_URL", "https://history.example.com/native")
	t.Setenv("AGENTSVIEW_SIGNING_KEY_ID", "reader-key")
	t.Setenv("AGENTSVIEW_SIGNING_KEY_FILE", "unused-key-file")

	original := &http.Client{}
	got, err := ClientFromEnvironment("http://127.0.0.2:8080/api", original)
	require.NoError(t, err)
	assert.Same(t, original, got)
}

func TestSignerDoesNotFollowRedirects(t *testing.T) {
	for _, code := range []int{301, 302, 303, 307, 308} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			calls := 0
			sink := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
			defer sink.Close()
			source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, sink.URL, code) }))
			defer source.Close()
			signer, err := NewSigner(source.URL, signingKey(t))
			require.NoError(t, err)
			client, err := signer.Client(source.Client())
			require.NoError(t, err)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, source.URL+"/api", strings.NewReader("private-body"))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer credential")
			resp, err := client.Do(req)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			assert.Equal(t, code, resp.StatusCode)
			assert.Zero(t, calls)
		})
	}
}

func TestSignerReusesPoolAndClosesIdleConnections(t *testing.T) {
	var connections atomic.Int32
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	ts.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	ts.StartTLS()
	defer ts.Close()
	signer, err := NewSigner(ts.URL, signingKey(t))
	require.NoError(t, err)
	var client *http.Client
	for range 3 {
		// Production service operations construct API clients independently.
		client, err = signer.Client(ts.Client())
		require.NoError(t, err)
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/api", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer test")
		resp, err := client.Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
	}
	assert.EqualValues(t, 1, connections.Load())
	client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/api", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer test")
	resp, err := client.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.EqualValues(t, 2, connections.Load())
}

func FuzzSignerTarget(f *testing.F) {
	for _, seed := range []string{"/native/api?", "/native/%2fapi", "/native/../admin", "/native/api?q=%20"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, target string) {
		signer, err := NewSigner("https://example.com/native", signingKey(t))
		require.NoError(t, err)
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com"+target, nil)
		if err != nil {
			return
		}
		req.Header.Set("Authorization", "Bearer test")
		if signer.Sign(req) == nil {
			assert.Equal(t, "example.com", req.URL.Host)
			assert.True(t, strings.HasPrefix(req.URL.Path, "/native/"))
			assert.NotContains(t, req.URL.Path, "/../")
			assert.LessOrEqual(t, len(req.Header.Get("Signature-Input")), 2048)
		}
	})
}
