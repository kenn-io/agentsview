package server_test

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/sessionwatch"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// TestServerTimeouts starts a real HTTP server and verifies that
// streaming connections (SSE) are not closed prematurely by WriteTimeout.
func TestServerTimeouts(t *testing.T) {
	// Set a very short WriteTimeout to verify SSE is exempt.
	writeTimeout := 100 * time.Millisecond
	sleepDuration := 300 * time.Millisecond
	const watchPoll = 25 * time.Millisecond
	t.Cleanup(sessionwatch.SetTimingsForTest(
		watchPoll, 50*time.Millisecond,
	))

	te := setup(t, withWriteTimeout(writeTimeout))

	initial := testjsonl.NewSessionBuilder().
		AddClaudeUser("2025-01-01T00:00:00Z", "initial message")
	sessionPath := te.writeSessionFile(
		t, "test-project", "watch-test.jsonl", initial,
	)

	// Seed the DB.
	te.engine.SyncAll(context.Background(), nil)

	baseURL := te.listenAndServe(t)

	// Connect to the SSE endpoint.
	url := fmt.Sprintf(
		"%s/api/v1/sessions/%s/watch", baseURL, "watch-test",
	)
	ctx, cancel := context.WithTimeout(
		context.Background(), 5*time.Second,
	)
	defer cancel()

	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet, url, nil,
	)
	require.NoError(t, err)

	resp, err := (&http.Client{}).Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Trigger an update after 500ms (> WriteTimeout).
	errCh := make(chan error, 1)
	go func() {
		time.Sleep(sleepDuration)
		f, err := os.OpenFile(
			sessionPath, os.O_APPEND|os.O_WRONLY, 0644,
		)
		if err != nil {
			errCh <- fmt.Errorf("opening file: %w", err)
			return
		}

		update := testjsonl.NewSessionBuilder().
			AddClaudeAssistant(
				"2025-01-01T00:00:05Z", "response",
			)
		if _, err := f.WriteString(update.String()); err != nil {
			f.Close()
			errCh <- fmt.Errorf("writing update: %w", err)
			return
		}
		f.Close()

		// Sync to update the DB — the session monitor polls
		// DB changes, not file mtime.
		te.engine.SyncPaths([]string{sessionPath})
	}()

	readCh := make(chan string)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			if strings.Contains(
				scanner.Text(), "session_updated",
			) {
				readCh <- scanner.Text()
				return
			}
		}
		if err := scanner.Err(); err != nil {
			t.Logf("reading watch stream: %v", err)
		}
		close(readCh)
	}()

	select {
	case writeErr := <-errCh:
		if writeErr != nil {
			t.Fatalf("update writer failed: %v", writeErr)
		}
	case line, ok := <-readCh:
		if !ok {
			t.Fatal("stream closed before receiving update")
		}
		t.Logf("Received delayed event: %s", line)
	case <-ctx.Done():
		t.Fatal("test timed out waiting for delayed event")
	}
}
