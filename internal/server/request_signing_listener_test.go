package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/requestsign"
)

func TestRestrictedListenerLogsServeError(t *testing.T) {
	var logs bytes.Buffer
	server := &http.Server{ErrorLog: log.New(&logs, "", 0)}
	closed := false
	listener := &RestrictedListener{
		server:   server,
		listener: acceptErrorListener{err: errors.New("listener accept failed")},
		closeState: func() error {
			closed = true
			return nil
		},
	}

	listener.serve()

	assert.Contains(t, logs.String(), "machine signing listener stopped")
	assert.Contains(t, logs.String(), "listener accept failed")
	assert.True(t, closed)
}

type acceptErrorListener struct {
	err error
}

func (l acceptErrorListener) Accept() (net.Conn, error) { return nil, l.err }
func (l acceptErrorListener) Close() error              { return nil }
func (l acceptErrorListener) Addr() net.Addr            { return &net.TCPAddr{} }

func TestRestrictedListenerShutdownDrainsBeforeClosingState(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-release:
			w.WriteHeader(http.StatusNoContent)
		case <-r.Context().Done():
		}
	})}
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var closed atomic.Bool
	listener := &RestrictedListener{server: srv, listener: ln, closeState: func() error { closed.Store(true); return nil }}
	defer listener.close()
	served := make(chan struct{})
	go func() { listener.serve(); close(served) }()
	response := make(chan *http.Response, 1)
	requestErrors := make(chan error, 1)
	go func() {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+ln.Addr().String(), nil)
		if err != nil {
			requestErrors <- err
			return
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			requestErrors <- err
			return
		}
		defer r.Body.Close()
		response <- r
	}()
	<-entered
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- listener.shutdown(ctx) }()
	select {
	case <-served:
	case <-ctx.Done():
		require.FailNow(t, "listener did not stop accepting requests")
	}
	require.False(t, closed.Load())
	close(release)
	select {
	case r := <-response:
		assert.Equal(t, http.StatusNoContent, r.StatusCode)
	case err := <-requestErrors:
		require.NoError(t, err)
	case <-ctx.Done():
		require.FailNow(t, "shutdown aborted an active request")
	}
	require.NoError(t, <-done)
	assert.True(t, closed.Load())
}

func TestServerShutdownStartsBothListenerDrainsConcurrently(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHandlers := func() { releaseOnce.Do(func() { close(release) }) }

	mainEntered, restrictedEntered := make(chan struct{}), make(chan struct{})
	newHandler := func(entered chan struct{}) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			close(entered)
			<-release
			w.WriteHeader(http.StatusNoContent)
		})
	}
	mainListener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	restrictedNetListener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	mainShutdownStarted, restrictedShutdownStarted := make(chan struct{}), make(chan struct{})
	mainServer := &http.Server{Handler: newHandler(mainEntered)}
	mainServer.RegisterOnShutdown(func() { close(mainShutdownStarted) })
	restrictedServer := &http.Server{Handler: newHandler(restrictedEntered)}
	restrictedServer.RegisterOnShutdown(func() { close(restrictedShutdownStarted) })
	ingress := &RestrictedListener{
		server: restrictedServer, listener: restrictedNetListener,
		closeState: func() error { return nil },
	}
	t.Cleanup(func() {
		releaseHandlers()
		_ = mainServer.Close()
		ingress.close()
		_ = mainListener.Close()
		_ = restrictedNetListener.Close()
	})
	go func() { _ = mainServer.Serve(mainListener) }()
	go ingress.serve()
	server := &Server{httpSrv: mainServer, restrictedListener: ingress}

	type responseResult struct {
		status int
		err    error
	}
	responses := make(chan responseResult, 2)
	for _, address := range []string{mainListener.Addr().String(), restrictedNetListener.Addr().String()} {
		go func(address string) {
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+address, nil)
			if err != nil {
				responses <- responseResult{err: err}
				return
			}
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				responses <- responseResult{err: err}
				return
			}
			defer response.Body.Close()
			responses <- responseResult{status: response.StatusCode}
		}(address)
	}
	for _, entered := range []chan struct{}{mainEntered, restrictedEntered} {
		require.Eventually(t, func() bool {
			select {
			case <-entered:
				return true
			default:
				return false
			}
		}, 5*time.Second, 10*time.Millisecond, "listener request did not start")
	}

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- server.Shutdown(t.Context()) }()
	mainStarted, restrictedStarted := false, false
	startDeadline := time.NewTimer(3 * time.Second)
