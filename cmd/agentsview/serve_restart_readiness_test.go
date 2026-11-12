package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
)

func TestRestartDuringRuntimePublication(t *testing.T) {
	_, dataDir := isolateServeTestHome(t)
	cmd := newServeCommand()
	require.NoError(t, cmd.Flags().Parse([]string{"--host=127.0.0.1", "--port=0", "--no-sync", "--require-auth=false"}))
	cfg, err := config.LoadPFlags(cmd.Flags())
	require.NoError(t, err)
	opts := serveOptions{NoSyncExplicit: true, RestartArgs: serveRestartArgs(cmd.Flags()), ReloadConfig: func() (config.Config, error) { return config.LoadPFlags(cmd.Flags()) }}
	previousArgs := os.Args
	original := writeDaemonRuntimeWithAuthAndNoSync
	entered, unblock := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(unblock) })
	var writes atomic.Int32
	writeDaemonRuntimeWithAuthAndNoSync = func(lifetime *daemonLifetime, dataDir, host string, port int, version, browserURL string, readOnly, requireAuth, noSync bool, explicitPort *int, caddyPID ...int) (string, error) {
		if writes.Add(1) == 2 {
			close(entered)
			<-unblock
		}
		return original(lifetime, dataDir, host, port, version, browserURL, readOnly, requireAuth, noSync, explicitPort, caddyPID...)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runServeLoop(ctx, cfg, opts, 0, runServe) }()
	t.Cleanup(func() {
		release()
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("server did not stop")
		}
		writeDaemonRuntimeWithAuthAndNoSync = original
		os.Args = previousArgs
	})
	rt := waitForRestartRuntime(t, dataDir)
	base := fmt.Sprintf("http://127.0.0.1:%d", rt.Port)
	restartTestSend(t, http.MethodPost, base+"/api/v1/daemon/restart", "", "", http.StatusOK)
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("replacement did not reach runtime publication")
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/api/v1/daemon/restart", nil)
	require.NoError(t, err)
	req.Header.Set("Origin", base)
	type result struct {
		resp *http.Response
		err  error
	}
	resultCh := make(chan result, 1)
	go func() { resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req); resultCh <- result{resp, err} }()
	var outcome result
	select {
	case outcome = <-resultCh:
		release()
	case <-time.After(500 * time.Millisecond):
		release()
		outcome = <-resultCh
	}
	require.NoError(t, outcome.err)
	defer outcome.resp.Body.Close()
	body, err := io.ReadAll(outcome.resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, outcome.resp.StatusCode, "%s", body)
}

func TestRestartLaunchLockRespectsCancellation(t *testing.T) {
	t.Run("already canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		release, err := waitForegroundServeLaunchLock(ctx, config.Config{DataDir: t.TempDir()})
		if release != nil {
			release()
		}
		require.ErrorIs(t, err, context.Canceled)
	})
	t.Run("canceled while waiting", func(t *testing.T) {
		dir := t.TempDir()
		lock, ok := acquireBackgroundLaunchLock(dir)
		require.True(t, ok)
		t.Cleanup(func() { require.NoError(t, lock.Unlock()) })
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		release, err := waitForegroundServeLaunchLock(ctx, config.Config{DataDir: dir})
		if release != nil {
			release()
		}
		require.ErrorIs(t, err, context.DeadlineExceeded)
	})
}

func TestRestartLaunchLockReportsDirectoryFailure(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err := waitForegroundServeLaunchLock(ctx, config.Config{DataDir: filepath.Join(file, "data")})
	require.ErrorContains(t, err, "creating data dir")
}
