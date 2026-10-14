package requestsign

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
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

func TestVerifierBodyLimitDeadlineAndHeaderBudget(t *testing.T) {
	key := signingKey(t)
	path := filepath.Join(t.TempDir(), "replay.db")
	require.NoError(t, InitReplay(path))
	replay, err := OpenReplay(path)
	require.NoError(t, err)
	defer replay.Close()
	var verifier *Verifier
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		verifier.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }), nil, nil).ServeHTTP(w, r)
	}))
	defer ts.Close()
	verifier, err = NewVerifier(VerifyConfig{ExternalURL: ts.URL, Replay: replay, Keys: func() (map[string]Key, error) { return map[string]Key{key.ID: key}, nil }})
	require.NoError(t, err)
	signer, err := NewSigner(ts.URL, key)
	require.NoError(t, err)
	makeRequest := func(body []byte) *http.Request {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/api", bytes.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer test")
		require.NoError(t, signer.Sign(req))
		return req
	}
	req := makeRequest(make([]byte, MaxBodyBytes))
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	req = makeRequest([]byte("body"))
	large := make([]byte, MaxBodyBytes+1)
	req.ContentLength = int64(len(large))
	req.Body = io.NopCloser(bytes.NewReader(large))
	req.Header.Set("Content-Digest", digest(large))
	params := strings.TrimPrefix(req.Header.Get("Signature-Input"), "sig1=")
	req.Header.Set("Signature", "sig1=:"+base64.StdEncoding.EncodeToString(macValue(key.Secret, signatureBase(req, req.URL.String(), params)))+":")
	resp, err = ts.Client().Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, 413, resp.StatusCode)
	req = makeRequest(nil)
	req.Header.Set("Extra", strings.Repeat("x", MaxHeaderBytes))
	resp, err = ts.Client().Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, 401, resp.StatusCode)
	// A real stalled socket proves context timeout alone is not the implementation.
	verifier.bodyTimeout = time.Second
	req = makeRequest([]byte("body"))
	address := strings.TrimPrefix(ts.URL, "https://")
	config := ts.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	conn, err := (&tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: config}).DialContext(t.Context(), "tcp", address)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	_, err = fmt.Fprintf(conn, "POST /api HTTP/1.1\r\nHost: %s\r\nContent-Length: 4\r\n", address)
	require.NoError(t, err)
	for name, values := range req.Header {
		for _, value := range values {
			_, err = fmt.Fprintf(conn, "%s: %s\r\n", name, value)
			require.NoError(t, err)
		}
	}
	_, err = io.WriteString(conn, "\r\nb")
	require.NoError(t, err)
	data, err := io.ReadAll(conn)
	require.NoError(t, err)
	assert.Contains(t, string(data), "408 Request Timeout")
}

func TestParserRejectsDuplicateParametersAndMalformedMAC(t *testing.T) {
	now := time.Now().Unix()
	nonce := base64.RawURLEncoding.EncodeToString(make([]byte, 24))
	canonical := fmt.Sprintf(`sig1=%s;created=%d;expires=%d;nonce="%s";keyid="key";alg="hmac-sha256"`, components, now, now+30, nonce)
	_, err := parseInput(canonical)
	require.NoError(t, err)
	for _, value := range []string{canonical + `;created=1`, canonical + `,sig2=("@method")`, strings.Replace(canonical, ";keyid=", `;unknown="x";keyid=`, 1), strings.Replace(canonical, "created=", "created=0", 1)} {
		_, err = parseInput(value)
		assert.Error(t, err)
	}
}