waitForBoth:
	for !mainStarted || !restrictedStarted {
		select {
		case <-mainShutdownStarted:
			mainStarted = true
		case <-restrictedShutdownStarted:
			restrictedStarted = true
		case <-startDeadline.C:
			break waitForBoth
		}
	}
	startDeadline.Stop()
	releaseHandlers()
	require.NoError(t, <-shutdownDone)
	for range 2 {
		result := <-responses
		require.NoError(t, result.err)
		assert.Equal(t, http.StatusNoContent, result.status)
	}
	assert.True(t, mainStarted && restrictedStarted,
		"main and restricted listener shutdown must both start before draining either listener")
}

func TestRestrictedListenerRequiresAuthTokenForReaderGrant(t *testing.T) {
	srv := testServer(t, 30*time.Second)
	srv.mu.Lock()
	srv.cfg.RequireAuth = false
	srv.cfg.AuthToken = ""
	srv.mu.Unlock()

	dir := t.TempDir()
	secretFile := filepath.Join(dir, "secret")
	require.NoError(t, requestsign.GenerateSecretFile(secretFile))
	replayPath := filepath.Join(dir, "replay.db")
	require.NoError(t, requestsign.InitReplay(replayPath))
	policyFile := filepath.Join(dir, "policy.json")
	policy := requestsign.Policy{
		ExternalURL: "https://example.com/native",
		StripPrefix: true,
		ReplayDB:    replayPath,
		Listen:      "127.0.0.1:0",
		Keys:        []requestsign.PolicyKey{{ID: "reader", File: secretFile, Grant: "reader"}},
	}
	data, err := json.Marshal(policy)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(policyFile, data, 0o600))

	listener, err := srv.StartRestrictedListener(t.Context(), policyFile)
	if err == nil {
		require.NoError(t, listener.shutdown(t.Context()))
	}
	require.ErrorContains(t, err, "auth_token")
}

func TestRestrictedListenerRejectsPublicBinding(t *testing.T) {
	srv := testServer(t, 30*time.Second)
	srv.cfg.AuthToken = "test-bearer"
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "secret")
	// A key generated at runtime avoids committing secret fixtures.
	require.NoError(t, requestsign.GenerateSecretFile(secretFile))
	state := filepath.Join(dir, "replay.db")
	require.NoError(t, requestsign.InitReplay(state))
	policyFile := filepath.Join(dir, "policy.json")
	policy := requestsign.Policy{ExternalURL: "https://example.com/native", StripPrefix: true, ReplayDB: state, Listen: "0.0.0.0:0", Keys: []requestsign.PolicyKey{{ID: "reader", File: secretFile, Grant: "reader"}}}
	data, err := json.Marshal(policy)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(policyFile, data, 0o600))
	_, err = srv.StartRestrictedListener(t.Context(), policyFile)
	require.ErrorContains(t, err, "loopback")
	policy.Listen = "127.0.0.1:0"
	data, err = json.Marshal(policy)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(policyFile, data, 0o600))
	listener, err := srv.StartRestrictedListener(t.Context(), policyFile)
	require.NoError(t, err)
	assert.Contains(t, listener.listener.Addr().String(), "127.0.0.1:")
	require.NoError(t, srv.Shutdown(t.Context()))
	require.Error(t, requestsign.GenerateSecretFile(secretFile), "key generation cannot overwrite a key")
}

func TestRestrictedListenerDefaultAvoidsOccupiedHTTPPort(t *testing.T) {
	listenConfig := net.ListenConfig{}
	proxy, err := listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:8081")
	if err != nil {
		t.Skipf("cannot reserve the existing HTTP port: %v", err)
	}
	defer proxy.Close()

	srv := testServer(t, 30*time.Second)
	srv.cfg.AuthToken = "test-bearer"
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "secret")
	require.NoError(t, requestsign.GenerateSecretFile(secretFile))
	state := filepath.Join(dir, "replay.db")
	require.NoError(t, requestsign.InitReplay(state))
	policyFile := filepath.Join(dir, "policy.json")
	policy := requestsign.Policy{
		ExternalURL: "https://example.com/native",
		StripPrefix: true,
		ReplayDB:    state,
		Keys:        []requestsign.PolicyKey{{ID: "reader", File: secretFile, Grant: "reader"}},
	}
	data, err := json.Marshal(policy)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(policyFile, data, 0o600))

	listener, err := srv.StartRestrictedListener(t.Context(), policyFile)
	require.NoError(t, err)
	_, port, err := net.SplitHostPort(listener.listener.Addr().String())
	require.NoError(t, err)
	assert.NotEqual(t, "8081", port, "the default signing listener must not reserve the existing HTTP port")
	require.NoError(t, listener.shutdown(t.Context()))
}
