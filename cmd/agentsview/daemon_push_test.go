package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/postgres"
)

func TestParseDaemonPushSSE(t *testing.T) {
	stream := func(events ...string) string {
		return strings.Join(events, "")
	}
	progressEvent := `event: progress` + "\n" +
		`data: {"SessionsDone":3,"SessionsTotal":10}` + "\n\n"
	doneEvent := `event: done` + "\n" +
		`data: {"SessionsPushed":10,"MessagesPushed":42}` + "\n\n"

	t.Run("progress then done", func(t *testing.T) {
		var progress []postgres.PushProgress
		result, err := parseDaemonPushSSE[postgres.PushResult](
			strings.NewReader(stream(progressEvent, doneEvent)),
			func(p postgres.PushProgress) { progress = append(progress, p) },
		)
		require.NoError(t, err)
		assert.Equal(t, 10, result.SessionsPushed)
		assert.Equal(t, 42, result.MessagesPushed)
		require.Len(t, progress, 1)
		assert.Equal(t, 3, progress[0].SessionsDone)
		assert.Equal(t, 10, progress[0].SessionsTotal)
	})

	t.Run("nil onProgress is safe", func(t *testing.T) {
		result, err := parseDaemonPushSSE[postgres.PushResult, postgres.PushProgress](
			strings.NewReader(stream(progressEvent, doneEvent)), nil,
		)
		require.NoError(t, err)
		assert.Equal(t, 10, result.SessionsPushed)
	})

	t.Run("error event fails the push", func(t *testing.T) {
		errEvent := "event: error\n" + `data: {"error":"schema: boom"}` + "\n\n"
		_, err := parseDaemonPushSSE[postgres.PushResult, postgres.PushProgress](
			strings.NewReader(stream(progressEvent, errEvent)), nil,
		)
		require.Error(t, err)
		assert.Equal(t, "schema: boom", err.Error())
	})

	t.Run("stream without done event fails", func(t *testing.T) {
		_, err := parseDaemonPushSSE[postgres.PushResult, postgres.PushProgress](
			strings.NewReader(stream(progressEvent)), nil,
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "missing done event")
	})
}

// TestPostDaemonPushConsumesSSE pins the daemon-delegated push end to end
// against a stub daemon that streams SSE: progress events reach the callback
// and the done event becomes the returned result.
func TestPostDaemonPushConsumesSSE(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, "/api/v1/push/pg", r.URL.Path)
			require.Contains(t, r.Header.Get("Accept"), "text/event-stream")
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(
				"event: progress\ndata: {\"SessionsDone\":1,\"SessionsTotal\":2}\n\n" +
					"event: done\ndata: {\"SessionsPushed\":2}\n\n"))
		}))
	t.Cleanup(ts.Close)

	var progress []postgres.PushProgress
	result, err := postDaemonPush[postgres.PushResult](
		context.Background(), transport{URL: ts.URL}, "", "/api/v1/push/pg",
		daemonPushRequest{},
		func(p postgres.PushProgress) { progress = append(progress, p) },
	)
	require.NoError(t, err)
	assert.Equal(t, 2, result.SessionsPushed)
	require.Len(t, progress, 1)
	assert.Equal(t, 1, progress[0].SessionsDone)
}

// TestPostDaemonPushJSONFallback pins compatibility with a daemon that
// answers with a plain JSON body instead of an event stream.
func TestPostDaemonPushJSONFallback(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"SessionsPushed":7}`))
		}))
	t.Cleanup(ts.Close)

	result, err := postDaemonPush[postgres.PushResult, postgres.PushProgress](
		context.Background(), transport{URL: ts.URL}, "", "/api/v1/push/pg",
		daemonPushRequest{}, nil,
	)
	require.NoError(t, err)
	assert.Equal(t, 7, result.SessionsPushed)
}
