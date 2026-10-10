package requestsign

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVerifierTamperingRequiredAndReplay(t *testing.T) {
	key := signingKey(t)
	state := filepath.Join(t.TempDir(), "replay.db")
	require.NoError(t, InitReplay(state))
	replay, err := OpenReplay(state)
	require.NoError(t, err)
	defer replay.Close()
	var verifier *Verifier
	var reached int
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		verifier.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached++; w.WriteHeader(http.StatusNoContent) }), nil, nil).ServeHTTP(w, r)
	}))
	defer ts.Close()
	verifier, err = NewVerifier(VerifyConfig{ExternalURL: ts.URL + "/native", Replay: replay, Keys: func() (map[string]Key, error) { return map[string]Key{key.ID: key}, nil }})
	require.NoError(t, err)
	signer, err := NewSigner(ts.URL+"/native", key)
	require.NoError(t, err)
	request := func() *http.Request {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/native/api?q=1", strings.NewReader("{}"))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer test")
		require.NoError(t, signer.Sign(req))
		return req
	}
	for name, mutate := range map[string]func(*http.Request){
		"unsigned":              func(r *http.Request) { r.Header.Del("Signature"); r.Header.Del("Signature-Input") },
		"method":                func(r *http.Request) { r.Method = http.MethodGet },
		"path":                  func(r *http.Request) { r.URL.Path = "/native/other" },
		"query":                 func(r *http.Request) { r.URL.RawQuery = "q=2" },
		"auth":                  func(r *http.Request) { r.Header.Set("Authorization", "Bearer other") },
		"type":                  func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") },
		"device":                func(r *http.Request) { r.Header.Set("X-AgentsView-Device-ID", "other") },
		"offset":                func(r *http.Request) { r.Header.Set("Upload-Offset", "1") },
		"intent":                func(r *http.Request) { r.Header.Set("X-AgentsView-Search-Intent", "other") },
		"missing-covered-field": func(r *http.Request) { r.Header.Del("Upload-Offset") },
		"duplicate":             func(r *http.Request) { r.Header.Add("Authorization", "Bearer second") },
		"digest":                func(r *http.Request) { r.Header.Set("Content-Digest", digest([]byte("different"))) },
		"body":                  func(r *http.Request) { r.Body = io.NopCloser(strings.NewReader("[]")) },
		"key": func(r *http.Request) {
			r.Header.Set("Signature-Input", strings.ReplaceAll(r.Header.Get("Signature-Input"), `keyid="test-key"`, `keyid="other"`))
		},
		"nonce": func(r *http.Request) {
			r.Header.Set("Signature-Input", strings.ReplaceAll(r.Header.Get("Signature-Input"), `;nonce="`, `;nonce="x`))
		},
		"timestamp": func(r *http.Request) {
			r.Header.Set("Signature-Input", strings.ReplaceAll(r.Header.Get("Signature-Input"), `;created=`, `;created=0`))
		},
		"expiry": func(r *http.Request) {
			r.Header.Set("Signature-Input", strings.ReplaceAll(r.Header.Get("Signature-Input"), `;expires=`, `;expires=0`))
		},
		"alg": func(r *http.Request) {
			r.Header.Set("Signature-Input", strings.ReplaceAll(r.Header.Get("Signature-Input"), "hmac-sha256", "hmac-sha512"))
		},
		"encoding": func(r *http.Request) { r.Header.Set("Content-Encoding", "gzip") },
	} {
		t.Run(name, func(t *testing.T) {
			req := request()
			mutate(req)
			resp, err := ts.Client().Do(req)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		})
	}
	assert.Zero(t, reached)
	req := request()
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	retry := req.Clone(t.Context())
	retry.Body, err = req.GetBody()
	require.NoError(t, err)
	resp, err = ts.Client().Do(retry)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Equal(t, 1, reached)
}

func TestVerifierRechecksRevocationAfterBody(t *testing.T) {
	key := signingKey(t)
	state := filepath.Join(t.TempDir(), "replay.db")
	require.NoError(t, InitReplay(state))
	replay, err := OpenReplay(state)
	require.NoError(t, err)
	defer replay.Close()
	var mu sync.Mutex
	active := true
	reads := 0
	var verifier *Verifier
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		verifier.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { assert.Fail(t, "revoked key reached handler") }), func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				active = false
				mu.Unlock()
				next.ServeHTTP(w, r)
			})
		}, nil).ServeHTTP(w, r)
	}))
	defer ts.Close()
	verifier, err = NewVerifier(VerifyConfig{ExternalURL: ts.URL, Replay: replay, Keys: func() (map[string]Key, error) {
		mu.Lock()
		defer mu.Unlock()
		reads++
		if active {
			return map[string]Key{key.ID: key}, nil
		}
		return map[string]Key{}, nil
	}})
	require.NoError(t, err)
	signer, err := NewSigner(ts.URL, key)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/api", bytes.NewReader([]byte("body")))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer test")
	require.NoError(t, signer.Sign(req))
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.GreaterOrEqual(t, reads, 2)
}
