package requestsign

import (
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Each admission retains another nonce. Compare fixed iteration counts.
func BenchmarkReplayAdmission(b *testing.B) {
	path := filepath.Join(b.TempDir(), "replay.db")
	require.NoError(b, InitReplay(b.Context(), path))
	store, err := OpenReplay(b.Context(), path)
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, store.Close()) })
	nonces := make([]string, b.N)
	for i := range nonces {
		nonces[i] = fmt.Sprintf("%032x", i+1)
	}
	now := time.Now()
	require.NoError(b, store.Admit(b.Context(), "key-a", fmt.Sprintf("%032x", 0), now.Unix(), now.Unix()+30, now))
	b.ReportAllocs()
	b.ResetTimer()
	for _, nonce := range nonces {
		now := time.Now()
		require.NoError(b, store.Admit(b.Context(), "key-a", nonce, now.Unix(), now.Unix()+30, now))
	}
}

// Request creation and signing are outside the timer. Loopback HTTPS exchange,
// verification, durable admission, and dispatch are timed. Nonces accumulate.
func BenchmarkSignedRequestDispatch(b *testing.B) {
	path := filepath.Join(b.TempDir(), "replay.db")
	require.NoError(b, InitReplay(b.Context(), path))
	store, err := OpenReplay(b.Context(), path)
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, store.Close()) })
	secret := make([]byte, 64)
	_, err = rand.Read(secret)
	require.NoError(b, err)
	key := Key{ID: "benchmark-key", Secret: secret, Grant: "reader"}
	var handler http.Handler
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	}))
	b.Cleanup(server.Close)
	verifier, err := NewVerifier(VerifyConfig{
		ExternalURL: server.URL, Replay: store,
		Keys: func() (map[string]Key, error) { return map[string]Key{key.ID: key}, nil },
	})
	require.NoError(b, err)
	signer, err := NewSigner(server.URL, key)
	require.NoError(b, err)
	requests := make([]*http.Request, b.N+1)
	for i := range requests {
		request, err := http.NewRequestWithContext(b.Context(), http.MethodPost,
			server.URL+"/api", strings.NewReader("{}"))
		require.NoError(b, err)
		request.Header.Set("Authorization", "Bearer test")
		require.NoError(b, signer.Sign(request))
		requests[i] = request
	}
	var reached atomic.Int64
	handler = verifier.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}), nil, nil)
	warm, err := server.Client().Do(requests[0])
	require.NoError(b, err)
	require.NoError(b, warm.Body.Close())
	require.Equal(b, http.StatusNoContent, warm.StatusCode)
	b.ReportAllocs()
	b.ResetTimer()
	for _, request := range requests[1:] {
		response, err := server.Client().Do(request)
		require.NoError(b, err)
		require.NoError(b, response.Body.Close())
		require.Equal(b, http.StatusNoContent, response.StatusCode)
	}
	b.StopTimer()
	require.Equal(b, int64(b.N+1), reached.Load())
}
