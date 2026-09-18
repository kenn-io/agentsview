package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/sessionwatch"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// TestServerTimeouts verifies that the configured request timeout does not
// terminate an SSE handler before it can deliver a later session update.
func TestServerTimeouts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const writeTimeout = 100 * time.Millisecond
		t.Cleanup(sessionwatch.SetTimingsForTest(25*time.Millisecond, 50*time.Millisecond))
		te := setup(t, withWriteTimeout(writeTimeout))
		initial := testjsonl.NewSessionBuilder().
			AddClaudeUser("2025-01-01T00:00:00Z", "initial message")
		path := te.writeSessionFile(t, "test-project", "watch-test.jsonl", initial)
		te.engine.SyncAll(t.Context(), nil)

		ctx, cancel := context.WithCancel(t.Context())
		req := httptest.NewRequestWithContext(ctx, http.MethodGet,
			"/api/v1/sessions/watch-test/watch", nil)
		w := newFlushRecorder()
		done := make(chan struct{})
		go func() {
			defer close(done)
			te.handler.ServeHTTP(w, req)
		}()
		defer func() {
			cancel()
			<-done
		}()
		te.waitForSSEEvent(t, w, "session.timing", 5*time.Second)

		// Advance the fake clock past the configured request timeout.
		time.Sleep(3 * writeTimeout)
		select {
		case <-done:
			require.FailNow(t, "SSE handler stopped at the request timeout")
		default:
		}
		update := testjsonl.NewSessionBuilder().
			AddClaudeAssistant("2025-01-01T00:00:05Z", "response")
		require.NoError(t, os.WriteFile(path, []byte(initial.String()+update.String()), 0o644))
		te.engine.SyncPaths([]string{path})
		te.waitForSSEEvent(t, w, "session_updated", 5*time.Second)
	})
}
