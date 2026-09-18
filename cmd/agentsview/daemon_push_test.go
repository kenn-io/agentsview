package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/server"
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
		assert := assert.New(t)
		require := require.New(t)

		var progress []postgres.PushProgress
		result, err := consumeDaemonPushEvents[postgres.PushResult](daemonEventStream(strings.NewReader(stream(progressEvent, doneEvent))),
			func(p postgres.PushProgress) { progress = append(progress, p) },
		)
		require.NoError(err)
		assert.Equal(10, result.SessionsPushed)
		assert.Equal(42, result.MessagesPushed)
		require.Len(progress, 1)
		assert.Equal(3, progress[0].SessionsDone)
		assert.Equal(10, progress[0].SessionsTotal)
	})

	t.Run("nil onProgress is safe", func(t *testing.T) {
		result, err := consumeDaemonPushEvents[postgres.PushResult, postgres.PushProgress](daemonEventStream(strings.NewReader(stream(progressEvent, doneEvent))), nil)
		require.NoError(t, err)
		assert.Equal(t, 10, result.SessionsPushed)
	})

	t.Run("error event fails the push", func(t *testing.T) {
		errEvent := "event: error\n" + `data: {"error":"schema: boom"}` + "\n\n"
		_, err := consumeDaemonPushEvents[postgres.PushResult, postgres.PushProgress](daemonEventStream(strings.NewReader(stream(progressEvent, errEvent))), nil)
		require.Error(t, err)
		assert.Equal(t, "schema: boom", err.Error())
	})

	t.Run("stream without done event fails", func(t *testing.T) {
		_, err := consumeDaemonPushEvents[postgres.PushResult, postgres.PushProgress](daemonEventStream(strings.NewReader(stream(progressEvent))), nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "missing done event")
	})

	t.Run("terminal report can exceed ten mebibytes", func(t *testing.T) {
		type largeReport struct {
			ProjectMetadata string `json:"project_metadata"`
		}
		metadata := strings.Repeat("x", 10*1024*1024+1)
		reportEvent := "event: report\ndata: {\"project_metadata\":\"" +
			metadata + "\"}\n\n"

		result, err := consumeDaemonPushEvents[largeReport, struct{}](daemonEventStream(strings.NewReader(reportEvent)), nil)
		require.NoError(t, err)
		assert.Equal(t, metadata, result.ProjectMetadata)
	})
}

// TestPostDaemonPushConsumesSSE pins the daemon-delegated push end to end
// against a stub daemon that streams SSE: progress events reach the callback
// and the done event becomes the returned result.
func TestPostDaemonPushConsumesSSE(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ts := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			require.Equal("/api/v1/push/pg", r.URL.Path)
			require.Contains(r.Header.Get("Accept"), "text/event-stream")
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(
				"event: progress\ndata: {\"SessionsDone\":1,\"SessionsTotal\":2}\n\n" +
					"event: done\ndata: {\"SessionsPushed\":2}\n\n"))
		}))
	t.Cleanup(ts.Close)

	var progress []postgres.PushProgress
	result, err := postDaemonPush[postgres.PushResult](
		t.Context(), transport{URL: ts.URL}, "", daemonPushPG,
		apiclient.DaemonPushRequest{},
		func(p postgres.PushProgress) { progress = append(progress, p) },
	)
	require.NoError(err)
	assert.Equal(2, result.SessionsPushed)
	require.Len(progress, 1)
	assert.Equal(1, progress[0].SessionsDone)
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
		t.Context(), transport{URL: ts.URL}, "", daemonPushPG,
		apiclient.DaemonPushRequest{}, nil,
	)
	require.NoError(t, err)
	assert.Equal(t, 7, result.SessionsPushed)
}

func TestDaemonPushWatchTransportRetriesWithoutScopeForOlderSchema(t *testing.T) {
	attempts := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		var body map[string]jsontext.Value
		require.NoError(t, json.UnmarshalRead(r.Body, &body))
		if attempts == 1 {
			assert.Contains(t, body, "watch_batch")
			assert.Contains(t, body, "watch_recovery")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(
				`{"errors":[{"location":"body.watch_batch","message":"unexpected property"}]}`,
			))
			return
		}
		assert.NotContains(t, body, "watch_batch")
		assert.NotContains(t, body, "watch_recovery")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"SessionsPushed":1}`))
	}))
	t.Cleanup(ts.Close)

	batch := apiclient.SyncWatchBatch{Paths: []string{"/sessions/changed.jsonl"}}
	recovery := apiclient.SyncWatchRecoveryScope{
		AvailableRoots: []string{"/sessions"},
		DeferredRoots:  []string{"/offline"},
	}
	result, err := postDaemonPush[postgres.PushResult, postgres.PushProgress](
		t.Context(), transport{URL: ts.URL}, "", daemonPushPG,
		apiclient.DaemonPushRequest{WatchBatch: &batch, WatchRecovery: &recovery}, nil,
	)
	require.NoError(t, err)
	assert.Equal(t, 1, result.SessionsPushed)
	assert.Equal(t, 2, attempts)
}

func TestDaemonPushWatchTransportOmitsScopeForKnownOlderDaemon(t *testing.T) {
	attempts := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		var body map[string]jsontext.Value
		require.NoError(t, json.UnmarshalRead(r.Body, &body))
		assert.NotContains(t, body, "watch_batch")
		assert.NotContains(t, body, "watch_recovery")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"SessionsPushed":1}`))
	}))
	t.Cleanup(ts.Close)

	batch := apiclient.SyncWatchBatch{Paths: []string{"/sessions/changed.jsonl"}}
	result, err := postDaemonPush[postgres.PushResult, postgres.PushProgress](
		t.Context(), transport{
			URL: ts.URL,
			Runtime: &DaemonRuntime{
				API: server.ScopedWatchPushAPIVersion - 1,
			},
		}, "", daemonPushPG,
		apiclient.DaemonPushRequest{WatchBatch: &batch}, nil,
	)
	require.NoError(t, err)
	assert.Equal(t, 1, result.SessionsPushed)
	assert.Equal(t, 1, attempts)
}

func daemonEventStream(body io.Reader) *runtime.Stream[[]byte] {
	return runtime.NewEventStream[[]byte](&http.Response{Body: io.NopCloser(body)})
}
