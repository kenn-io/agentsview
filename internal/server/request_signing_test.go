package server

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/requestsign"
)

func TestRestrictedIngressGrantsRequireNativeAuth(t *testing.T) {
	srv := testServer(t, 30*time.Second)
	srv.cfg.AuthToken = "test-bearer"
	// Main stays unsigned; the machine listener must independently require auth.
	srv.cfg.RequireAuth = false
	var handler http.Handler
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	defer ts.Close()
	key := make([]byte, 64)
	_, err := rand.Read(key)
	require.NoError(t, err)
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	require.NoError(t, os.WriteFile(keyFile, []byte(base64.StdEncoding.EncodeToString(key)), 0o600))
	replayPath := filepath.Join(dir, "replay.db")
	require.NoError(t, requestsign.InitReplay(replayPath))
	policyPath := filepath.Join(dir, "policy.json")
	externalPrefix := "/company/agentsview"
	policy := requestsign.Policy{ExternalURL: ts.URL + externalPrefix, ReplayDB: replayPath, Keys: []requestsign.PolicyKey{{ID: "reader-key", File: keyFile, Grant: "reader"}}}
	data, err := json.Marshal(policy)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(policyPath, data, 0o600))
	handler, closeState, err := srv.RestrictedHandler(policyPath)
	require.NoError(t, err)
	defer closeState()
	signer, err := requestsign.NewSigner(ts.URL+externalPrefix, requestsign.Key{ID: "reader-key", Secret: key})
	require.NoError(t, err)
	client, err := signer.Client(ts.Client())
	require.NoError(t, err)
	started := time.Now().UTC().Format(time.RFC3339)
	_, err = srv.db.(*db.DB).WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{Session: db.Session{ID: "stream-session", Project: "sample", Machine: "test", Agent: "claude", StartedAt: &started}}})
	require.NoError(t, err)
	// Each reconnect goes through the native transport with a fresh nonce.
	for range 2 {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+externalPrefix+"/api/v1/sessions/stream-session/watch", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer test-bearer")
		resp, err := client.Do(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		line, err := bufio.NewReader(resp.Body).ReadString('\n')
		require.NoError(t, err)
		assert.Contains(t, line, "session.timing")
		require.NoError(t, resp.Body.Close())
	}
	for _, tc := range []struct {
		method, path, token, body string
		status                    int
	}{
		{"GET", "/api/ping", "test-bearer", "", 200},
		{"GET", "/api/v1/version", "test-bearer", "", 200},
		{"GET", "/api/v1/sessions", "test-bearer", "", 200},
		{"POST", "/api/v1/recall/query", "test-bearer", `{"query":"missing","mode":"lexical"}`, 200},
		{"POST", "/api/v1/recall/query", "test-bearer", `{"query":"missing","mode":"lexical","skip_recording":false}`, 200},
		{"GET", "/api/v1/version", "wrong", "", 401},
		{"GET", "/api/v1/config", "test-bearer", "", 403},
		{"GET", "/api/v1/settings", "test-bearer", "", 403},
		{"POST", "/api/v1/shutdown", "test-bearer", "{}", 403},
		{"POST", "/api/v1/sessions/sync", "test-bearer", "{}", 403},
		{"GET", "/api/v1/sessions/session/directory", "test-bearer", "", 403},
		{"GET", "/debug/pprof/", "test-bearer", "", 403},
		{"OPTIONS", "/api/v1/version", "test-bearer", "", 403},
		{"GET", "/api/v1/search/content?pattern=test&reveal=true", "test-bearer", "", 403},
	} {
		t.Run(tc.method+tc.path+tc.token, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), tc.method, ts.URL+externalPrefix+tc.path, strings.NewReader(tc.body))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+tc.token)
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			require.NoError(t, err)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			assert.Equal(t, tc.status, resp.StatusCode, string(body))
			if tc.path == "/api/v1/version" && tc.status == 200 {
				assert.Contains(t, string(body), `"read_only":true`)
			}
		})
	}
	var eventCount int
	require.NoError(t, srv.db.(*db.DB).Reader().QueryRow(t.Context(), "SELECT count(*) FROM recall_query_events").Scan(&eventCount))
	assert.Zero(t, eventCount, "restricted recall requests must never write the query ledger")
	// Native credentials can change while a signed body is still in flight.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+externalPrefix+"/api/v1/version", strings.NewReader("{}"))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer test-bearer")
	req.Header.Set("Expect", "100-continue")
	require.NoError(t, signer.Sign(req))
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	req.Body = reader
	ready := make(chan struct{})
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{Got100Continue: func() { close(ready) }}))
	result := make(chan *http.Response, 1)
	requestErrors := make(chan error, 1)
	go func() {
		resp, err := ts.Client().Do(req)
		if err != nil {
			requestErrors <- err
			return
		}
		if err = resp.Body.Close(); err != nil {
			requestErrors <- err
			return
		}
		result <- resp
	}()
	select {
	case <-ready:
	case err := <-requestErrors:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		require.FailNow(t, "native auth did not reach body validation")
	}
	srv.mu.Lock()
	srv.cfg.AuthToken = "rotated-bearer"
	srv.mu.Unlock()
	_, err = io.WriteString(writer, "{}")
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	select {
	case resp := <-result:
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	case err := <-requestErrors:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		require.FailNow(t, "native credential revocation did not finish")
	}
	unsignedRequest, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+externalPrefix+"/api/v1/version", nil)
	require.NoError(t, err)
	originalLogWriter := log.Writer()
	var requestLog bytes.Buffer
	log.SetOutput(&requestLog)
	defer log.SetOutput(originalLogWriter)
	unsigned, err := ts.Client().Do(unsignedRequest)
	require.NoError(t, err)
	require.NoError(t, unsigned.Body.Close())
	assert.Equal(t, http.StatusUnauthorized, unsigned.StatusCode)
	assert.Contains(t, requestLog.String(), "GET /api/v1/version")
	// Unchanged main listener keeps its local unsigned API behavior.
	main := httptest.NewRecorder()
	srv.Handler().ServeHTTP(main, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:0/api/v1/version", nil))
	assert.Equal(t, http.StatusOK, main.Code)
}

func TestContributorGrantWithoutAuthTokenForbidsReaderRequest(t *testing.T) {
	srv := testServer(t, 30*time.Second)
	srv.cfg.RequireAuth = false
	srv.cfg.AuthToken = ""
	var handler http.Handler
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	defer ts.Close()

	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	require.NoError(t, requestsign.GenerateSecretFile(keyFile))
	key, err := requestsign.ReadSecret(keyFile)
	require.NoError(t, err)
	replayPath := filepath.Join(dir, "replay.db")
	require.NoError(t, requestsign.InitReplay(replayPath))
	policyPath := filepath.Join(dir, "policy.json")
	externalPrefix := "/native"
	policy := requestsign.Policy{
		ExternalURL: ts.URL + externalPrefix,
		ReplayDB:    replayPath,
		Keys:        []requestsign.PolicyKey{{ID: "contributor-key", File: keyFile, Grant: "contributor", DeviceID: "device-1"}},
	}
	data, err := json.Marshal(policy)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(policyPath, data, 0o600))

	var closeState func() error
	handler, closeState, err = srv.RestrictedHandler(policyPath)
	require.NoError(t, err)
	defer closeState()
	signer, err := requestsign.NewSigner(ts.URL+externalPrefix, requestsign.Key{ID: "contributor-key", Secret: key})
	require.NoError(t, err)
	client, err := signer.Client(ts.Client())
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+externalPrefix+"/api/v1/version", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer test-bearer")
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}
